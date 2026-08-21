package engine

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-259 — the operator-retry path's three costs, and the verb that avoids
// paying them for the common case.
//
// Most retries in the RUN-13 epoch existed only to re-run a gate after a trust
// entry was added or an environment was fixed. The step's own output was never
// in question, and `retry` — the only lever — re-executed everything.

// scriptedGates is a GateRunner whose verdict is decided per RUN, so a test can
// fail a gate, fix the world, and re-run it passing. It counts spawns per gate
// so a case can prove which gates ran a second time.
type scriptedGates struct {
	mu    sync.Mutex
	fail  bool
	spawn map[string]int
}

func (g *scriptedGates) Run(_ context.Context, spec GateSpec, _ StepContext) (GateResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.spawn == nil {
		g.spawn = map[string]int{}
	}
	g.spawn[spec.Name]++
	verdict := VerdictPass
	exit := 0
	if g.fail {
		verdict, exit = VerdictFail, 1
	}
	return GateResult{Gate: spec.Name, Exit: exit, Verdict: verdict}, nil
}

func (g *scriptedGates) spawns(gate string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.spawn[gate]
}

// parkedByFailingGate drives `implement@0` to `waiting-human` through a gate
// that failed, which is the state every one of these retries started from.
func parkedByFailingGate(t *testing.T, conn *sql.DB, e *Engine) int {
	t.Helper()
	id := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w1", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)
	return id
}

// TestRerunGatesRemeasuresWithoutReExecuting is the verb's whole contract.
//
// A gate failed on the environment rather than on the work. The operator fixes
// the environment out of band and asks for the gates to run again. What must
// happen: every gate re-runs, the step routes on the new verdicts, and the
// step's own execution is NOT repeated — no new attempt, no new claim, and the
// artifact it recorded the first time is still the artifact.
func TestRerunGatesRemeasuresWithoutReExecuting(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	gates := &scriptedGates{fail: true}
	e := testEngine()
	e.Gates = gates

	id := parkedByFailingGate(t, conn, e)
	if got := stepStatus(t, conn, "implement@0"); got != db.StepWaitingHuman {
		t.Fatalf("status = %q after a failing gate, want %q", got, db.StepWaitingHuman)
	}

	before, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	artifactsBefore := artifactCount(t, conn, id)
	firstSpawns := gates.spawns("build")

	// The environment is fixed; the work was never in question.
	gates.fail = false
	err = e.ResolveStep(conn, id, ResolveRerunGates, "trust entry added", nowMS+1)
	testsupport.Must(t, err, "resolve --as rerun-gates: %v", err)

	after, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep after: %v", err)

	// THE GATES RAN AGAIN.
	if got := gates.spawns("build"); got <= firstSpawns {
		t.Errorf("the `build` gate spawned %d times, was %d before the rerun — "+
			"the whole verb is that the gates measure again", got, firstSpawns)
	}
	// THE STEP DID NOT RE-EXECUTE. `attempt` increments only at claim, so an
	// unchanged attempt is proof no claim happened, and an unchanged artifact
	// count is proof no second execution recorded.
	if after.Attempt != before.Attempt {
		t.Errorf("attempt moved %d -> %d; rerun-gates must not re-claim the "+
			"step, or it is `retry` under another name",
			before.Attempt, after.Attempt)
	}
	if got := artifactCount(t, conn, id); got != artifactsBefore {
		t.Errorf("the step recorded %d artifacts, was %d — rerun-gates must "+
			"leave the recorded work exactly as it found it",
			got, artifactsBefore)
	}
	// And it converged: the passing gates routed the step out of the park.
	if got := stepStatus(t, conn, "implement@0"); got == db.StepWaitingHuman {
		t.Errorf("the step is still %q after its gates passed on the re-run; "+
			"the rewind must reach routing, not stop at the gates", got)
	}
}

// TestRerunGatesStillFailsWhenTheGatesStillFail: the verb re-measures, it does
// not forgive.
//
// If a rerun could only ever move a step forward it would be `override-pass`
// with extra steps. An operator who reruns without fixing anything must land
// exactly where they started, having learned that the problem is real.
func TestRerunGatesStillFailsWhenTheGatesStillFail(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	gates := &scriptedGates{fail: true}
	e := testEngine()
	e.Gates = gates

	id := parkedByFailingGate(t, conn, e)
	err := e.ResolveStep(conn, id, ResolveRerunGates, "nothing was fixed", nowMS+1)
	testsupport.Must(t, err, "resolve --as rerun-gates: %v", err)

	if got := stepStatus(t, conn, "implement@0"); got != db.StepWaitingHuman {
		t.Errorf("status = %q after re-running gates that still fail, want %q",
			got, db.StepWaitingHuman)
	}
}

