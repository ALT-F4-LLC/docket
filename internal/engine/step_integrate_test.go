package engine

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `step integrate` is the engine's one git write: it cherry-picks a finished
// writer's recorded chain onto the shared checkout and annotates the result.
// Every test here runs real git in a throwaway repository; the observable is
// `git rev-parse HEAD` and the step's recorded annotation.

type integrateEnv struct {
	conn         *sql.DB
	e            *Engine
	stepID       int
	shared, base string
	head         string
	stepID2      int // the second writer, when the fixture has one
}

// newIntegrateEnv builds the shared checkout (one trunk commit, a sibling that
// landed after the writer forked, and a two-commit `writer` branch), records
// the writer's worktree as a write-class step's hand-back, and wires the real
// integration seams. complete=false leaves the step claimed and unfinished.
func newIntegrateEnv(t *testing.T, complete bool) integrateEnv {
	t.Helper()
	return newIntegrateEnvWith(t, complete, false)
}

// newIntegrateEnvWith with second=true adds a second finished writer whose one
// commit adds internal/a.go with other content, so it conflicts once the first
// writer has landed.
func newIntegrateEnvWith(t *testing.T, complete, second bool) integrateEnv {
	t.Helper()
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "qa", "GIT_AUTHOR_EMAIL": "qa@example.invalid",
		"GIT_COMMITTER_NAME": "qa", "GIT_COMMITTER_EMAIL": "qa@example.invalid",
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null",
	} {
		t.Setenv(k, v)
	}
	shared, base, head := rangeRepo(t)
	pinned := gitRun(t, shared, "rev-parse", "HEAD")

	conn := mustDB(t)
	registerSource(t, conn, []byte(writeClassWorkflowSrc), "integrate-fixture.toml")
	issue := createIssue(t, conn, "integrate fixture", "a body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "integrate run", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue")
	issue2 := 0
	if second {
		issue2 = createIssue(t, conn, "integrate fixture two", "a body", "task", nil)
		testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue2), "AddRunIssue two")
	}
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	e.DiffFn = GitDiff
	e.HeadFn = sharedCheckoutHead
	e.IsAncestorFn = gitAncestorOfHead
	e.IntegrationRangeFn = gitIntegratedRange
	e.CommitPatchFn = GitCommitPatch
	e.RangePatchFn = GitRangePatch
	e.IntegrationChainFn = gitIntegrationChain
	e.CherryPickFn = gitCherryPickRange

	work := filepath.Join(t.TempDir(), "wf-implement")
	gitRun(t, shared, "worktree", "add", "-q", work, "writer")
	stepID := stepIDIn(t, conn, issue, "implement@0")
	if complete {
		completeStepAt(t, conn, e, issue, "implement@0", work)
		if st := mustGetStep(t, conn, stepID); st.Status != db.StepDone {
			t.Fatalf("fixture: implement@0 is %s, want done", st.Status)
		}
	}
	stepID2 := 0
	if second {
		work2 := filepath.Join(t.TempDir(), "wf-implement-two")
		gitRun(t, shared, "worktree", "add", "-q", "-b", "writer2", work2, base)
		writeFile(t, work2, "internal/a.go", "package second\n")
		gitRun(t, work2, "add", "-A")
		gitRun(t, work2, "commit", "-q", "-m", "writer two takes a.go")
		stepID2 = stepIDIn(t, conn, issue2, "implement@0")
		completeStepAt(t, conn, e, issue2, "implement@0", work2)
	}
	return integrateEnv{conn: conn, e: e, stepID: stepID, stepID2: stepID2, shared: shared, base: base, head: head}
}

func (f integrateEnv) integrate(token string, ids ...int) ([]StepIntegration, error) {
	return f.e.IntegrateSteps(f.conn, ids, testBy, token, nowMS+1)
}

func (f integrateEnv) sharedHead(t *testing.T) string {
	t.Helper()
	return gitRun(t, f.shared, "rev-parse", "HEAD")
}

func (f integrateEnv) integratedSHA(t *testing.T) string {
	t.Helper()
	var meta map[string]any
	_ = json.Unmarshal([]byte(mustGetStep(t, f.conn, f.stepID).Metadata), &meta)
	s, _ := meta[MetadataKeyIntegratedSHA].(string)
	return s
}

