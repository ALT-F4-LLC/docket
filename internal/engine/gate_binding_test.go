package engine

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// bindingWorld is a step recorded from a worktree with one commit of its own,
// completed with its gates handed to a detached run that has not started.
type bindingWorld struct {
	conn     *sql.DB
	e        *Engine
	captured *treeCapture
	worktree string
	recorded string
	stepID   int
}

func newBindingWorld(t *testing.T, dirty bool) *bindingWorld {
	t.Helper()
	shared := gitRepo(t)
	pinned := strings.TrimSpace(gitOutput(t, shared, "rev-parse", "HEAD"))
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "binding", "body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "binding", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue")
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	w := &bindingWorld{conn: conn, e: testEngine()}
	w.worktree = filepath.Join(t.TempDir(), "wt")
	runGit(t, shared, "worktree", "add", "-q", w.worktree)
	writeFile(t, w.worktree, "mine/first.txt", "first\n")
	runGit(t, w.worktree, "add", "-A")
	runGit(t, w.worktree, "commit", "-q", "-m", "first")
	w.recorded = strings.TrimSpace(gitOutput(t, w.worktree, "rev-parse", "HEAD"))
	if dirty {
		writeFile(t, w.worktree, "mine/untracked.txt", "left over\n")
	}

	w.e.DiffFn = GitDiff
	w.captured = &treeCapture{baseCapture: baseCapture{seen: map[string]StepContext{}}, heads: map[string]string{}}
	w.e.Gates = w.captured
	withDetachedPreGateLockDir(t)
	installDetachedSagaLauncher(t, func(int) error { return nil })
	w.stepID = stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, w.stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	var pending bool
	testsupport.Must(t, w.e.CompleteStep(conn, w.stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), WorkDir: w.worktree,
		Pending: &pending, NowMS: nowMS,
	}), "complete")
	if !pending {
		t.Fatal("premise: the gates were not handed to a detached run")
	}
	return w
}

func (w *bindingWorld) gateVerdicts(t *testing.T) []string {
	t.Helper()
	rows, err := w.conn.Query(`SELECT verdict FROM gate_results WHERE step_id = ?`, w.stepID)
	testsupport.Must(t, err, "reading gate results: %v", err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		testsupport.Must(t, rows.Scan(&v), "scan")
		out = append(out, v)
	}
	return out
}

