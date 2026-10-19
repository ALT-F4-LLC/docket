package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// shortRuntimeBase points XDG_RUNTIME_DIR at a fresh short directory. A
// t.TempDir path is long enough to push a socket path past the sun_path limit.
func shortRuntimeBase(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("", "dkt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	t.Setenv("XDG_RUNTIME_DIR", base)
	return base
}

func newStoreDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustEndpoint(t *testing.T, storeDir string) Endpoint {
	t.Helper()
	ep, err := NewEndpoint(storeDir)
	if err != nil {
		t.Fatalf("NewEndpoint(%q): %v", storeDir, err)
	}
	return ep
}

func TestSocketNameIsStablePerStoreDistinctAcrossStoresAndHashed(t *testing.T) {
	shortRuntimeBase(t)
	storeA := newStoreDir(t)
	storeB := newStoreDir(t)

	first := mustEndpoint(t, storeA)
	again := mustEndpoint(t, storeA)
	other := mustEndpoint(t, storeB)

	if first.name != again.name {
		t.Errorf("same store gave two names: %q and %q", first.name, again.name)
	}
	if first.name == other.name {
		t.Errorf("stores %q and %q share the name %q", storeA, storeB, first.name)
	}
	for _, ep := range []Endpoint{first, other} {
		if strings.ContainsRune(ep.name, filepath.Separator) || strings.Contains(ep.name, "store") {
			t.Errorf("name %q carries the raw store path", ep.name)
		}
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(storeA, link); err != nil {
		t.Fatal(err)
	}
	if viaLink := mustEndpoint(t, link); viaLink.name != first.name {
		t.Errorf("symlink to the store named %q, the store itself %q", viaLink.name, first.name)
	}
}

func TestNewEndpointRefusesMissingStoreDir(t *testing.T) {
	shortRuntimeBase(t)
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := NewEndpoint(missing); err == nil {
		t.Fatalf("NewEndpoint(%q) succeeded on a store dir that does not exist", missing)
	}
}

func TestRuntimeDirIsCreated0700AndOwnedByCurrentUID(t *testing.T) {
	base := shortRuntimeBase(t)
	ep := mustEndpoint(t, newStoreDir(t))

	if filepath.Dir(ep.dir) != base {
		t.Errorf("runtime dir %q is not under XDG_RUNTIME_DIR %q", ep.dir, base)
	}
	info, err := os.Lstat(ep.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("runtime dir %q is %v, not a directory", ep.dir, info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("runtime dir mode = %#o, want 0700", perm)
	}
	if uid := info.Sys().(*syscall.Stat_t).Uid; int(uid) != os.Geteuid() {
		t.Errorf("runtime dir owner = %d, want %d", uid, os.Geteuid())
	}

	if again := mustEndpoint(t, newStoreDir(t)); again.dir != ep.dir {
		t.Errorf("an existing 0700 runtime dir was not reused: %q then %q", ep.dir, again.dir)
	}
}

func TestRuntimeDirRefusesLooseMode(t *testing.T) {
	base := shortRuntimeBase(t)
	loose := filepath.Join(base, runtimeDirName())
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := NewEndpoint(newStoreDir(t))
	if err == nil || !strings.Contains(err.Error(), loose) {
		t.Fatalf("NewEndpoint over a 0755 runtime dir: err = %v, want a refusal naming %q", err, loose)
	}
}

func TestRuntimeDirRefusesSymlink(t *testing.T) {
	base := shortRuntimeBase(t)
	target := t.TempDir()
	if err := os.Chmod(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, runtimeDirName())
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := NewEndpoint(newStoreDir(t))
	if err == nil || !strings.Contains(err.Error(), link) {
		t.Fatalf("NewEndpoint over a symlinked runtime dir: err = %v, want a refusal naming %q", err, link)
	}
}

func TestListenAcceptsSameProcessConnectionInsideRuntimeDir(t *testing.T) {
	shortRuntimeBase(t)
	ep := mustEndpoint(t, newStoreDir(t))

	ln, err := Listen(ep)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().String()
	if filepath.Dir(addr) != ep.dir {
		t.Errorf("listener bound at %q, outside runtime dir %q", addr, ep.dir)
	}
	sock, err := os.Lstat(filepath.Join(ep.dir, filepath.Base(addr)))
	if err != nil {
		t.Fatalf("socket file missing from runtime dir: %v", err)
	}
	if sock.Mode().Type() != os.ModeSocket {
		t.Errorf("%q is %v, not a socket", addr, sock.Mode())
	}
	dirInfo, err := os.Lstat(ep.dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("socket's runtime dir mode = %#o, want 0700", perm)
	}

	accepted := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
		accepted <- err
	}()

	conn, err := Connect(context.Background(), ep, nil, nil)
	if err != nil {
		t.Fatalf("Connect to a live daemon: %v", err)
	}
	conn.Close()
	if err := <-accepted; err != nil {
		t.Fatalf("Accept: %v", err)
	}
}

func TestListenRefusesSecondDaemonForSameStore(t *testing.T) {
	shortRuntimeBase(t)
	ep := mustEndpoint(t, newStoreDir(t))

	first, err := Listen(ep)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer first.Close()

	if second, err := Listen(ep); !errors.Is(err, ErrDaemonRunning) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second Listen: err = %v, want ErrDaemonRunning", err)
	}

	go func() {
		if conn, err := first.Accept(); err == nil {
			conn.Close()
		}
	}()
	conn, err := Connect(context.Background(), ep, nil, nil)
	if err != nil {
		t.Fatalf("the refused second start broke the first daemon's socket: %v", err)
	}
	conn.Close()
}

func TestListenReplacesStaleSocket(t *testing.T) {
	shortRuntimeBase(t)
	ep := mustEndpoint(t, newStoreDir(t))

	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: ep.SocketPath(), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()

	ln, err := Listen(ep)
	if err != nil {
		t.Fatalf("Listen over a stale socket file: %v", err)
	}
	ln.Close()
}

func TestConnectTimesOutOnBlockingDialer(t *testing.T) {
	shortRuntimeBase(t)
	ep := mustEndpoint(t, newStoreDir(t))

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	blocking := func(ctx context.Context, _, _ string) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, errors.New("released by test cleanup")
		}
	}

	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		conn, err := Connect(context.Background(), ep, blocking, nil)
		if conn != nil {
			conn.Close()
		}
		done <- result{err, time.Since(start)}
	}()

	const tolerance = 250 * time.Millisecond
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("Connect through a blocking dialer returned no error")
		}
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Errorf("Connect err = %v, want the connect deadline", r.err)
		}
		if r.elapsed > ConnectTimeout+tolerance {
			t.Errorf("Connect returned after %v, want within ConnectTimeout %v (+%v)", r.elapsed, ConnectTimeout, tolerance)
		}
	case <-time.After(ConnectTimeout + tolerance):
		t.Fatalf("Connect still blocked after ConnectTimeout %v (+%v)", ConnectTimeout, tolerance)
	}
}