func (f integrateEnv) requireNothingLanded(t *testing.T, headBefore string) {
	t.Helper()
	if got := f.sharedHead(t); got != headBefore {
		t.Errorf("shared HEAD = %.12s, want it unchanged at %.12s", got, headBefore)
	}
	if got := f.integratedSHA(t); got != "" {
		t.Errorf("step annotated integrated_sha %.12s, want none", got)
	}
	if _, err := os.Stat(filepath.Join(f.shared, ".git", "CHERRY_PICK_HEAD")); err == nil {
		t.Error("a cherry-pick is still in progress in the shared checkout")
	}
}

// TestIntegrateStepsPicksTheChainAndAnnotatesTheResultingHead: a two-commit
// writer lands both commits with -x and the step is annotated with HEAD.
func TestIntegrateStepsPicksTheChainAndAnnotatesTheResultingHead(t *testing.T) {
	f := newIntegrateEnv(t, true)
	before := f.sharedHead(t)

	got, err := f.integrate(testConductorToken, f.stepID)
	testsupport.Must(t, err, "integrate: %v", err)

	after := f.sharedHead(t)
	if len(got) != 1 || got[0].IntegratedSHA != after || len(got[0].Picked) != 2 {
		t.Fatalf("result = %+v, want one step integrated at %s with two commits", got, after)
	}
	if n := gitRun(t, f.shared, "rev-list", "--count", before+".."+after); n != "2" {
		t.Errorf("shared branch gained %s commits, want 2", n)
	}
	log := gitRun(t, f.shared, "log", "-2", "--format=%B")
	if strings.Count(log, "(cherry picked from commit ") != 2 {
		t.Errorf("picked commits lack the -x trailer:\n%s", log)
	}
	if f.integratedSHA(t) != after {
		t.Errorf("step integrated_sha = %q, want %s", f.integratedSHA(t), after)
	}
	latest, err := stepLatestIssueDiff(f.conn, f.stepID)
	testsupport.Must(t, err, "stepLatestIssueDiff: %v", err)
	if handBackHead(latest.Payload) != after {
		t.Errorf("issue.diff head = %q, want %s", handBackHead(latest.Payload), after)
	}
	for _, file := range []string{"internal/a.go", "internal/b.go"} {
		if !strings.Contains(latest.Body, file) {
			t.Errorf("re-pinned issue.diff lacks %s:\n%s", file, latest.Body)
		}
	}
}

// TestIntegrateStepsAbortsAConflictAndAnnotatesNothing is the defect's shape: a
// pick that does not apply leaves HEAD where it was, no pick in progress and
// the step unannotated, instead of annotating the previous HEAD.
func TestIntegrateStepsAbortsAConflictAndAnnotatesNothing(t *testing.T) {
	f := newIntegrateEnv(t, true)
	// The trunk adds the file the writer's first commit adds, with other content.
	writeFile(t, f.shared, "internal/a.go", "package trunk\n")
	gitRun(t, f.shared, "add", "-A")
	gitRun(t, f.shared, "commit", "-q", "-m", "trunk takes a.go")
	before := f.sharedHead(t)

	got, err := f.integrate(testConductorToken, f.stepID)
	if err == nil {
		t.Fatalf("integrate over a conflict succeeded: %+v", got)
	}
	if !strings.Contains(err.Error(), "aborted") || !strings.Contains(err.Error(), "unchanged") {
		t.Errorf("error = %v, want it to say the pick was aborted and HEAD is unchanged", err)
	}
	f.requireNothingLanded(t, before)
	if status := gitRun(t, f.shared, "status", "--porcelain", "--untracked-files=no"); status != "" {
		t.Errorf("the checkout is dirty after the abort:\n%s", status)
	}
}

// TestIntegrateStepsWritesNothingWhenALaterStepFailsItsChecks: every step's
// read-only checks run before the first pick.
func TestIntegrateStepsWritesNothingWhenALaterStepFailsItsChecks(t *testing.T) {
	f := newIntegrateEnv(t, true)
	before := f.sharedHead(t)
	if _, err := f.integrate(testConductorToken, f.stepID, 987654); err == nil {
		t.Fatal("integrate with an unknown second step succeeded")
	}
	f.requireNothingLanded(t, before)
	if _, err := f.integrate(testConductorToken, f.stepID, f.stepID); err == nil {
		t.Fatal("integrate naming one step twice succeeded")
	}
	f.requireNothingLanded(t, before)
}

