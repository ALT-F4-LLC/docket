// Package daemon locates and serves a store's daemon on a Unix socket.
//
// One daemon serves one store. Its socket lives in a per-user runtime dir that
// only the owning uid can traverse; that directory's mode is the access
// control, because a socket file's own mode follows the umask.
package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// ConnectTimeout bounds every dial to the daemon socket. It covers connect()
// only; a daemon that accepts and never answers needs a per-call deadline.
const ConnectTimeout = 250 * time.Millisecond

// BackoffExpiry is how long a failed auto-start suppresses the next one,
// measured from the backoff marker's mtime.
const BackoffExpiry = 30 * time.Second

var (
	// ErrDaemonRunning reports that another daemon already holds the store's lock.
	ErrDaemonRunning = errors.New("a daemon is already running for this store")
	// ErrBackoff reports that auto-start was skipped because a recent one failed.
	ErrBackoff = errors.New("daemon auto-start is backing off after a recent failure")
)

// Endpoint is a store's daemon address: a verified runtime dir and the name
// hashed from the canonical store dir. Build it with NewEndpoint.
type Endpoint struct {
	dir  string
	name string
	// lookupPeerUID reads the uid at the other end of a socket conn. Nil means
	// the kernel's credential lookup; tests set it to impersonate another uid.
	lookupPeerUID func(net.Conn) (uint32, error)
}

// NewEndpoint verifies (creating it when absent) the per-user runtime dir and
// derives the store's socket name. Client and daemon both start here, so both
// refuse a runtime dir that is a symlink, foreign-owned, or not exactly 0700.
func NewEndpoint(storeDir string) (Endpoint, error) {
	name, err := socketName(storeDir)
	if err != nil {
		return Endpoint{}, err
	}
	dir, err := ensureRuntimeDir()
	if err != nil {
		return Endpoint{}, err
	}
	return Endpoint{dir: dir, name: name}, nil
}

// SocketPath is where the store's daemon listens.
func (e Endpoint) SocketPath() string { return filepath.Join(e.dir, e.name+".sock") }

// checkPeer refuses a conn unless the process at its other end runs as this
// process's euid. A failed lookup, including on a conn that is not a Unix
// socket, is a refusal.
func (e Endpoint) checkPeer(conn net.Conn) error {
	lookup := e.lookupPeerUID
	if lookup == nil {
		lookup = kernelPeerUID
	}
	uid, err := lookup(conn)
	if err != nil {
		return err
	}
	if want := uint32(os.Geteuid()); uid != want {
		return fmt.Errorf("peer runs as uid %d, not %d", uid, want)
	}
	return nil
}

func kernelPeerUID(conn net.Conn) (uint32, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("peer credentials: %T is not a Unix socket conn", conn)
	}
	return peerUID(unixConn)
}

func (e Endpoint) lockPath() string    { return filepath.Join(e.dir, e.name+".lock") }
func (e Endpoint) backoffPath() string { return filepath.Join(e.dir, e.name+".backoff") }

// socketName hashes the canonical store dir, so every spelling of one store
// (relative, through a symlink) names one daemon. A store dir that cannot be
// resolved is an error rather than a hash of the raw string.
func socketName(storeDir string) (string, error) {
	abs, err := filepath.Abs(storeDir)
	if err != nil {
		return "", fmt.Errorf("resolve store dir %q: %w", storeDir, err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve store dir %q: %w", storeDir, err)
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:8]), nil
}

func runtimeDirName() string { return "docket-" + strconv.Itoa(os.Geteuid()) }

// ensureRuntimeDir returns <base>/docket-<euid>, where base is $XDG_RUNTIME_DIR
// when set and os.TempDir() otherwise. An existing entry is refused, never
// repaired, unless it is a real directory owned by the euid with mode 0700.
func ensureRuntimeDir() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, runtimeDirName())

	err := os.Mkdir(dir, 0o700)
	if err == nil {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", fmt.Errorf("runtime dir %s: %w", dir, err)
		}
		return dir, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("create runtime dir %s: %w", dir, err)
	}

	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("runtime dir %s: %w", dir, err)
	}
	switch {
	case !info.IsDir():
		return "", fmt.Errorf("runtime dir %s is not a directory (%v); refusing it", dir, info.Mode().Type())
	case info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()):
		return "", fmt.Errorf("runtime dir %s is owned by uid %d, not %d; refusing it",
			dir, info.Sys().(*syscall.Stat_t).Uid, os.Geteuid())
	case info.Mode().Perm() != 0o700:
		return "", fmt.Errorf("runtime dir %s has mode %#o, want 0700; refusing it", dir, info.Mode().Perm())
	}
	return dir, nil
}

