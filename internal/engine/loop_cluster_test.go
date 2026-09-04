package engine

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-544: per-trigger fix-loop scoping. Two independent quality gates each
// carry their own loop cluster — a `serves`-scoped body, its own `after_loop`
// re-entry, and its own round budget — under one issue-level counter and one
// global ceiling.
//
// The fixture is the dotfiles.vorpal shape reduced to two gates: `draft`
// fans into two PARALLEL gates, each of which wants reject -> respawn-the-
// offender -> re-check semantics of its own. Before cluster scoping only one
// loop construct existed per workflow, so entering either gate's loop would
// have instantiated BOTH bodies and re-instantiated the union of both
// downstream chains.
//
//   - cluster A: `prd-gate` -> `prd-fix` (serves prd-gate, bound 1,
//     re-enters at prd-gate);
//   - cluster B: `design-gate` -> `design-fix` (serves design-gate, bound 5,
//     re-enters at design-gate);
//   - the GLOBAL ceiling is `max_fix_loops = 2` on `prd-gate` — a non-cluster
//     declaration, so it stays the issue's bound exactly as before.
const clusterSrc = `
[pipeline]
name = "two-clusters"
version = 1

[match]
kind = ["task"]

[[step]]
name = "draft"
executor = "draft"
emits = "doc"

[[step]]
name = "prd-gate"
after = ["draft"]
executor = "prd-review"
emits = "findings"
inputs = ["draft.doc"]
threshold = { "fix-loop" = "any(status == blocked)" }
max_fix_loops = 2

[[step]]
name = "design-gate"
after = ["draft"]
executor = "design-review"
emits = "report"
inputs = ["draft.doc"]
threshold = { "fix-loop" = "any(status == blocked)" }

[[step]]
name = "prd-fix"
executor = "fix"
emits = "doc"
loop = true
serves = ["prd-gate"]
after_loop = "prd-gate"
max_fix_loops = 1
inputs = ["prd-gate.findings", "draft.doc"]

[[step]]
name = "design-fix"
executor = "fix"
emits = "doc"
loop = true
serves = ["design-gate"]
after_loop = "design-gate"
max_fix_loops = 5
inputs = ["design-gate.report", "draft.doc"]
`

const blockedPayload = `[{"status":"blocked"}]`
const okPayload = `[{"status":"ok"}]`

// TestClusterEntryInstantiatesOnlyItsServingBodies is the scoping itself:
// each gate's `fix-loop` instantiates ITS body, re-instantiates ITS re-entry
// chain, sweeps ONLY its own downstream — and leaves the other cluster's
// instances exactly where they were.
func TestClusterEntryInstantiatesOnlyItsServingBodies(t *testing.T) {
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, clusterSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "draft@0", "the draft", "")

	// Cluster A enters: prd-gate@0 routes fix-loop.
	claimAndComplete(t, conn, e, "prd-gate@0", "findings", blockedPayload)

	if got := loopCount(t, conn, runID, issue); got != 1 {
		t.Fatalf("loop_count = %d after prd-gate's fix-loop, want 1", got)
	}
	for _, want := range []string{"prd-fix@1", "prd-gate@1"} {
		if !stepExists(t, conn, want) {
			t.Errorf("%s does not exist; cluster A's entry must instantiate "+
				"its serving body and its after_loop chain", want)
		}
	}
	for _, mustNot := range []string{"design-fix@1", "design-gate@1"} {
		if stepExists(t, conn, mustNot) {
			t.Errorf("%s exists; cluster A's entry must not instantiate "+
				"cluster B's body or chain", mustNot)
		}
	}
	// Cluster B's pending gate is NOT swept: it is outside cluster A's
	// after_loop downstream, so its ordinal-0 instance stays the current one.
	if got := stepStatus(t, conn, "design-gate@0"); got != db.StepPending {
		t.Errorf("design-gate@0 = %q after cluster A's entry, want %q — "+
			"another cluster's sweep must not reach it", got, db.StepPending)
	}
	if staleLineage(t, conn, "design-gate@0") {
		t.Error("design-gate@0 reads as a stale lineage after cluster A's " +
			"entry; nothing replaced it, so its routing must stay live")
	}

	// Round 1 of cluster A passes.
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "prd-fix@1", "the fixed draft", "")
	claimAndComplete(t, conn, e, "prd-gate@1", "findings", okPayload)

	// Cluster B enters, at the issue's NEXT ordinal — one counter, shared.
	claimAndComplete(t, conn, e, "design-gate@0", "report", blockedPayload)

	if got := loopCount(t, conn, runID, issue); got != 2 {
		t.Fatalf("loop_count = %d after design-gate's fix-loop, want 2", got)
	}
	for _, want := range []string{"design-fix@2", "design-gate@2"} {
		if !stepExists(t, conn, want) {
			t.Errorf("%s does not exist; cluster B's entry must instantiate "+
				"its serving body and its after_loop chain", want)
		}
	}
	for _, mustNot := range []string{"prd-fix@2", "prd-gate@2"} {
		if stepExists(t, conn, mustNot) {
			t.Errorf("%s exists; cluster B's entry must not re-instantiate "+
				"cluster A's body or chain", mustNot)
		}
	}
	// Cluster A's finished chain is untouched by cluster B's sweep.
	if got := stepStatus(t, conn, "prd-gate@1"); got != db.StepDone {
		t.Errorf("prd-gate@1 = %q after cluster B's entry, want %q", got, db.StepDone)
	}

	// The ledger names each entry's trigger.
	triggers := loopEnteredTriggers(t, conn, runID)
	if len(triggers) != 2 || !strings.Contains(triggers[0], "prd-gate") ||
		!strings.Contains(triggers[1], "design-gate") {
		t.Errorf("loop-entered events carry data %v, want the first to name "+
			"prd-gate and the second design-gate", triggers)
	}

	// Round 2 of cluster B passes, and the issue completes over
	// highest-ordinal instances per name — prd-fix at 1, design-fix at 2.
	driveFixtureRound(t, 2)
	claimAndComplete(t, conn, e, "design-fix@2", "the redesigned draft", "")
	claimAndComplete(t, conn, e, "design-gate@2", "report", okPayload)

	if got := issueStatusOf(t, conn, issue); got != "done" {
		t.Errorf("issue status = %q with every chain finished, want done", got)
	}
	if got := runStatusOf(t, conn, runID); got != "done" {
		t.Errorf("run status = %q, want done", got)
	}
}