// TestIntegrateStepsRefusesAStepThatIsNotDone: a claimed, unfinished step has
// no recorded chain and is refused before git is asked anything.
func TestIntegrateStepsRefusesAStepThatIsNotDone(t *testing.T) {
	f := newIntegrateEnv(t, false)
	before := f.sharedHead(t)
	_, err := f.integrate(testConductorToken, f.stepID)
	if err == nil || !strings.Contains(err.Error(), "only a done step") {
		t.Fatalf("error = %v, want a refusal naming that only a done step is integrated", err)
	}
	f.requireNothingLanded(t, before)
}

// TestIntegrateStepsRefusesTheWrongConductorToken: the verb is conductor-gated.
func TestIntegrateStepsRefusesTheWrongConductorToken(t *testing.T) {
	f := newIntegrateEnv(t, true)
	before := f.sharedHead(t)
	if _, err := f.integrate("not-the-token", f.stepID); err == nil {
		t.Fatal("integrate with a wrong token succeeded")
	}
	f.requireNothingLanded(t, before)
	if _, err := f.e.IntegrateSteps(f.conn, []int{f.stepID}, Attribution{}, testConductorToken, nowMS); err == nil {
		t.Fatal("integrate without attribution succeeded")
	}
	f.requireNothingLanded(t, before)
}

// TestIntegrateStepsRefusesAnEngineWithoutTheGitSeams: an engine built without
// the write seam, every test engine by default, cannot write the shared branch.
func TestIntegrateStepsRefusesAnEngineWithoutTheGitSeams(t *testing.T) {
	f := newIntegrateEnv(t, true)
	before := f.sharedHead(t)
	f.e.CherryPickFn = nil
	_, err := f.integrate(testConductorToken, f.stepID)
	if err == nil || !strings.Contains(err.Error(), "no git integration wired") {
		t.Fatalf("error = %v, want the unwired-engine refusal", err)
	}
	f.requireNothingLanded(t, before)
}

// TestIntegrateStepsRefusesASharedCheckoutItWouldMixWith: a tracked change, a
// detached HEAD and an operation in progress each stop the verb with nothing
// written.
func TestIntegrateStepsRefusesASharedCheckoutItWouldMixWith(t *testing.T) {
	t.Run("tracked change", func(t *testing.T) {
		f := newIntegrateEnv(t, true)
		writeFile(t, f.shared, "internal/tracked.txt", "edited\n")
		before := f.sharedHead(t)
		_, err := f.integrate(testConductorToken, f.stepID)
		if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
			t.Fatalf("error = %v, want the dirty-checkout refusal", err)
		}
		f.requireNothingLanded(t, before)
		if got := gitRun(t, f.shared, "status", "--porcelain", "--untracked-files=no"); !strings.Contains(got, "tracked.txt") {
			t.Errorf("the operator's edit is gone: %q", got)
		}
	})
	t.Run("untracked file is not a change", func(t *testing.T) {
		f := newIntegrateEnv(t, true)
		writeFile(t, f.shared, "scratch-notes.txt", "mine\n")
		if _, err := f.integrate(testConductorToken, f.stepID); err != nil {
			t.Fatalf("an untracked file blocked the pick: %v", err)
		}
	})
	t.Run("detached HEAD", func(t *testing.T) {
		f := newIntegrateEnv(t, true)
		gitRun(t, f.shared, "checkout", "-q", "--detach")
		before := f.sharedHead(t)
		_, err := f.integrate(testConductorToken, f.stepID)
		if err == nil || !strings.Contains(err.Error(), "detached") {
			t.Fatalf("error = %v, want the detached-HEAD refusal", err)
		}
		f.requireNothingLanded(t, before)
	})
	t.Run("operation in progress", func(t *testing.T) {
		f := newIntegrateEnv(t, true)
		// Leave a conflicted pick of an unrelated commit in progress.
		writeFile(t, f.shared, "internal/a.go", "package trunk\n")
		gitRun(t, f.shared, "add", "-A")
		gitRun(t, f.shared, "commit", "-q", "-m", "trunk takes a.go")
		first := gitRun(t, f.shared, "rev-parse", f.head+"~1")
		cmd := exec.Command("git", "-C", f.shared, "cherry-pick", first)
		_ = cmd.Run() // conflicts by construction
		if _, err := os.Stat(filepath.Join(f.shared, ".git", "CHERRY_PICK_HEAD")); err != nil {
			t.Fatalf("fixture: no cherry-pick in progress: %v", err)
		}
		_, err := f.integrate(testConductorToken, f.stepID)
		if err == nil || !strings.Contains(err.Error(), "in progress") {
			t.Fatalf("error = %v, want the operation-in-progress refusal", err)
		}
		if _, statErr := os.Stat(filepath.Join(f.shared, ".git", "CHERRY_PICK_HEAD")); statErr != nil {
			t.Error("the operator's pick in progress was aborted by the verb")
		}
	})
}