// Listener is a daemon's socket listener. It holds the store's lock for its
// lifetime, which is what keeps a store to one daemon.
type Listener struct {
	net.Listener
	lock     *os.File
	endpoint Endpoint
}

// Listen takes the store's lock, removes a stale socket left by a dead
// daemon, and listens on the endpoint's socket. It returns ErrDaemonRunning
// when another daemon holds the lock; only the lock holder touches the socket
// path, so a refused start never unlinks a live daemon's socket.
func Listen(e Endpoint) (*Listener, error) {
	lock, err := os.OpenFile(e.lockPath(), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrDaemonRunning
		}
		return nil, fmt.Errorf("lock %s: %w", e.lockPath(), err)
	}
	if err := os.Remove(e.SocketPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		lock.Close()
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", e.SocketPath())
	if err != nil {
		lock.Close()
		return nil, err
	}
	return &Listener{Listener: ln, lock: lock, endpoint: e}, nil
}

// Accept returns the next connection from a peer running as this process's
// euid. It closes and skips any other connection, so a rejected peer never
// ends serving; only the socket's own errors are returned.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.endpoint.checkPeer(conn) == nil {
			return conn, nil
		}
		conn.Close()
	}
}

// Close stops listening, which unlinks the socket, then releases the lock.
func (l *Listener) Close() error {
	return errors.Join(l.Listener.Close(), l.lock.Close())
}

// DialFunc dials the daemon socket; it must honor ctx's deadline.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// SpawnFunc starts the store's daemon and returns once it accepts connections.
type SpawnFunc func(ctx context.Context, e Endpoint) error

// Connect dials the store's daemon, each dial bounded by ConnectTimeout. When
// no daemon is listening and spawn is non-nil, it auto-starts one and redials.
// A failed auto-start leaves a backoff marker; until it expires, Connect
// returns ErrBackoff instead of spawning again. A nil dial uses net.Dialer.
// Connect closes, unwritten, any conn whose listener runs as another uid.
func Connect(ctx context.Context, e Endpoint, dial DialFunc, spawn SpawnFunc) (net.Conn, error) {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dialBounded(ctx, dial, e.SocketPath())
	if err == nil {
		return e.verifiedDaemon(conn)
	}
	if spawn == nil || !daemonAbsent(err) {
		return nil, err
	}
	if backingOff(e.backoffPath()) {
		return nil, fmt.Errorf("%w: %w", ErrBackoff, err)
	}
	if err := spawn(ctx, e); err != nil {
		return nil, errors.Join(fmt.Errorf("auto-start daemon: %w", err), markBackoff(e.backoffPath()))
	}
	conn, err = dialBounded(ctx, dial, e.SocketPath())
	if err != nil {
		return nil, errors.Join(fmt.Errorf("dial auto-started daemon: %w", err), markBackoff(e.backoffPath()))
	}
	return e.verifiedDaemon(conn)
}

func (e Endpoint) verifiedDaemon(conn net.Conn) (net.Conn, error) {
	if err := e.checkPeer(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("refusing daemon socket %s: %w", e.SocketPath(), err)
	}
	return conn, nil
}

func dialBounded(ctx context.Context, dial DialFunc, path string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	return dial(ctx, "unix", path)
}

// daemonAbsent reports a dial error meaning nothing listens: no socket file,
// or a stale one. A timeout is not absence; a wedged daemon still holds the lock.
func daemonAbsent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

// backingOff reports an unexpired marker. A future mtime counts as expired,
// so a skewed clock cannot suppress auto-start indefinitely.
func backingOff(marker string) bool {
	info, err := os.Lstat(marker)
	if err != nil {
		return false
	}
	age := time.Since(info.ModTime())
	return age >= 0 && age < BackoffExpiry
}

func markBackoff(marker string) error {
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("write backoff marker: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write backoff marker: %w", err)
	}
	now := time.Now()
	if err := os.Chtimes(marker, now, now); err != nil {
		return fmt.Errorf("refresh backoff marker: %w", err)
	}
	return nil
}