// TestClusterBoundParksItsOwnTriggerOnly: a cluster's own `max_fix_loops`
// refuses ITS next round — waiting-human, counter restored, nothing
// instantiated, the fix-round way out named — while the global budget still
// has room and the other cluster's instances stand untouched.
func TestClusterBoundParksItsOwnTriggerOnly(t *testing.T) {
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, clusterSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "draft@0", "the draft", "")

	// Cluster A's one round is minted...
	claimAndComplete(t, conn, e, "prd-gate@0", "findings", blockedPayload)
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "prd-fix@1", "the fixed draft", "")

	// ...and its second is refused by the CLUSTER bound (1), not the global
	// ceiling (2), which still has room.
	claimAndComplete(t, conn, e, "prd-gate@1", "findings", blockedPayload)

	if got := stepStatus(t, conn, "prd-gate@1"); got != db.StepWaitingHuman {
		t.Fatalf("prd-gate@1 = %q after its cluster's rounds are spent, want %q",
			got, db.StepWaitingHuman)
	}
	raw := stepRoutingRaw(t, conn, "prd-gate@1")
	if !strings.Contains(raw, "cluster") || !strings.Contains(raw, "max_fix_loops = 1") ||
		!strings.Contains(raw, "fix-round") {
		t.Errorf("prd-gate@1 routing = %q, want it to name the cluster bound "+
			"of 1 and the fix-round way out", raw)
	}
	if got := loopCount(t, conn, runID, issue); got != 1 {
		t.Errorf("loop_count = %d after a refused cluster round, want 1 — "+
			"a refusal is not an ordinal", got)
	}
	if stepExists(t, conn, "prd-fix@2") {
		t.Error("prd-fix@2 exists; a bounded cluster entry must instantiate nothing")
	}
	// Cluster B's budget and instances are untouched by A's exhaustion: no
	// body of B was ever minted, and its gate still waits at ordinal 0.
	if stepExists(t, conn, "design-fix@1") || stepExists(t, conn, "design-fix@2") {
		t.Error("a design-fix instance exists; cluster A's refusals must not touch cluster B")
	}
	if got := stepStatus(t, conn, "design-gate@0"); got != db.StepPending {
		t.Errorf("design-gate@0 = %q, want %q", got, db.StepPending)
	}
}

