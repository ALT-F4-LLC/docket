package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestIsGuardCmd pins which commands get the not-applicable treatment.
//
// The predicate walks the parent chain rather than matching a name list, so a
// guard added later is covered without editing anything, and an unrelated verb
// that happens to share a name with a guard is not.
func TestIsGuardCmd(t *testing.T) {
	guard := &cobra.Command{Use: "guard"}
	stop := &cobra.Command{Use: "stop"}
	guard.AddCommand(stop)

	// A verb named "stop" that is NOT under guard — the case a name-matching
	// implementation would get wrong.
	other := &cobra.Command{Use: "run"}
	otherStop := &cobra.Command{Use: "stop"}
	other.AddCommand(otherStop)

	// A guard added after the fact, to show nothing needs updating for it.
	future := &cobra.Command{Use: "some-future-guard"}
	guard.AddCommand(future)

	tests := []struct {
		name string
		cmd  *cobra.Command
		want bool
	}{
		{"the guard parent itself", guard, true},
		{"a registered guard verb", stop, true},
		{"a guard added later", future, true},
		{"a same-named verb elsewhere", otherStop, false},
		{"an unrelated parent", other, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGuardCmd(tt.cmd); got != tt.want {
				t.Errorf("isGuardCmd(%s) = %v, want %v", tt.cmd.Name(), got, tt.want)
			}
		})
	}
}

// TestGuardNotFoundExitsZero is the regression, driven through the real
// binary because the defect is about an EXIT CODE and nothing below
// Execute() produces one.
//
// Before the fix every guard verb exited 2 in a directory with no `.docket`,
// and 2 is also the DENY code (§6.12). A hook wired as
// `docket guard stop || exit` therefore denied in every non-docket repo —
// observed blocking a mid-bootstrap turn-end before init had run.
//
// The three properties that matter, all checked here:
//
//   - no engine        -> exit 0, and the reason says so
//   - a genuine deny   -> still exit 2 (the fix must not blunt the contract)
//   - a non-guard verb -> still exit 2 NOT_FOUND (the fix is scoped to guards)
func TestGuardNotFoundExitsZero(t *testing.T) {
	bin := buildDocketBinary(t)

	// A directory with no `.docket` anywhere up-tree. t.TempDir is under the
	// test's own temp root, which has no docket database above it.
	empty := t.TempDir()

	guardVerbs := [][]string{
		{"guard", "stop"},
		{"guard", "record"},
		{"guard", "gate", "--step", "some-gate"},
		{"guard", "spawn", "--run", "RUN-1"},
		{"guard", "spawn", "--active"},
	}

	for _, argv := range guardVerbs {
		t.Run("no engine: "+strings.Join(argv, " "), func(t *testing.T) {
			stdout, stderr, code := runDocket(t, bin, empty, argv...)

			if code != 0 {
				t.Errorf("exit = %d, want 0; a repo with no engine is not a denial\nstderr: %s",
					code, stderr)
			}
			// The reason must travel, or an operator cannot tell a
			// not-applicable allow from a real one.
			if !strings.Contains(stderr, "no docket database") {
				t.Errorf("stderr = %q, want it to explain why the guard did not apply", stderr)
			}
			// stdout stays the ordinary allow, so a hook parsing it needs no
			// special case.
			if !strings.Contains(stdout, "allowed") {
				t.Errorf("stdout = %q, want the ordinary allow verdict", stdout)
			}
		})
	}

	t.Run("no engine: JSON marks it not_applicable", func(t *testing.T) {
		stdout, _, code := runDocket(t, bin, empty, "guard", "stop", "--json=v2")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		for _, want := range []string{`"allowed":true`, `"not_applicable":true`, `"reason"`} {
			if !strings.Contains(stdout, want) {
				t.Errorf("stdout = %q, missing %s", stdout, want)
			}
		}
	})

	// THE CONTRACT MUST SURVIVE. If the fix made every guard exit 0, it would
	// have replaced a false deny with a false allow — strictly worse for a
	// predicate a hook trusts.
	t.Run("a genuine deny still exits 2", func(t *testing.T) {
		repo := t.TempDir()
		if _, stderr, code := runDocket(t, bin, repo, "init"); code != 0 {
			t.Fatalf("init failed: %s", stderr)
		}

		_, stderr, code := runDocket(t, bin, repo, "guard", "gate", "--step", "nonexistent")
		if code != 2 {
			t.Errorf("exit = %d, want 2 for a real denial\nstderr: %s", code, stderr)
		}
		if strings.Contains(stderr, "no docket database") {
			t.Errorf("a real denial reported the not-applicable reason: %q", stderr)
		}
	})

	t.Run("a genuine allow carries no not_applicable", func(t *testing.T) {
		repo := t.TempDir()
		if _, stderr, code := runDocket(t, bin, repo, "init"); code != 0 {
			t.Fatalf("init failed: %s", stderr)
		}

		stdout, _, code := runDocket(t, bin, repo, "guard", "stop", "--json=v2")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if strings.Contains(stdout, "not_applicable") {
			t.Errorf("a real allow was marked not_applicable: %q", stdout)
		}
	})

	// The fix is scoped to guards. Every other verb still reports a missing
	// database as NOT_FOUND, which is correct for a verb that needed one.
	t.Run("non-guard verbs still fail NOT_FOUND", func(t *testing.T) {
		for _, argv := range [][]string{{"issue", "list"}, {"next"}} {
			_, stderr, code := runDocket(t, bin, empty, argv...)
			if code != 2 {
				t.Errorf("%v: exit = %d, want 2 NOT_FOUND", argv, code)
			}
			if !strings.Contains(stderr, "no docket database") {
				t.Errorf("%v: stderr = %q, want the NOT_FOUND message", argv, stderr)
			}
		}
	})
}