// TestRerunGatesRefusesAStepWithNoCompletionGates names the alternative.
//
// Rewinding a gateless step to `recorded` would walk straight to routing and
// decide on the same evidence — an expensive no-op that looks like it did
// something. An operator reaching for this verb has a real problem, so the
// refusal has to point at the verb that solves it.
//
// The fixture's `verify` is the exact case worth guarding: it declares a gate,
// but a `pre = true` one, which runs at CLAIM and is not part of the completion
// saga at all. A check written against "declares gates" rather than "declares
// COMPLETION gates" would accept it and rewind to a stage with nothing to run.
func TestRerunGatesRefusesAStepWithNoCompletionGates(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	id := advanceToVerify(t, conn, e)
	claim, err := e.ClaimStepWithGates(conn, id, ClaimOptions{Owner: "v", NowMS: nowMS})
	testsupport.Must(t, err, "claim verify: %v", err)
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token: claim.Token, Artifact: []byte("  \n"),
		Gaps:  [][]byte{[]byte("# Cannot verify here\n\nResidue.")},
		NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete verify: %v", err)
	if got := stepStatus(t, conn, "verify@0"); got != db.StepWaitingHuman {
		t.Fatalf("verify parked as %q, want %q", got, db.StepWaitingHuman)
	}

	err = e.ResolveStep(conn, id, ResolveRerunGates, "", nowMS+1)
	if err == nil {
		t.Fatal("rerun-gates succeeded on a step with no COMPLETION gates; a " +
			"pre-gate runs at claim and is not part of the saga, so the rewind " +
			"would reach routing and decide on the same evidence")
	}
	if !strings.Contains(err.Error(), ResolveRetry) {
		t.Errorf("the refusal does not name the verb that would work: %v", err)
	}
}

// TestRetryLeavesNoLiveLease pins the invariant DKT-259's accounting half rests
// on: a step returned to `pending` is a step NOBODY HOLDS.
//
// `claimPredicate` claims only against `owner IS NULL OR owner = ” OR
// expires_ms <= now`, so `pending` with a live lease is a step the scheduler
// offers and no claimant can take — while the original holder's token still
// works, letting a re-execution record without re-claiming and land on the
// first execution's attempt number.
//
// Every path to `waiting-human` retires the token today, so this is a guard
// rather than a repro. It is worth having as a guard precisely because the
// contradiction is invisible: nothing fails loudly when a lease survives, the
// accounting just quietly stops adding up.
func TestRetryLeavesNoLiveLease(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	gates := &scriptedGates{fail: true}
	e := testEngine()
	e.Gates = gates

	id := parkedByFailingGate(t, conn, e)

	// Force the contradiction the guard exists for, whatever the paths above
	// happen to do today: a parked step wearing a live lease.
	_, err := conn.Exec(
		`UPDATE steps SET owner = 'ghost', token_hash = 'h', expires_ms = ?
		  WHERE id = ?`, nowMS+3_600_000, id)
	testsupport.Must(t, err, "planting a live lease: %v", err)

	err = e.ResolveStep(conn, id, ResolveRetry, "re-run it", nowMS+1)
	testsupport.Must(t, err, "resolve --as retry: %v", err)

	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepPending {
		t.Fatalf("status = %q after retry, want %q", step.Status, db.StepPending)
	}
	if step.Owner != "" {
		t.Errorf("the step is %q but still owned by %q; claimPredicate will "+
			"refuse every new claimant while the old holder's token still "+
			"records — which is how two executions come to share one attempt",
			step.Status, step.Owner)
	}
	if step.ExpiresMS != 0 {
		t.Errorf("the step is %q with an expiry of %d still set",
			step.Status, step.ExpiresMS)
	}

	// And the step is genuinely claimable again, which is the property the
	// released lease exists to produce.
	if _, err := ClaimStep(conn, id, ClaimOptions{Owner: "w2", NowMS: nowMS + 2}); err != nil {
		t.Errorf("the retried step could not be re-claimed: %v", err)
	}
}

