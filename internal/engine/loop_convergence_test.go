package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
)

// DKT-340: a fix-loop could be re-triggered indefinitely by a condition no
// round can change. RUN-34 paid a full review fanout and a verify pass per
// round on an acceptance criterion three judge panels and two votes had
// already read as correctly unmet, and needed an ad-hoc budget raise to
// survive the churn.
//
// Core cannot know that a criterion is unmeetable — that is instance
// vocabulary. It CAN see that a round moved no bytes the issue is scoped to,
// which means the next round reads the same tree and reaches the same verdict.

// treeState is a mutable stand-in for the working tree: the test moves it when
// a round makes a change and leaves it alone when a round makes none.
//
// THE DIFF IS DRIVEN THROUGH DiffFn, not by inserting artifacts, and that is
// the only honest way to model this. The engine suppresses an `issue.diff`
// write whose bytes match the last one recorded (DKT-258) — which is exactly
// how "this round changed nothing" reaches the ledger — and hand-inserting a
// row defeats that suppression, so every later step in the round starts
// recording its own live-tree diff and the artifact chain stops resembling any
// real run's.
type treeState struct{ body string }

// convergenceEngine is testEngine with the tree under the test's control.
func convergenceEngine(tree *treeState) *Engine {
	e := testEngine()
	e.DiffFn = func(_, _ string, _ []string) (string, error) {
		return tree.body, nil
	}
	return e
}

// driveRound completes one loop ordinal against whatever the tree currently
// says, up to and including its `reconcile`.
func driveRound(t *testing.T, conn *sql.DB, e *Engine, ordinal int) {
	t.Helper()
	if ordinal == 0 {
		claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	} else {
		claimAndComplete(t, conn, e, fmt.Sprintf("fix@%d", ordinal), "the fix summary", "")
	}
	completeReviewFanout(t, conn, e, ordinal)
	claimAndComplete(t, conn, e, fmt.Sprintf("synthesize@%d", ordinal),
		"the synthesis", roundPayload(ordinal))
	driveAction(t, conn, e, fmt.Sprintf("reconcile@%d", ordinal))
}

// TestLoopParksWhenARoundMovedNothing is the guard firing.
func TestLoopParksWhenARoundMovedNothing(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	tree := &treeState{}
	e := convergenceEngine(tree)

	// Round 0 does real work: the tree moves.
	tree.body = "the original change"
	driveRound(t, conn, e, 0)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: round 0 must have entered the loop")
	}

	// Round 1's fix changes NOTHING in scope. The tree does not move, so the
	// engine's own suppression records no new diff — which IS the signal.
	driveRound(t, conn, e, 1)

	// Entering round 2 would read the identical tree.
	if stepExists(t, conn, "fix@2") {
		t.Error("a second round was minted after one that changed nothing in " +
			"scope; the next round reads the same tree and reaches the same " +
			"verdict, so it can only repeat")
	}
	if got := stepStatus(t, conn, "reconcile@1"); got != db.StepWaitingHuman {
		t.Errorf("reconcile@1 = %q, want the non-convergence park %q",
			got, db.StepWaitingHuman)
	}

	// The park NAMES THE WAY OUT, like every other refusal in this file.
	routing := stepRoutingRaw(t, conn, "reconcile@1")
	if !strings.Contains(routing, "changed nothing") {
		t.Errorf("the park does not say why: %q", routing)
	}
	if !strings.Contains(routing, "fix-round") {
		t.Errorf("the park names no next move: %q", routing)
	}

	// AND THE COUNTER IS PUT BACK, exactly as the bound refusal does: a
	// refusal created no ordinal, and a counter left above the highest
	// instantiated one declares that ordinal stale in its entirety (DKT-78).
	if got := loopCount(t, conn, run.ID, issue); got != 1 {
		t.Errorf("loop_count = %d after a refused entry, want 1 — the refusal "+
			"minted no round", got)
	}
}