func TestConnectAfterFailedAutoStartLeavesBackoffAndSkipsSpawn(t *testing.T) {
	shortRuntimeBase(t)
	ep := mustEndpoint(t, newStoreDir(t))

	var spawns atomic.Int32
	failingSpawn := func(context.Context, Endpoint) error {
		spawns.Add(1)
		return errors.New("daemon exited at startup")
	}

	if _, err := Connect(context.Background(), ep, nil, failingSpawn); err == nil {
		t.Fatal("Connect succeeded with no daemon and a failing spawner")
	}
	if n := spawns.Load(); n != 1 {
		t.Fatalf("first Connect spawned %d times, want 1", n)
	}
	marker, err := os.Lstat(ep.backoffPath())
	if err != nil {
		t.Fatalf("no backoff marker after a failed auto-start: %v", err)
	}
	if !marker.Mode().IsRegular() || marker.Mode().Perm() != 0o600 {
		t.Errorf("backoff marker mode = %v, want a regular 0600 file", marker.Mode())
	}

	_, err = Connect(context.Background(), ep, nil, failingSpawn)
	if !errors.Is(err, ErrBackoff) {
		t.Errorf("second Connect err = %v, want ErrBackoff", err)
	}
	if n := spawns.Load(); n != 1 {
		t.Errorf("second Connect inside the backoff spawned again: %d spawns", n)
	}
}

func TestConnectSpawnsAgainOnceBackoffExpires(t *testing.T) {
	shortRuntimeBase(t)
	ep := mustEndpoint(t, newStoreDir(t))

	var spawns atomic.Int32
	failingSpawn := func(context.Context, Endpoint) error {
		spawns.Add(1)
		return errors.New("daemon exited at startup")
	}
	if _, err := Connect(context.Background(), ep, nil, failingSpawn); err == nil {
		t.Fatal("Connect succeeded with no daemon and a failing spawner")
	}

	for _, mtime := range []time.Time{
		time.Now().Add(-BackoffExpiry - time.Second),
		time.Now().Add(time.Hour),
	} {
		if err := os.Chtimes(ep.backoffPath(), mtime, mtime); err != nil {
			t.Fatal(err)
		}
		before := spawns.Load()
		if _, err := Connect(context.Background(), ep, nil, failingSpawn); errors.Is(err, ErrBackoff) {
			t.Errorf("marker mtime %v still held the backoff: %v", mtime, err)
		}
		if spawns.Load() != before+1 {
			t.Errorf("marker mtime %v: auto-start did not run", mtime)
		}
	}
}

