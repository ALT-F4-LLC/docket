package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-587: RUN-34 (security-load-bearing@10, max_fix_loops = 2) showed three
// completed loop ordinals — fix@1, fix@2, fix@3 — while RUN-39 (spec-doc,
// max_fix_loops = 2) exhausted at ordinal 2, and the retro read that as the
// counter differing by entry path (reconcile threshold vs verify on_fail) or
// being off by one on one of them.
//
// These tests pin the opposite finding: at HEAD there is ONE counter and ONE
// definition. Every routing source — a threshold resolving `fix-loop`, an
// `on_fail = "fix-loop"` from a gate failure or a rejected human gate, a
// rejected vote tally, a quorum miss — reaches enterLoop through the same
// applyFixLoop bridge with the same workflow definition and `authorized =
// false`, and the arithmetic is post-increment: the admitted entry's count IS
// the issue's new 1-indexed ordinal, `count > max` refuses and restores the
// counter. `max_fix_loops = N` therefore admits exactly N entries from ANY mix
// of routing sources and parks the N+1th `waiting-human`.
//
// The one sanctioned way to a third ordinal under `max_fix_loops = 2` is an
// operator's `step resolve --as fix-round` (DKT-237), which records a grant
// and enters the round in one transaction — leaving loop_grants = 1, a
// step-resolved event, and the parked step superseded. That is the state
// RUN-34's ledger shape ("fix@3 done, no park left standing") matches, and the
// last test here reproduces it deliberately.
//
// Each round's routing step records its OWN artifact (roundReport) alongside
// the shared `unmetPayload`, for driveFixtureRound's reason one guard over:
// DKT-589 parks a loop whose routing step repeats the BYTE-IDENTICAL verdict at
// two consecutive ordinals, so a fixture reusing one constant body would park
// at entry 2 and never reach the bound these tests are about. The payload — the
// thing the threshold actually reads — is unchanged, so every entry still
// routes `fix-loop` from the same predicate it always did.

// dkt587ThresholdSrc drives every loop entry through a THRESHOLD routing —
// the reconcile-threshold entry shape, reduced: `check` aggregates nothing
// and simply records a payload its own threshold reads.
const dkt587ThresholdSrc = `
[pipeline]
name = "dkt587-threshold"
version = 1

[match]
kind = ["task"]

[[step]]
name = "check"
executor = "check"
emits = "findings"
threshold = { "fix-loop" = "any(status == unmet)" }
max_fix_loops = 2

[[step]]
name = "fix"
executor = "fix"
emits = "findings"
loop = true
after_loop = "check"
`

// dkt587OnFailSrc drives every loop entry through an ON_FAIL routing — a
// rejected human gate, the verify-on_fail entry shape's decision path.
const dkt587OnFailSrc = `
[pipeline]
name = "dkt587-onfail"
version = 1

[match]
kind = ["task"]

[[step]]
name = "seed"
executor = "seed"
emits = "doc"

[[step]]
name = "gate"
after = ["seed"]
type = "human"
on_fail = "fix-loop"
max_fix_loops = 2

[[step]]
name = "fix"
executor = "fix"
emits = "doc"
loop = true
after_loop = "gate"
`

// dkt587MixedSrc lets ONE issue enter its loop through BOTH routing sources:
// `check`'s threshold and `gate`'s on_fail both feed the same (serves-free,
// single-cluster) loop.
const dkt587MixedSrc = `
[pipeline]
name = "dkt587-mixed"
version = 1

[match]
kind = ["task"]

[[step]]
name = "check"
executor = "check"
emits = "findings"
threshold = { "fix-loop" = "any(status == unmet)" }
max_fix_loops = 2

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

// dkt587ClusterSrc declares the bound on a `serves`-SCOPED loop body — the
// shape under which a `max_fix_loops = 2` is the CLUSTER's round budget
// (clusterMaxFixLoops/clusterRoundsUsed) rather than the issue ceiling. The
// arithmetic must land on the same answer: two rounds admitted, the third
// refused.
const dkt587ClusterSrc = `
[pipeline]
name = "dkt587-cluster"
version = 1

