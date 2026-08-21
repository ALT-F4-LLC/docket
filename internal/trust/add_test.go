package trust

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// --- Add/rm semantics (§3.5) ------------------------------------------------

func TestAddIsIdempotentForAnIdenticalEntry(t *testing.T) {
	path := sandbox(t)
	repo := t.TempDir()
	req := AddRequest{Name: "checks", Argv: []string{"make", "test"}, RepoRoot: repo, ReRunnable: true}

	first, err := addAt(path, req)
	testsupport.Must(t, err, "first add: %v", err)
	if first.Idempotent {
		t.Error("the first add is an insert, not an idempotent no-op")
	}

	// Re-approving the SAME command with the SAME flags is not a change.
	second, err := addAt(path, req)
	testsupport.Must(t, err, "second add of an identical entry must succeed: %v", err)
	if !second.Idempotent {
		t.Error("an identical re-add must report itself idempotent")
	}

	st, err := loadAt(path)
	testsupport.Must(t, err, "loadAt: %v", err)
	if len(st.Entries) != 1 {
		t.Errorf("an idempotent add must not duplicate the entry; got %d entries", len(st.Entries))
	}
}

// TestAddConflictsOnDifferingArgv is §3.5's important row.
//
// A SILENT OVERWRITE would mean a trusted name's meaning can change without the
// operator ever seeing the old value — the same reasoning that makes a
// re-register with differing bytes a CONFLICT, applied to a security-relevant
// file. Both argvs are named so the operator can see exactly what would have
// changed.
func TestAddConflictsOnDifferingArgv(t *testing.T) {
	path := sandbox(t)
	repo := t.TempDir()

	_, err := addAt(path, AddRequest{Name: "checks", Argv: []string{"make", "test"}, RepoRoot: repo})
	testsupport.Must(t, err, "first add: %v", err)

	_, err = addAt(path, AddRequest{Name: "checks", Argv: []string{"make", "deploy"}, RepoRoot: repo})
	if err == nil {
		t.Fatal("a differing argv at the same name+repo must be a CONFLICT, never a silent overwrite")
	}
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict, got %v", err)
	}
	// BOTH argvs are named.
	if !strings.Contains(err.Error(), "make") || !strings.Contains(err.Error(), "test") {
		t.Errorf("the conflict must name the existing argv; got: %v", err)
	}
	if !strings.Contains(err.Error(), "deploy") {
		t.Errorf("the conflict must name the proposed argv; got: %v", err)
	}
	// And it instructs the remedy.
	if !strings.Contains(err.Error(), "trust rm") {
		t.Errorf("the conflict must instruct `trust rm` first; got: %v", err)
	}

	// The ORIGINAL entry survives the refused add. A conflict that half-applied
	// would be worse than either outcome.
	st, err := loadAt(path)
	testsupport.Must(t, err, "loadAt: %v", err)
	if len(st.Entries) != 1 || st.Entries[0].Argv[1] != "test" {
		t.Errorf("the refused add must leave the original entry intact; got %+v", st.Entries)
	}
}

