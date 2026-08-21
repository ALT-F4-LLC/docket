package exec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// plantBinary copies the witness into dir under the given name, so a test can
// stage a repo-resident or PATH-resident executable that PROVES whether it ran.
func plantBinary(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	src, err := os.ReadFile(witness(t))
	testsupport.Must(t, err, "reading the witness: %v", err)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, src, 0o755); err != nil {
		t.Fatalf("planting %s: %v", path, err)
	}
	return path
}

// TestArgv0NeverResolvesIntoTheRepoByName is T17 / §5.2.1's table.
//
// Each row is a DISTINCT WAY IN, which is why the fix is worth a table rather
// than a single case. The sentinel is what makes the refusal rows meaningful:
// asserting only on a return value would pass against an implementation that
// RAN the command and then reported a refusal.
func TestArgv0NeverResolvesIntoTheRepoByName(t *testing.T) {
	// (a) PATH contains <repo>/bin and the entry names the command BY NAME.
	//     This is the direnv case: an operator allows a repo's .envrc for
	//     legitimate dev tooling and it prepends a repo-resident bin. The
	//     trust decision was about a NAME; the repo supplies the CODE.
	t.Run("a_by_name_with_repo_resident_PATH_is_refused", func(t *testing.T) {
		repo := realTempDir(t)
		repoBin := filepath.Join(repo, "bin")
		plantBinary(t, repoBin, "witness-tool")

		t.Setenv("PATH", repoBin+string(os.PathListSeparator)+os.Getenv("PATH"))

		resolved, err := Resolve("witness-tool", repo, "witness-tool")
		if err == nil {
			t.Fatalf("a repo-resident binary reached by NAME must be refused; resolved to %s", resolved)
		}
		if !errors.Is(err, ErrRefused) {
			t.Errorf("expected ErrRefused, got %v", err)
		}
		// The reason names the RESOLVED ABSOLUTE PATH and the rule, so the
		// operator can see exactly which file was about to run.
		wantPath := filepath.Join(repoBin, "witness-tool")
		if !strings.Contains(err.Error(), wantPath) {
			t.Errorf("the refusal must name the resolved path %q; got: %v", wantPath, err)
		}
		if !strings.Contains(err.Error(), "inside the repository") {
			t.Errorf("the refusal must name the rule; got: %v", err)
		}
		// And it names the REMEDY, which is one line.
		if !strings.Contains(err.Error(), "trust the absolute path") {
			t.Errorf("the refusal must state the remedy; got: %v", err)
		}
	})

	// (a') The same row, proven by SENTINEL: nothing executed.
	t.Run("a_refused_command_does_not_execute", func(t *testing.T) {
		repo := realTempDir(t)
		repoBin := filepath.Join(repo, "bin")
		planted := plantBinary(t, repoBin, "witness-tool")
		sentinel := filepath.Join(t.TempDir(), "sentinel")

		t.Setenv("PATH", repoBin+string(os.PathListSeparator)+os.Getenv("PATH"))

		if _, err := Resolve("witness-tool", repo, "witness-tool"); err == nil {
			t.Fatal("expected a refusal")
		}
		if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
			t.Error("THE WITNESS SENTINEL EXISTS — a refused command executed anyway")
		}
		_ = planted
	})

	// (b) THE SAME BINARY trusted by its ABSOLUTE PATH runs (R4). The escape
	//     hatch is proven OPEN, so repo-owned scripts stay usable: the operator
	//     trusted THAT FILE, not a name that happened to resolve to it.
	t.Run("b_absolute_path_entry_is_permitted", func(t *testing.T) {
		repo := realTempDir(t)
		repoBin := filepath.Join(repo, "bin")
		planted := plantBinary(t, repoBin, "witness-tool")

		resolved, err := Resolve(planted, repo, planted)
		testsupport.Must(t, err, "an absolute-path entry for a repo-owned script must run: %v", err)
		if resolved != planted {
			// Both sides are normalized, so compare against the normalized form.
			wantResolved, _ := NormalizePath(planted)
			if resolved != wantResolved {
				t.Errorf("resolved to %q, want %q", resolved, wantResolved)
			}
		}

		// And it really does execute, sentinel and all.
		sentinel := filepath.Join(t.TempDir(), "sentinel")
		env, err := BuildEnv(EnvPolicy{Gate: "checks", Repo: repo})
		testsupport.Must(t, err, "BuildEnv: %v", err)
		env = append(env, "WITNESS_MODE=sentinel", "WITNESS_SENTINEL="+sentinel)
		if _, err := Run(Spec{Argv: []string{resolved}, Dir: repo, Env: env}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if _, err := os.Stat(sentinel); err != nil {
			t.Errorf("the permitted absolute-path command must actually run: %v", err)
		}
	})

	// (c) A PATH directory OUTSIDE the repo holding a SYMLINK into the repo.
	//     R1's one-indirection variant: resolving symlinks is required, not
	//     cosmetic — a check on the unresolved path would wave this through.
	t.Run("c_symlink_from_outside_PATH_into_the_repo_is_refused", func(t *testing.T) {
		repo := realTempDir(t)
		repoBin := filepath.Join(repo, "bin")
		planted := plantBinary(t, repoBin, "witness-tool")

		outsideDir := realTempDir(t)
		link := filepath.Join(outsideDir, "witness-tool")
		if err := os.Symlink(planted, link); err != nil {
			t.Fatalf("creating the symlink: %v", err)
		}

		t.Setenv("PATH", outsideDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		if _, err := Resolve("witness-tool", repo, "witness-tool"); err == nil {
			t.Fatal("a symlink from an outside PATH directory into the repo must be refused")
		} else if !strings.Contains(err.Error(), "inside the repository") {
			t.Errorf("the refusal must name the rule; got: %v", err)
		}
	})

	// (d) A repo root reached THROUGH A SYMLINK is still refused (R2): the
	//     same normalization runs on both sides, so a symlinked checkout does
	//     not defeat the comparison.
	t.Run("d_symlinked_repo_root_is_still_refused", func(t *testing.T) {
		realRepo := realTempDir(t)
		repoBin := filepath.Join(realRepo, "bin")
		plantBinary(t, repoBin, "witness-tool")

		linkedRepo := filepath.Join(realTempDir(t), "repo-link")
		if err := os.Symlink(realRepo, linkedRepo); err != nil {
			t.Fatalf("creating the repo symlink: %v", err)
		}

		t.Setenv("PATH", repoBin+string(os.PathListSeparator)+os.Getenv("PATH"))

		// The repo root is given as the SYMLINKED path; the binary resolves to
		// the real one. Without normalizing both sides these two strings never
		// meet and the check silently passes.
		if _, err := Resolve("witness-tool", linkedRepo, "witness-tool"); err == nil {
			t.Fatal("a symlinked repo root must not defeat the containment check")
		}
	})

	// (e) THE TRAP ROW. /src/docket-evil is NOT under /src/docket. A
	//     string-prefix implementation says it is — "/src/docket" IS a string
	//     prefix of "/src/docket-evil" — and would refuse a perfectly
	//     legitimate binary in a NEIGHBOURING directory while looking correct
	//     in every other row of this table.
	t.Run("e_a_sibling_directory_sharing_a_prefix_is_NOT_contained", func(t *testing.T) {
		parent := realTempDir(t)
		repo := filepath.Join(parent, "docket")
		evil := filepath.Join(parent, "docket-evil")
		if err := os.MkdirAll(repo, 0o755); err != nil {
			t.Fatalf("creating the repo: %v", err)
		}
		evilBin := filepath.Join(evil, "bin")
		plantBinary(t, evilBin, "witness-tool")

		t.Setenv("PATH", evilBin+string(os.PathListSeparator)+os.Getenv("PATH"))

		resolved, err := Resolve("witness-tool", repo, "witness-tool")
		if err != nil {
			t.Fatalf("A SIBLING DIRECTORY SHARING A PATH PREFIX IS NOT CONTAINED — "+
				"the containment test must be component-wise, not a string prefix: %v", err)
		}
		if !strings.Contains(resolved, "docket-evil") {
			t.Errorf("resolved to %q, expected the sibling's binary", resolved)
		}

		// The unit-level statement of the same rule.
		if Under("/src/docket", "/src/docket-evil/bin/make") {
			t.Error("Under must be component-wise: /src/docket-evil is NOT under /src/docket")
		}
		if !Under("/src/docket", "/src/docket/bin/make") {
			t.Error("Under must report a genuine descendant as contained")
		}
	})

	// (f) An ordinary binary outside the repo RUNS, so the check is not
	//     refusing everything — a test suite where every row refuses would
	//     pass against an implementation that refuses unconditionally.
	t.Run("f_an_ordinary_outside_binary_runs", func(t *testing.T) {
		repo := realTempDir(t)
		outsideDir := realTempDir(t)
		plantBinary(t, outsideDir, "witness-tool")

		t.Setenv("PATH", outsideDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		resolved, err := Resolve("witness-tool", repo, "witness-tool")
		testsupport.Must(t, err, "an ordinary binary outside the repo must resolve: %v", err)
		if !filepath.IsAbs(resolved) {
			t.Errorf("Resolve must return an absolute path, got %q", resolved)
		}
	})
}

// realTempDir returns a symlink-resolved temp directory.
//
// On macOS, t.TempDir() lives under /var, which is itself a symlink to
// /private/var. Tests that compare paths must start from the resolved form, or
// they measure the platform's symlink rather than the rule under test.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	testsupport.Must(t, err, "resolving the temp directory: %v", err)
	return dir
}

// TestArgv0IsNotResolvedAgainstTheWorkingDirectory is T15.
//
// A repo that ships ./make must not win. Go's exec.ErrDot exists precisely for
// this, and the runner treats it as a HARD REFUSAL rather than something to
// re-resolve into an absolute path and run anyway.
func TestArgv0IsNotResolvedAgainstTheWorkingDirectory(t *testing.T) {
	repo := realTempDir(t)
	plantBinary(t, repo, "witness-tool")

	// A PATH that does NOT contain the repo. The only way `witness-tool` could
	// resolve is against the working directory.
	t.Setenv("PATH", realTempDir(t))

	if _, err := Resolve("witness-tool", repo, "witness-tool"); err == nil {
		t.Fatal("a bare name must not resolve against the working directory")
	}

	// The explicit ./-relative spelling is refused too, and the refusal says
	// what happened rather than only that it failed.
	//
	// The working directory is restored to its EXACT prior value rather than
	// to "..": a test that leaves the process chdir'd elsewhere breaks every
	// later test that opens a file by a relative path — which is how the
	// source guards, which read their own package directory, would fail for a
	// reason that has nothing to do with them.
	t.Chdir(repo)

	if _, err := Resolve("./witness-tool", repo, "./witness-tool"); err == nil {
		t.Fatal("a ./-relative argv[0] must be refused")
	} else if !errors.Is(err, ErrRefused) {
		t.Errorf("expected ErrRefused, got %v", err)
	}
}

func TestResolveRefusesAnEmptyOrMissingCommand(t *testing.T) {
	repo := realTempDir(t)

	if _, err := Resolve("", repo, ""); !errors.Is(err, ErrRefused) {
		t.Errorf("an empty argv[0] must be refused; got %v", err)
	}

	t.Setenv("PATH", realTempDir(t))
	_, err := Resolve("definitely-not-a-real-command-xyz", repo, "definitely-not-a-real-command-xyz")
	if !errors.Is(err, ErrRefused) {
		t.Errorf("a command not on PATH must be refused; got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "not found") {
		t.Errorf("the refusal should say the command was not found; got: %v", err)
	}
}

// TestContainmentCheckIsUnconditional is R5: the check applies to named gates,
// fence gates, and pre-gates alike, because it lives in Resolve and there is
// only one Resolve. A path that skipped it would have to be a second
// resolution function, and there is not one.
func TestContainmentCheckIsUnconditional(t *testing.T) {
	code := codeWithoutComments(t, "resolve.go")

	// LookPath is called exactly once in the package, inside Resolve, and the
	// containment test follows it. Two call sites would be two policies.
	if n := strings.Count(code, "LookPath"); n != 1 {
		t.Errorf("LookPath appears %d times in resolve.go; there must be exactly one resolution path (R5)", n)
	}

	for _, other := range []string{"run.go", "flaky.go"} {
		if strings.Contains(codeWithoutComments(t, other), "LookPath") {
			t.Errorf("%s calls LookPath; resolution must happen in exactly one place", other)
		}
	}
}
