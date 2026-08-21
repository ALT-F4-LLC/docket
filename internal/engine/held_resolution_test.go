package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// Resolution CONTENT on held clusters.
//
// DKT-84: `operator_resolved` used to be the whole message — the fixer
// downstream was told a decision existed and given nothing to act on, because
// the operator's note lived on the held step's routing record and never
// reached the resolved payload.
//
// DKT-42: the operator's only moves were "accept the number the judges could
// not agree on" and "escalate everything". A structured correction —
// validated against the pinned schema's declared enum, never parsed out of
// prose — is the third move.

// resolvedElements reads the ROUTING step's latest artifact payload.
func resolvedElements(t *testing.T, conn *sql.DB) []map[string]any {
	t.Helper()
	payloads := artifactPayloads(t, conn,
		stepIDByInstance(t, conn, "reconcile@0"))
	if len(payloads) == 0 {
		t.Fatal("the routing step has no artifacts")
	}
	return payloads[len(payloads)-1]
}

// TestApprovedClusterCarriesDecisionContent is DKT-84's headline: the note
// travels with the flag, per cluster, and clusters the decision did not
// address carry nothing.
func TestApprovedClusterCarriesDecisionContent(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToReconcile(t, conn, e, multiClusterPayload)
	held := heldInstances(t, conn)
	if len(held) != 2 {
		t.Fatalf("expected 2 held steps, got %v", held)
	}

	err := e.DecideStepValue(conn, stepIDByInstance(t, conn, held[0]),
		true, "take the larger fix", "", nowMS)
	testsupport.Must(t, err, "approving %s: %v", held[0], err)

	elements := resolvedElements(t, conn)
	if got, _ := elements[0][KeyOperatorNote].(string); got != "take the larger fix" {
		t.Errorf("element 0 operator_note = %q; the decision's content must "+
			"travel with operator_resolved (DKT-84)", got)
	}
	if resolved, _ := elements[0][KeyOperatorResolved].(bool); !resolved {
		t.Error("element 0 is not marked operator_resolved")
	}
	// The undecided cluster carries neither the flag nor someone else's note.
	if _, ok := elements[1][KeyOperatorNote]; ok {
		t.Error("element 1 carries a note from a decision about element 0")
	}
	if resolved, _ := elements[1][KeyOperatorResolved].(bool); resolved {
		t.Error("element 1 reads operator_resolved with nobody having decided it")
	}
}

// TestSequentialDecisionsPreserveEachOther: the second cluster's approval
// starts from the latest artifact, so the first cluster's content survives.
func TestSequentialDecisionsPreserveEachOther(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToReconcile(t, conn, e, multiClusterPayload)
	held := heldInstances(t, conn)

	err := e.DecideStepValue(conn, stepIDByInstance(t, conn, held[0]),
		true, "first answer", "", nowMS)
	testsupport.Must(t, err, "approving %s: %v", held[0], err)
	err = e.DecideStepValue(conn, stepIDByInstance(t, conn, held[1]),
		true, "second answer", "", nowMS)
	testsupport.Must(t, err, "approving %s: %v", held[1], err)

	elements := resolvedElements(t, conn)
	if got, _ := elements[0][KeyOperatorNote].(string); got != "first answer" {
		t.Errorf("element 0 note = %q after the second decision; earlier "+
			"content must survive later artifacts", got)
	}
	if got, _ := elements[1][KeyOperatorNote].(string); got != "second answer" {
		t.Errorf("element 1 note = %q", got)
	}
}

// TestApproveValueCorrectsTheAggregatedField is DKT-42's third move: the
// corrected value lands on the field itself — so thresholds and downstream
// inputs route on the number the operator endorsed — with the computed value
// it replaced kept in the trail.
func TestApproveValueCorrectsTheAggregatedField(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToReconcile(t, conn, e, multiClusterPayload)
	held := heldInstances(t, conn)

	// C-1 is {low, blocker}: the even-count rule medians to "low". The
	// operator's judgment is "high".
	err := e.DecideStepValue(conn, stepIDByInstance(t, conn, held[0]),
		true, "the blocker report is right", "high", nowMS)
	testsupport.Must(t, err, "approving with --value: %v", err)

	elements := resolvedElements(t, conn)
	if got, _ := elements[0]["severity"].(string); got != "high" {
		t.Errorf("severity = %q, want the operator's corrected value", got)
	}
	if got, _ := elements[0][KeyOperatorSetFrom].(string); got != "low" {
		t.Errorf("operator_set_from = %q, want the computed value it replaced", got)
	}
	if got, _ := elements[0][KeyOperatorNote].(string); got != "the blocker report is right" {
		t.Errorf("operator_note = %q", got)
	}
}

// TestApproveValueRefusals: the value is validated against the pinned
// schema's declared enum BEFORE anything is written, and it accompanies
// approve only.
func TestApproveValueRefusals(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToReconcile(t, conn, e, multiClusterPayload)
	held := heldInstances(t, conn)
	heldID := stepIDByInstance(t, conn, held[0])

	// A value outside the declared membership set refuses, naming what the
	// schema does declare, and records no decision.
	err := e.DecideStepValue(conn, heldID, true, "", "urgent", nowMS)
	if err == nil {
		t.Fatal("a value outside the declared enum was accepted")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}
	if !strings.Contains(err.Error(), "blocker") {
		t.Errorf("refusal does not list the declared values: %v", err)
	}
	if status := stepStatus(t, conn, held[0]); db.StepTerminal(status) {
		t.Errorf("a refused value still decided the step (status %q)", status)
	}

	// A value on reject refuses: reject records no artifact for the cluster,
	// so there is nothing for a value to land on.
	err = e.DecideStepValue(conn, heldID, false, "", "high", nowMS)
	if err == nil {
		t.Fatal("--value on reject was accepted")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}
}