// TestGuardSpawnActiveFlagValidation is DKT-1287: `--active` is mutually
// exclusive with `--run`, `--rows`, `--ack-reap` and `--deciding-vote`, since
// each of those names an act about ONE run and `--active` answers over every
// active run at once.
func TestGuardSpawnActiveFlagValidation(t *testing.T) {
	bin := buildDocketBinary(t)
	repo := t.TempDir()
	if _, stderr, code := runDocket(t, bin, repo, "init"); code != 0 {
		t.Fatalf("init failed: %s", stderr)
	}

	rowsFile := filepath.Join(repo, "rows.json")
	if err := os.WriteFile(rowsFile, []byte("[]"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", rowsFile, err)
	}

	cases := [][]string{
		{"guard", "spawn", "--active", "--run", "RUN-1"},
		{"guard", "spawn", "--active", "--rows", rowsFile},
		{"guard", "spawn", "--active", "--ack-reap", "1"},
		{"guard", "spawn", "--active", "--deciding-vote", "PROPOSAL-1"},
	}
	for _, argv := range cases {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			_, stderr, code := runDocket(t, bin, repo, argv...)
			if code != 3 {
				t.Errorf("exit = %d, want 3 VALIDATION_ERROR\nstderr: %s", code, stderr)
			}
			if !strings.Contains(stderr, "mutually exclusive") &&
				!strings.Contains(stderr, "instead") {
				t.Errorf("stderr = %q, want it to explain the conflict", stderr)
			}
		})
	}
}

// buildDocketBinary compiles the CLI once for the exit-code tests.
//
// The defect under test is an exit code, which only exists at the process
// boundary — calling RunE directly would pass whether or not the bug is
// present.
func buildDocketBinary(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "docket")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/docket")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building docket: %v\n%s", err, out)
	}
	return bin
}

// runDocket invokes the built binary in dir and returns stdout, stderr, and
// the exit code.
func runDocket(t *testing.T, bin, dir string, argv ...string) (string, string, int) {
	t.Helper()

	cmd := exec.Command(bin, argv...)
	cmd.Dir = dir

	// A HERMETIC ENVIRONMENT, or the test reads the developer's own state.
	// DOCKET_PATH would point the binary at a real database, and XDG_CONFIG_HOME
	// at a real trust store — either would make "no engine here" false and the
	// test would pass for the wrong reason.
	cmd.Env = append(os.Environ(),
		"DOCKET_PATH=",
		"XDG_CONFIG_HOME="+filepath.Join(dir, "xdg"),
		"HOME="+dir,
	)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running %v: %v", argv, err)
	}

	return stdout.String(), stderr.String(), code
}