// TestLoopEntersWhenARoundMovedSomething is the lower bound, and it is the half
// that matters most: a guard that fired on a converging loop would break every
// workflow that uses one.
func TestLoopEntersWhenARoundMovedSomething(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	tree := &treeState{}
	e := convergenceEngine(tree)

	tree.body = "the original change"
	driveRound(t, conn, e, 0)
	tree.body = "the original change, plus the fix"
	driveRound(t, conn, e, 1)

	if !stepExists(t, conn, "fix@2") {
		t.Error("a round that changed the scoped tree did not mint the next " +
			"one; the loop must keep running while it is converging")
	}
}

// TestLoopEntersWhenTheDiffIsUnknown pins the guard's direction under
// uncertainty. Every ambiguous case must permit the round: a wrongly-entered
// round costs one round, a wrongly-refused one costs an operator a park they
// have to reason about and undo.
func TestLoopEntersWhenTheDiffIsUnknown(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	// NO DIFF FUNCTION: the run records no `issue.diff` at all, so the guard
	// has nothing to compare and must not act on the absence.
	e := testEngine()
	e.DiffFn = nil

	driveRound(t, conn, e, 0)
	driveRound(t, conn, e, 1)

	if !stepExists(t, conn, "fix@2") {
		t.Error("the loop refused a round with no diff evidence either way; " +
			"the guard must not act on absence of evidence")
	}
}

// TestFirstRoundIsNeverRefusedAsRepeating is the count < 2 case. Ordinal 0 is
// the original work, not a repeat of it, so there is nothing for round 1 to be
// compared against.
func TestFirstRoundIsNeverRefusedAsRepeating(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	tree := &treeState{}
	e := convergenceEngine(tree)

	tree.body = "the original change"
	driveRound(t, conn, e, 0)

	if !stepExists(t, conn, "fix@1") {
		t.Error("the FIRST fix round was refused as a repeat; ordinal 0 is the " +
			"work, not a round of it")
	}
}

// TestFixRoundOverridesTheNonConvergencePark is the escape hatch, and the
// reason parking is the right refusal rather than a hard stop: the operator
// keeps the decision, exactly as they do at the bound (DKT-237).
func TestFixRoundOverridesTheNonConvergencePark(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	tree := &treeState{}
	e := convergenceEngine(tree)

	tree.body = "the original change"
	driveRound(t, conn, e, 0)
	driveRound(t, conn, e, 1)

	if got := stepStatus(t, conn, "reconcile@1"); got != db.StepWaitingHuman {
		t.Fatalf("premise: reconcile@1 = %q, want the park", got)
	}

	id := stepIDByInstance(t, conn, "reconcile@1")
	if err := e.ResolveStep(conn, id, ResolveFixRound,
		"the criterion is unmeetable; one more round to document it", nowMS); err != nil {
		t.Fatalf("resolve --as fix-round: %v", err)
	}

	if !stepExists(t, conn, "fix@2") {
		t.Error("the operator authorized a round and none was minted")
	}
}

// TestDegenerateDiffNeverParksTheLoop is the guard's most important refusal to
// fire, and the reason it reads the diff BODY and not only its hash.
//
// The saga writes a `# issue.diff: could not resolve the run's pinned base
// commit ...` marker when it cannot compute a real diff, and 36 of RUN-5's 71
// issue.diff artifacts hashed empty for related reasons. Two such measurements
// agree with each other trivially — not because the tree stood still, but
// because it was never measured. Parking a loop on that would turn a broken
// diff setup into a stalled run, which is far worse than the wasted round this
// guard exists to prevent.
func TestDegenerateDiffNeverParksTheLoop(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	tree := &treeState{}
	e := convergenceEngine(tree)

	// Every round measures nothing, identically — the unresolvable-base shape.
	tree.body = "# issue.diff: could not resolve the run's pinned base commit\n"
	driveRound(t, conn, e, 0)
	driveRound(t, conn, e, 1)

	if !stepExists(t, conn, "fix@2") {
		t.Error("the loop parked on two degenerate measurements agreeing; " +
			"a diff that records no content is evidence the tree was not " +
			"measured, not evidence that it did not move")
	}
}