func TestAddConflictsOnDifferingFlags(t *testing.T) {
	// Differing FLAGS conflict too, not only a differing argv: `prefix` and
	// `re_runnable` change what is authorized and how a crash is handled, so
	// silently changing either is the same failure as changing the command.
	repo := t.TempDir()

	for _, tc := range []struct {
		name   string
		second AddRequest
	}{
		{"prefix", AddRequest{Prefix: true}},
		{"re_runnable", AddRequest{ReRunnable: true}},
		{"tree", AddRequest{Tree: true}},
		{"flaky", AddRequest{Flaky: true}},
		{"timeout", AddRequest{Timeout: "30s"}},
		// `network` was the row this table was missing, and the omission was
		// not cosmetic: the declared host list went uncompared, so an add that
		// asked for new egress at an existing name reported "already trusted",
		// wrote nothing, and exited 0 — the operator's requested widening
		// discarded under a success message.
		{"network", AddRequest{Network: []string{"vuln.go.dev"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := sandbox(t)
			base := AddRequest{Name: "checks", Argv: []string{"make", "test"}, RepoRoot: repo}
			_, err := addAt(path, base)
			testsupport.Must(t, err, "first add: %v", err)

			second := tc.second
			second.Name, second.Argv, second.RepoRoot = base.Name, base.Argv, base.RepoRoot
			_, err = addAt(path, second)
			if !errors.Is(err, ErrConflict) {
				t.Errorf("a differing %s must be a CONFLICT; got %v", tc.name, err)
			}
			// The refusal NAMES the property. Without it a flags-only conflict
			// printed the same argv twice — "already trusted as X; it would
			// become X" — which reads as a bug in docket rather than as the
			// refusal it is, and leaves the operator no way to see what they
			// changed.
			if err != nil && !strings.Contains(err.Error(), tc.name) {
				t.Errorf("the conflict must name the property that differs (%s); got: %v", tc.name, err)
			}
		})
	}
}

// TestOnChangeRunsBeforeTheStoreIsWritten is DKT-81's ordering guarantee: the
// caller's record of a change lands BEFORE the change, so a store that gained an
// entry can never be a store whose grant went unrecorded.
//
// The hook asserts from INSIDE that the store does not yet hold the entry, which
// is the only way to observe the order — a test that checked afterwards would
// pass against a hook called after the write.
func TestOnChangeRunsBeforeTheStoreIsWritten(t *testing.T) {
	path := sandbox(t)
	repo := t.TempDir()

	var called int
	_, err := addAt(path, AddRequest{
		Name: "checks", Argv: []string{"make", "test"}, RepoRoot: repo,
		OnChange: func(e Entry) error {
			called++
			if e.Name != "checks" || e.ArgvSHA256 != ArgvSHA256([]string{"make", "test"}) {
				t.Errorf("the hook must receive the entry being written; got %+v", e)
			}
			st, loadErr := loadAt(path)
			testsupport.Must(t, loadErr, "loadAt inside the hook: %v", loadErr)
			if len(st.Entries) != 0 {
				t.Errorf("the hook ran AFTER the store was written; got %+v", st.Entries)
			}
			return nil
		},
	})
	testsupport.Must(t, err, "addAt: %v", err)
	if called != 1 {
		t.Errorf("the hook ran %d times, want exactly 1", called)
	}
}

// TestOnChangeFailureWritesNothing is the other half: the hook's error aborts
// the add. A grant that landed anyway would be exactly the divergence the
// ordering exists to close.
func TestOnChangeFailureWritesNothing(t *testing.T) {
	path := sandbox(t)
	repo := t.TempDir()

	boom := errors.New("the record could not be written")
	_, err := addAt(path, AddRequest{
		Name: "checks", Argv: []string{"make", "test"}, RepoRoot: repo,
		OnChange: func(Entry) error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("the hook's error must fail the add; got %v", err)
	}

	st, err := loadAt(path)
	testsupport.Must(t, err, "loadAt: %v", err)
	if len(st.Entries) != 0 {
		t.Errorf("a refused add must write nothing; got %+v", st.Entries)
	}
}

// TestOnChangeIsNotCalledWhenNothingChanges pins the idempotence row's other
// consequence: a re-add of an identical entry writes nothing, so there is
// nothing to record. A record that fired here would prove neither novelty nor
// change.
func TestOnChangeIsNotCalledWhenNothingChanges(t *testing.T) {
	path := sandbox(t)
	repo := t.TempDir()
	base := AddRequest{
		Name: "checks", Argv: []string{"make", "test"}, RepoRoot: repo,
		Tree: true, Network: []string{"vuln.go.dev"},
	}

	_, err := addAt(path, base)
	testsupport.Must(t, err, "first add: %v", err)

	second := base
	var called int
	second.OnChange = func(Entry) error { called++; return nil }
	res, err := addAt(path, second)
	testsupport.Must(t, err, "idempotent re-add: %v", err)
	if !res.Idempotent {
		t.Error("an identical re-add must report itself idempotent")
	}
	if called != 0 {
		t.Errorf("the hook ran %d times on a no-op add; a change record must prove a change", called)
	}

	// And a CONFLICT is likewise not a change: the store is untouched, so
	// nothing may be recorded about it either.
	third := base
	third.Tree = false
	third.OnChange = func(Entry) error { called++; return nil }
	if _, err := addAt(path, third); !errors.Is(err, ErrConflict) {
		t.Fatalf("a differing flag must conflict; got %v", err)
	}
	if called != 0 {
		t.Errorf("the hook ran %d times on a refused add", called)
	}
}

// TestRemoveOnChangeSeesTheEntryItRemoves is the remove-side hook, and the
// reason it takes the entry rather than leaving the caller to look one up: the
// entry handed over is the one this binding resolved to, flags and all, so a
// record of the revocation says what was revoked.
func TestRemoveOnChangeSeesTheEntryItRemoves(t *testing.T) {
	path := sandbox(t)
	repoA, repoB := t.TempDir(), t.TempDir()

	_, err := addAt(path, AddRequest{
		Name: "checks", Argv: []string{"make", "a"}, RepoRoot: repoA,
	})
	testsupport.Must(t, err, "addAt repoA: %v", err)
	_, err = addAt(path, AddRequest{
		Name: "checks", Argv: []string{"make", "b"}, RepoRoot: repoB,
		Tree: true, Network: []string{"vuln.go.dev"},
	})
	testsupport.Must(t, err, "addAt repoB: %v", err)

	var seen Entry
	removed, err := removeAt(path, RemoveRequest{
		Name: "checks", RepoRoot: repoB,
		OnChange: func(e Entry) error { seen = e; return nil },
	})
	testsupport.Must(t, err, "removeAt: %v", err)
	if !removed {
		t.Fatal("removeAt must report that it removed the entry")
	}
	// A lookup by name alone would have found repoA's entry first and described
	// THAT one — the wrong argv, the wrong flags, for a revocation in repoB.
	if seen.ArgvSHA256 != ArgvSHA256([]string{"make", "b"}) {
		t.Errorf("the hook saw the wrong entry: %+v", seen)
	}
	if !seen.Tree || len(seen.Network) != 1 {
		t.Errorf("the hook must see the removed entry's flags; got tree=%t network=%v",
			seen.Tree, seen.Network)
	}

	// The refusal path, same as add: the store keeps the entry.
	boom := errors.New("the record could not be written")
	_, err = removeAt(path, RemoveRequest{
		Name: "checks", RepoRoot: repoA,
		OnChange: func(Entry) error { return boom },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("the hook's error must fail the remove; got %v", err)
	}
	st, err := loadAt(path)
	testsupport.Must(t, err, "loadAt: %v", err)
	if len(st.Entries) != 1 {
		t.Errorf("a refused remove must delete nothing; got %+v", st.Entries)
	}
}

// TestPrefixWarningIsEmittedAndNeverSuppressed covers §3.3's over-authorization
// warning and the rule that `--yes` never silences it.
//
// Suppressing the warning is exactly what would make the conversational posture
// unsafe: the whole bound on self-trust is that the grant is VISIBLE.
func TestPrefixWarningIsEmittedAndNeverSuppressed(t *testing.T) {
	path := sandbox(t)
	repo := t.TempDir()

	res, err := addAt(path, AddRequest{
		Name: "checks", Argv: []string{"make"}, RepoRoot: repo, Prefix: true,
	})
	testsupport.Must(t, err, "addAt: %v", err)

	if len(res.Warnings) == 0 {
		t.Fatal("a --prefix add must emit the over-authorization warning")
	}
	w := strings.Join(res.Warnings, "\n")

	for _, want := range []string{
		"prefix entry",            // names what it is
		"ANY command beginning",   // names the blast radius
		"make",                    // names the authorized prefix
		"Use a full argv instead", // says what to do instead
	} {
		if !strings.Contains(w, want) {
			t.Errorf("the warning must contain %q; got:\n%s", want, w)
		}
	}

	// It names the REPO, because the blast radius is repo-scoped.
	repoID, err := RepoIdentity(repo)
	testsupport.Must(t, err, "RepoIdentity: %v", err)
	if !strings.Contains(w, repoID) {
		t.Errorf("the warning must name the repo it binds to; got:\n%s", w)
	}

	// NEVER SUPPRESSED. The add path has no parameter that could silence it —
	// there is no `Quiet` or `SuppressWarnings` field on AddRequest, which is
	// the structural half of the guarantee.
	code := goCodeWithoutComments(t, "add.go")
	for _, forbidden := range []string{"Quiet", "SuppressWarning", "NoWarn"} {
		if strings.Contains(code, forbidden) {
			t.Errorf("add.go names %q; the over-authorization warning must never be suppressible", forbidden)
		}
	}

	// A non-prefix add emits no warning, so the signal is not noise.
	plain, err := addAt(path, AddRequest{Name: "fmt", Argv: []string{"gofmt"}, RepoRoot: repo})
	testsupport.Must(t, err, "addAt: %v", err)
	if len(plain.Warnings) != 0 {
		t.Errorf("a full-argv add must emit no over-authorization warning; got %q", plain.Warnings)
	}
}

// TestPrefixWarningRendersControlBytesEscaped ties the warning to §5.7: the
// argv it echoes is attacker-influenced, so it goes through the renderer.
func TestPrefixWarningRendersControlBytesEscaped(t *testing.T) {
	path := sandbox(t)
	repo := t.TempDir()

	res, err := addAt(path, AddRequest{
		Name: "checks", Argv: []string{"make\x1b[2K\rtest"}, RepoRoot: repo, Prefix: true,
	})
	testsupport.Must(t, err, "addAt: %v", err)
	w := strings.Join(res.Warnings, "\n")

	if strings.ContainsRune(w, '\x1b') || strings.ContainsRune(w, '\r') {
		t.Error("the warning must not carry raw control bytes to a terminal")
	}
	if !strings.Contains(w, `\x1b`) {
		t.Errorf("the escape must appear as visible text; got: %s", w)
	}
}

func TestRemove(t *testing.T) {
	path := sandbox(t)
	repo := t.TempDir()
	repoID, err := RepoIdentity(repo)
	testsupport.Must(t, err, "RepoIdentity: %v", err)

	_, err = addAt(path, AddRequest{Name: "checks", Argv: []string{"make", "test"}, RepoRoot: repo})
	testsupport.Must(t, err, "addAt: %v", err)

	removed, err := removeAt(path, RemoveRequest{Name: "checks", RepoRoot: repo})
	testsupport.Must(t, err, "removeAt: %v", err)
	if !removed {
		t.Fatal("removeAt must report that it removed the entry")
	}

	st, err := loadAt(path)
	testsupport.Must(t, err, "loadAt: %v", err)
	if len(st.Entries) != 0 {
		t.Errorf("the entry must be gone; got %+v", st.Entries)
	}
	// And the gate it authorized is now unmatched — revocation takes effect
	// immediately, because every match reads the live store.
	if m := st.Lookup(repoID, "checks", nil); m.Matched {
		t.Error("a removed entry must no longer match")
	}

	// Removing something absent is not an error worth an exit code.
	removed, err = removeAt(path, RemoveRequest{Name: "checks", RepoRoot: repo})
	testsupport.Must(t, err, "removing an absent entry must not error: %v", err)
	if removed {
		t.Error("removing an absent entry must report false")
	}
}

func TestRemoveDoesNotTouchOtherBindings(t *testing.T) {
	path := sandbox(t)
	repoA, repoB := t.TempDir(), t.TempDir()

	for _, r := range []string{repoA, repoB} {
		_, err := addAt(path, AddRequest{Name: "checks", Argv: []string{"make", "test"}, RepoRoot: r})
		testsupport.Must(t, err, "addAt: %v", err)
	}
	_, err := addAt(path, AddRequest{Name: "checks", Argv: []string{"make", "test"}, Global: true})
	testsupport.Must(t, err, "addAt --global: %v", err)

	_, err = removeAt(path, RemoveRequest{Name: "checks", RepoRoot: repoA})
	testsupport.Must(t, err, "removeAt: %v", err)

	st, err := loadAt(path)
	testsupport.Must(t, err, "loadAt: %v", err)
	if len(st.Entries) != 2 {
		t.Fatalf("removing one binding must leave the others; got %d entries", len(st.Entries))
	}
	// The global one specifically survives a repo-scoped remove.
	var sawGlobal bool
	for _, e := range st.Entries {
		if e.Global {
			sawGlobal = true
		}
	}
	if !sawGlobal {
		t.Error("a repo-scoped remove must not remove the global entry")
	}
}

func TestListShowsThisRepoPlusGlobals(t *testing.T) {
	path := sandbox(t)
	repoA, repoB := t.TempDir(), t.TempDir()
	repoAID, err := RepoIdentity(repoA)
	testsupport.Must(t, err, "RepoIdentity: %v", err)

	_, err = addAt(path, AddRequest{Name: "a", Argv: []string{"make", "a"}, RepoRoot: repoA})
	testsupport.Must(t, err, "addAt: %v", err)
	_, err = addAt(path, AddRequest{Name: "b", Argv: []string{"make", "b"}, RepoRoot: repoB})
	testsupport.Must(t, err, "addAt: %v", err)
	_, err = addAt(path, AddRequest{Name: "g", Argv: []string{"gofmt"}, Global: true})
	testsupport.Must(t, err, "addAt: %v", err)

	st, err := loadAt(path)
	testsupport.Must(t, err, "loadAt: %v", err)
	got := st.List(repoAID)
	if len(got) != 2 {
		t.Fatalf("List must show this repo's entries plus globals; got %d: %+v", len(got), got)
	}
	names := map[string]bool{}
	for _, e := range got {
		names[e.Name] = true
	}
	if !names["a"] || !names["g"] || names["b"] {
		t.Errorf("List returned the wrong set: %v", names)
	}
}

// --- The locked read-modify-write (§3.5.1, F5) ------------------------------

// TestConcurrentTrustAddsBothLand is F5's test, in both forms §9.1 requires.
//
// WITHOUT THE LOCK, two concurrent adds interleave as read-A / read-B /
// write-A / write-B, and B's write SILENTLY DROPS A's entry. The operator sees
// two successful adds and has one. That is not a security hole — it fails
// closed, and the lost gate goes unmatched — but it is a silent failure of an
// authorization operation the operator was told succeeded.
func TestConcurrentTrustAddsBothLand(t *testing.T) {
	t.Run("goroutines", func(t *testing.T) {
		path := sandbox(t)
		repo := t.TempDir()

		const n = 8
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = addAt(path, AddRequest{
					Name:     fmt.Sprintf("gate-%d", i),
					Argv:     []string{"make", fmt.Sprintf("target-%d", i)},
					RepoRoot: repo,
				})
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("add %d failed: %v", i, err)
			}
		}
		assertAllEntriesPresent(t, path, n)
	})

	// THE CASE THAT MATTERS. The flock's real job is CROSS-PROCESS exclusion,
	// and an implementation that took only an in-process sync.Mutex would pass
	// the goroutine case above and fail this one.
	t.Run("subprocesses", func(t *testing.T) {
		if os.Getenv("DOCKET_TRUST_ADD_CHILD") != "" {
			return // the child path is handled in TestMain-free style below
		}
		path := sandbox(t)
		repo := t.TempDir()

		const n = 8
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				// Re-exec THIS test binary, which runs TestTrustAddChildHelper
				// in a fresh process. Each child does a real addAt against the
				// same sandbox store.
				cmd := exec.Command(os.Args[0], "-test.run=TestTrustAddChildHelper")
				cmd.Env = append(os.Environ(),
					"DOCKET_TRUST_ADD_CHILD=1",
					"DOCKET_TRUST_ADD_PATH="+path,
					"DOCKET_TRUST_ADD_REPO="+repo,
					fmt.Sprintf("DOCKET_TRUST_ADD_INDEX=%d", i),
				)
				out, err := cmd.CombinedOutput()
				if err != nil {
					errs[i] = fmt.Errorf("child %d: %v: %s", i, err, out)
				}
			}(i)
		}
		wg.Wait()

		for _, err := range errs {
			if err != nil {
				t.Error(err)
			}
		}
		assertAllEntriesPresent(t, path, n)
	})
}