// A worktree holding untracked state cannot be replaced by a checkout, so the
// gates measure it live. The record still pins its head: a worktree that moves
// to another commit after the record is not measured, and its diff is refused.
func TestALiveRecordRefusesAWorktreeThatMovedOn(t *testing.T) {
	w := newBindingWorld(t, true)

	writeFile(t, w.worktree, "mine/second.txt", "after the record\n")
	runGit(t, w.worktree, "add", "-A")
	runGit(t, w.worktree, "commit", "-q", "-m", "second")

	_, err := w.e.RunDetachedSaga(w.conn, w.stepID, nowMS)
	testsupport.Must(t, err, "detached saga: %v", err)
	if len(w.captured.seen) != 0 {
		t.Errorf("%d gate(s) ran against a worktree that moved off the recorded commit", len(w.captured.seen))
	}
	verdicts := w.gateVerdicts(t)
	if len(verdicts) == 0 {
		t.Fatal("no gate row was recorded")
	}
	for _, v := range verdicts {
		if v != VerdictSkipped {
			t.Errorf("gate verdicts = %v; want only %q", verdicts, VerdictSkipped)
		}
	}

	step, err := db.GetStep(w.conn, w.stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	var refusal *diffRefusalError
	if _, _, err := w.e.computeIssueDiff(w.conn, step, true); !errors.As(err, &refusal) {
		t.Errorf("the diff stage = %v for a worktree that moved off the recorded commit; want a diff refusal", err)
	}
	w.requireParkedWithoutADiff(t)
}

// A Live record whose worktree stays put is measured as before.
func TestALiveRecordStillMeasuresAWorktreeThatStayedPut(t *testing.T) {
	w := newBindingWorld(t, true)
	_, err := w.e.RunDetachedSaga(w.conn, w.stepID, nowMS)
	testsupport.Must(t, err, "detached saga: %v", err)
	if len(w.captured.seen) == 0 {
		t.Fatal("no gate measured the live worktree")
	}
	for gate, sc := range w.captured.seen {
		if sc.WorkRoot != w.worktree {
			t.Errorf("gate %s measured %q, want the live worktree %q", gate, sc.WorkRoot, w.worktree)
		}
	}
}

// Once a record is honored there is no fallback to the live worktree: a sha the
// object database no longer holds records a skipped row, and the diff stage
// refuses, rather than either one reading whatever the worktree holds now.
func TestAnUnreachableRecordedShaDoesNotFallBackToTheWorktree(t *testing.T) {
	w := newBindingWorld(t, false)
	bogus := strings.Repeat("a", 40)
	execSQL(t, w.conn,
		`UPDATE artifacts SET payload = ? WHERE step_id = ? AND kind = ?`,
		`{"head":"`+bogus+`","base":"`+bogus+`","worktree":"`+w.worktree+`"}`,
		w.stepID, RecordedHeadArtifactKind)

	_, err := w.e.RunDetachedSaga(w.conn, w.stepID, nowMS)
	testsupport.Must(t, err, "detached saga: %v", err)
	if len(w.captured.seen) != 0 {
		t.Errorf("%d gate(s) measured a tree though the recorded commit was unreachable", len(w.captured.seen))
	}
	for _, v := range w.gateVerdicts(t) {
		if v != VerdictSkipped {
			t.Errorf("gate verdict = %q; want %q", v, VerdictSkipped)
		}
	}
	step, err := db.GetStep(w.conn, w.stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	var refusal *diffRefusalError
	if _, _, err := w.e.computeIssueDiff(w.conn, step, true); !errors.As(err, &refusal) {
		t.Errorf("the diff stage = %v for an unreachable recorded commit; want a diff refusal", err)
	}
	w.requireParkedWithoutADiff(t)
}

// A step recorded again under the same id (a re-claim) must not keep the first
// attempt's head when the second attempt records none.
func TestARecordingWithNoHeadClearsTheEarlierRecord(t *testing.T) {
	w := newBindingWorld(t, false)
	step, err := db.GetStep(w.conn, w.stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if _, ok := recordedHeadOf(w.conn, step); !ok {
		t.Fatal("premise: the first recording left no head")
	}

	tx, err := w.conn.Begin()
	testsupport.Must(t, err, "begin")
	testsupport.Must(t, insertRecordedHeadTx(tx, step, recordedHead{}, false, w.worktree, nowMS+1), "insert")
	testsupport.Must(t, tx.Commit(), "commit")
	if rec, ok := recordedHeadOf(w.conn, step); ok {
		t.Errorf("the first attempt's head %q still stands after a recording with none", rec.Head)
	}
}

// The residual, pinned so it is stated rather than assumed: a Live record binds
// the worktree's HEAD only. Uncommitted content edited at the SAME head after
// the record is still measured live, because binding it would take a gate run
// in a checkout of a snapshot of that content (and close never verifies
// uncommitted content either).
func TestALiveRecordPinsHeadOnlyNotUncommittedContent(t *testing.T) {
	w := newBindingWorld(t, true)
	writeFile(t, w.worktree, "mine/untracked.txt", "edited after the record\n")

	_, err := w.e.RunDetachedSaga(w.conn, w.stepID, nowMS)
	testsupport.Must(t, err, "detached saga: %v", err)
	if len(w.captured.seen) == 0 {
		t.Error("no gate measured the live worktree; the residual changed, update the docs")
	}
}

// A refused diff PARKS the step for the operator: it neither errors the stage
// (which would leave the step at the routing stage, unresolvable) nor records a
// diff, and the run's `next` sweep keeps working.
func (w *bindingWorld) requireParkedWithoutADiff(t *testing.T) {
	t.Helper()
	step, err := db.GetStep(w.conn, w.stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepWaitingHuman {
		t.Errorf("step status = %q after a refused diff; want %q", step.Status, db.StepWaitingHuman)
	}
	if step.SagaStage != "" {
		t.Errorf("saga stage = %q after a refused diff; want the saga finished", step.SagaStage)
	}
	var diffs int
	testsupport.Must(t, w.conn.QueryRow(
		`SELECT COUNT(*) FROM artifacts WHERE step_id = ? AND kind = 'issue.diff'`, w.stepID,
	).Scan(&diffs), "counting diffs")
	if diffs != 0 {
		t.Errorf("%d issue.diff artifact(s) recorded for a refused diff", diffs)
	}
	testsupport.Must(t, w.e.ResumeStrandedSagas(w.conn, step.RunID, nowMS+10*strandedSagaGraceMS),
		"the stranded-saga sweep after a refused diff")
}
