package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// The tree mutex (TDD docs/tdd/gates-trust.md §7.4, threat T13).
//
// §4, verbatim: "Gates that touch the working tree declare `tree = true` and
// serialize on an engine-held per-repo mutex — parallel read-step completions
// never race a build."
//
// IT CANNOT BE THE DATABASE TRANSACTION, and the spec's own rules say why:
// gates run OUTSIDE transactions (engine-spec §6, "No subprocess ever executes
// inside a transaction"). A transaction held across a subprocess would also
// serialize every unrelated engine operation for the duration of a build, and
// would be released by a crash BEFORE the subprocess it was protecting had
// exited.
//
// The mechanism is an OS-level advisory file lock on `.docket/tree.lock`.
// flock(2) is chosen over a lockfile-with-a-pid for ONE property (L1): the
// kernel releases an flock when the fd closes, INCLUDING on SIGKILL. A
// pid-file scheme would need stale-lock detection, which needs deciding whether
// a pid is alive — the "probe-once death evidence" doctrine engine-core §5
// retires.

// treeLockPollInterval bounds how often a blocked acquisition retries. flock's
// blocking mode cannot be given a deadline directly, so the lock is taken
// non-blocking on a poll — which is what lets L4 bound the wait by the gate's
// own timeout instead of hanging forever.
const treeLockPollInterval = 10 * time.Millisecond

// inProcessTreeLocks serializes tree gates WITHIN one process, keyed by the
// lockfile path (L2, L5) — one file, one mutex, whether the file is a repo's
// own `.docket/tree.lock` or a per-project lock under the global store.
//
// BOTH LOCKS ARE REQUIRED, and this is the clause an implementation drops.
// flock semantics between two fds in the SAME process are not exclusion: two
// goroutines in one engine each opening the lockfile would both "acquire" it.
// So two goroutines in one engine serialize on the Go mutex, and two engine
// processes serialize on the flock. Either one alone silently races.
var inProcessTreeLocks sync.Map // lockfile path -> *sync.Mutex

func inProcessTreeLock(lockPath string) *sync.Mutex {
	actual, _ := inProcessTreeLocks.LoadOrStore(lockPath, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

// treeLock is a held tree mutex. Release drops both halves, in the order that
// makes a later acquisition see a consistent state.
type treeLock struct {
	file  *os.File
	mutex *sync.Mutex
}

// acquireTreeLock takes the per-project tree mutex, blocking up to timeout
// (L4). The lockfile path comes from config.TreeLockPath: a repo-resident
// `.docket/tree.lock` for env/local stores, a per-project file under the
// global store's `locks/` otherwise.
//
// Blocking rather than failing fast is correct: the whole purpose is to make
// the second gate WAIT for the first. Exceeding the bound is the caller's
// signal to record verdict='fail' with a reason naming the wait.
func acquireTreeLock(path string, timeout time.Duration) (*treeLock, error) {
	if path == "" {
		return nil, fmt.Errorf(
			"no tree lockfile could be resolved for this invocation; " +
				"refusing to run a tree-declaring command unserialized")
	}
	mu := inProcessTreeLock(path)

	// The in-process half first, bounded by the same deadline: a goroutine
	// blocked here is waiting on a sibling gate in this very process, which is
	// the case flock cannot see at all.
	deadline := time.Now().Add(timeout)
	if !lockMutexBefore(mu, deadline) {
		return nil, fmt.Errorf(
			"waited longer than %s for the working-tree mutex held by another gate in this process",
			timeout)
	}

	file, err := openTreeLockFile(path)
	if err != nil {
		mu.Unlock()
		return nil, err
	}

	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &treeLock{file: file, mutex: mu}, nil
		}
		if err != syscall.EWOULDBLOCK {
			file.Close()
			mu.Unlock()
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			file.Close()
			mu.Unlock()
			return nil, fmt.Errorf(
				"waited longer than %s for the working-tree mutex on %s, held by another docket process",
				timeout, path)
		}
		time.Sleep(treeLockPollInterval)
	}
}

// openTreeLockFile opens the lockfile with §3.2's integrity discipline (L7).
//
// `.docket/tree.lock` SITS INSIDE THE REPOSITORY, so it is repo-shippable
// content: a hostile repo can commit it as a SYMLINK — to ~/.ssh/config, to a
// device node, to a FIFO — and an ordinary open would follow it. O_NOFOLLOW
// refuses the symlink at the syscall, and the Lstat refuses every other
// non-regular file with the identical language §3.2 I1 established for the
// trust file.
//
// The blast radius is small on its own — an flock on a file the operator can
// already open is DoS-shaped, not an execution or disclosure path — and it is
// closed anyway because the cost is one Lstat and one open flag, and because
// leaving one repo-resident file opened without the discipline established for
// another is exactly the inconsistency a later refactor generalizes the wrong
// way.
func openTreeLockFile(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf(
			"%s is %s, not a regular file; docket will not lock it",
			path, describeFileKind(info.Mode()))
	}

	// The global store's `locks/` directory does not exist until the first
	// tree gate needs it. Creating it here rather than at init keeps the
	// store's layout an implementation detail of the code that locks, and a
	// repo-resident lockfile's parent (`.docket/`) already exists, so this is
	// a no-op there.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf(
			"creating the lock directory for %s: %w", path, err)
	}

	file, err := os.OpenFile(path,
		os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf(
			"opening the working-tree lockfile %s: %w", path, err)
	}
	return file, nil
}

// describeFileKind names what was found, so the refusal tells an operator what
// to look at rather than only that something was wrong.
func describeFileKind(m os.FileMode) string {
	switch {
	case m&os.ModeSymlink != 0:
		return "a symlink"
	case m&os.ModeNamedPipe != 0:
		return "a FIFO"
	case m.IsDir():
		return "a directory"
	case m&os.ModeDevice != 0:
		return "a device"
	case m&os.ModeSocket != 0:
		return "a socket"
	default:
		return "not a regular file"
	}
}

// lockMutexBefore takes a mutex, giving up at the deadline. sync.Mutex has no
// timed acquisition, so this polls TryLock.
func lockMutexBefore(mu *sync.Mutex, deadline time.Time) bool {
	for {
		if mu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(treeLockPollInterval)
	}
}

// release drops the flock and the in-process mutex.
//
// The flock is released by CLOSING THE FD, which is also what makes L1 hold
// under SIGKILL: a crashed engine leaves no stale lock for anyone to clear.
func (l *treeLock) release() {
	if l == nil {
		return
	}
	if l.file != nil {
		syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		l.file.Close()
	}
	if l.mutex != nil {
		l.mutex.Unlock()
	}
}