// TestTrustAddChildHelper is the subprocess body for the case above. It is a
// no-op unless the parent set the environment, so it costs nothing in an
// ordinary run.
func TestTrustAddChildHelper(t *testing.T) {
	if os.Getenv("DOCKET_TRUST_ADD_CHILD") == "" {
		t.Skip("not a child invocation")
	}
	path := os.Getenv("DOCKET_TRUST_ADD_PATH")
	repo := os.Getenv("DOCKET_TRUST_ADD_REPO")
	idx := os.Getenv("DOCKET_TRUST_ADD_INDEX")

	_, err := addAt(path, AddRequest{
		Name:     "gate-" + idx,
		Argv:     []string{"make", "target-" + idx},
		RepoRoot: repo,
	})
	testsupport.Must(t, err, "child add: %v", err)
}

// assertAllEntriesPresent is the assertion both concurrency cases share: EVERY
// entry is present afterwards and the file still parses.
func assertAllEntriesPresent(t *testing.T, path string, n int) {
	t.Helper()

	st, err := loadAt(path)
	testsupport.Must(t, err, "the store must still parse after concurrent writes: %v", err)
	if len(st.Entries) != n {
		t.Fatalf("EXPECTED ALL %d ENTRIES, GOT %d — a lost entry means the read-modify-write is not locked", n, len(st.Entries))
	}
	seen := map[string]bool{}
	for _, e := range st.Entries {
		seen[e.Name] = true
	}
	for i := 0; i < n; i++ {
		if !seen[fmt.Sprintf("gate-%d", i)] {
			t.Errorf("entry gate-%d was lost", i)
		}
	}
}

