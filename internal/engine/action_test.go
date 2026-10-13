package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The action seam's three asserted consequences, plus the register-time
// rule that `params.output` is mandatory.

// TestActionRecordsARealArtifact is the seam's first consequence, now that the
// computation behind it is real: the artifact is present, addressable, and of
// the DECLARED kind — which is what makes downstream `inputs` resolution against
// it work, and is exactly what the fixture's `fix` step needs.
//
// The stub's two markers are gone with it: the payload is the PLAIN
// ARRAY, and `artifacts.stub` reads 0. What replaces them as the audit record is
// an `action_results` row, which says more than a boolean could — that core
// computed this itself, and that it passed.
func TestActionRecordsARealArtifact(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	// Drive the fixture to `reconcile`, the action step.
	claimAndComplete(t, conn, e, "implement@0", "summary", "")
	for i := range 4 {
		claimAndComplete(t, conn, e, "review@0#"+strconv.Itoa(i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", "")
	driveAction(t, conn, e, "reconcile@0")

	if got := stepStatus(t, conn, "reconcile@0"); got != db.StepDone {
		t.Fatalf("reconcile@0 status = %q, want %q", got, db.StepDone)
	}

	artifacts, err := db.ListRunArtifacts(conn, run.ID)
	testsupport.Must(t, err, "ListRunArtifacts: %v", err)

	reconcileID := stepIDByInstance(t, conn, "reconcile@0")
	var found *db.Artifact
	for _, a := range artifacts {
		if a.StepID == reconcileID && a.Kind == "findings" {
			found = a
		}
	}
	if found == nil {
		t.Fatalf("reconcile@0 recorded no artifact of kind `findings` "+
			"(params.output); it recorded %d artifacts overall", len(artifacts))
	}

	// The PLAIN ARRAY. Nothing this stage writes carries the old stub wrapper.
	var payload []map[string]any
	err = json.Unmarshal([]byte(found.Payload), &payload)
	testsupport.Must(t, err, "the action payload is not a plain array: %v (payload = %s)",
		err, found.Payload)

	if strings.Contains(found.Payload, `"stub"`) {
		t.Errorf("a real action artifact carries the retired stub wrapper: %s",
			found.Payload)
	}
	if found.Stub {
		t.Error("artifacts.stub is set on a computation that actually ran; the " +
			"column exists for MIGRATED stub rows and nothing writes it now")
	}

	// The audit record, in place of the marker it retired.
	rows, err := db.ActionResultsForStep(conn, reconcileID)
	testsupport.Must(t, err, "ActionResultsForStep: %v", err)
	if len(rows) != 1 {
		t.Fatalf("recorded %d action results, want one per attempt: %+v", len(rows), rows)
	}
	if !rows[0].Builtin || rows[0].Verdict != db.ActionVerdictPass {
		t.Errorf("the recorded result is %+v, want a passing builtin", rows[0])
	}
	if rows[0].Argv != nil || rows[0].Exit != nil {
		t.Error("the builtin recorded an argv or an exit; it spawned nothing")
	}
	_ = issue
}

// TestReconcileRoutesPassByT4ShortCircuit asserts the empty-set short-circuit
// DIRECTLY, so a future change to it breaks THIS test rather than making the
// whole phase-4 loop fail mysteriously.
//
// The fixture's `reconcile` carries `threshold = { "fix-loop" = "any(severity
// >= high)" }`. Its ORDER IS NOW LIVE — `findings@1` declares it — so the
// comparison would evaluate if there were anything to evaluate it over. There
// is not: `synthesize` recorded no payload here, so the aggregate reduces zero
// clusters, `any` short-circuits to false over the empty set before the `>=`
// is attempted, and "no match ⇒ pass" applies.
//
// That is the case an action step with no result set must still route
// through, and it is why the aggregate's empty output is not a failure.
func TestReconcileRoutesPassByT4ShortCircuit(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "summary", "")
	for i := range 4 {
		claimAndComplete(t, conn, e, "review@0#"+strconv.Itoa(i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", "")
	driveAction(t, conn, e, "reconcile@0")

	step, err := db.GetStep(conn, stepIDByInstance(t, conn, "reconcile@0"))
	testsupport.Must(t, err, "GetStep: %v", err)

	if step.Routing != RoutingPass {
		t.Errorf("reconcile@0 routed %q, want %q.\n\n"+
			"Its threshold is an ORDERED comparison (any(severity >= high)) over "+
			"an EMPTY result set, so `any` short-circuits to false before the "+
			"`>=` is attempted. If this fails, either the empty-set "+
			"short-circuit no longer precedes the comparison or the aggregate "+
			"no longer yields an empty payload over no clusters — and the QA "+
			"loop cannot reach `verify`.",
			step.Routing, RoutingPass)
	}
	if step.Status != db.StepWaitingHuman && step.Status != db.StepDone {
		t.Errorf("unexpected status %q", step.Status)
	}
	if step.Status == db.StepWaitingHuman {
		t.Error("reconcile@0 PARKED; the engine guessed nothing but also " +
			"short-circuited nothing")
	}
}

// TestBuiltinSpawnsNoProcess: a builtin SPAWNS NOTHING.
//
// The rule matters structurally rather than as an optimization: "no
// subprocess ever executes inside a transaction" holds TRIVIALLY for a builtin,
// so the action stage's position — outside the routing transaction, exactly
// where it has always been — needs no argument for the aggregate path.
func TestBuiltinSpawnsNoProcess(t *testing.T) {
	before := childProcessCount(t)

	runner := &ExecActionRunner{
		RepoRoot: t.TempDir(),
		LoadStore: func() (*trust.Store, error) {
			t.Error("the builtin consulted the trust store; resolution is " +
				"builtin-first and a builtin never looks one up")
			return &trust.Store{}, nil
		},
	}

	for range 20 {
		result, err := runner.Run(context.Background(), ActionSpec{
			Name: workflow.ActionAggregate, Output: "findings",
			Params: map[string]any{
				"field": "severity", "method": "median", "output": "findings",
			},
			Inputs: []map[string]any{{"severity": "low"}},
			Order:  severityOrder,
		}, StepContext{Instance: "reconcile@0", RunID: 1, IssueID: 1})
		testsupport.Must(t, err, "Run: %v", err)
		if result.Failed {
			t.Fatalf("the builtin failed: %s", result.Reason)
		}
		if result.Kind != "findings" {
			t.Errorf("kind = %q, want params.output `findings`", result.Kind)
		}
		if len(result.Results) != 1 || !result.Results[0].Builtin {
			t.Errorf("results = %+v, want one row marked builtin", result.Results)
		}
		if result.Results[0].Argv != nil || result.Results[0].Exit != nil {
			t.Error("the builtin recorded an argv or an exit; it spawned nothing")
		}
	}

	if after := childProcessCount(t); after > before {
		t.Errorf("the process count rose %d -> %d: the builtin spawned something",
			before, after)
	}
}

// TestBuiltinRouteAtSplitsTheOutputFromTheRecord is DKT-593 at the runner
// seam: the ARTIFACT payload — the wire the threshold evaluates and every
// downstream `inputs` reader consumes — carries only the clusters at or above
// the floor, while the below-floor clusters land, fully reduced, in the
// builtin's own `action_results` row. Routed, not erased: the record is
// attributed, and the loop is not fed.
func TestBuiltinRouteAtSplitsTheOutputFromTheRecord(t *testing.T) {
	runner := &ExecActionRunner{
		RepoRoot: t.TempDir(),
		LoadStore: func() (*trust.Store, error) {
			t.Error("the builtin consulted the trust store")
			return &trust.Store{}, nil
		},
	}

	result, err := runner.Run(context.Background(), ActionSpec{
		Name: workflow.ActionAggregate, Output: "findings",
		Params: map[string]any{
			"field": "severity", "method": "median", "output": "findings",
			"route_at": "high",
		},
		Inputs: []map[string]any{
			{"severity": "low", "id": "A"},
			{"severity": "blocker", "id": "B"},
		},
		Order: severityOrder,
	}, StepContext{Instance: "reconcile@0", RunID: 1, IssueID: 1})
	testsupport.Must(t, err, "Run: %v", err)
	if result.Failed {
		t.Fatalf("the builtin failed: %s", result.Reason)
	}

	var emitted []map[string]any
	testsupport.Must(t, json.Unmarshal([]byte(result.Payload), &emitted),
		"decoding the payload: %v", err)
	if len(emitted) != 1 || emitted[0]["id"] != "B" {
		t.Errorf("the artifact payload is %s, want only cluster B — the "+
			"below-floor cluster must not reach the loop output", result.Payload)
	}

	if len(result.Results) != 1 {
		t.Fatalf("results = %+v, want the builtin's one row", result.Results)
	}
	var recorded []map[string]any
	testsupport.Must(t, json.Unmarshal([]byte(result.Results[0].Output), &recorded),
		"decoding the recorded clusters: %v", err)
	if len(recorded) != 1 || recorded[0]["id"] != "A" || recorded[0]["severity"] != "low" {
		t.Errorf("the action row records %s, want the reduced below-floor "+
			"cluster A", result.Results[0].Output)
	}

	if !strings.Contains(result.Body, "2 cluster(s) reduced") ||
		!strings.Contains(result.Body, "1 below the `route_at` floor") {
		t.Errorf("the body does not account for both halves: %q", result.Body)
	}
}

// TestBuiltinWithoutRouteAtRecordsNothingInItsRow is the absent case at the
// same seam: no floor, an empty audit Output, and a body with no floor
// sentence — byte-for-byte the pre-route_at builtin.
func TestBuiltinWithoutRouteAtRecordsNothingInItsRow(t *testing.T) {
	runner := &ExecActionRunner{
		RepoRoot:  t.TempDir(),
		LoadStore: func() (*trust.Store, error) { return &trust.Store{}, nil },
	}
	result, err := runner.Run(context.Background(), ActionSpec{
		Name: workflow.ActionAggregate, Output: "findings",
		Params: map[string]any{
			"field": "severity", "method": "median", "output": "findings",
		},
		Inputs: []map[string]any{{"severity": "low", "id": "A"}},
		Order:  severityOrder,
	}, StepContext{Instance: "reconcile@0", RunID: 1, IssueID: 1})
	testsupport.Must(t, err, "Run: %v", err)
	if result.Failed {
		t.Fatalf("the builtin failed: %s", result.Reason)
	}
	if result.Results[0].Output != "" {
		t.Errorf("the action row carries %q; with no route_at there is nothing "+
			"to record", result.Results[0].Output)
	}
	if strings.Contains(result.Body, "route_at") {
		t.Errorf("the body mentions a floor nobody declared: %q", result.Body)
	}
	if result.Payload != `[{"held":false,"id":"A","members":["low"],`+
		`"operator_resolved":false,"severity":"low"}]` {
		t.Errorf("the absent case's payload changed shape: %s", result.Payload)
	}
}

// childProcessCount counts this process's children, so a spawn is observable.
func childProcessCount(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("ps", "-o", "ppid=", "-A").Output()
	if err != nil {
		t.Skipf("cannot enumerate processes on this platform: %v", err)
	}
	pid := strconv.Itoa(os.Getpid())
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == pid {
			n++
		}
	}
	return n
}

// TestActionWithoutOutputFailsRegister is the register-time rule:
// `params.output` is the step's PRODUCED KIND, so an action step
// without it can never satisfy a downstream consumer's `inputs`.
func TestActionWithoutOutputFailsRegister(t *testing.T) {
	const src = `
[pipeline]
name = "outputless"
version = 1

[match]
kind = ["task"]

[[step]]
name = "compute"
after = []
action = "aggregate"
params = { field = "severity" }
`
	_, err := workflow.Load([]byte(src))
	if err == nil {
		t.Fatal("an action step without params.output registered; it produces no " +
			"kind, so no downstream consumer could ever resolve an input from it")
	}
	if !strings.Contains(err.Error(), "output") {
		t.Errorf("the refusal does not name `output`: %v", err)
	}
}

// TestActionSeamMirrorsTheGateSeam pins that the two seams have the same shape,
// which is what makes "the real runner is one constructor call" true for
// actions as it is for gates.
func TestActionSeamMirrorsTheGateSeam(t *testing.T) {
	e := NewEngine()

	// BOTH SEAMS ARE NOW REAL. The gate runner and the action runner were both
	// swapped, and the shape being mirrored is what matters: both are
	// interface fields assigned in one constructor, which is the "one
	// constructor call" property this test exists to pin.
	if _, ok := e.Gates.(*ExecRunner); !ok {
		t.Errorf("the gate runner is %T, want *ExecRunner", e.Gates)
	}
	if _, ok := e.Actions.(*ExecActionRunner); !ok {
		t.Errorf("the action runner is %T, want *ExecActionRunner", e.Actions)
	}

	// Both are swappable by assignment — the "one constructor call" property.
	e.Actions = recordingRunner{}
	result, err := e.Actions.Run(context.Background(),
		ActionSpec{Name: "x", Output: "k"}, StepContext{})
	testsupport.Must(t, err, "swapped runner: %v", err)
	if result.Payload != `[{"v":1}]` {
		t.Errorf("the swapped runner's payload did not reach the caller: %q",
			result.Payload)
	}
}

// recordingRunner is a non-stub runner, standing in for the real one.
type recordingRunner struct{}

func (recordingRunner) Run(context.Context, ActionSpec, StepContext) (ActionResult, error) {
	return ActionResult{Kind: "k", Body: "computed", Payload: `[{"v":1}]`}, nil
}
