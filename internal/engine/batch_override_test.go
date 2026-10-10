package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-546 — the run-scoped batch gate-override.
//
// One operator override decision covers subsequent identical environmental
// gate failures in the same run. The gates still run; what the grant changes
// is the routing of a failure whose signature (gate + exit + reason) the
// operator already ruled on. A different signature still parks, another run
// re-asks, and every auto-pass is attributed to its grant in the ledger.

// batchOverrideSrc: two sequential executor steps sharing one gate, so a
// grant minted on the first step's park has a second, identical failure to
// cover.
const batchOverrideSrc = `
[pipeline]
name = "batch-override"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "implement"
emits = "change-summary"
gates = ["build"]
on_fail = "waiting-human"

[[step]]
name = "package"
after = ["implement"]
executor = "package"
emits = "package-record"
gates = ["build"]
on_fail = "waiting-human"
`

// batchInterposeSrc adds the DKT-470 shape: the second gated step's threshold
// interposes a vote step, which forbids the auto-apply.
const batchInterposeSrc = `
[pipeline]
name = "batch-interpose"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "implement"
emits = "change-summary"
gates = ["build"]
on_fail = "waiting-human"

[[step]]
name = "audit"
after = ["implement"]
executor = "audit"
emits = "report"
gates = ["build"]
on_fail = "waiting-human"
threshold = { "tribunal" = "any(status == blocked)" }

[[step]]
name = "tribunal"
after = ["audit"]
type = "vote"
voters = ["seat-a", "seat-b", "seat-c"]
vote_rule = "majority"
on_fail = "waiting-human"

[[step]]
name = "finish"
after = ["tribunal"]
executor = "finish"
emits = "record"
`

// exitGates is a GateRunner whose failure EXIT CODE and OUTPUT are settable,
// so a test can produce two failures of the same gate with the same signature
// or with different ones. Since DKT-1796 the output is part of the signature:
// a failure with no output names no failure and matches no grant, so the
// default failure text below is what makes two failures IDENTICAL.
type exitGates struct {
	mu     sync.Mutex
	fail   bool
	exit   int
	output string
	// truncated names the gates whose failing capture reports Truncated, the
	// flag internal/exec sets when output overran the capture cap.
	truncated map[string]bool
	// skipped names the gates that report VerdictSkipped instead of failing,
	// the shape a gate takes when it could not measure its tree.
	skipped map[string]bool
}

// environmentalFailure is the capture both the granted failure and the
// failures it covers print: one unchanged sandbox artifact, seen repeatedly.
const environmentalFailure = "clang: error: unable to spawn process " +
	"(Operation not permitted)\n"

func (g *exitGates) Run(_ context.Context, spec GateSpec, _ StepContext) (GateResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.skipped[spec.Name] {
		return GateResult{Gate: spec.Name, Verdict: VerdictSkipped}, nil
	}
	if g.fail {
		out := g.output
		if out == "" {
			out = environmentalFailure
		}
		return GateResult{
			Gate: spec.Name, Exit: g.exit, Verdict: VerdictFail, Output: out,
			Truncated: g.truncated[spec.Name],
		}, nil
	}
	return GateResult{Gate: spec.Name, Exit: 0, Verdict: VerdictPass}, nil
}