// TestHeldLockMakesASecondAddConflict is W5: acquisition blocks, bounded by a
// short timeout, and exceeding it is a CONFLICT naming the lock path.
//
// A trust add is a human-scale operation and the bound is far beyond its honest
// duration, so a timeout means something is genuinely STUCK rather than merely
// contended.
func TestHeldLockMakesASecondAddConflict(t *testing.T) {
	path := sandbox(t)
	repo := t.TempDir()

	// Hold the lock for longer than the acquisition timeout.
	held, err := acquireLock(path)
	testsupport.Must(t, err, "acquiring the lock: %v", err)
	defer held.release()

	_, err = addAt(path, AddRequest{Name: "checks", Argv: []string{"make", "test"}, RepoRoot: repo})
	if err == nil {
		t.Fatal("an add against a held lock must fail rather than corrupt the store")
	}
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict, got %v", err)
	}
	if !strings.Contains(err.Error(), path+".lock") {
		t.Errorf("the refusal must name the lock path; got: %v", err)
	}
	if !strings.Contains(err.Error(), "another `docket trust` is in progress") {
		t.Errorf("the refusal must say what is happening; got: %v", err)
	}
}

// TestLockfileIntegrityTable is W3: the lockfile is opened with §3.2's
// discipline — refuse a symlink, refuse a non-regular file, check the mode.
//
// It lives in the user-owned config directory, so the exposure is smaller than
// the repo-resident tree lock's — but it is the same rule, and applying it in
// one place and not the other is the inconsistency a later refactor
// generalizes in the wrong direction.
func TestLockfileIntegrityTable(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		path := sandbox(t)
		target := filepath.Join(t.TempDir(), "elsewhere")
		err := os.WriteFile(target, nil, storeFileMode)
		testsupport.Must(t, err, "creating the link target: %v", err)
		err = os.Symlink(target, path+".lock")
		testsupport.Must(t, err, "creating the symlink: %v", err)

		_, err = acquireLock(path)
		if err == nil {
			t.Fatal("a symlinked lockfile must be refused")
		}
		if !errors.Is(err, ErrIntegrity) {
			t.Errorf("expected ErrIntegrity, got %v", err)
		}
	})

	t.Run("fifo", func(t *testing.T) {
		path := sandbox(t)
		if err := mkfifo(path + ".lock"); err != nil {
			t.Skipf("cannot create a FIFO here: %v", err)
		}
		if _, err := acquireLock(path); !errors.Is(err, ErrIntegrity) {
			t.Errorf("a FIFO lockfile must be refused with ErrIntegrity; got %v", err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		path := sandbox(t)
		err := os.Mkdir(path+".lock", storeDirMode)
		testsupport.Must(t, err, "creating the directory: %v", err)
		if _, err := acquireLock(path); !errors.Is(err, ErrIntegrity) {
			t.Errorf("a directory at the lock path must be refused; got %v", err)
		}
	})

	t.Run("bad mode", func(t *testing.T) {
		path := sandbox(t)
		err := os.WriteFile(path+".lock", nil, 0o666)
		testsupport.Must(t, err, "creating the lockfile: %v", err)
		err = os.Chmod(path+".lock", 0o666)
		testsupport.Must(t, err, "chmod: %v", err)
		if _, err := acquireLock(path); !errors.Is(err, ErrIntegrity) {
			t.Errorf("a group- or world-accessible lockfile must be refused; got %v", err)
		}
	})

	t.Run("regular file and missing file both succeed", func(t *testing.T) {
		// The check is not refusing everything.
		path := sandbox(t)

		l, err := acquireLock(path) // missing
		testsupport.Must(t, err, "a missing lockfile must be created and locked: %v", err)
		l.release()

		l2, err := acquireLock(path) // now a regular 0600 file
		testsupport.Must(t, err, "an existing regular lockfile must lock: %v", err)
		l2.release()
	})
}

// TestLockIsOnASiblingNotTheStore is W2, asserted structurally.
//
// Locking trust.toml directly does not work with rename-based writes: the
// rename REPLACES THE INODE, so the lock the writer holds and the lock the next
// writer takes are on DIFFERENT FILES and exclude nothing. The sibling's inode
// is stable.
func TestLockIsOnASiblingNotTheStore(t *testing.T) {
	path := sandbox(t)

	l, err := acquireLock(path)
	testsupport.Must(t, err, "acquireLock: %v", err)
	defer l.release()

	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Errorf("the lock must be taken on the sibling lockfile: %v", err)
	}
	// The store itself is untouched by locking.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("acquiring the lock must not create the store file")
	}
}