// TestGlobalCeilingBoundsAClusterWithBudgetLeft: the issue-level counter is
// the ceiling over EVERY cluster — cluster B has four of its five rounds
// unspent, and the third entry is still refused because the ISSUE has none.
func TestGlobalCeilingBoundsAClusterWithBudgetLeft(t *testing.T) {
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, clusterSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "draft@0", "the draft", "")

	// Two rounds of cluster B spend the whole GLOBAL budget (2). Each gate
	// records its OWN report (roundReport): a routing step repeating the
	// byte-identical verdict parks the loop under DKT-589, and this test's
	// subject is the global ceiling, not the identical-verdict park.
	claimAndComplete(t, conn, e, "design-gate@0", roundReport(0), blockedPayload)
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "design-fix@1", "the redesigned draft", "")
	claimAndComplete(t, conn, e, "design-gate@1", roundReport(1), blockedPayload)
	driveFixtureRound(t, 2)
	claimAndComplete(t, conn, e, "design-fix@2", "the re-redesigned draft", "")

	if got := loopCount(t, conn, runID, issue); got != 2 {
		t.Fatalf("loop_count = %d after two cluster B rounds, want 2", got)
	}

	// The third refusal is the GLOBAL ceiling's, though the cluster's own
	// bound (5) has plenty of room.
	claimAndComplete(t, conn, e, "design-gate@2", roundReport(2), blockedPayload)

	if got := stepStatus(t, conn, "design-gate@2"); got != db.StepWaitingHuman {
		t.Fatalf("design-gate@2 = %q past the global ceiling, want %q",
			got, db.StepWaitingHuman)
	}
	raw := stepRoutingRaw(t, conn, "design-gate@2")
	if !strings.Contains(raw, "max_fix_loops = 2") {
		t.Errorf("design-gate@2 routing = %q, want it to name the global "+
			"ceiling of 2", raw)
	}
	if got := loopCount(t, conn, runID, issue); got != 2 {
		t.Errorf("loop_count = %d after the global refusal, want 2", got)
	}
	if stepExists(t, conn, "design-fix@3") {
		t.Error("design-fix@3 exists; the global ceiling must instantiate nothing")
	}
}

// TestNoServesDeclaredIsOneClusterExactly pins backward compatibility at the
// set level: with no `serves` anywhere, every trigger's cluster is EVERY body
// and EVERY root's downstream — the merged sets the single-construct reading
// always used. The whole legacy loop suite (loop_test.go and its siblings,
// over the serves-free fixture) is the behavioral half of this check.
func TestNoServesDeclaredIsOneClusterExactly(t *testing.T) {
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading the fixture: %v", err)
	def, err := workflow.Parse(src)
	testsupport.Must(t, err, "parsing the fixture: %v", err)

	for _, trigger := range []string{"reconcile", "verify", "commit-gate"} {
		bodies := workflow.LoopBodiesFor(def, trigger)
		if len(bodies) != 1 || !bodies["fix"] {
			t.Errorf("LoopBodiesFor(%q) = %v, want every body (fix)", trigger, bodies)
		}

		scoped := afterLoopDownstreamFor(def, trigger)
		merged := afterLoopDownstream(def)
		if len(scoped) != len(merged) {
			t.Errorf("afterLoopDownstreamFor(%q) has %d members, want the "+
				"merged closure's %d", trigger, len(scoped), len(merged))
		}
		for name := range merged {
			if !scoped[name] {
				t.Errorf("afterLoopDownstreamFor(%q) is missing %q", trigger, name)
			}
		}
	}

	// And the fixture's global bound is still read as the global bound.
	if got := maxFixLoops(def); got != 2 {
		t.Errorf("maxFixLoops = %d on the serves-free fixture, want 2", got)
	}
	if got := clusterMaxFixLoops(def, "verify"); got != 0 {
		t.Errorf("clusterMaxFixLoops = %d on the serves-free fixture, want 0 "+
			"— no scoped body, no cluster bound", got)
	}
}

// loopEnteredTriggers reads the run's loop-entered event data, in entry order.
func loopEnteredTriggers(t *testing.T, conn *sql.DB, runID int) []string {
	t.Helper()
	rows, err := conn.Query(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq`,
		runID, EventLoopEntered)
	testsupport.Must(t, err, "reading loop-entered events: %v", err)
	defer rows.Close()

	var out []string
	for rows.Next() {
		var data string
		testsupport.Must(t, rows.Scan(&data), "scanning loop-entered data")
		out = append(out, data)
	}
	testsupport.Must(t, rows.Err(), "iterating loop-entered events")
	return out
}
