package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-982 — the failed gate a completion parks on has a NAME, and the engine
// has always known it at the moment the recording verb prints.
//
// `step record` printed `✔ Completed STEP-N (waiting-human)`: no gate, no
// verdict, a success glyph on a park. The executor read it as a pass and
// reported the wave green; the conductor reconstructed the real outcome from
// later engine reads. FailedGates is the accessor that was missing — these
// tests pin that it reports the SAME rows the routing decided on.

// TestFailedGatesNamesTheGateThatParkedTheStep drives a REAL saga: a gate that
// exits 2 parks the step, and the accessor the CLI prints from names that gate
// and that exit code.
//
// It goes through CompleteStep rather than inserting rows because the claim in
// the issue is about what is knowable AT PRINT TIME — a test over hand-written
// rows would prove the reduction and leave the timing unproven.
func TestFailedGatesNamesTheGateThatParkedTheStep(t *testing.T) {
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchOverrideSrc, "dkt982.toml")

	e := testEngine()
	e.Gates = &exitGates{fail: true, exit: 2}

	stepID := stepIDInRun(t, conn, runID, "implement@0")
	parkThroughFailingGate(t, conn, e, stepID)

	failed, err := FailedGates(conn, stepID)
	testsupport.Must(t, err, "FailedGates: %v", err)

	if len(failed) != 1 {
		t.Fatalf("FailedGates = %d rows, want 1 — the park has exactly one cause "+
			"and the verb that prints it must be able to name it", len(failed))
	}
	if failed[0].Gate != "build" {
		t.Errorf("gate = %q, want %q", failed[0].Gate, "build")
	}
	if failed[0].Verdict != db.GateVerdictFail {
		t.Errorf("verdict = %q, want %q", failed[0].Verdict, db.GateVerdictFail)
	}
	if failed[0].Exit == nil || *failed[0].Exit != 2 {
		t.Errorf("exit = %v, want 2 — the exit code is the half of the report an "+
			"executor can act on", failed[0].Exit)
	}
}

// TestFailedGatesIsSilentWhenEveryGatePasses is the other half of the contract,
// and the one that protects the unchanged success line: a record whose gates
// all passed must give the printer nothing to say.
func TestFailedGatesIsSilentWhenEveryGatePasses(t *testing.T) {
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchOverrideSrc, "dkt982-pass.toml")

	e := testEngine()
	e.Gates = &exitGates{}

	stepID := stepIDInRun(t, conn, runID, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)

	failed, err := FailedGates(conn, stepID)
	testsupport.Must(t, err, "FailedGates: %v", err)
	if len(failed) != 0 {
		t.Errorf("FailedGates = %v over an all-passing record, want none", failed)
	}
}

// TestFailedGatesReportsWhatTheRoutingDecidedOn walks the reduction's edges over
// hand-built rows: the last attempt of a flaky re-run wins, a pre-gate is not a
// judgment of the step, `unmatched` and `skipped` are failures with no exit
// code, and the order is stable.
//
// The invariant at the end is the point of the whole exercise. A REPORT built
// from a second reduction over the same table is how a printed line comes to
// contradict the routing beside it — which is the defect class DKT-982 is — so
// the test asserts the two agree rather than asserting each in isolation.
func TestFailedGatesReportsWhatTheRoutingDecidedOn(t *testing.T) {
	exit := func(n int) *int { return &n }

	rows := []db.GateResultRow{
		// A gate declared flaky: failed, then passed. F4 makes the last attempt
		// the one that routes, so this must NOT be reported.
		{Gate: "flaky", Ordinal: 0, Verdict: db.GateVerdictFail, Exit: exit(1)},
		{Gate: "flaky", Ordinal: 1, Verdict: db.GateVerdictPass, Exit: exit(0)},
		// A pre-gate that failed. PG4: an input to the step, not a judgment of
		// it, and it routed nothing.
		{Gate: "pre-scan", Ordinal: 0, Verdict: db.GateVerdictFail, Exit: exit(9), Pre: true},
		{Gate: "build", Ordinal: 0, Verdict: db.GateVerdictPass, Exit: exit(0)},
		{Gate: "self-hygiene", Ordinal: 0, Verdict: db.GateVerdictFail, Exit: exit(2)},
		// Never ran: no exit code exists, and 0 would read as a pass (T11).
		{Gate: "audit", Ordinal: 0, Verdict: db.GateVerdictUnmatched,
			Reason: "no trust entry matched"},
		{Gate: "coverage", Ordinal: 0, Verdict: db.GateVerdictSkipped,
			Reason: "the tree was gone at spawn time"},
	}

	failed := failedGatesOverRows(rows)

	var names []string
	for _, g := range failed {
		names = append(names, g.Gate)
	}
	want := []string{"audit", "coverage", "self-hygiene"}
	if len(names) != len(want) {
		t.Fatalf("failed gates = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("failed gates = %v, want %v (sorted, so one run's sentence is "+
				"every run's)", names, want)
		}
	}

	for _, g := range failed {
		switch g.Gate {
		case "self-hygiene":
			if g.Exit == nil || *g.Exit != 2 {
				t.Errorf("self-hygiene exit = %v, want 2", g.Exit)
			}
		case "audit", "coverage":
			if g.Exit != nil {
				t.Errorf("%s exit = %d, want none — nothing ran, and a zero exit "+
					"reads as a pass", g.Gate, *g.Exit)
			}
			if g.Reason == "" {
				t.Errorf("%s carries no reason; the recorded row had one", g.Gate)
			}
		}
	}

	// The invariant: the printer's rows and the router's verdict come from one
	// reduction, so they cannot disagree.
	verdict, _ := verdictOverRows(rows)
	if (verdict == VerdictFail) != (len(failed) > 0) {
		t.Errorf("verdict = %q with %d failed gates — the report and the routing "+
			"disagree about the same rows", verdict, len(failed))
	}
}
