package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-588: a fix round whose hand-back sha equals the prior round's re-ran the
// whole fanout. RUN-34 round 2: fix@2 recorded "Empty work list this round ...
// hand-back sha is unchanged HEAD 64d3336b3d71", judge-testing confirmed
// identical trees, 4 of 5 judges emitted zero findings — and a full 5-judge +
// synthesize + verify round (~2.9 budget units) still ran over a zero-byte
// delta, because DKT-340's non-convergence guard fires only at the NEXT loop's
// entry, after the wasted round has already been paid for.
//
// The fix parks the round AT ITS SOURCE: when a loop body at ordinal k > 0
// hands back the same commit the same body recorded at an earlier ordinal, the
// body's own completion routes `waiting-human` naming the unchanged sha, and
// the downstream chain — already withheld from every offer while its
// same-ordinal loop body is non-terminal (DKT-48/DKT-61) — never runs.
//
// These tests drive the diff AND the head through the engine's own seams
// (DiffFn/HeadFn), for loop_convergence_test.go's stated reason: hand-inserted
// artifacts defeat the suppression rules the real ledger obeys.

// handBackEngine is convergenceEngine with the hand-back head also under the
// test's control: HeadFn answers whatever the test last assigned, modelling a
// checkout whose HEAD moves only when a round actually commits something.
func handBackEngine(tree *treeState, head *string) *Engine {
	e := convergenceEngine(tree)
	e.HeadFn = func(string) string { return *head }
	return e
}

// enterSecondRound drives rounds 0 and 1 with real movement — the tree and the
// head both advance — so round 2 is genuinely entered and fix@2 exists. It
// returns the head fix@1 recorded, which is the sha the guard compares
// against.
func enterSecondRound(t *testing.T, conn *sql.DB, e *Engine, tree *treeState, head *string) string {
	t.Helper()
	tree.body, *head = "the original change", "aaaa1111bbbb2222cccc3333dddd4444"
	driveRound(t, conn, e, 0)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: round 0 must have entered the loop")
	}
	tree.body, *head = "the original change, plus fix 1", "eeee5555ffff6666aaaa7777bbbb8888"
	driveRound(t, conn, e, 1)
	if !stepExists(t, conn, "fix@2") {
		t.Fatal("premise: round 1 must have entered round 2")
	}
	return *head
}

// TestUnchangedHandBackParksTheRoundBeforeTheFanout is the verbatim RUN-34
// shape: fix@2 completes handing back the same commit fix@1 recorded, and the
// round short-circuits to `waiting-human` before a single judge is offered.
func TestUnchangedHandBackParksTheRoundBeforeTheFanout(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	tree := &treeState{}
	head := ""
	e := handBackEngine(tree, &head)

	prior := enterSecondRound(t, conn, e, tree, &head)

	// fix@2 does nothing: the tree does not move and HEAD stays where fix@1
	// left it.
	claimAndComplete(t, conn, e, "fix@2", "no work this round", "")

	if got := stepStatus(t, conn, "fix@2"); got != db.StepWaitingHuman {
		t.Errorf("fix@2 = %q, want the unchanged-hand-back park %q",
			got, db.StepWaitingHuman)
	}

	// The park NAMES THE UNCHANGED SHA and the way out, like every other
	// refusal beside it.
	routing := stepRoutingRaw(t, conn, "fix@2")
	if !strings.Contains(routing, prior[:12]) {
		t.Errorf("the park does not name the unchanged sha: %q", routing)
	}
	if !strings.Contains(routing, "override-pass") {
		t.Errorf("the park names no way out: %q", routing)
	}

	// THE FANOUT NEVER RUNS. The run itself parks with the step, so nothing at
	// all is offered — this is the round DKT-340 could only refuse after it
	// had already been paid for.
	if offered := readyInstances(t, conn); len(offered) != 0 {
		t.Errorf("a parked round still offers work: %v", offered)
	}
	var runStatus string
	testsupport.Must(t,
		conn.QueryRow(`SELECT status FROM runs WHERE id = ?`, run.ID).Scan(&runStatus),
		"reading run status: %v", nil)
	if model.RunStatus(runStatus) != model.RunWaitingHuman {
		t.Errorf("run = %q, want %q", runStatus, model.RunWaitingHuman)
	}

	// The chain is PENDING, not superseded: `superseded` is terminal, and a
	// swept chain would let the issue complete without its judges the moment
	// the park resolves. The park holds it; the resolution releases it.
	if got := stepStatus(t, conn, "review@2#0"); got != db.StepPending {
		t.Errorf("review@2#0 = %q, want %q — the chain waits, it is not swept",
			got, db.StepPending)
	}

	// The counter is untouched: unlike a refused ENTRY, this round exists —
	// its body ran and parked — so ordinal 2 is genuinely the issue's current
	// ordinal.
	if got := loopCount(t, conn, run.ID, issue); got != 2 {
		t.Errorf("loop_count = %d, want 2 — the round was entered, then parked", got)
	}

	// THE WAY OUT WORKS: `--as override-pass` accepts the hand-back anyway,
	// and the chain the park was holding becomes offerable again.
	testsupport.Must(t, e.ResolveStep(conn, stepIDByInstance(t, conn, "fix@2"),
		ResolveOverridePass, "run the chain anyway", nowMS),
		"resolving fix@2: %v", nil)
	offered := readyInstances(t, conn)
	found := false
	for _, instance := range offered {
		if strings.HasPrefix(instance, "review@2") {
			found = true
		}
	}
	if !found {
		t.Errorf("override-pass did not release the round's chain; offered %v", offered)
	}
}