// activatedBatchRun registers src, activates a one-issue run over it, and
// returns the run id.
func activatedBatchRun(t *testing.T, conn *sql.DB, src, path string) int {
	t.Helper()
	registerSource(t, conn, []byte(src), path)
	issue := createIssue(t, conn, "batch fixture", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return run.ID
}

// stepIDIn resolves an instance WITHIN one run — stepIDByInstance is ambiguous
// once a second run expands the same workflow.
func stepIDInRun(t *testing.T, conn *sql.DB, runID int, instance string) int {
	t.Helper()
	var id int
	err := conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND instance = ?`, runID, instance,
	).Scan(&id)
	testsupport.Must(t, err, "finding step %q in run %d: %v", instance, runID, err)
	return id
}

// parkThroughFailingGate claims and completes one step so its failing gate
// parks it, and asserts the park.
func parkThroughFailingGate(t *testing.T, conn *sql.DB, e *Engine, stepID int) {
	t.Helper()
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepWaitingHuman {
		t.Fatalf("premise: %s = %q after a failing gate, want %q",
			step.Instance, step.Status, db.StepWaitingHuman)
	}
}

func stepRoutingByID(t *testing.T, conn *sql.DB, stepID int) string {
	t.Helper()
	var routing string
	err := conn.QueryRow(
		`SELECT routing FROM steps WHERE id = ?`, stepID).Scan(&routing)
	testsupport.Must(t, err, "reading routing of step %d: %v", stepID, err)
	return routing
}

// TestBatchOverrideMintsGrantsAndCoversAMatchingLaterFailure is the feature's
// whole contract in one run: the operator override-passes the first park with
// --batch, one grant per failed gate is recorded and event-logged, and the
// SECOND step failing the same gate with the same signature routes `done`
// with a `pass` naming the grant — no second park, no second operator ruling.
func TestBatchOverrideMintsGrantsAndCoversAMatchingLaterFailure(t *testing.T) {
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchOverrideSrc, "batch-override.toml")

	gates := &exitGates{fail: true, exit: 1}
	e := testEngine()
	e.Gates = gates

	implementID := stepIDInRun(t, conn, runID, "implement@0")
	parkThroughFailingGate(t, conn, e, implementID)

	err := e.ResolveStepBatch(conn, implementID, ResolveOverridePass,
		"sandbox artifact, not a code defect", nowMS+1)
	testsupport.Must(t, err, "resolve --as override-pass --batch: %v", err)

	// ONE grant, carrying the failure signature and the shared justification.
	grants, err := db.GateOverrideGrantsForRun(conn, runID)
	testsupport.Must(t, err, "reading grants: %v", err)
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1 (one per failed gate)", len(grants))
	}
	g := grants[0]
	if g.Gate != "build" || g.Exit == nil || *g.Exit != 1 || g.Reason != "" {
		t.Errorf("grant signature = (%q, %v, %q), want (build, 1, \"\")",
			g.Gate, g.Exit, g.Reason)
	}
	if g.Note != "sandbox artifact, not a code defect" {
		t.Errorf("grant note = %q; the shared justification must ride the grant", g.Note)
	}
	if g.OriginStepID != implementID {
		t.Errorf("grant origin = %d, want %d", g.OriginStepID, implementID)
	}
	if got := eventKindCount(t, conn, runID, EventGateOverrideGranted); got != 1 {
		t.Errorf("%s events = %d, want 1", EventGateOverrideGranted, got)
	}

	// The second step fails the SAME gate with the SAME signature — and does
	// not park.
	packageID := stepIDInRun(t, conn, runID, "package@0")
	claim, err := ClaimStep(conn, packageID, ClaimOptions{Owner: "w2", NowMS: nowMS + 2})
	testsupport.Must(t, err, "claim package: %v", err)
	err = e.CompleteStep(conn, packageID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the package record"), NowMS: nowMS + 2,
	})
	testsupport.Must(t, err, "complete package: %v", err)

	step, err := db.GetStep(conn, packageID)
	testsupport.Must(t, err, "GetStep package: %v", err)
	if step.Status != db.StepDone {
		t.Fatalf("package@0 = %q after a covered failure, want %q",
			step.Status, db.StepDone)
	}
	routing := stepRoutingByID(t, conn, packageID)
	if !strings.HasPrefix(routing, RoutingPass) {
		t.Errorf("package@0 routing = %q, want a %q routing", routing, RoutingPass)
	}
	if !strings.Contains(routing, "grant") {
		t.Errorf("package@0 routing = %q; the auto-pass must name its authority", routing)
	}

	// The ledger: the grant covered one step, attributably.
	grants, err = db.GateOverrideGrantsForRun(conn, runID)
	testsupport.Must(t, err, "re-reading grants: %v", err)
	if grants[0].CoveredSteps != 1 {
		t.Errorf("covered_steps = %d, want 1", grants[0].CoveredSteps)
	}
	if got := eventKindCount(t, conn, runID, EventStepBatchOverridden); got != 1 {
		t.Errorf("%s events = %d, want 1", EventStepBatchOverridden, got)
	}
}

// TestBatchGrantIgnoresADifferentSignature: the grant covers the ruled-on
// failure, not the gate. The same gate failing with a DIFFERENT exit carries
// information the operator has not seen, and it parks exactly as before.
func TestBatchGrantIgnoresADifferentSignature(t *testing.T) {
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchOverrideSrc, "batch-override.toml")

	gates := &exitGates{fail: true, exit: 1}
	e := testEngine()
	e.Gates = gates

	implementID := stepIDInRun(t, conn, runID, "implement@0")
	parkThroughFailingGate(t, conn, e, implementID)
	err := e.ResolveStepBatch(conn, implementID, ResolveOverridePass,
		"no docker socket", nowMS+1)
	testsupport.Must(t, err, "batch resolve: %v", err)

	// Same gate, different exit: a different failure.
	gates.mu.Lock()
	gates.exit = 2
	gates.mu.Unlock()
	packageID := stepIDInRun(t, conn, runID, "package@0")
	parkThroughFailingGate(t, conn, e, packageID)

	grants, err := db.GateOverrideGrantsForRun(conn, runID)
	testsupport.Must(t, err, "reading grants: %v", err)
	if grants[0].CoveredSteps != 0 {
		t.Errorf("covered_steps = %d after a non-matching failure, want 0",
			grants[0].CoveredSteps)
	}
	if got := eventKindCount(t, conn, runID, EventStepBatchOverridden); got != 0 {
		t.Errorf("%s events = %d, want 0", EventStepBatchOverridden, got)
	}
}

// TestBatchGrantIsRunScoped: a new run re-asks. The grant's run_id is the
// whole scope rule, so an identical failure in a SECOND run parks.
func TestBatchGrantIsRunScoped(t *testing.T) {
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchOverrideSrc, "batch-override.toml")

	gates := &exitGates{fail: true, exit: 1}
	e := testEngine()
	e.Gates = gates

	implementID := stepIDInRun(t, conn, runID, "implement@0")
	parkThroughFailingGate(t, conn, e, implementID)
	err := e.ResolveStepBatch(conn, implementID, ResolveOverridePass,
		"sandbox artifact", nowMS+1)
	testsupport.Must(t, err, "batch resolve: %v", err)

	// A second run over the same workflow, failing the same gate the same way.
	issue2 := createIssue(t, conn, "second issue", "a body", "task", nil)
	run2 := startRun(t, conn, issue2)
	_, err = activate(conn, run2.ID)
	testsupport.Must(t, err, "activate run 2: %v", err)

	implement2 := stepIDInRun(t, conn, run2.ID, "implement@0")
	parkThroughFailingGate(t, conn, e, implement2) // asserts the park itself

	grants, err := db.GateOverrideGrantsForRun(conn, runID)
	testsupport.Must(t, err, "reading grants: %v", err)
	if grants[0].CoveredSteps != 0 {
		t.Errorf("covered_steps = %d after another RUN's failure, want 0 — "+
			"the grant must die at its run's edge", grants[0].CoveredSteps)
	}
}

// TestBatchRequiresOverridePass: --batch widens what one authorization covers,
// so it rides only the verb whose ruling it extends.
func TestBatchRequiresOverridePass(t *testing.T) {
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchOverrideSrc, "batch-override.toml")

	gates := &exitGates{fail: true, exit: 1}
	e := testEngine()
	e.Gates = gates

	implementID := stepIDInRun(t, conn, runID, "implement@0")
	parkThroughFailingGate(t, conn, e, implementID)

	err := e.ResolveStepBatch(conn, implementID, ResolveSkip, "nope", nowMS+1)
	if err == nil {
		t.Fatal("--batch with --as skip was accepted; it must require override-pass")
	}
	if !strings.Contains(err.Error(), ResolveOverridePass) {
		t.Errorf("refusal = %q, want it to name %s", err, ResolveOverridePass)
	}
}

// TestBatchRefusesAParkWithNoFailedGate: a park with no failed completion gate
// has no signature to grant from — a vote step waiting on its quorum is the
// reachable case — and the refusal is honest where an unmatchable grant would
// look like it did something.
func TestBatchRefusesAParkWithNoFailedGate(t *testing.T) {
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchInterposeSrc, "batch-interpose.toml")

	e := testEngine()
	tribunalID := stepIDInRun(t, conn, runID, "tribunal@0")

	err := e.ResolveStepBatch(conn, tribunalID, ResolveOverridePass, "moving on", nowMS)
	if err == nil {
		t.Fatal("--batch on a step with no failed gate was accepted")
	}
	if !strings.Contains(err.Error(), "no failed completion gate") {
		t.Errorf("refusal = %q, want it to say there is nothing to grant from", err)
	}
}

// batchTwoGateSrc: one executor step with two completion gates, so a single
// park carries two failing rows for one batch to grant from.
const batchTwoGateSrc = `
[pipeline]
name = "batch-two-gate"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "implement"
emits = "change-summary"
gates = ["build", "tests"]
on_fail = "waiting-human"
`

// TestBatchOverrideRefusesATruncatedRow: a truncated capture is fingerprinted
// over its head only, so a grant minted from it would cover failures whose
// differing content fell past the cap. One truncated row refuses the WHOLE
// batch before anything is written: no grant for either gate, and the step
// stays parked with its routing untouched.
func TestBatchOverrideRefusesATruncatedRow(t *testing.T) {
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchTwoGateSrc, "batch-two-gate.toml")

	gates := &exitGates{fail: true, exit: 1, truncated: map[string]bool{"tests": true}}
	e := testEngine()
	e.Gates = gates

	implementID := stepIDInRun(t, conn, runID, "implement@0")
	parkThroughFailingGate(t, conn, e, implementID)

	rows, err := db.GateResultsForStep(conn, implementID)
	testsupport.Must(t, err, "reading gate rows: %v", err)
	failing := failingCompletionRows(rows)
	if len(failing) != 2 {
		t.Fatalf("premise: %d failing completion rows, want 2 (build, tests)", len(failing))
	}
	for _, r := range failing {
		if want := r.Gate == "tests"; r.Truncated != want {
			t.Fatalf("premise: gate %q Truncated = %v, want %v", r.Gate, r.Truncated, want)
		}
	}
	routingBefore := stepRoutingByID(t, conn, implementID)

	err = e.ResolveStepBatch(conn, implementID, ResolveOverridePass,
		"sandbox artifact", nowMS+1)
	if err == nil {
		t.Fatal("--batch over a truncated failing row was accepted")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("refusal code = %q, want %q (%v)", code, CodeValidation, err)
	}
	if msg := err.Error(); !strings.Contains(msg, `"tests"`) || !strings.Contains(msg, "truncated") {
		t.Errorf("refusal = %q, want it to name gate \"tests\" and its truncated capture", msg)
	}

	var grantsFromStep int
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM gate_override_grants WHERE origin_step_id = ?`,
		implementID).Scan(&grantsFromStep)
	testsupport.Must(t, err, "counting grants: %v", err)
	if grantsFromStep != 0 {
		t.Errorf("grants with origin %d = %d, want 0 for every gate in the batch",
			implementID, grantsFromStep)
	}
	if got := eventKindCount(t, conn, runID, EventStepResolved); got != 0 {
		t.Errorf("%s events = %d, want 0: the resolution must not be recorded",
			EventStepResolved, got)
	}
	step, err := db.GetStep(conn, implementID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepWaitingHuman {
		t.Errorf("status = %q after the refusal, want %q", step.Status, db.StepWaitingHuman)
	}
	if got := stepRoutingByID(t, conn, implementID); got != routingBefore {
		t.Errorf("routing = %q after the refusal, want it unchanged from %q", got, routingBefore)
	}
}

// TestBatchOverrideRefusesAParkWithASkippedGate: a skipped gate measured
// nothing, and routing parks such a step as gate-skipped without consulting a
// grant, so a grant minted from it could never apply. One skipped row refuses
// the WHOLE batch: no grant for the skipped gate, and none for the `fail` gate
// beside it, which the operator ruled on only as part of a park whose cause
// was the skip.
func TestBatchOverrideRefusesAParkWithASkippedGate(t *testing.T) {
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchTwoGateSrc, "batch-two-gate.toml")

	gates := &exitGates{fail: true, exit: 1, skipped: map[string]bool{"tests": true}}
	e := testEngine()
	e.Gates = gates

	implementID := stepIDInRun(t, conn, runID, "implement@0")
	parkThroughFailingGate(t, conn, e, implementID)

	rows, err := db.GateResultsForStep(conn, implementID)
	testsupport.Must(t, err, "reading gate rows: %v", err)
	verdicts := make(map[string]string)
	for _, r := range failingCompletionRows(rows) {
		verdicts[r.Gate] = r.Verdict
	}
	if verdicts["build"] != db.GateVerdictFail || verdicts["tests"] != db.GateVerdictSkipped {
		t.Fatalf("premise: failing completion verdicts = %v, want build=fail tests=skipped",
			verdicts)
	}

	err = e.ResolveStepBatch(conn, implementID, ResolveOverridePass,
		"sandbox artifact", nowMS+1)
	if err == nil {
		t.Error("--batch over a park with a skipped gate was accepted")
	} else {
		if code, _ := CodeOf(err); code != CodeValidation {
			t.Errorf("refusal code = %q, want %q (%v)", code, CodeValidation, err)
		}
		if msg := err.Error(); !strings.Contains(msg, `"tests"`) || !strings.Contains(msg, "skipped") {
			t.Errorf("refusal = %q, want it to name skipped gate \"tests\"", msg)
		}
	}

	grants, err := db.GateOverrideGrantsForRun(conn, runID)
	testsupport.Must(t, err, "reading grants: %v", err)
	if len(grants) != 0 {
		t.Errorf("grants = %d, want 0: neither the skipped nor the failing gate "+
			"may be granted from this park", len(grants))
	}
}