// --- SB4: the poisoned-store guard ------------------------------------------

// TestNoTestReadsTheRealTrustStore enforces §9.5's sandbox rule MECHANICALLY.
//
// It points HOME and XDG_CONFIG_HOME at a directory containing a POISONED trust
// file — an entry that would match a gate no legitimate test approves — and
// asserts that a Load() through the ordinary entry point sees the poison,
// proving the environment is what the package reads, and that a sandboxed
// lookup does NOT see it.
//
// The failure this catches is a test that reads the operator's real store: such
// a test would match an entry it should not see, and on a developer's machine
// it would pass or fail depending on what THAT PERSON had trusted. Worse, a
// test that WROTE would modify the machine it is auditing.
func TestNoTestReadsTheRealTrustStore(t *testing.T) {
	poisonDir := t.TempDir()
	storeDir := filepath.Join(poisonDir, "docket")
	err := os.MkdirAll(storeDir, storeDirMode)
	testsupport.Must(t, err, "creating the poisoned store dir: %v", err)
	poisonPath := filepath.Join(storeDir, "trust.toml")
	poison := `version = 1

[[entry]]
name = "poisoned-gate-no-test-may-match"
argv = ["/nonexistent/poison"]
global = true
`
	err = os.WriteFile(poisonPath, []byte(poison), storeFileMode)
	testsupport.Must(t, err, "writing the poisoned store: %v", err)
	err = os.Chmod(poisonPath, storeFileMode)
	testsupport.Must(t, err, "chmod: %v", err)

	t.Setenv("XDG_CONFIG_HOME", poisonDir)
	t.Setenv("HOME", poisonDir)

	// The environment IS what the package reads: with XDG pointed at the
	// poison, Load() finds it. This half proves the sandbox has teeth — if
	// Load ignored the environment, every other test's isolation would be
	// fictional.
	st, err := Load()
	testsupport.Must(t, err, "Load: %v", err)
	if len(st.Entries) != 1 || st.Entries[0].Name != "poisoned-gate-no-test-may-match" {
		t.Fatalf("Load must read the store the environment names; got %+v", st.Entries)
	}

	// And a test that sandboxes properly sees NOTHING, which is what every
	// other test in this package relies on.
	inner := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", inner)
	t.Setenv("HOME", inner)

	sandboxed, err := Load()
	testsupport.Must(t, err, "Load in the sandbox: %v", err)
	if len(sandboxed.Entries) != 0 {
		t.Fatalf("A SANDBOXED LOAD MUST SEE NO ENTRIES; got %+v — a test is reading outside its sandbox", sandboxed.Entries)
	}
	if m := sandboxed.Lookup("/anywhere", "poisoned-gate-no-test-may-match", nil); m.Matched {
		t.Fatal("a sandboxed lookup matched the poisoned entry — the sandbox is not isolating")
	}
}

// TestEveryTestFileSandboxesXDG is SB1 as a source-level guard: a test added
// later cannot forget.
//
// It asserts that every _test.go file in this package which mentions the store
// path or the add/remove entry points also sets XDG_CONFIG_HOME. The rule is
// one line to state and one new test function to violate.
func TestEveryTestFileSandboxesXDG(t *testing.T) {
	entries, err := os.ReadDir(".")
	testsupport.Must(t, err, "reading the package directory: %v", err)

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		testsupport.Must(t, err, "reading %s: %v", name, err)
		src := string(b)

		touchesStore := strings.Contains(src, "loadAt(") || strings.Contains(src, "addAt(") ||
			strings.Contains(src, "removeAt(") || strings.Contains(src, "Load()")
		if !touchesStore {
			continue
		}
		// Either the file sandboxes directly, or it uses the sandbox helper
		// that does.
		if !strings.Contains(src, "XDG_CONFIG_HOME") && !strings.Contains(src, "sandbox(t)") {
			t.Errorf("%s touches the trust store but never sandboxes XDG_CONFIG_HOME (§9.5 SB1)", name)
		}
	}
}
