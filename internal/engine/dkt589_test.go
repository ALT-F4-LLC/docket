package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-589 extends DKT-340's non-convergence guard to the signal DKT-340 cannot
// see: a round that MOVED BYTES and still changed nothing that matters.
//
// RUN-31 (DKT-294) is the shape. verify@0 reported AC2 and AC5 unmet; fix@1
// committed an 82,742-byte diff; verify@1 reported AC2 and AC5 unmet with AC9
// newly regressed; fix@2 committed 2,571 bytes; verify@2 came back
// BYTE-IDENTICAL to verify@1. Both rounds moved real bytes, so DKT-340's
// byte-based guard correctly stayed silent — and 342,490 output tokens, 45.8%
// of the run, closed zero acceptance criteria. RUN-34 is the same shape with
// four byte-identical ac-reports.
//
// WHAT CORE COMPARES IS BYTES, NOT CRITERIA. The issue describes the symptom as
// "the same criterion ids at the same statuses", but `criterion`, `id` and
// `status` are the workflow author's vocabulary — core no more parses them than
// it parses `severity` (genericity.md, roundMovedNothing's own reasoning). The
// engine reads the kind the routing step's OWN `emits` declares and compares
// the recorded artifact fingerprints. These tests therefore drive the CANONICAL
// fixture, whose `verify` emits `ac-report`, and one interposed fixture whose
// `check` emits `findings`: the same guard fires on both, because neither kind
// is known to it.

// driveRoundToVerify completes one ordinal's chain up to but NOT including its
// `verify`, leaving the stub tree exactly where the caller put it.
//
// It is driveToVerify without the driveFixtureRound move, which is the whole
// point: the tests below need to control the tree and the verdict
// INDEPENDENTLY, because the two guards read one each and the interesting cases
// are the ones where they disagree.
func driveRoundToVerify(t *testing.T, conn *sql.DB, e *Engine, ordinal int) {
	t.Helper()
	if ordinal == 0 {
		claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	} else {
		claimAndComplete(t, conn, e, fmt.Sprintf("fix@%d", ordinal), "the fix summary", "")
	}
	completeReviewFanout(t, conn, e, ordinal)
	claimAndComplete(t, conn, e, fmt.Sprintf("synthesize@%d", ordinal), "the synthesis", "")
	driveAction(t, conn, e, fmt.Sprintf("reconcile@%d", ordinal))
}

// theSameACReport is RUN-31's verify@1 and verify@2: one report, recorded
// twice. A constant is the SUBJECT here, not a fixture shortcut.
const theSameACReport = `AC2 unmet: the retry budget is still unbounded
AC5 unmet: no test covers the exhausted path`

// TestIdenticalVerdictAtTwoOrdinalsParksTheLoop is the verbatim RUN-31 shape.
//
// Both rounds MOVE THE TREE — driveToVerify advances the stub diff per ordinal,
// exactly as a fix round that commits 82,742 bytes does — so DKT-340's own
// guard sees genuine movement and stays silent. The only thing that did not
// change is the verdict, and that is enough.
func TestIdenticalVerdictAtTwoOrdinalsParksTheLoop(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", theSameACReport, unmetPayload)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: the first verdict must enter the loop")
	}
	if got := loopCount(t, conn, run.ID, issue); got != 1 {
		t.Fatalf("premise: loop_count = %d after the first entry, want 1", got)
	}

	// Round 1 does real work — the tree moves — and reaches the IDENTICAL
	// verdict.
	driveToVerify(t, conn, e, 1)
	claimAndComplete(t, conn, e, "verify@1", theSameACReport, unmetPayload)

	if stepExists(t, conn, "fix@2") {
		t.Error("a third round was minted after `verify` recorded the identical " +
			"verdict twice; the next round is handed the same verdict the last " +
			"one already failed to change")
	}
	if got := stepStatus(t, conn, "verify@1"); got != db.StepWaitingHuman {
		t.Errorf("verify@1 = %q, want the repeated-verdict park %q",
			got, db.StepWaitingHuman)
	}

	routing := stepRoutingRaw(t, conn, "verify@1")

	// The park NAMES THE UNCHANGED VERDICT — which step repeated itself, and at
	// which two ordinals — because that is what the operator has to decide
	// about.
	if !strings.Contains(routing, "identical verdict") ||
		!strings.Contains(routing, `"verify"`) {
		t.Errorf("the park does not name the unchanged verdict: %q", routing)
	}
	if !strings.Contains(routing, "ordinals 0 and 1") {
		t.Errorf("the park does not name the two ordinals: %q", routing)
	}

	// AND IT IS THE NEW SIGNAL THAT FIRED, not DKT-340's. roundMovedNothing is
	// evaluated first and its clause would have won; the tree moved, so it
	// correctly said nothing.
	if strings.Contains(routing, "changed nothing") {
		t.Errorf("DKT-340's byte guard claimed this park; the tree moved in both "+
			"rounds and only the verdict repeated: %q", routing)
	}

	// The way out is the EXISTING verb, not a new one.
	if !strings.Contains(routing, "--as fix-round") {
		t.Errorf("the park names no way out: %q", routing)
	}

	// AND THE COUNTER IS PUT BACK, exactly as the bound and DKT-340 refusals do:
	// a refusal minted no ordinal, and a counter left above the highest
	// instantiated one declares that ordinal stale in its entirety (DKT-78).
	if got := loopCount(t, conn, run.ID, issue); got != 1 {
		t.Errorf("loop_count = %d after a refused entry, want 1 — the refusal "+
			"minted no round", got)
	}
}