// TestBatchOverrideMintsOneGrantPerUntruncatedFailingGate: with no truncated
// row the batch mints exactly one grant per failing gate, each carrying its
// origin row's fingerprint verbatim.
func TestBatchOverrideMintsOneGrantPerUntruncatedFailingGate(t *testing.T) {
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchTwoGateSrc, "batch-two-gate.toml")

	gates := &exitGates{fail: true, exit: 1}
	e := testEngine()
	e.Gates = gates

	implementID := stepIDInRun(t, conn, runID, "implement@0")
	parkThroughFailingGate(t, conn, e, implementID)

	rows, err := db.GateResultsForStep(conn, implementID)
	testsupport.Must(t, err, "reading gate rows: %v", err)
	originFingerprint := make(map[string]string)
	for _, r := range failingCompletionRows(rows) {
		originFingerprint[r.Gate] = r.Fingerprint
	}
	if len(originFingerprint) != 2 {
		t.Fatalf("premise: failing gates = %v, want build and tests", originFingerprint)
	}

	err = e.ResolveStepBatch(conn, implementID, ResolveOverridePass,
		"sandbox artifact", nowMS+1)
	testsupport.Must(t, err, "batch resolve: %v", err)

	grants, err := db.GateOverrideGrantsForRun(conn, runID)
	testsupport.Must(t, err, "reading grants: %v", err)
	if len(grants) != 2 {
		t.Fatalf("grants = %d, want 2 (one per failing gate)", len(grants))
	}
	seen := make(map[string]bool)
	for _, g := range grants {
		want, ok := originFingerprint[g.Gate]
		if !ok || seen[g.Gate] {
			t.Errorf("grant for gate %q: unexpected or duplicate", g.Gate)
			continue
		}
		seen[g.Gate] = true
		if want == "" || g.Fingerprint != want {
			t.Errorf("grant %q fingerprint = %q, want origin row's %q", g.Gate, g.Fingerprint, want)
		}
	}
}