// artifactCount is how many artifacts a step has recorded.
func artifactCount(t *testing.T, conn *sql.DB, stepID int) int {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM artifacts WHERE step_id = ?`, stepID).Scan(&n)
	testsupport.Must(t, err, "counting artifacts: %v", err)
	return n
}

// TestEmptyDiffDoesNotSupersedeARecordedChange is DKT-259's destructive half.
//
// A re-execution diffs a tree that already contains the work against a base
// that already contains it too, so the diff comes back empty. Recording that
// replaced the issue's real `issue.diff` with 0 bytes — the sha of empty input.
// The evidence of the change was replaced by a recording of nothing, and every
// downstream reader that resolves "the latest diff" then reviewed an empty
// object.
func TestEmptyDiffDoesNotSupersedeARecordedChange(t *testing.T) {
	cases := []struct {
		name string
		// latest is what the issue already recorded.
		latest string
		// recomputed is what this pass measured.
		recomputed string
		wantDrop   bool
		why        string
	}{
		{
			name:       "an empty diff does not replace a real one",
			latest:     "diff --git a/x b/x\n+the change\n",
			recomputed: "",
			wantDrop:   true,
			why:        "this is RUN-13 STEP-132 exactly",
		},
		{
			name:       "a COMMENT-ONLY diff does not replace a real one",
			latest:     "diff --git a/x b/x\n+the change\n",
			recomputed: "# issue.diff: could not resolve the run's pinned base commit\n",
			wantDrop:   true,
			why: "this file writes `#` annotations into a diff body, so a body " +
				"that measured nothing is far from zero bytes; len(body)==0 " +
				"would let exactly the annotated cases through",
		},
		{
			name:       "a FIRST empty diff still records",
			latest:     "",
			recomputed: "",
			wantDrop:   false,
			why: "a genuine `nothing changed` is what happened, and suppressing " +
				"it would hide a step that produced no work",
		},
		{
			name:       "an empty diff does not replace an earlier EMPTY one either way",
			latest:     "# issue.diff: 2 changed file(s) fall outside this issue's scope\n",
			recomputed: "",
			wantDrop:   false,
			why:        "no recorded change exists to protect",
		},
		{
			name:       "a real diff replaces a real diff",
			latest:     "diff --git a/x b/x\n+first\n",
			recomputed: "diff --git a/x b/x\n+second\n",
			wantDrop:   false,
			why:        "this is an ordinary revision and must record",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			run, issue := activatedRun(t, conn)
			id := stepIDByInstance(t, conn, "implement@0")

			if tc.latest != "" {
				tx, err := conn.Begin()
				testsupport.Must(t, err, "Begin: %v", err)
				_, err = db.InsertArtifactTx(tx, db.Artifact{
					RunID: run.ID, StepID: id, Kind: ArtifactKindIssueDiff,
					Body: tc.latest,
				}, nowMS)
				testsupport.Must(t, err, "InsertArtifactTx: %v", err)
				testsupport.Must(t, tx.Commit(), "Commit: %v", err)
			}

			dropped := diffRecordsNoChange(tc.recomputed) &&
				issueHasRecordedChange(conn, run.ID, issue)
			if dropped != tc.wantDrop {
				t.Errorf("dropped = %v, want %v. %s", dropped, tc.wantDrop, tc.why)
			}
		})
	}
}