// TestChangedHandBackProceedsDownstream is the regression bound: a round that
// hands back a NEW commit is not the repeat case, and the chain proceeds
// exactly as before.
func TestChangedHandBackProceedsDownstream(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	tree := &treeState{}
	head := ""
	e := handBackEngine(tree, &head)

	enterSecondRound(t, conn, e, tree, &head)

	tree.body, head = "the original change, plus fixes 1 and 2",
		"9999aaaa8888bbbb7777cccc6666dddd"
	claimAndComplete(t, conn, e, "fix@2", "the fix summary", "")

	if got := stepStatus(t, conn, "fix@2"); got != db.StepDone {
		t.Errorf("fix@2 = %q, want %q — a moved hand-back is a real round", got, db.StepDone)
	}
	offered := readyInstances(t, conn)
	found := false
	for _, instance := range offered {
		if strings.HasPrefix(instance, "review@2") {
			found = true
		}
	}
	if !found {
		t.Errorf("a real round's chain was not offered; offered %v", offered)
	}
}

// TestUnresolvableHandBackNeverParksTheRound: a head that cannot be resolved
// ("" — no commit to name) is not evidence of an unchanged tree. The guard
// answers "changed" and the round proceeds, exactly as roundMovedNothing
// refuses to park on a degenerate diff — a broken head resolution must not
// become a stalled run.
func TestUnresolvableHandBackNeverParksTheRound(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	tree := &treeState{}
	head := ""
	e := handBackEngine(tree, &head)

	enterSecondRound(t, conn, e, tree, &head)

	// The tree does not move AND the head cannot be resolved: no measurement,
	// no park.
	head = ""
	claimAndComplete(t, conn, e, "fix@2", "no work this round", "")

	if got := stepStatus(t, conn, "fix@2"); got != db.StepDone {
		t.Errorf("fix@2 = %q, want %q — an unresolvable head is not evidence "+
			"of an unchanged tree", got, db.StepDone)
	}
}

// TestMissingPriorHandBackNeverParksTheRound is the other degenerate side: the
// prior round recorded no head at all (its checkout's HEAD was unresolvable at
// the time), so there is nothing real to compare against and the round
// proceeds.
func TestMissingPriorHandBackNeverParksTheRound(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	tree := &treeState{}
	head := ""
	e := handBackEngine(tree, &head)

	// Rounds 0 and 1 move the tree but never resolve a head, so no round
	// record carries one.
	tree.body, head = "the original change", ""
	driveRound(t, conn, e, 0)
	tree.body = "the original change, plus fix 1"
	driveRound(t, conn, e, 1)
	if !stepExists(t, conn, "fix@2") {
		t.Fatal("premise: round 1 must have entered round 2")
	}

	// fix@2 resolves a head for the first time; there is no prior hand-back to
	// equal, whatever the tree did.
	head = "1111eeee2222ffff3333aaaa4444bbbb"
	claimAndComplete(t, conn, e, "fix@2", "no work this round", "")

	if got := stepStatus(t, conn, "fix@2"); got != db.StepDone {
		t.Errorf("fix@2 = %q, want %q — no prior hand-back means no evidence",
			got, db.StepDone)
	}
}

// TestHeadMovementDoesNotSatisfyTheConvergenceGuard pins the two mechanisms
// apart (DKT-340 vs DKT-588). A round whose HEAD moved — an out-of-scope
// commit — while the scoped diff stayed identical is invisible to the
// hand-back guard (the sha moved) and still caught by roundMovedNothing at the
// next loop's entry, on its own later, differently-scoped trigger.
func TestHeadMovementDoesNotSatisfyTheConvergenceGuard(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	tree := &treeState{}
	head := ""
	e := handBackEngine(tree, &head)

	tree.body, head = "the original change", "aaaa1111bbbb2222cccc3333dddd4444"
	driveRound(t, conn, e, 0)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: round 0 must have entered the loop")
	}

	// Round 1: HEAD moves, the scoped tree does not.
	head = "eeee5555ffff6666aaaa7777bbbb8888"
	driveRound(t, conn, e, 1)

	// DKT-588 stayed silent — fix@1 has no prior hand-back and its sha moved —
	// so the round ran; DKT-340 then refused the NEXT entry.
	if got := stepStatus(t, conn, "fix@1"); got != db.StepDone {
		t.Errorf("fix@1 = %q, want %q — the hand-back guard has no business here",
			got, db.StepDone)
	}
	if stepExists(t, conn, "fix@2") {
		t.Error("a round that changed nothing in scope minted the next round")
	}
	if got := stepStatus(t, conn, "reconcile@1"); got != db.StepWaitingHuman {
		t.Errorf("reconcile@1 = %q, want the non-convergence park %q",
			got, db.StepWaitingHuman)
	}
	if routing := stepRoutingRaw(t, conn, "reconcile@1"); !strings.Contains(routing, "changed nothing") {
		t.Errorf("DKT-340's own park lost its reason: %q", routing)
	}
}