// TestChangedVerdictAtTwoOrdinalsEntersTheLoop is the lower bound, and the half
// that matters most: a guard that fired on a loop still learning something
// would break every workflow that uses one.
//
// RUN-31's round 1 is exactly this case — AC2 and AC5 still unmet but AC9 newly
// regressed — and it must not park. Only the round after it, whose report was
// byte-identical, is the repeat.
func TestChangedVerdictAtTwoOrdinalsEntersTheLoop(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", theSameACReport, unmetPayload)

	driveToVerify(t, conn, e, 1)
	claimAndComplete(t, conn, e, "verify@1",
		theSameACReport+"\nAC9 regressed: the reap path lost its narration",
		unmetPayload)

	if !stepExists(t, conn, "fix@2") {
		t.Error("a round whose verdict changed did not mint the next one; the " +
			"loop must keep running while it is still learning something")
	}
	if got := stepStatus(t, conn, "verify@1"); got != db.StepDone {
		t.Errorf("verify@1 = %q, want %q — a changed verdict is a real round",
			got, db.StepDone)
	}
	if got := loopCount(t, conn, run.ID, issue); got != 2 {
		t.Errorf("loop_count = %d, want 2", got)
	}
}

// TestFixRoundOverridesTheRepeatedVerdictPark is the escape hatch, and the
// reason parking is the right refusal rather than a hard stop. It is the SAME
// verb that gets past the bound (DKT-237) and past DKT-340's own park — the
// issue is explicit that no new override verb is invented.
func TestFixRoundOverridesTheRepeatedVerdictPark(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", theSameACReport, unmetPayload)
	driveToVerify(t, conn, e, 1)
	claimAndComplete(t, conn, e, "verify@1", theSameACReport, unmetPayload)

	if got := stepStatus(t, conn, "verify@1"); got != db.StepWaitingHuman {
		t.Fatalf("premise: verify@1 = %q, want the park", got)
	}

	testsupport.Must(t, e.ResolveStep(conn, stepIDByInstance(t, conn, "verify@1"),
		ResolveFixRound, "AC2 is unmeetable; one more round to document it", nowMS),
		"resolve --as fix-round: %v", nil)

	if !stepExists(t, conn, "fix@2") {
		t.Error("the operator authorized a round past the repeated-verdict park " +
			"and none was minted")
	}
}

// TestUnmovedTreeStillParksAsNonConvergence is DKT-340's own guard, unchanged.
//
// This addition is a SIBLING signal, not a replacement: the two are OR'd into
// one refusal and roundMovedNothing is still evaluated first, so a genuinely
// unmoved tree still parks with its own reason even when the verdicts differ
// and the new check would have said nothing.
func TestUnmovedTreeStillParksAsNonConvergence(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	// NO driveFixtureRound anywhere: the stub tree never moves, so the engine's
	// own DKT-258 suppression records one diff for the whole run.
	driveRoundToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", roundReport(0), unmetPayload)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: round 0 must have entered the loop")
	}

	driveRoundToVerify(t, conn, e, 1)
	claimAndComplete(t, conn, e, "verify@1", roundReport(1), unmetPayload)

	if stepExists(t, conn, "fix@2") {
		t.Error("a round was minted after one that changed nothing in scope")
	}
	if got := stepStatus(t, conn, "verify@1"); got != db.StepWaitingHuman {
		t.Errorf("verify@1 = %q, want DKT-340's park %q", got, db.StepWaitingHuman)
	}
	routing := stepRoutingRaw(t, conn, "verify@1")
	if !strings.Contains(routing, "changed nothing") ||
		!strings.Contains(routing, "reaches the same verdict") {
		t.Errorf("DKT-340's own park lost its reason: %q", routing)
	}
	if !strings.Contains(routing, "--as fix-round") {
		t.Errorf("DKT-340's park lost its way out: %q", routing)
	}
	if got := loopCount(t, conn, run.ID, issue); got != 1 {
		t.Errorf("loop_count = %d after DKT-340's refusal, want 1", got)
	}
}