[match]
kind = ["task"]

[[step]]
name = "check"
executor = "check"
emits = "findings"
threshold = { "fix-loop" = "any(status == unmet)" }

[[step]]
name = "fix"
executor = "fix"
emits = "findings"
loop = true
serves = ["check"]
after_loop = "check"
max_fix_loops = 2
`

func TestThresholdEntriesBoundAtExactlyMaxFixLoops(t *testing.T) {
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, dkt587ThresholdSrc)
	e := testEngine()

	// Entry 1 and entry 2: both admitted at max_fix_loops = 2, each minting
	// the issue's next ordinal.
	claimAndComplete(t, conn, e, "check@0", roundReport(0), unmetPayload)
	if got := loopCount(t, conn, runID, issue); got != 1 {
		t.Fatalf("loop_count = %d after the first threshold entry, want 1", got)
	}
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("fix@1 was not instantiated by the first threshold entry")
	}

	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")
	claimAndComplete(t, conn, e, "check@1", roundReport(1), unmetPayload)
	if got := loopCount(t, conn, runID, issue); got != 2 {
		t.Fatalf("loop_count = %d after the second threshold entry, want 2", got)
	}
	if !stepExists(t, conn, "fix@2") {
		t.Fatal("fix@2 was not instantiated by the second threshold entry")
	}

	// Entry 3: refused. count = 3 > max = 2; the counter is restored, nothing
	// is instantiated, and the routing step parks naming the way out.
	driveFixtureRound(t, 2)
	claimAndComplete(t, conn, e, "fix@2", "the second fix", "")
	claimAndComplete(t, conn, e, "check@2", roundReport(2), unmetPayload)

	if stepExists(t, conn, "fix@3") {
		t.Error("fix@3 exists; max_fix_loops = 2 must refuse the third threshold entry")
	}
	if got := stepStatus(t, conn, "check@2"); got != db.StepWaitingHuman {
		t.Errorf("check@2 = %q after the bound, want %q", got, db.StepWaitingHuman)
	}
	if got := loopCount(t, conn, runID, issue); got != 2 {
		t.Errorf("loop_count = %d after the refusal, want 2 — a refusal is not an ordinal", got)
	}
	raw := stepRoutingRaw(t, conn, "check@2")
	if !strings.Contains(raw, "max_fix_loops = 2") || !strings.Contains(raw, "fix-round") {
		t.Errorf("check@2 routing = %q, want it to name the bound and the fix-round way out", raw)
	}
}

func TestOnFailEntriesBoundAtExactlyMaxFixLoops(t *testing.T) {
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, dkt587OnFailSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "seed@0", "the doc", "")

	// Entry 1 and entry 2: rejected human gates, both admitted.
	err := e.DecideStep(conn, stepIDByInstance(t, conn, "gate@0"), false, "no", nowMS)
	testsupport.Must(t, err, "rejecting gate@0: %v", err)
	if got := loopCount(t, conn, runID, issue); got != 1 {
		t.Fatalf("loop_count = %d after the first on_fail entry, want 1", got)
	}
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("fix@1 was not instantiated by the first on_fail entry")
	}

	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")
	err = e.DecideStep(conn, stepIDByInstance(t, conn, "gate@1"), false, "still no", nowMS)
	testsupport.Must(t, err, "rejecting gate@1: %v", err)
	if got := loopCount(t, conn, runID, issue); got != 2 {
		t.Fatalf("loop_count = %d after the second on_fail entry, want 2", got)
	}
	if !stepExists(t, conn, "fix@2") {
		t.Fatal("fix@2 was not instantiated by the second on_fail entry")
	}

	// Entry 3: refused, identically to the threshold path.
	driveFixtureRound(t, 2)
	claimAndComplete(t, conn, e, "fix@2", "the second fix", "")
	err = e.DecideStep(conn, stepIDByInstance(t, conn, "gate@2"), false, "third no", nowMS)
	testsupport.Must(t, err, "rejecting gate@2: %v", err)

	if stepExists(t, conn, "fix@3") {
		t.Error("fix@3 exists; max_fix_loops = 2 must refuse the third on_fail entry")
	}
	if got := stepStatus(t, conn, "gate@2"); got != db.StepWaitingHuman {
		t.Errorf("gate@2 = %q after the bound, want %q", got, db.StepWaitingHuman)
	}
	if got := loopCount(t, conn, runID, issue); got != 2 {
		t.Errorf("loop_count = %d after the refusal, want 2", got)
	}
	raw := stepRoutingRaw(t, conn, "gate@2")
	if !strings.Contains(raw, "max_fix_loops = 2") || !strings.Contains(raw, "fix-round") {
		t.Errorf("gate@2 routing = %q, want it to name the bound and the fix-round way out", raw)
	}
}

// TestBothRoutingSourcesShareOneCounter is the acceptance criterion stated
// directly: an entry from the on_fail path and an entry from the threshold
// path move the SAME issue counter, so `max_fix_loops = 2` admits exactly two
// entries from any mix of sources — no divergence between them.
func TestBothRoutingSourcesShareOneCounter(t *testing.T) {
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, dkt587MixedSrc)
	e := testEngine()

	// check@0 passes; the human gate rejects — entry 1 arrives via ON_FAIL.
	claimAndComplete(t, conn, e, "check@0", "findings", metPayload)
	err := e.DecideStep(conn, stepIDByInstance(t, conn, "gate@0"), false, "no", nowMS)
	testsupport.Must(t, err, "rejecting gate@0: %v", err)
	if got := loopCount(t, conn, runID, issue); got != 1 {
		t.Fatalf("loop_count = %d after the on_fail entry, want 1", got)
	}

	// Entry 2 arrives via the THRESHOLD — and continues the same ordinal
	// sequence, not a second counter of its own.
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")
	claimAndComplete(t, conn, e, "check@1", roundReport(1), unmetPayload)
	if got := loopCount(t, conn, runID, issue); got != 2 {
		t.Fatalf("loop_count = %d after the threshold entry, want 2 — "+
			"both routing sources must move one counter", got)
	}
	if !stepExists(t, conn, "fix@2") {
		t.Fatal("fix@2 was not instantiated by the threshold entry")
	}

	// The third attempt — from either source — is refused. Here the threshold
	// fires it; the two prior entries, one per source, have spent the budget.
	driveFixtureRound(t, 2)
	claimAndComplete(t, conn, e, "fix@2", "the second fix", "")
	claimAndComplete(t, conn, e, "check@2", roundReport(2), unmetPayload)

	if stepExists(t, conn, "fix@3") {
		t.Error("fix@3 exists; two entries from mixed sources must exhaust max_fix_loops = 2")
	}
	if got := stepStatus(t, conn, "check@2"); got != db.StepWaitingHuman {
		t.Errorf("check@2 = %q after the mixed-source bound, want %q", got, db.StepWaitingHuman)
	}
	if got := loopCount(t, conn, runID, issue); got != 2 {
		t.Errorf("loop_count = %d after the refusal, want 2", got)
	}

	// The ledger names each entry's trigger: first the gate, then the check.
	triggers := loopEnteredTriggers(t, conn, runID)
	if len(triggers) != 2 ||
		!strings.Contains(triggers[0], `"gate"`) ||
		!strings.Contains(triggers[1], `"check"`) {
		t.Errorf("loop-entered events = %v, want exactly two entries, the first "+
			"triggered by gate and the second by check", triggers)
	}
}

// TestClusterScopedBoundAdmitsExactlyItsRounds pins the SECOND arithmetic —
// clusterMaxFixLoops/clusterRoundsUsed, the mechanism that bounds a
// `max_fix_loops` declared on a `serves`-scoped body — to the same answer:
// bound 2 admits rounds 1 and 2 and refuses round 3, with no issue-level
// ceiling in the workflow at all.
func TestClusterScopedBoundAdmitsExactlyItsRounds(t *testing.T) {
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, dkt587ClusterSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "check@0", roundReport(0), unmetPayload)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("fix@1 was not instantiated by the cluster's first round")
	}

	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")
	claimAndComplete(t, conn, e, "check@1", roundReport(1), unmetPayload)
	if !stepExists(t, conn, "fix@2") {
		t.Fatal("fix@2 was not instantiated by the cluster's second round")
	}
	if got := loopCount(t, conn, runID, issue); got != 2 {
		t.Fatalf("loop_count = %d after two cluster rounds, want 2", got)
	}

	driveFixtureRound(t, 2)
	claimAndComplete(t, conn, e, "fix@2", "the second fix", "")
	claimAndComplete(t, conn, e, "check@2", roundReport(2), unmetPayload)

	if stepExists(t, conn, "fix@3") {
		t.Error("fix@3 exists; a cluster bound of 2 must refuse its third round")
	}
	if got := stepStatus(t, conn, "check@2"); got != db.StepWaitingHuman {
		t.Errorf("check@2 = %q after the cluster bound, want %q", got, db.StepWaitingHuman)
	}
	if got := loopCount(t, conn, runID, issue); got != 2 {
		t.Errorf("loop_count = %d after the cluster refusal, want 2", got)
	}
	raw := stepRoutingRaw(t, conn, "check@2")
	if !strings.Contains(raw, "cluster") || !strings.Contains(raw, "max_fix_loops = 2") {
		t.Errorf("check@2 routing = %q, want it to name the cluster's bound of 2", raw)
	}
}

// TestFixRoundGrantIsTheOnlyWayToAThirdOrdinal reproduces the ONE sanctioned
// path to RUN-34's observed shape — fix@1, fix@2, fix@3 all done under
// `max_fix_loops = 2`: an operator resolves the parked step `--as fix-round`,
// which records a grant and mints the third ordinal in one transaction. The
// durable evidence it leaves (loop_grants = 1, the park superseded) is exactly
// what distinguishes an authorized third round from a counting defect.
func TestFixRoundGrantIsTheOnlyWayToAThirdOrdinal(t *testing.T) {
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, dkt587ThresholdSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "check@0", roundReport(0), unmetPayload)
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")
	claimAndComplete(t, conn, e, "check@1", roundReport(1), unmetPayload)
	driveFixtureRound(t, 2)
	claimAndComplete(t, conn, e, "fix@2", "the second fix", "")
	claimAndComplete(t, conn, e, "check@2", roundReport(2), unmetPayload)

	// Bounded, as the tests above pin.
	if got := stepStatus(t, conn, "check@2"); got != db.StepWaitingHuman {
		t.Fatalf("check@2 = %q, want %q before the grant", got, db.StepWaitingHuman)
	}

	// The operator authorizes one more round.
	err := e.ResolveStep(conn, stepIDByInstance(t, conn, "check@2"),
		ResolveFixRound, "one more round", nowMS)
	testsupport.Must(t, err, "resolving --as fix-round: %v", err)

	if !stepExists(t, conn, "fix@3") {
		t.Fatal("fix@3 was not minted by the fix-round grant")
	}
	if got := loopCount(t, conn, runID, issue); got != 3 {
		t.Errorf("loop_count = %d after the granted round, want 3", got)
	}
	if got := stepStatus(t, conn, "check@2"); got != db.StepSuperseded {
		t.Errorf("check@2 = %q after the grant, want %q — the park's question is "+
			"answered by the new round's work", got, db.StepSuperseded)
	}
	// The grant is durable evidence: a ledger reader can tell this third
	// ordinal from a counting defect.
	var grants int
	err = conn.QueryRow(
		`SELECT loop_grants FROM run_issues WHERE run_id = ? AND issue_id = ?`,
		runID, issue).Scan(&grants)
	testsupport.Must(t, err, "reading loop_grants: %v", err)
	if grants != 1 {
		t.Errorf("loop_grants = %d after one fix-round resolution, want 1", grants)
	}
}
