package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The tree mutex (TDD docs/tdd/gates-trust.md §7.4, threat T13).
//
// §4: "Gates that touch the working tree declare `tree = true` and serialize on
// an engine-held per-repo mutex — parallel read-step completions never race a
// build."

// treeLockRepo builds a repo root with a .docket directory for the lockfile.
func treeLockRepo(t *testing.T) (repoRoot, docketDir string) {
	t.Helper()
	repoRoot = t.TempDir()
	docketDir = filepath.Join(repoRoot, ".docket")
	err := os.MkdirAll(docketDir, 0o700)
	testsupport.Must(t, err, "creating .docket: %v", err)
	return repoRoot, docketDir
}

// TestTreeGatesSerialize is T13: two concurrent `tree = true` holders never
// overlap.
//
// Each records the interval it held the lock; the assertion is that no two
// intervals intersect. Asserting only "both completed" would pass against no
// lock at all.
func TestTreeGatesSerialize(t *testing.T) {
	_, docketDir := treeLockRepo(t)

	type interval struct{ start, end time.Time }
	var (
		mu        sync.Mutex
		intervals []interval
		wg        sync.WaitGroup
	)

	const holders = 4
	for range holders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lock, err := acquireTreeLock(filepath.Join(docketDir, "tree.lock"), 10*time.Second)
			if err != nil {
				t.Errorf("acquireTreeLock: %v", err)
				return
			}
			start := time.Now()
			// Hold it long enough that an unserialized implementation would
			// produce overlapping intervals rather than merely interleaving.
			time.Sleep(20 * time.Millisecond)
			end := time.Now()
			lock.release()

			mu.Lock()
			intervals = append(intervals, interval{start, end})
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(intervals) != holders {
		t.Fatalf("%d holders completed, want %d", len(intervals), holders)
	}
	for i := range intervals {
		for j := i + 1; j < len(intervals); j++ {
			a, b := intervals[i], intervals[j]
			if a.start.Before(b.end) && b.start.Before(a.end) {
				t.Errorf("two tree gates held the mutex concurrently: "+
					"[%s..%s] overlaps [%s..%s]",
					a.start, a.end, b.start, b.end)
			}
		}
	}
}

// TestNonTreeGatesRunConcurrently is L6's converse: a gate that does NOT
// declare `tree` takes no lock at all.
//
// Without it the suite could not tell "the mutex works" from "everything is
// serialized", and engine-core §5's read-only fan-out parallelism — the proven
// win — would be silently lost.
func TestNonTreeGatesRunConcurrently(t *testing.T) {
	repoRoot, docketDir := treeLockRepo(t)

	// The lock is HELD for the whole test. A non-tree gate must not care.
	held, err := acquireTreeLock(filepath.Join(docketDir, "tree.lock"), time.Second)
	testsupport.Must(t, err, "acquireTreeLock: %v", err)
	defer held.release()

	// The runner takes the lock only when the matched entry declares `tree`.
	// With no such entry there is no acquisition, so this returns promptly
	// rather than blocking on the holder above.
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner := NewExecRunner(RepoPaths{
			ExecRoot: repoRoot, Identity: repoRoot,
			LockPath: filepath.Join(docketDir, "tree.lock"),
		})
		runner.LoadStore = sandboxTrust(t) // no entries: unmatched, no lock
		runner.Execute(t.Context(), GateSpec{Name: "tests"}, StepContext{})
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("a gate without `tree = true` blocked on the tree mutex; " +
			"non-tree gates must take no lock (L6)")
	}
}