// TestFirstVerdictIsNeverRefusedAsRepeated is the ordinal-0 case: a routing step
// that has run ONCE has no previous verdict of its own, and absence of evidence
// is never evidence of a repeat.
func TestFirstVerdictIsNeverRefusedAsRepeated(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", theSameACReport, unmetPayload)

	if !stepExists(t, conn, "fix@1") {
		t.Error("the FIRST round was refused as a repeat; ordinal 0 has no " +
			"previous verdict to equal")
	}
}

// TestEmptyVerdictNeverParksTheLoop is the measurement-must-be-real discipline,
// roundMovedNothing's degenerate-diff rule one guard over.
//
// Two artifacts with no bytes in either channel agree with each other trivially
// — not because the step reached the same conclusion twice, but because it
// recorded no conclusion at all. Parking on that would turn a misconfigured
// executor into a stalled run, a far worse failure than the wasted round this
// exists to prevent.
func TestEmptyVerdictNeverParksTheLoop(t *testing.T) {
	conn := mustDB(t)
	activateInterposed(t, conn, dkt589EmptyVerdictSrc)
	e := testEngine()
	e.Gates = &exitGates{fail: true, exit: 1}

	// `check` records an artifact with NO BYTES IN EITHER CHANNEL and its gate
	// fails, so `on_fail = "fix-loop"` enters the loop off a step that recorded
	// no conclusion at all — at both ordinals.
	claimAndComplete(t, conn, e, "check@0", "", "")
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: the failed gate's on_fail must enter the loop")
	}
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")
	claimAndComplete(t, conn, e, "check@1", "", "")

	if !stepExists(t, conn, "fix@2") {
		t.Error("the loop parked on two empty verdicts agreeing; an artifact " +
			"with no bytes is an absent measurement, not a repeated one, and " +
			"parking on it turns a misconfigured executor into a stalled run")
	}
}

// dkt589EmptyVerdictSrc reaches the one state where a routing step records an
// artifact carrying NO BYTES and still routes `fix-loop`: a failing gate on a
// step whose single attempt is spent, with `on_fail = "fix-loop"`. The artifact
// lands at stage 1, before the gate runs, so the row exists and is empty.
const dkt589EmptyVerdictSrc = `
[pipeline]
name = "dkt589-empty"
version = 1

[match]
kind = ["task"]

[[step]]
name = "check"
executor = "check"
emits = "findings"
gates = ["build"]
max_attempts = 1
on_fail = "fix-loop"

[[step]]
name = "fix"
executor = "fix"
emits = "findings"
loop = true
after_loop = "check"
`

// dkt589OnFailSrc gives ONE issue two different loop-entry triggers: `check`'s
// threshold, and `gate`'s on_fail — the rejected human gate. `check` also
// carries `max_attempts = 1`, so its exhausted-budget entry is reachable too.
//
// The three misfire tests below all need this: the new signal must fire only
// when a routing step's OWN emitted verdict repeated, and must be invisible to
// every other way a loop is entered.
const dkt589OnFailSrc = `
[pipeline]
name = "dkt589-triggers"
version = 1

[match]
kind = ["task"]

[[step]]
name = "check"
executor = "check"
emits = "findings"
max_attempts = 1
on_fail = "fix-loop"
threshold = { "fix-loop" = "any(status == unmet)" }

[[step]]
name = "gate"
after = ["check"]
type = "human"
on_fail = "fix-loop"

[[step]]
name = "fix"
executor = "fix"
emits = "findings"
loop = true
after_loop = "check"
`