func TestConnectAutoStartsAbsentDaemon(t *testing.T) {
	shortRuntimeBase(t)
	ep := mustEndpoint(t, newStoreDir(t))

	var ln *Listener
	spawn := func(_ context.Context, ep Endpoint) error {
		var err error
		ln, err = Listen(ep)
		if err == nil {
			go func() {
				if conn, err := ln.Accept(); err == nil {
					conn.Close()
				}
			}()
		}
		return err
	}

	conn, err := Connect(context.Background(), ep, nil, spawn)
	if err != nil {
		t.Fatalf("Connect with a working spawner: %v", err)
	}
	conn.Close()
	ln.Close()
	if _, err := os.Lstat(ep.backoffPath()); !os.IsNotExist(err) {
		t.Errorf("a successful auto-start left a backoff marker: %v", err)
	}
}

func foreignUID() uint32 { return uint32(os.Geteuid()) + 1 }

func TestListenerClosesConnectionFromForeignUIDAndKeepsAccepting(t *testing.T) {
	shortRuntimeBase(t)
	ep := mustEndpoint(t, newStoreDir(t))
	var lookups atomic.Int32
	ep.lookupPeerUID = func(net.Conn) (uint32, error) {
		if lookups.Add(1) == 1 {
			return foreignUID(), nil
		}
		return uint32(os.Geteuid()), nil
	}

	ln, err := Listen(ep)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()

	foreign, err := net.Dial("unix", ep.SocketPath())
	if err != nil {
		t.Fatalf("dial as the foreign peer: %v", err)
	}
	defer foreign.Close()
	foreign.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = foreign.Read(make([]byte, 1))
	var netErr net.Error
	if err == nil || (errors.As(err, &netErr) && netErr.Timeout()) {
		t.Fatalf("foreign-uid conn read err = %v, want the server to have closed it", err)
	}

	owner, err := net.Dial("unix", ep.SocketPath())
	if err != nil {
		t.Fatalf("dial as the owner: %v", err)
	}
	defer owner.Close()
	served, ok := <-accepted
	if !ok {
		t.Fatal("Accept failed instead of returning the owner's conn")
	}
	defer served.Close()
	if _, err := served.Write([]byte{'k'}); err != nil {
		t.Fatalf("write to the accepted conn: %v", err)
	}
	owner.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := owner.Read(buf); err != nil || buf[0] != 'k' {
		t.Fatalf("Accept returned a conn other than the owner's: read %q, err %v", buf, err)
	}
}

// recordingConn counts the bytes written to it and whether it was closed.
type recordingConn struct {
	net.Conn
	written atomic.Int64
	closed  atomic.Bool
}

func (c *recordingConn) Write(p []byte) (int, error) {
	c.written.Add(int64(len(p)))
	return len(p), nil
}

func (c *recordingConn) Close() error {
	c.closed.Store(true)
	return nil
}

func TestConnectRefusesForeignUIDDaemonWithoutWriting(t *testing.T) {
	foreign := func(net.Conn) (uint32, error) { return foreignUID(), nil }
	cases := []struct {
		name       string
		lookup     func(net.Conn) (uint32, error)
		firstDial  error
		wantSpawns int32
	}{
		{name: "first dial", lookup: foreign},
		{name: "redial after auto-start", lookup: foreign, firstDial: syscall.ECONNREFUSED, wantSpawns: 1},
		{name: "non-Unix conn under the kernel lookup"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortRuntimeBase(t)
			ep := mustEndpoint(t, newStoreDir(t))
			ep.lookupPeerUID = tc.lookup

			rec := &recordingConn{}
			var dials atomic.Int32
			dial := func(context.Context, string, string) (net.Conn, error) {
				if dials.Add(1) == 1 && tc.firstDial != nil {
					return nil, tc.firstDial
				}
				return rec, nil
			}
			var spawns atomic.Int32
			spawn := func(context.Context, Endpoint) error {
				spawns.Add(1)
				return nil
			}

			conn, err := Connect(context.Background(), ep, dial, spawn)
			if err == nil {
				conn.Write([]byte("lease-token"))
				t.Error("Connect to a daemon of another uid returned no error")
			} else if conn != nil {
				t.Error("Connect returned a conn alongside its refusal")
			}
			if n := rec.written.Load(); n != 0 {
				t.Errorf("%d bytes written to the foreign daemon, want 0", n)
			}
			if !rec.closed.Load() {
				t.Error("the refused conn was left open")
			}
			if n := spawns.Load(); n != tc.wantSpawns {
				t.Errorf("spawned %d times, want %d", n, tc.wantSpawns)
			}
		})
	}
}
