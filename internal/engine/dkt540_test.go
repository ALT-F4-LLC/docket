package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-540: an interposed conditional gate downstream of a step OUTSIDE the
// loop's `after_loop` closure must skip-on-pass identically whether or not the
// issue's loop counter has moved.
//
// On RUN-41, two structurally identical verify -> verify-tribunal pairs
// diverged: the issue whose review chain had looped once (`loop-entered ...
// ordinal 1`) recorded `verify@0` routing `pass` and NO `step-skipped`
// companion ever fired for its tribunal — it sat `pending` with "no threshold
// has routed to this interposed step" forever, and the issue never reached
// `done`. The mechanism: StaleLineage read `ordinal < loop_count` as
// "superseded lineage", but the loop entry had never swept or re-instantiated
// the verify branch — its ordinal-0 instances were still the issue's current
// ones — so a live routing was declared inert and the whole reconcile
// (interposed-gate skip, issue completion, run rollup) was skipped.
//
// The fixture reproduces the shape minimally: a review/fix loop on one branch,
// and a parallel verify branch (after `draft`, NOT after anything in the
// `after_loop` closure) carrying a threshold-interposed human gate.
const dkt540Src = `
[pipeline]
name = "loop-beside-interpose"
version = 1

[match]
kind = ["task"]

[[step]]
name = "draft"
executor = "draft"
emits = "doc"

[[step]]
name = "review"
after = ["draft"]
executor = "review"
emits = "findings"
inputs = ["draft.doc"]
threshold = { "fix-loop" = "any(status == blocked)" }
max_fix_loops = 3

[[step]]
name = "fix"
executor = "fix"
emits = "doc"
loop = true
inputs = ["review.findings", "draft.doc"]
after_loop = "review"

[[step]]
name = "verify"
after = ["draft"]
executor = "verify"
emits = "report"
threshold = { "tribunal" = "any(status == blocked)" }

[[step]]
name = "tribunal"
after = ["verify"]
type = "human"
on_fail = "skip"
`

func TestInterposedSkipFiresAfterAnAncestorLoop(t *testing.T) {
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, dkt540Src)
	e := testEngine()

	// Round 0 of the loop branch: draft, then a blocked review that enters
	// loop 1. The verify branch is deliberately untouched — its ordinal-0
	// instances survive the entry, exactly as RUN-41's did, because nothing
	// in the `after_loop = "review"` closure reaches them.
	claimAndComplete(t, conn, e, "draft@0", "the draft", "")
	claimAndComplete(t, conn, e, "review@0", "findings", `[{"status":"blocked"}]`)

	if got := loopCount(t, conn, runID, issue); got != 1 {
		t.Fatalf("loop_count = %d after the blocked review, want 1", got)
	}
	if got := stepStatus(t, conn, "verify@0"); got != db.StepPending {
		t.Fatalf("verify@0 = %q after the loop entry, want %q — it is outside "+
			"the after_loop closure and must not be swept", got, db.StepPending)
	}
	if stepExists(t, conn, "verify@1") {
		t.Fatal("verify@1 exists; the loop must not re-instantiate a step " +
			"outside the after_loop closure")
	}

	// Its lineage is LIVE: nothing replaced it. This is the predicate the
	// defect got wrong — `ordinal < loop_count` alone read it as superseded.
	if staleLineage(t, conn, "verify@0") {
		t.Error("verify@0 reads as a stale lineage after the ancestor loop; " +
			"no later instance of it exists, so its routing must stay live")
	}

	// Round 1 of the loop branch passes cleanly.
	claimAndComplete(t, conn, e, "fix@1", "the fixed draft", "")
	claimAndComplete(t, conn, e, "review@1", "findings", `[{"status":"ok"}]`)

	// The verify branch records and routes `pass` — AFTER the loop iteration.
	// Same routing, same threshold shape as the never-looped sibling issue on
	// RUN-41; the skip companion must fire identically.
	claimAndComplete(t, conn, e, "verify@0", "all clear", `[{"status":"ok"}]`)

	if got := stepRouting(t, conn, "verify@0"); got != "pass" {
		t.Fatalf("verify@0 routing = %q, want pass", got)
	}
	if got := stepStatus(t, conn, "tribunal@0"); got != db.StepSkipped {
		t.Errorf("tribunal@0 = %q after verify@0 routed pass, want %q — the "+
			"interposed gate's skip-on-pass must fire whether or not the "+
			"ancestor chain has looped", got, db.StepSkipped)
	}

	// The skip is event-logged, naming the routing that decided against it —
	// the companion event RUN-41's stuck issue never got.
	var data string
	err := conn.QueryRow(
		`SELECT e.data FROM events e JOIN steps s ON s.id = e.step_id
		  WHERE e.run_id = ? AND e.kind = 'step-skipped'
		    AND s.instance = 'tribunal@0' ORDER BY e.seq DESC LIMIT 1`,
		runID).Scan(&data)
	testsupport.Must(t, err, "reading tribunal@0's step-skipped event: %v", err)
	if !strings.Contains(data, "verify@0") || !strings.Contains(data, "pass") {
		t.Errorf("step-skipped data = %q, want it to name verify@0 and pass", data)
	}

	// And the reconcile the routing carries ran to the end: the issue
	// completes instead of sitting `in-progress` with every step terminal.
	if got := issueStatusOf(t, conn, issue); got != "done" {
		t.Errorf("issue status = %q with every step terminal, want done — a "+
			"live routing wrongly read as stale completes nothing", got)
	}
	if got := runStatusOf(t, conn, runID); got != "done" {
		t.Errorf("run status = %q, want done", got)
	}
}