// TestBatchCoverBlockedByAnInterposedThreshold is DKT-470 at scale: a matching
// grant must NOT auto-apply on a step whose threshold interposes another step,
// because the generic pass would silently skip it with no operator present to
// read the warning. The step parks per its on_fail, with the block named.
func TestBatchCoverBlockedByAnInterposedThreshold(t *testing.T) {
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchInterposeSrc, "batch-interpose.toml")

	gates := &exitGates{fail: true, exit: 1}
	e := testEngine()
	e.Gates = gates

	implementID := stepIDInRun(t, conn, runID, "implement@0")
	parkThroughFailingGate(t, conn, e, implementID)
	err := e.ResolveStepBatch(conn, implementID, ResolveOverridePass,
		"sandbox artifact", nowMS+1)
	testsupport.Must(t, err, "batch resolve: %v", err)

	// audit@0 fails the same gate with the same signature — but its threshold
	// interposes tribunal, so the cover is blocked and the step parks.
	auditID := stepIDInRun(t, conn, runID, "audit@0")
	parkThroughFailingGate(t, conn, e, auditID) // asserts the park itself

	routing := stepRoutingByID(t, conn, auditID)
	if !strings.Contains(routing, "tribunal") || !strings.Contains(routing, "DKT-470") {
		t.Errorf("audit@0 routing = %q, want the park reason to name the "+
			"interposed step and the DKT-470 block", routing)
	}
	grants, err := db.GateOverrideGrantsForRun(conn, runID)
	testsupport.Must(t, err, "reading grants: %v", err)
	if grants[0].CoveredSteps != 0 {
		t.Errorf("covered_steps = %d for a blocked cover, want 0", grants[0].CoveredSteps)
	}
	if got := eventKindCount(t, conn, runID, EventStepBatchOverridden); got != 0 {
		t.Errorf("%s events = %d for a blocked cover, want 0",
			EventStepBatchOverridden, got)
	}
}

