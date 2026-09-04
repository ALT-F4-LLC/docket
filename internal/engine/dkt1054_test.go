package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-1054: `step context` on a step that has been handed out replays the
// bindings its claim RECORDED (`step_inputs`), instead of re-resolving §6.7
// over the run's current artifact table.
//
// The claim always wrote the table; nothing read it. So a completed `fix@1`
// — which bound `reconcile@0` at claim, the round that routed it — read back
// as consuming `reconcile@1` once that step completed at fix@1's own ordinal,
// and a judge's `target_sha` moved onto whichever record superseded its
// producer's diff after the judge had already been seated. RUN-64's STEP-2992
// is the first shape; RUN-53/56/61's target drift is the second. Both are the
// one defect: assembly is a function of run state, run state moves, and the
// read verb asked the live question about a step whose answer was frozen.

// inputRefs renders inputs the way the issue's evidence table reads them —
// kind, producer, artifact — for a failure message a reader can check against
// the ledger.
func inputRefs(inputs []ContextInput) []string {
	out := make([]string, 0, len(inputs))
	for _, in := range inputs {
		out = append(out, fmt.Sprintf("%s/%s/%s", in.Kind, in.ProducerStep, in.Artifact))
	}
	return out
}

// producersOfKind lists the producer instances bound for one input kind, in
// bundle order.
func producersOfKind(inputs []ContextInput, kind string) []string {
	var out []string
	for _, in := range inputs {
		if in.Kind == kind {
			out = append(out, in.ProducerStep)
		}
	}
	return out
}

// artifactOfKind is the artifact ref bound for one input kind, or "" when the
// bundle carries none.
func artifactOfKind(inputs []ContextInput, kind string) string {
	for _, in := range inputs {
		if in.Kind == kind {
			return in.Artifact
		}
	}
	return ""
}

// newestArtifactID is the highest artifact id one step recorded of a kind.
func newestArtifactID(t *testing.T, conn *sql.DB, stepID int, kind string) int {
	t.Helper()
	var id int
	err := conn.QueryRow(
		`SELECT MAX(id) FROM artifacts WHERE step_id = ? AND kind = ?`, stepID, kind,
	).Scan(&id)
	testsupport.Must(t, err, "newest %s of step %d: %v", kind, stepID, err)
	return id
}

// recordedInputIDs reads a step's `step_inputs` bindings, by artifact id.
func recordedInputIDs(t *testing.T, conn *sql.DB, stepID int) []int {
	t.Helper()
	rows, err := conn.Query(
		`SELECT artifact_id FROM step_inputs WHERE step_id = ? ORDER BY artifact_id`, stepID)
	testsupport.Must(t, err, "reading step_inputs: %v", err)
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		testsupport.Must(t, rows.Scan(&id), "scanning step_inputs")
		out = append(out, id)
	}
	return out
}

// supersedeIssueDiff records a NEW `issue.diff` for a step that supersedes
// its newest one — the row a re-pin (DKT-1034) writes — carrying the round
// record every consumer lifts its target from.
func supersedeIssueDiff(
	t *testing.T, conn *sql.DB, runID, stepID int, head, worktree string,
) int {
	t.Helper()
	prior := newestArtifactID(t, conn, stepID, ArtifactKindIssueDiff)
	payload, err := json.Marshal(map[string]string{"head": head, "worktree": worktree})
	testsupport.Must(t, err, "encoding the round record: %v", err)
	body := "the re-pinned diff at " + head

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	id, err := db.InsertArtifactTx(tx, db.Artifact{
		RunID: runID, StepID: stepID, Kind: ArtifactKindIssueDiff,
		Body: body, Payload: string(payload),
		SHA256: workflow.SHA256([]byte(body)), Supersedes: &prior,
	}, nowMS+1)
	if err != nil {
		tx.Rollback()
		t.Fatalf("InsertArtifactTx: %v", err)
	}
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)
	return id
}

// reapStep returns a lapsed step to the pool, as `next`/`claim` do lazily.
func reapStep(t *testing.T, conn *sql.DB, stepID int) {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	if err := db.ReapStepTx(tx, stepID, nowMS); err != nil {
		tx.Rollback()
		t.Fatalf("ReapStepTx: %v", err)
	}
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)
}