// TestTreeLockReleasedOnProcessDeath is L1's whole reason for choosing flock
// over a lockfile-with-a-pid: the kernel releases it when the fd closes,
// INCLUDING on SIGKILL.
//
// A pid-file scheme fails exactly this case — it would need stale-lock
// detection, which needs deciding whether a pid is alive, which is the
// probe-once death evidence the design retires.
func TestTreeLockReleasedOnProcessDeath(t *testing.T) {
	_, docketDir := treeLockRepo(t)
	lockPath := filepath.Join(docketDir, "tree.lock")

	// The lock must be held by a process this test can KILL, so that "the
	// kernel released it" is what the assertion below actually measures.
	//
	// The fd is opened and locked here, then INHERITED by a child; closing the
	// local handle leaves the lock living on the child's fd alone. Killing the
	// child is therefore the only thing that can release it — no cleanup code
	// runs, no handler fires, and nothing rewrites the file.
	file, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	testsupport.Must(t, err, "opening the lockfile: %v", err)
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
	testsupport.Must(t, err, "locking: %v", err)

	cmd := exec.Command("/bin/sleep", "60")
	cmd.ExtraFiles = []*os.File{file} // the child inherits the locked fd
	err = cmd.Start()
	testsupport.Must(t, err, "starting the lock holder: %v", err)

	// Release this process's handle. The child's inherited fd still holds it.
	file.Close()

	// The lock is held by the child: an acquisition must NOT succeed.
	if lock, err := acquireTreeLock(filepath.Join(docketDir, "tree.lock"), 300*time.Millisecond); err == nil {
		lock.release()
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatal("acquired the tree mutex while a live process held it")
	}

	// KILL -9. No cleanup runs, no handler fires, nothing rewrites the file.
	err = cmd.Process.Signal(syscall.SIGKILL)
	testsupport.Must(t, err, "killing the lock holder: %v", err)
	cmd.Wait()

	// The kernel released it. A second acquisition succeeds immediately, with
	// no stale-lock detection anywhere in the path.
	lock, err := acquireTreeLock(filepath.Join(docketDir, "tree.lock"), 5*time.Second)
	testsupport.Must(t, err, "the tree mutex was not released by process death (L1): %v", err)
	lock.release()
}

// TestTreeLockRefusesNonRegularFile is L7's table.
//
// `.docket/tree.lock` sits INSIDE the repository, so it is repo-shippable
// content: a hostile repo can commit it as a symlink — to ~/.ssh/config, to a
// device node, to a FIFO — and an ordinary open would follow it. The blast
// radius is small on its own (an flock on a file the operator can already open
// is DoS-shaped), and it is closed anyway because the cost is one Lstat and one
// open flag.
func TestTreeLockRefusesNonRegularFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		// plant creates the hostile thing at the lock path. A nil plant leaves
		// the path missing.
		plant     func(t *testing.T, lockPath, outside string)
		wantRefus bool
	}{
		{
			name: "symlink pointing outside the repo",
			plant: func(t *testing.T, lockPath, outside string) {
				if err := os.Symlink(outside, lockPath); err != nil {
					t.Fatalf("planting a symlink: %v", err)
				}
			},
			wantRefus: true,
		},
		{
			name: "FIFO",
			plant: func(t *testing.T, lockPath, _ string) {
				if err := syscall.Mkfifo(lockPath, 0o600); err != nil {
					t.Fatalf("planting a FIFO: %v", err)
				}
			},
			wantRefus: true,
		},
		{
			name: "directory",
			plant: func(t *testing.T, lockPath, _ string) {
				if err := os.Mkdir(lockPath, 0o700); err != nil {
					t.Fatalf("planting a directory: %v", err)
				}
			},
			wantRefus: true,
		},
		{
			// The negative half, so the check is not refusing everything.
			name: "an ordinary regular file",
			plant: func(t *testing.T, lockPath, _ string) {
				if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
					t.Fatalf("planting a regular file: %v", err)
				}
			},
			wantRefus: false,
		},
		{
			name:      "a missing file",
			plant:     nil,
			wantRefus: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, docketDir := treeLockRepo(t)
			lockPath := filepath.Join(docketDir, "tree.lock")

			// The symlink target lives OUTSIDE the repo, so following it would
			// be observable rather than merely wrong.
			outside := filepath.Join(t.TempDir(), "victim")
			err := os.WriteFile(outside, []byte("do not touch"), 0o600)
			testsupport.Must(t, err, "creating the symlink target: %v", err)

			if tc.plant != nil {
				tc.plant(t, lockPath, outside)
			}

			lock, err := acquireTreeLock(filepath.Join(docketDir, "tree.lock"), time.Second)
			if lock != nil {
				defer lock.release()
			}

			if tc.wantRefus {
				if err == nil {
					t.Fatal("acquired the tree mutex on a non-regular file")
				}
				// The refusal NAMES the path, so an operator is told what to
				// look at rather than only that something was wrong.
				if !strings.Contains(err.Error(), lockPath) {
					t.Errorf("the refusal does not name the path: %v", err)
				}
				// And the victim is untouched: the symlink was not followed.
				body, readErr := os.ReadFile(outside)
				if readErr != nil {
					t.Fatalf("reading the symlink target: %v", readErr)
				}
				if string(body) != "do not touch" {
					t.Error("the symlink was FOLLOWED — the target was modified")
				}
				return
			}

			if err != nil {
				t.Errorf("refused a legitimate lockfile: %v", err)
			}
		})
	}
}