// ---------------------------------------------------------------------------
// DKT-734 — the grant's reach ACROSS FIX-LOOP ROUNDS, and the audit trail that
// makes each application walk back to the operator.
// ---------------------------------------------------------------------------

// batchLoopRoundSrc is RUN-51's shape: the gated step is the LOOP step, so the
// steps a grant covers are minted by later fix-round loop entries and did not
// exist when the operator ruled. Every test above covers steps that were
// already expanded at activation, which is the easy half of "run-scoped".
const batchLoopRoundSrc = `
[pipeline]
name = "batch-loop-round"
version = 1

[match]
kind = ["task"]

[[step]]
name = "check"
executor = "check"
emits = "findings"
threshold = { "fix-loop" = "any(status == unmet)" }
max_fix_loops = 4

[[step]]
name = "fix"
executor = "fix"
emits = "change-summary"
loop = true
after_loop = "check"
gates = ["build"]
on_fail = "waiting-human"
`

// eventDetailsOfKind returns one run's events of a kind, in sequence order, as
// the `detail` each carries — the field `docket events list` prints as
// `detail=...`, and the one an auditor walks. eventKindCount cannot see it,
// which is why every assertion above could only count.
func eventDetailsOfKind(t *testing.T, conn *sql.DB, runID int, kind string) []string {
	t.Helper()
	rows, err := conn.Query(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq`,
		runID, kind)
	testsupport.Must(t, err, "reading %s events: %v", kind, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var data string
		testsupport.Must(t, rows.Scan(&data), "scanning a %s event", kind)
		var fields struct {
			Detail string `json:"detail"`
		}
		testsupport.Must(t, json.Unmarshal([]byte(data), &fields),
			"decoding a %s event's data", kind)
		out = append(out, fields.Detail)
	}
	testsupport.Must(t, rows.Err(), "iterating %s events: %v", kind, rows.Err())
	return out
}

// TestBatchGrantCoversLaterFixLoopRoundsSteps is DKT-734's question, answered
// in the engine rather than in prose: an operator grant minted on ONE round's
// parked step covers the steps LATER ROUNDS mint, because the grant's scope is
// the RUN and a fix round does not start a new one.
//
// This is BY DESIGN, not an escape: `gate_override_grants.run_id` is the whole
// scope rule (see GateOverrideGrant's doc comment), and a fix round mints new
// steps inside the same run. RUN-51 observed exactly this — one grant on
// `fix@7`, auto-passes on `fix@8` and `fix@9` — and read three authorizations
// where one standing authorization was spent three times.
//
// What keeps that honest is the signature match plus the ledger: the gates
// still run every round, a round whose failure differs still parks (the test
// below), and every application is its own `step-batch-overridden` event
// naming the grant id.
func TestBatchGrantCoversLaterFixLoopRoundsSteps(t *testing.T) {
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchLoopRoundSrc, "batch-loop-round.toml")

	gates := &exitGates{fail: true, exit: 2}
	e := testEngine()
	e.Gates = gates

	// Round 0's check finds an unmet criterion, which enters the loop and
	// mints `fix@1` — a step that did not exist at activation.
	driveFixtureRound(t, 0)
	claimAndComplete(t, conn, e, "check@0", "the round 0 assessment", unmetPayload)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: the unmet criterion must enter the loop and mint fix@1")
	}

	// `fix@1`'s gate fails and it parks. The operator rules the failure
	// environmental for the run. (Each round moves the stub tree, or DKT-340's
	// non-convergence guard parks the loop before a second round is minted.)
	driveFixtureRound(t, 1)
	fix1 := stepIDInRun(t, conn, runID, "fix@1")
	parkThroughFailingGate(t, conn, e, fix1)
	testsupport.Must(t, e.ResolveStepBatch(conn, fix1, ResolveOverridePass,
		"sandbox artifact, not a code defect", nowMS+1),
		"resolve fix@1 --as override-pass --batch")

	granted := eventDetailsOfKind(t, conn, runID, EventGateOverrideGranted)
	if len(granted) != 1 {
		t.Fatalf("%s events = %d, want exactly 1 — one operator ruling",
			EventGateOverrideGranted, len(granted))
	}
	grants, err := db.GateOverrideGrantsForRun(conn, runID)
	testsupport.Must(t, err, "reading grants: %v", err)
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(grants))
	}
	grantID := grants[0].ID
	// The minting event carries `gate#grantid fp=<signature>`, so the feed's
	// ruling names the grant row every later application will cite AND the
	// failure content that authority is bound to (DKT-1796).
	if want := fmt.Sprintf("build#%d fp=%s",
		grantID, shortFingerprint(grants[0].Fingerprint)); granted[0] != want {
		t.Errorf("%s data = %q, want %q — the ruling must name its grant row",
			EventGateOverrideGranted, granted[0], want)
	}
	if grants[0].OriginStepID != fix1 {
		t.Errorf("grant origin = %d, want fix@1 (%d)", grants[0].OriginStepID, fix1)
	}

	// Rounds 2 and 3: each is minted by its own loop entry, AFTER the ruling,
	// and each fails the same gate with the same signature.
	for ordinal := 2; ordinal <= 3; ordinal++ {
		prev := ordinal - 1
		claimAndComplete(t, conn, e,
			fmt.Sprintf("check@%d", prev),
			fmt.Sprintf("the round %d assessment", prev), unmetPayload)

		instance := fmt.Sprintf("fix@%d", ordinal)
		if !stepExists(t, conn, instance) {
			t.Fatalf("premise: round %d must mint %s", ordinal, instance)
		}
		driveFixtureRound(t, ordinal)
		stepID := stepIDInRun(t, conn, runID, instance)
		claim, err := ClaimStep(conn, stepID, ClaimOptions{
			Owner: "w", NowMS: nowMS + int64(ordinal)})
		testsupport.Must(t, err, "claim %s: %v", instance, err)
		testsupport.Must(t, e.CompleteStep(conn, stepID, CompleteOptions{
			Token: claim.Token, Artifact: []byte("the fix summary"),
			NowMS: nowMS + int64(ordinal),
		}), "complete %s", instance)

		step, err := db.GetStep(conn, stepID)
		testsupport.Must(t, err, "GetStep %s: %v", instance, err)
		if step.Status != db.StepDone {
			t.Fatalf("%s = %q after a covered failure, want %q — the grant's "+
				"scope is the RUN, and a fix round does not start a new one",
				instance, step.Status, db.StepDone)
		}
		routing := stepRoutingByID(t, conn, stepID)
		if !strings.HasPrefix(routing, RoutingPass) ||
			!strings.Contains(routing, strconv.Itoa(grantID)) {
			t.Errorf("%s routing = %q, want a %q naming grant %d",
				instance, routing, RoutingPass, grantID)
		}
	}

	// STILL exactly one authorization: later rounds spend the standing grant,
	// they do not mint new ones.
	if got := eventKindCount(t, conn, runID, EventGateOverrideGranted); got != 1 {
		t.Errorf("%s events = %d, want 1 — a covered round must not mint "+
			"authority the operator never granted", EventGateOverrideGranted, got)
	}
	// And each application is its own event, naming the grant it spent, so the
	// feed distinguishes "authorized three times" from "one authorization
	// spent three times".
	spent := eventDetailsOfKind(t, conn, runID, EventStepBatchOverridden)
	spentData := fmt.Sprintf("%d fp=%s",
		grantID, shortFingerprint(grants[0].Fingerprint))
	want := []string{spentData, spentData}
	if len(spent) != len(want) {
		t.Fatalf("%s events = %d, want %d — one per covered step",
			EventStepBatchOverridden, len(spent), len(want))
	}
	for i, data := range spent {
		if data != want[i] {
			t.Errorf("%s[%d] data = %q, want %q — every application must walk "+
				"back to the grant, and the grant to the operator's ruling",
				EventStepBatchOverridden, i, data, want[i])
		}
	}
	grants, err = db.GateOverrideGrantsForRun(conn, runID)
	testsupport.Must(t, err, "re-reading grants: %v", err)
	if grants[0].CoveredSteps != 2 {
		t.Errorf("covered_steps = %d, want 2 — the counter is the grant row's "+
			"own record of how far one ruling reached", grants[0].CoveredSteps)
	}
}

// TestBatchGrantParksALaterRoundWithADifferentSignature is the other half of
// DKT-734's answer, and the reason the run-scoped reach is safe: a standing
// grant does not make the loop step immune. A later round whose gate fails
// DIFFERENTLY parks for a fresh operator decision, exactly as an ungranted
// failure would.
func TestBatchGrantParksALaterRoundWithADifferentSignature(t *testing.T) {
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchLoopRoundSrc, "batch-loop-round.toml")

	gates := &exitGates{fail: true, exit: 2}
	e := testEngine()
	e.Gates = gates

	driveFixtureRound(t, 0)
	claimAndComplete(t, conn, e, "check@0", "the round 0 assessment", unmetPayload)
	driveFixtureRound(t, 1)
	fix1 := stepIDInRun(t, conn, runID, "fix@1")
	parkThroughFailingGate(t, conn, e, fix1)
	testsupport.Must(t, e.ResolveStepBatch(conn, fix1, ResolveOverridePass,
		"sandbox artifact", nowMS+1), "batch resolve")

	// The next round's gate fails with a DIFFERENT exit: a failure the
	// operator has not ruled on.
	gates.mu.Lock()
	gates.exit = 3
	gates.mu.Unlock()

	claimAndComplete(t, conn, e, "check@1", "the round 1 assessment", unmetPayload)
	driveFixtureRound(t, 2)
	fix2 := stepIDInRun(t, conn, runID, "fix@2")
	parkThroughFailingGate(t, conn, e, fix2) // asserts the park itself

	grants, err := db.GateOverrideGrantsForRun(conn, runID)
	testsupport.Must(t, err, "reading grants: %v", err)
	if grants[0].CoveredSteps != 0 {
		t.Errorf("covered_steps = %d after a differently-failing round, want 0",
			grants[0].CoveredSteps)
	}
	if got := eventKindCount(t, conn, runID, EventStepBatchOverridden); got != 0 {
		t.Errorf("%s events = %d, want 0 — a new failure signature parks for a "+
			"fresh decision no matter how many rounds a grant has covered",
			EventStepBatchOverridden, got)
	}
}