// TestIntegrateStepsDoesNotLandAStepTwice: an annotated step is refused, and so
// is one whose chain the checkout already carries (a hand pick whose annotation
// never ran).
func TestIntegrateStepsDoesNotLandAStepTwice(t *testing.T) {
	t.Run("already annotated", func(t *testing.T) {
		f := newIntegrateEnv(t, true)
		_, err := f.integrate(testConductorToken, f.stepID)
		testsupport.Must(t, err, "first integrate: %v", err)
		landed := f.sharedHead(t)
		_, err = f.integrate(testConductorToken, f.stepID)
		if err == nil || !strings.Contains(err.Error(), "already annotated") {
			t.Fatalf("error = %v, want the already-annotated refusal", err)
		}
		if f.sharedHead(t) != landed {
			t.Error("the second integrate moved HEAD")
		}
	})
	t.Run("already carried by HEAD", func(t *testing.T) {
		f := newIntegrateEnv(t, true)
		gitRun(t, f.shared, "cherry-pick", "-x", f.base+".."+f.head)
		landed := f.sharedHead(t)
		_, err := f.integrate(testConductorToken, f.stepID)
		if err == nil || !strings.Contains(err.Error(), "already carries") ||
			!strings.Contains(err.Error(), "step annotate") {
			t.Fatalf("error = %v, want a refusal pointing at `step annotate`", err)
		}
		if f.sharedHead(t) != landed {
			t.Error("integrate picked a chain HEAD already carried")
		}
	})
}

// TestIntegrateRunsNoRepositoryHook: a repository hook is code the repository's
// writers control; the engine's pick runs none, so a post-commit hook leaves no
// mark and a failing prepare-commit-msg hook cannot block the pick.
func TestIntegrateRunsNoRepositoryHook(t *testing.T) {
	f := newIntegrateEnv(t, true)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hooks := filepath.Join(f.shared, ".git", "hooks")
	testsupport.Must(t, os.MkdirAll(hooks, 0o755), "mkdir hooks")
	for name, body := range map[string]string{
		"post-commit":        "#!/bin/sh\ntouch " + marker + "\n",
		"prepare-commit-msg": "#!/bin/sh\ntouch " + marker + "\nexit 1\n",
		"post-rewrite":       "#!/bin/sh\ntouch " + marker + "\n",
	} {
		testsupport.Must(t, os.WriteFile(filepath.Join(hooks, name), []byte(body), 0o755), "write hook")
	}
	if _, err := f.integrate(testConductorToken, f.stepID); err != nil {
		t.Fatalf("integrate with hooks installed: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a repository hook ran from the engine's pick")
	}
}

// TestIntegrateStepsKeepsEarlierStepsWhenALaterOneFails: steps are integrated in
// the order named; a later pick that conflicts is aborted, and the earlier step
// stays landed, annotated and reported.
func TestIntegrateStepsKeepsEarlierStepsWhenALaterOneFails(t *testing.T) {
	f := newIntegrateEnvWith(t, true, true)
	got, err := f.integrate(testConductorToken, f.stepID, f.stepID2)
	if err == nil {
		t.Fatalf("integrating a conflicting second step succeeded: %+v", got)
	}
	if len(got) != 1 || got[0].Step != model.FormatStepID(f.stepID) {
		t.Fatalf("completed = %+v, want only %s", got, model.FormatStepID(f.stepID))
	}
	if !strings.Contains(err.Error(), model.FormatStepID(f.stepID2)) {
		t.Errorf("error = %v, want it to name the step that failed", err)
	}
	landed := f.sharedHead(t)
	if landed != got[0].IntegratedSHA || f.integratedSHA(t) != landed {
		t.Errorf("HEAD %.12s, first step annotated %.12s, reported %.12s; want all equal",
			landed, f.integratedSHA(t), got[0].IntegratedSHA)
	}
	var meta map[string]any
	_ = json.Unmarshal([]byte(mustGetStep(t, f.conn, f.stepID2).Metadata), &meta)
	if meta[MetadataKeyIntegratedSHA] != nil {
		t.Errorf("the failed step was annotated: %v", meta)
	}
	if _, statErr := os.Stat(filepath.Join(f.shared, ".git", "CHERRY_PICK_HEAD")); statErr == nil {
		t.Error("the failed pick was left in progress")
	}
}
