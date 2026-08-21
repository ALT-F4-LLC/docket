package trust

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockTimeout is W5's bound on acquisition.
//
// A `trust add` is a human-scale operation and 5s is far beyond its honest
// duration, so a timeout means something is genuinely STUCK rather than merely
// contended. Exceeding it is a CONFLICT naming the lock path, not a silent
// wait forever.
const lockTimeout = 5 * time.Second

// lockPollInterval is how often a blocked acquirer retries. flock(2) has a
// blocking mode, but a blocking syscall cannot be bounded by a timeout without
// a signal, so the bound is implemented by polling LOCK_NB. The interval is
// short enough that an uncontended-after-a-moment case is not perceptibly
// delayed and long enough not to spin a core.
const lockPollInterval = 10 * time.Millisecond

// storeLock is an exclusive flock held across a whole read-modify-write.
type storeLock struct {
	f *os.File
}

// acquireLock takes W1's exclusive flock for the store at path.
//
// WHY A LOCK AT ALL (§3.5.1): I5's temp-file-plus-rename makes each WRITE
// atomic, but it does NOT make the READ-MODIFY-WRITE atomic — and `trust add`
// and `trust rm` are exactly that. Two concurrent adds of different names
// interleave as read-A / read-B / write-A / write-B, and B's write SILENTLY
// DROPS A's entry. The operator sees two successful adds and has one.
//
// That is not hypothetical for this design: the posture is that a session runs
// `trust add --yes`, and a run with parallel steps can have more than one
// session doing so while an operator also types one by hand. Losing an entry is
// not a security hole — it fails closed, and the lost gate goes unmatched — but
// it is a SILENT failure of an authorization operation the operator was told
// succeeded.
func acquireLock(path string) (*storeLock, error) {
	lockPath := path + ".lock"

	// W3: the lockfile is opened with §3.2's integrity discipline. It lives in
	// the user-owned config directory, so the exposure is smaller than the
	// repo-resident tree lock's — but it is the same rule, and stating it once
	// here means an implementer does not have to decide.
	if err := checkLockfileIntegrity(lockPath); err != nil {
		return nil, err
	}

	// O_NOFOLLOW: refuse to follow a symlink at the lock path, for the same
	// reason I1 refuses one at the store path.
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, storeFileMode)
	if err != nil {
		return nil, fmt.Errorf("%w: opening the lock file %s: %v", ErrIntegrity, lockPath, err)
	}

	deadline := time.Now().Add(lockTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &storeLock{f: f}, nil
		}
		if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, fmt.Errorf("locking the trust store %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			// W5: a CONFLICT naming the lock path, so the operator knows what
			// to look for rather than only that something failed.
			return nil, fmt.Errorf("%w: another `docket trust` is in progress; the lock %s was still held after %s", ErrConflict, lockPath, lockTimeout)
		}
		time.Sleep(lockPollInterval)
	}
}

// release drops the lock.
//
// The lock is released by FD CLOSE, so a crashed or killed `docket trust`
// leaves no stale lock — the same property that makes flock the right choice
// here and for the tree mutex. There is no stale-lock detection to write,
// because there is no stale lock to detect.
func (l *storeLock) release() {
	if l == nil || l.f == nil {
		return
	}
	// The explicit unlock is redundant with Close but makes the intent legible
	// and keeps the ordering obvious if a future edit adds work between them.
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

// checkLockfileIntegrity is W3: refuse a non-regular existing file, and check
// owner and mode as I1–I4 check the store.
//
// W2 explains why the lock is on a SIBLING trust.toml.lock rather than on
// trust.toml itself: locking the store file directly does not work with
// rename-based writes, because the rename REPLACES THE INODE — the lock the
// writer holds and the lock the next writer takes end up on different files and
// exclude nothing. The sibling's inode is stable.
func checkLockfileIntegrity(lockPath string) error {
	info, err := os.Lstat(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%w: cannot stat the lock file %s: %v", ErrIntegrity, lockPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: the lock file %s is a symlink, not a regular file", ErrIntegrity, lockPath)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: the lock file %s is %s, not a regular file", ErrIntegrity, lockPath, describeMode(info.Mode()))
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: the lock file %s has mode %04o, but it must be %04o; fix it with: chmod 600 %s", ErrIntegrity, lockPath, perm, storeFileMode, lockPath)
	}
	owner, ok := fileOwner(info)
	if ok && owner != os.Getuid() {
		return fmt.Errorf("%w: the lock file %s is owned by uid %d, but docket is running as uid %d", ErrIntegrity, lockPath, owner, os.Getuid())
	}
	return nil
}