// TestClaimedStepContextReplaysTheRecordedInputs is RUN-64's STEP-2992, in
// the fixture: `fix@1` reads `reconcile.findings`, is handed `reconcile@0`
// (the instance whose routing minted it), and then `review@1`, `synthesize@1`
// and `reconcile@1` complete AT ITS OWN ORDINAL. The read-back must be the
// bundle the worker was handed — and the live resolution, kept reachable as
// `--live`, must be the different answer it always was, or the test proves
// nothing.
func TestClaimedStepContextReplaysTheRecordedInputs(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	// Round 0 through its reconcile, whose `fix-loop` routing mints fix@1.
	driveRoundToReconcile(t, conn, e, 0)

	// fix@1 is claimed by hand, so the bundle the worker was HANDED is in
	// hand for the comparison below.
	driveFixtureRound(t, 1)
	fixID := stepIDByInstance(t, conn, "fix@1")
	claim, err := ClaimStep(conn, fixID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim fix@1: %v", err)
	handed := claim.Context
	err = e.CompleteStep(conn, fixID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the fix summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete fix@1: %v", err)

	if got := producersOfKind(handed.Inputs, "findings"); !reflect.DeepEqual(got, []string{"reconcile@0"}) {
		t.Fatalf("premise: fix@1's claim bound `reconcile.findings` to %v, want [reconcile@0]", got)
	}

	// The rest of round 1 completes at fix@1's OWN ordinal, ending in a
	// `findings` artifact from `reconcile@1` — produced by reviewing fix@1's
	// diff, and so an artifact fix@1 cannot have read.
	completeReviewFanout(t, conn, e, 1)
	claimAndComplete(t, conn, e, "synthesize@1", "the synthesis", roundPayload(1))
	driveAction(t, conn, e, "reconcile@1")
	if got := stepStatus(t, conn, "reconcile@1"); got != db.StepDone {
		t.Fatalf("premise: reconcile@1 = %q, want %q", got, db.StepDone)
	}

	replayed, err := ReadContext(conn, fixID, nowMS)
	testsupport.Must(t, err, "ReadContext(fix@1): %v", err)

	if !reflect.DeepEqual(replayed.Inputs, handed.Inputs) {
		t.Errorf("fix@1's read-back is not the bundle its claim handed over.\n"+
			"handed:   %v\nread-back: %v", inputRefs(handed.Inputs), inputRefs(replayed.Inputs))
	}
	if got := producersOfKind(replayed.Inputs, "findings"); !reflect.DeepEqual(got, []string{"reconcile@0"}) {
		t.Errorf("`reconcile.findings` reads back from %v, want [reconcile@0] — "+
			"the read-back re-bound to a later step of the same ordinal", got)
	}
	for _, in := range replayed.Inputs {
		if in.Kind == "findings" && strings.Contains(in.Payload, "C-201") {
			t.Errorf("fix@1's read-back carries round 2's cluster from %s: %s",
				in.ProducerStep, in.Payload)
		}
	}
	if got := producersOfKind(replayed.Inputs, "change-summary"); !reflect.DeepEqual(got, []string{"implement@0"}) {
		t.Errorf("`implement.change-summary` reads back from %v, want [implement@0]", got)
	}

	// The read-back is stable: a second call at a later run state, and after
	// the run moved on, returns the same bytes.
	first := bundleJSON(t, conn, fixID)
	driveFixtureRound(t, 2)
	claimAndComplete(t, conn, e, "fix@2", "the second fix summary", "")
	if again := bundleJSON(t, conn, fixID); again != first {
		t.Errorf("fix@1's bundle changed after fix@2 recorded:\n%s\n%s", first, again)
	}

	// --live is the old reading, and it is DIFFERENT — which is what makes the
	// assertions above non-vacuous.
	live, err := ReadLiveContext(conn, fixID, nowMS)
	testsupport.Must(t, err, "ReadLiveContext(fix@1): %v", err)
	if got := producersOfKind(live.Inputs, "findings"); !reflect.DeepEqual(got, []string{"reconcile@1"}) {
		t.Errorf("--live binds `reconcile.findings` to %v, want [reconcile@1] (the current resolution)", got)
	}
}

// TestClaimedStepTargetIsTheHandedOverTarget is the `target_sha` half: a
// record superseding the producer's diff at the SAME ordinal — what a re-pin
// writes — moves every not-yet-seated consumer onto the new tree (DKT-1034's
// contract) and moves NO consumer that was already seated. `step show` must
// say the same as `step context` on both, since the wave seats a panel from
// the row (DKT-1056).
func TestClaimedStepTargetIsTheHandedOverTarget(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	e.HeadFn = func(string) string { return "head-original" }

	implementID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, implementID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = e.CompleteStep(conn, implementID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"),
		WorkDir: "/worktrees/original", NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete implement: %v", err)

	seatedID := stepIDByInstance(t, conn, "review@0#0")
	seated, err := ClaimStep(conn, seatedID, ClaimOptions{Owner: "judge", NowMS: nowMS})
	testsupport.Must(t, err, "claim review@0#0: %v", err)
	if seated.Context.TargetSHA != "head-original" {
		t.Fatalf("premise: the seated judge's target = %q, want head-original", seated.Context.TargetSHA)
	}
	handedDiff := artifactOfKind(seated.Context.Inputs, ArtifactKindIssueDiff)

	repinned := supersedeIssueDiff(t, conn, run.ID, implementID, "head-repinned", "/worktrees/repinned")

	// The seated judge: the bundle, the row, and the render all keep the
	// target it was handed.
	replayed, err := ReadContext(conn, seatedID, nowMS)
	testsupport.Must(t, err, "ReadContext(review@0#0): %v", err)
	if replayed.TargetSHA != "head-original" || replayed.TargetWorktree != "/worktrees/original" {
		t.Errorf("the seated judge reads back target (%q, %q), want the handed-over (head-original, /worktrees/original)",
			replayed.TargetSHA, replayed.TargetWorktree)
	}
	if got := artifactOfKind(replayed.Inputs, ArtifactKindIssueDiff); got != handedDiff {
		t.Errorf("the seated judge's issue.diff reads back as %s, want the handed-over %s", got, handedDiff)
	}
	view, err := LoadStepView(conn, seatedID, nowMS)
	testsupport.Must(t, err, "LoadStepView(review@0#0): %v", err)
	if view.TargetSHA != replayed.TargetSHA || view.TargetWorktree != replayed.TargetWorktree {
		t.Errorf("step show says (%q, %q) while the bundle says (%q, %q)",
			view.TargetSHA, view.TargetWorktree, replayed.TargetSHA, replayed.TargetWorktree)
	}
	rendered, err := RenderStep(conn, seatedID, "", nowMS)
	testsupport.Must(t, err, "RenderStep(review@0#0): %v", err)
	if !strings.Contains(rendered.Packet, "target_sha: head-original") {
		t.Errorf("the seated judge's packet no longer names its handed-over target:\n%s", rendered.Packet)
	}

	// --live is the current resolution: the superseding record.
	live, err := ReadLiveContext(conn, seatedID, nowMS)
	testsupport.Must(t, err, "ReadLiveContext(review@0#0): %v", err)
	if live.TargetSHA != "head-repinned" {
		t.Errorf("--live target = %q, want head-repinned", live.TargetSHA)
	}
	if got := artifactOfKind(live.Inputs, ArtifactKindIssueDiff); got != fmt.Sprintf("ARTIFACT-%d", repinned) {
		t.Errorf("--live issue.diff = %s, want the superseding ARTIFACT-%d", got, repinned)
	}

	// An UNSEATED sibling has no snapshot: it binds the superseding record,
	// and the row says so too.
	unseatedID := stepIDByInstance(t, conn, "review@0#1")
	pending, err := ReadContext(conn, unseatedID, nowMS)
	testsupport.Must(t, err, "ReadContext(review@0#1): %v", err)
	if pending.TargetSHA != "head-repinned" || pending.TargetWorktree != "/worktrees/repinned" {
		t.Errorf("the unseated judge's target = (%q, %q), want the superseding (head-repinned, /worktrees/repinned)",
			pending.TargetSHA, pending.TargetWorktree)
	}
	pendingView, err := LoadStepView(conn, unseatedID, nowMS)
	testsupport.Must(t, err, "LoadStepView(review@0#1): %v", err)
	if pendingView.TargetSHA != "head-repinned" {
		t.Errorf("step show on the unseated judge says %q, want head-repinned", pendingView.TargetSHA)
	}
}

// TestReclaimRecordsTheNewAttemptsBindings pins the snapshot's lifecycle: it
// is the CURRENT attempt's. A lapsed lease still reads back its attempt; a
// step reaped to `pending` reads live, because no attempt is in flight and the
// next claim is what will hand it out; and that claim records its bindings IN
// PLACE of the old — not beside them, which the table's (step, position,
// artifact) key would otherwise allow.
func TestReclaimRecordsTheNewAttemptsBindings(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	implementID := stepIDByInstance(t, conn, "implement@0")
	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	first := fmt.Sprintf("ARTIFACT-%d", newestArtifactID(t, conn, implementID, "change-summary"))

	reviewID := stepIDByInstance(t, conn, "review@0#0")
	claim, err := ClaimStep(conn, reviewID, ClaimOptions{Owner: "judge-1", NowMS: nowMS})
	testsupport.Must(t, err, "claim review@0#0: %v", err)
	if got := artifactOfKind(claim.Context.Inputs, "change-summary"); got != first {
		t.Fatalf("premise: the first claim bound change-summary %s, want %s", got, first)
	}

	// implement@0 records a newer change-summary after the judge was handed
	// the first; latestPerProducer binds the newest live.
	revised := fmt.Sprintf("ARTIFACT-%d",
		recordArtifact(t, conn, run.ID, implementID, "change-summary", "the revised summary"))

	changeSummary := func(label string, read func(*sql.DB, int, int64) (*Context, error)) string {
		t.Helper()
		bundle, err := read(conn, reviewID, nowMS)
		testsupport.Must(t, err, "%s: %v", label, err)
		return artifactOfKind(bundle.Inputs, "change-summary")
	}
	if got := changeSummary("read-back under a live lease", ReadContext); got != first {
		t.Errorf("under a live lease the read-back binds %s, want the handed-over %s", got, first)
	}
	if got := changeSummary("live under a live lease", ReadLiveContext); got != revised {
		t.Errorf("--live binds %s, want the newest %s", got, revised)
	}

	// The lease lapses: the attempt is over, but it is still the attempt this
	// row's snapshot describes.
	execSQL(t, conn, `UPDATE steps SET expires_ms = ? WHERE id = ?`, nowMS-1, reviewID)
	if got := changeSummary("read-back under a lapsed lease", ReadContext); got != first {
		t.Errorf("under a lapsed lease the read-back binds %s, want the attempt's %s", got, first)
	}

	// Reaped to pending: nothing is in flight, so the read is what the next
	// claim will hand over.
	reapStep(t, conn, reviewID)
	if got := stepStatus(t, conn, "review@0#0"); got != db.StepPending {
		t.Fatalf("premise: reaped review@0#0 = %q, want %q", got, db.StepPending)
	}
	if got := changeSummary("read-back at pending", ReadContext); got != revised {
		t.Errorf("back at pending the read-back binds %s, want the live %s", got, revised)
	}

	// The re-claim records the new attempt's bindings in place of the old.
	again, err := ClaimStep(conn, reviewID, ClaimOptions{Owner: "judge-2", NowMS: nowMS})
	testsupport.Must(t, err, "re-claim review@0#0: %v", err)
	if got := artifactOfKind(again.Context.Inputs, "change-summary"); got != revised {
		t.Errorf("the re-claim handed over %s, want %s", got, revised)
	}
	if got := changeSummary("read-back after the re-claim", ReadContext); got != revised {
		t.Errorf("after the re-claim the read-back binds %s, want %s", got, revised)
	}
	ids := recordedInputIDs(t, conn, reviewID)
	var want []int
	for _, in := range again.Context.Inputs {
		if id, ok := artifactIDOf(in.Artifact); ok {
			want = append(want, id)
		}
	}
	sort.Ints(want)
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("step_inputs after the re-claim = %v, want exactly the re-claim's bindings %v — "+
			"the first attempt's %s must not linger beside them", ids, want, first)
	}
}

// TestNeverClaimedStepsReadLive pins the other side of the rule: a step the
// engine runs itself is never claimed, records no snapshot, and reads live —
// so the action's own stdin (actionContext) and a read of it agree.
func TestNeverClaimedStepsReadLive(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	driveRoundToReconcile(t, conn, e, 0)

	reconcileID := stepIDByInstance(t, conn, "reconcile@0")
	step, err := db.GetStep(conn, reconcileID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Attempt != 0 || recordedClaim(step) {
		t.Fatalf("premise: reconcile@0 (attempt %d, %s) reads as a recorded claim", step.Attempt, step.Status)
	}
	if ids := recordedInputIDs(t, conn, reconcileID); len(ids) != 0 {
		t.Fatalf("premise: an action step recorded step_inputs %v", ids)
	}

	recorded, err := ReadContext(conn, reconcileID, nowMS)
	testsupport.Must(t, err, "ReadContext(reconcile@0): %v", err)
	live, err := ReadLiveContext(conn, reconcileID, nowMS)
	testsupport.Must(t, err, "ReadLiveContext(reconcile@0): %v", err)
	if !reflect.DeepEqual(recorded.Inputs, live.Inputs) {
		t.Errorf("a never-claimed step reads differently with and without --live:\n%v\n%v",
			inputRefs(recorded.Inputs), inputRefs(live.Inputs))
	}
	if got := producersOfKind(recorded.Inputs, "findings"); !reflect.DeepEqual(got, []string{"synthesize@0"}) {
		t.Errorf("reconcile@0 reads `synthesize.findings` from %v, want [synthesize@0]", got)
	}
}