// TestRejectedGateEntryIsUnaffected is the first misfire bound. A `type =
// "human"` gate records no artifact at all — workflow.ArtifactKind is "" for
// its class — so a gate rejected identically at two consecutive ordinals has
// nothing for this check to compare and enters the loop exactly as it always
// did, whatever the loop BODY's artifacts look like.
func TestRejectedGateEntryIsUnaffected(t *testing.T) {
	conn := mustDB(t)
	_, _ = activateInterposed(t, conn, dkt589OnFailSrc)
	e := testEngine()

	// `check` passes at both ordinals, with the IDENTICAL findings — and it is
	// not the trigger, so its repetition is not this guard's business either.
	claimAndComplete(t, conn, e, "check@0", "no findings", metPayload)
	err := e.DecideStep(conn, stepIDByInstance(t, conn, "gate@0"), false, "no", nowMS)
	testsupport.Must(t, err, "rejecting gate@0: %v", err)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: the first rejection must enter the loop")
	}

	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")
	claimAndComplete(t, conn, e, "check@1", "no findings", metPayload)
	err = e.DecideStep(conn, stepIDByInstance(t, conn, "gate@1"), false, "still no", nowMS)
	testsupport.Must(t, err, "rejecting gate@1: %v", err)

	if !stepExists(t, conn, "fix@2") {
		t.Error("a rejected human gate's second entry was refused as a repeated " +
			"verdict; a gate records no artifact, so this check has nothing to " +
			"compare and must stay out of its way")
	}
}

// TestExhaustedAttemptEntryIsUnaffected is the second misfire bound, and it is
// what fixes the CURRENT side of the comparison at the step's exact ordinal
// rather than "at or below" it.
//
// `check@1` exhausts its single attempt and routes `fix-loop` from on_fail
// having recorded NO artifact of its own. Read "at or below ordinal 1", both
// sides of the comparison would resolve to check@0's row — the guard would
// compare one artifact with itself, find it equal, and park a run for a reason
// that never happened.
func TestExhaustedAttemptEntryIsUnaffected(t *testing.T) {
	conn := mustDB(t)
	activateInterposed(t, conn, dkt589OnFailSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "check@0", "the findings", unmetPayload)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: the threshold entry must mint fix@1")
	}

	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")

	// check@1 never completes: its one attempt fails, so `on_fail = "fix-loop"`
	// enters the loop with nothing recorded at ordinal 1.
	checkID := stepIDByInstance(t, conn, "check@1")
	claim, err := ClaimStep(conn, checkID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claiming check@1: %v", err)
	testsupport.Must(t, e.FailStep(conn, checkID, claim.Token, "the tool crashed", "", nowMS),
		"failing check@1: %v", nil)

	if !stepExists(t, conn, "fix@2") {
		t.Error("an exhausted attempt budget was refused as a repeated verdict; " +
			"the step recorded nothing at this ordinal, so there is no verdict " +
			"to have repeated")
	}
}

// dkt589SiblingSrc puts a SECOND producer of the same artifact kind beside the
// routing step, outside its `after_loop` chain so it survives a loop entry and
// can record after one.
const dkt589SiblingSrc = `
[pipeline]
name = "dkt589-sibling"
version = 1

[match]
kind = ["task"]

[[step]]
name = "check"
executor = "check"
emits = "findings"
threshold = { "fix-loop" = "any(status == unmet)" }

[[step]]
name = "note"
after = []
executor = "note"
emits = "findings"

[[step]]
name = "fix"
executor = "fix"
emits = "findings"
loop = true
after_loop = "check"
`

// TestAnotherProducersArtifactIsNotThisStepsVerdict pins the query's SCOPE:
// (run, issue, STEP NAME, kind), priorRoundHandBack's shape rather than
// latestIssueDiffHead's issue-wide one.
//
// `note` emits `findings` too, and records the newest one at ordinal 0 — so a
// comparison scoped by kind alone would ask whether `check@1`'s verdict equals
// NOTE's artifact, conclude it does not, and let a genuinely repeated verdict
// through. The issue-wide read is right for `issue.diff`, which is one
// cumulative fact about the tree whoever produced it, and wrong for a question
// about whether ONE step reached the same conclusion twice.
func TestAnotherProducersArtifactIsNotThisStepsVerdict(t *testing.T) {
	conn := mustDB(t)
	activateInterposed(t, conn, dkt589SiblingSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "check@0", "the same findings", unmetPayload)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: check@0's threshold must enter the loop")
	}

	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")

	// `note` records LAST at ordinal 0, so it — not check@0 — owns the issue's
	// newest `findings` below ordinal 1.
	claimAndComplete(t, conn, e, "note@0", "an unrelated note", "")

	claimAndComplete(t, conn, e, "check@1", "the same findings", unmetPayload)

	if stepExists(t, conn, "fix@2") {
		t.Error("a repeated verdict got through because another producer's " +
			"artifact of the same kind was newer; the comparison must be scoped " +
			"to the ROUTING STEP's own emitted stream")
	}
	if got := stepStatus(t, conn, "check@1"); got != db.StepWaitingHuman {
		t.Errorf("check@1 = %q, want the repeated-verdict park %q",
			got, db.StepWaitingHuman)
	}
}