// TestDiffRecordsNoChangeStripsAnnotations is the predicate on its own.
//
// The comment-stripping is the load-bearing part: `len(body) == 0` would treat
// an annotated empty diff as content, and those are exactly the bodies most
// likely to be empty for a bad reason — an unresolvable base is what produces
// them.
func TestDiffRecordsNoChangeStripsAnnotations(t *testing.T) {
	for body, want := range map[string]bool{
		"":         true,
		"\n\n  \n": true,
		"# issue.diff: could not resolve the base\n":           true,
		"# one\n#two\n\n   # three\n":                          true,
		"diff --git a/x b/x\n":                                 false,
		"# issue.diff: annotated\ndiff --git a/x b/x\n+line\n": false,
	} {
		if got := diffRecordsNoChange(body); got != want {
			t.Errorf("diffRecordsNoChange(%q) = %v, want %v", body, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// DKT-261 — the rework packet carries what a ladder would classify on
// ---------------------------------------------------------------------------

// TestReworkPacketCarriesTheFailingGates is DKT-261's deliverable.
//
// `escalate_to` lives in policy.toml and is read by the RELAY, never by the
// engine — grepping non-test engine source for escalation finds only a vote
// proposal's human-authored EscalationReason. So core cannot wire a ladder, and
// the issue's own evidence says a naive one would be worse than none: all three
// genuine capability-suspect retries of the epoch were ENVIRONMENTAL, so "gate
// failed twice, escalate" would have escalated three times and helped zero.
//
// What core can do is supply the classification input. After DKT-254 the
// verdicts already separate the cases — `skipped` means nothing was measured
// and parks the step, `unmatched` means the command was never trusted here,
// `fail` alone means a measurement was taken and the work did not pass. This
// puts those verdicts, with their reasons, where a relay composing a retry can
// read them.
func TestReworkPacketCarriesTheFailingGates(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	gates := &scriptedGates{fail: true}
	e := testEngine()
	e.Gates = gates

	id := parkedByFailingGate(t, conn, e)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()

	step, err := db.GetStepTx(tx, id)
	testsupport.Must(t, err, "GetStepTx: %v", err)

	res := resolutionOf(step)
	if res == nil {
		t.Fatal("the parked step carries no resolution to attach gates to")
	}
	testsupport.Must(t, attachGateOutcomes(tx, step, res), "attachGateOutcomes: %v", err)

	if len(res.Gates) == 0 {
		t.Fatal("the resolution names no failing gates; a relay classifying an " +
			"environmental failure against a capability one has nothing to read")
	}
	for _, g := range res.Gates {
		if g.Verdict == db.GateVerdictPass {
			t.Errorf("%s is listed as passing; a passing gate is not why the "+
				"step came back", g.Gate)
		}
		if g.Gate == "" || g.Verdict == "" {
			t.Errorf("an outcome is missing its gate or verdict: %+v", g)
		}
	}
}

// TestReworkPacketOmitsPassingAndPreGates.
//
// PG4: a pre-gate is an INPUT to the step, not a judgment of it, so it must not
// appear in the list of reasons the step came back. And a gate that passed is
// not a reason at all — listing it would invite a classifier to weigh it.
func TestReworkPacketOmitsPassingAndPreGates(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	id := stepIDByInstance(t, conn, "implement@0")

	exitZero, exitOne := 0, 1
	rows := []db.GateResultRow{
		{RunID: run.ID, StepID: id, Gate: "build", Ordinal: 0, Exit: &exitZero,
			Verdict: db.GateVerdictPass, CreatedAtMS: nowMS},
		{RunID: run.ID, StepID: id, Gate: "tests", Ordinal: 0, Exit: &exitOne,
			Verdict: db.GateVerdictFail, Reason: "3 assertions failed", CreatedAtMS: nowMS},
		{RunID: run.ID, StepID: id, Gate: "ac-commands", Ordinal: 0,
			Verdict: db.GateVerdictSkipped, Pre: true,
			Reason: "the tree under review could not be bound", CreatedAtMS: nowMS},
	}
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	for _, r := range rows {
		testsupport.Must(t, db.InsertGateResultTx(tx, r), "InsertGateResultTx: %v", err)
	}

	res := &ContextResolution{Routing: "fix-loop"}
	step, err := db.GetStepTx(tx, id)
	testsupport.Must(t, err, "GetStepTx: %v", err)
	testsupport.Must(t, attachGateOutcomes(tx, step, res), "attachGateOutcomes: %v", err)
	testsupport.Must(t, tx.Rollback(), "Rollback: %v", err)

	if len(res.Gates) != 1 {
		t.Fatalf("gates = %+v, want exactly the failing completion gate", res.Gates)
	}
	got := res.Gates[0]
	if got.Gate != "tests" || got.Verdict != db.GateVerdictFail {
		t.Errorf("gates[0] = %+v, want the failing `tests` gate", got)
	}
	// The reason is what separates a real failure from a broken environment,
	// and it is the field a classifier reads after the verdict.
	if got.Reason != "3 assertions failed" {
		t.Errorf("reason = %q; the diagnosis must ride along verbatim", got.Reason)
	}
}

// TestReworkPacketTakesTheLastAttemptPerGate is F4's rule, applied here.
//
// A gate that failed twice and passed on the third try DID NOT FAIL. A packet
// listing all three attempts would invite a classifier to conclude the opposite
// — which for an escalation ladder means burning a more expensive variant on a
// gate that already went green.
func TestReworkPacketTakesTheLastAttemptPerGate(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	id := stepIDByInstance(t, conn, "implement@0")

	exitZero, exitOne := 0, 1
	rows := []db.GateResultRow{
		{RunID: run.ID, StepID: id, Gate: "flaky", Ordinal: 0, Exit: &exitOne,
			Verdict: db.GateVerdictFail, CreatedAtMS: nowMS},
		{RunID: run.ID, StepID: id, Gate: "flaky", Ordinal: 1, Exit: &exitOne,
			Verdict: db.GateVerdictFail, CreatedAtMS: nowMS},
		{RunID: run.ID, StepID: id, Gate: "flaky", Ordinal: 2, Exit: &exitZero,
			Verdict: db.GateVerdictPass, CreatedAtMS: nowMS},
	}
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	for _, r := range rows {
		testsupport.Must(t, db.InsertGateResultTx(tx, r), "InsertGateResultTx: %v", err)
	}

	res := &ContextResolution{Routing: "fix-loop"}
	step, err := db.GetStepTx(tx, id)
	testsupport.Must(t, err, "GetStepTx: %v", err)
	testsupport.Must(t, attachGateOutcomes(tx, step, res), "attachGateOutcomes: %v", err)
	testsupport.Must(t, tx.Rollback(), "Rollback: %v", err)

	if len(res.Gates) != 0 {
		t.Errorf("gates = %+v; the last attempt PASSED, so this gate is not a "+
			"reason the step came back (F4)", res.Gates)
	}
}
