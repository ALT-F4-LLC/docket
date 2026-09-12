package engine

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// probeRepo builds a throwaway repository with one commit, the same fixture
// gitdiff_test.go's gitRepo builds, so `git rev-parse HEAD` and `git worktree
// add` have a real object database to work against.
func probeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv()
		out, err := cmd.CombinedOutput()
		testsupport.Must(t, err, "git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	run("init", "-q", ".")
	writeFile(t, dir, "README.md", "hello\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	return dir
}

func worktreeCount(t *testing.T, repo string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	testsupport.Must(t, err, "git worktree list: %v", err)
	// One "worktree <path>" line for the main checkout, one more per linked
	// worktree — so N linked worktrees means N+1 such lines.
	return strings.Count(string(out), "worktree ") - 1
}

func noAction(string) bool { return false }

// TestProbeTrustRunsEveryEntryEvenAfterAFailure is AC1: nothing
// short-circuits on the first failure, and each entry gets its own row with
// its own exit status.
func TestProbeTrustRunsEveryEntryEvenAfterAFailure(t *testing.T) {
	repo := probeRepo(t)
	entries := []trust.Entry{
		{Name: "fails", Argv: []string{"false"}},
		{Name: "passes-after", Argv: []string{"true"}},
	}

	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if len(result.Gates) != 2 {
		t.Fatalf("want 2 gate rows (nothing short-circuits), got %d: %+v", len(result.Gates), result.Gates)
	}
	if result.Gates[0].Exit == nil || *result.Gates[0].Exit == 0 {
		t.Errorf("fails: want a nonzero exit, got %+v", result.Gates[0])
	}
	if result.Gates[1].Exit == nil || *result.Gates[1].Exit != 0 {
		t.Errorf("passes-after: want exit 0 — the prior failure must not have stopped it: %+v", result.Gates[1])
	}
	if result.Passed {
		t.Error("one failing gate must make the whole probe not-passed")
	}
	if len(result.Failed) != 1 || result.Failed[0] != "fails" {
		t.Errorf("want failed = [fails], got %v", result.Failed)
	}
}

// TestProbeTrustSkipsActionEntries is AC2: an entry the classifier reports as
// an action is never run and is reported in Skipped with a reason.
func TestProbeTrustSkipsActionEntries(t *testing.T) {
	repo := probeRepo(t)
	entries := []trust.Entry{
		{Name: "aggregate", Argv: []string{"true"}},
		{Name: "lint", Argv: []string{"true"}},
	}
	isAction := func(name string) bool { return name == "aggregate" }

	result, err := ProbeTrust(context.Background(), repo, entries, isAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if len(result.Gates) != 1 || result.Gates[0].Name != "lint" {
		t.Fatalf("want exactly the non-action entry run, got %+v", result.Gates)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Name != "aggregate" {
		t.Fatalf("want aggregate skipped, got %+v", result.Skipped)
	}
	if result.Skipped[0].Reason == "" {
		t.Error("a skip must name its reason")
	}
	if !result.Passed {
		t.Errorf("the one runnable entry passed; the probe must report passed: %+v", result)
	}
}

// TestProbeTrustEnforcesPerEntryTimeout is AC3: a hung gate reports a timeout
// exit rather than hanging the verb.
func TestProbeTrustEnforcesPerEntryTimeout(t *testing.T) {
	repo := probeRepo(t)
	entries := []trust.Entry{
		{Name: "hangs", Argv: []string{"sleep", "30"}, Timeout: "200ms"},
	}

	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if len(result.Gates) != 1 {
		t.Fatalf("want one gate row, got %d", len(result.Gates))
	}
	g := result.Gates[0]
	if g.Exit == nil || *g.Exit == 0 {
		t.Errorf("a timed-out gate must not report a zero or absent exit: %+v", g)
	}
	if !strings.Contains(g.LogTail, "timeout") {
		t.Errorf("the log tail should name the timeout, got %q", g.LogTail)
	}
}

// TestProbeTrustRemovesWorktreeOnSuccess and its two siblings below are AC4:
// removed on success, on a failure inside ProbeTrust, and on interruption
// (a canceled context) — `git worktree list` shows no leftover in any case.
func TestProbeTrustRemovesWorktreeOnSuccess(t *testing.T) {
	repo := probeRepo(t)
	entries := []trust.Entry{{Name: "ok", Argv: []string{"true"}}}

	_, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if n := worktreeCount(t, repo); n != 0 {
		t.Errorf("want no leftover worktree after a clean probe, got %d", n)
	}
}

func TestProbeTrustRemovesWorktreeWhenEntriesFail(t *testing.T) {
	repo := probeRepo(t)
	entries := []trust.Entry{{Name: "fails", Argv: []string{"false"}}}

	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)
	if result.Passed {
		t.Fatal("the fixture entry fails; the probe must not report passed")
	}

	if n := worktreeCount(t, repo); n != 0 {
		t.Errorf("want no leftover worktree after a probe with failures, got %d", n)
	}
}

func TestProbeTrustRemovesWorktreeOnCanceledContext(t *testing.T) {
	repo := probeRepo(t)
	entries := []trust.Entry{
		{Name: "one", Argv: []string{"true"}},
		{Name: "two", Argv: []string{"true"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ProbeTrust(ctx, repo, entries, noAction)
	if err == nil {
		t.Fatal("a canceled context should surface as an error")
	}
	if n := worktreeCount(t, repo); n != 0 {
		t.Errorf("want no leftover worktree after interruption, got %d", n)
	}
}

// TestProbeTrustRefusesAnEmptyRoster mirrors gate-probe.js's rule: a probe
// that measured nothing must not read as a pass.
func TestProbeTrustRefusesAnEmptyRoster(t *testing.T) {
	repo := probeRepo(t)

	if _, err := ProbeTrust(context.Background(), repo, nil, noAction); err == nil {
		t.Error("an empty roster must refuse rather than report zero gates as a pass")
	}

	entries := []trust.Entry{{Name: "aggregate", Argv: []string{"true"}}}
	isAction := func(string) bool { return true }
	if _, err := ProbeTrust(context.Background(), repo, entries, isAction); err == nil {
		t.Error("a roster that is entirely declared actions must refuse, not report a vacuous pass")
	}
}

// TestProbeTrustHeadIsRepoHead pins Head to the resolved sha, which is what a
// caller uses to say a failure was measured on clean HEAD.
func TestProbeTrustHeadIsRepoHead(t *testing.T) {
	repo := probeRepo(t)
	want, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	testsupport.Must(t, err, "git rev-parse HEAD: %v", err)

	entries := []trust.Entry{{Name: "ok", Argv: []string{"true"}}}
	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)

	if result.Head != strings.TrimSpace(string(want)) {
		t.Errorf("Head = %q, want %q", result.Head, strings.TrimSpace(string(want)))
	}
}

// TestProbeTrustMeasuresTheWorktreeNotTheSharedCheckout: a gate that reads its
// own cwd sees the throwaway checkout, not the shared repo — so a later `git
// worktree add` of a different branch in the shared checkout cannot leak into
// a probe's measurement.
func TestProbeTrustMeasuresTheWorktreeNotTheSharedCheckout(t *testing.T) {
	repo := probeRepo(t)
	marker := repo + "/marker-only-in-shared-checkout"
	err := os.WriteFile(marker, []byte("x"), 0o644)
	testsupport.Must(t, err, "writing marker: %v", err)
	// Deliberately left UNCOMMITTED: the throwaway worktree is a checkout of
	// HEAD, which never saw this file.
	entries := []trust.Entry{{Name: "check", Argv: []string{"test", "!", "-e", "marker-only-in-shared-checkout"}}}

	result, err := ProbeTrust(context.Background(), repo, entries, noAction)
	testsupport.Must(t, err, "ProbeTrust: %v", err)
	if !result.Passed {
		t.Errorf("the probed tree must be the throwaway HEAD checkout, not the shared working tree: %+v", result.Gates)
	}
}
