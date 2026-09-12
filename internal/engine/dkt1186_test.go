package engine

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// DKT-1186 — a gate child learns WHICH STEP it is running for.
//
// Before this, the child environment named the gate, the repo, the issue, the
// scope and the network, and nothing that identified the step. A gate needing
// an artifact an earlier step of the same issue produced therefore had to
// reconstruct its own identity from the outside: list the issue's steps, pick
// itself out by a hardcoded instance-name convention, parse that listing's JSON
// shape, and only then ask for the artifact. Three couplings to things the
// engine may change freely, and all three fail silently when it does.
//
// DOCKET_STEP is the whole fix: `STEP-N`, the identity `docket step context`
// and `docket step artifacts` already take. The contract asserted here is the
// same one DOCKET_SCOPE and DOCKET_GATE_BASE keep — set when docket knows,
// ABSENT when it does not, never an invented value.

// TestGateChildSeesDocketStep is the runner half, with a REAL spawn: printenv
// itself is the witness, so the assertion is about the environment the child
// actually received rather than about a struct field.
func TestGateChildSeesDocketStep(t *testing.T) {
	execRoot, _, _, _ := gitFixture(t)
	printenvPath, err := exec.LookPath("printenv")
	if err != nil {
		t.Skip("printenv is not installed")
	}

	argv := []string{printenvPath, "DOCKET_STEP"}
	runner := NewExecRunner(testRepoPaths(execRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "sdet-abuse", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(execRoot),
	})

	ex, err := runner.Execute(context.Background(),
		GateSpec{Name: "sdet-abuse"}, StepContext{StepID: 2939})
	testsupport.Must(t, err, "running the gate with a step id: %v", err)
	if ex.Verdict != VerdictPass {
		t.Fatalf("gate verdict = %q (reason %q), want %q — printenv found no "+
			"DOCKET_STEP in the child environment",
			ex.Verdict, ex.Results[0].Reason, VerdictPass)
	}
	if got := strings.TrimSpace(ex.Results[0].Output); got != "STEP-2939" {
		t.Errorf("the child saw DOCKET_STEP=%q, want the step's rendered "+
			"reference STEP-2939 — the form `docket step artifacts` takes", got)
	}

	// No step in the context: the variable must be ABSENT, not `STEP-0`, which
	// is well-formed and resolves to nothing. printenv exiting 1 having printed
	// nothing is the child's own proof of absence.
	ex, err = runner.Execute(context.Background(),
		GateSpec{Name: "sdet-abuse"}, StepContext{})
	testsupport.Must(t, err, "running the gate with no step id: %v", err)
	if got := strings.TrimSpace(ex.Results[0].Output); got != "" {
		t.Errorf("the child saw DOCKET_STEP=%q with no step in the context, "+
			"want the variable unset", got)
	}
	if ex.Verdict != VerdictFail {
		t.Errorf("gate verdict = %q, want %q — printenv must exit 1 when the "+
			"variable is genuinely unset", ex.Verdict, VerdictFail)
	}
}

// TestStepRefRendersOnlyRealSteps pins the guard directly, because it is the
// one place the "absent, never invented" encoding is decided.
func TestStepRefRendersOnlyRealSteps(t *testing.T) {
	if got := stepRef(0); got != "" {
		t.Errorf("stepRef(0) = %q, want \"\" — STEP-0 names no row, and a gate "+
			"handed one fails inside its own tooling instead of on a condition "+
			"it can test", got)
	}
	if got := stepRef(-1); got != "" {
		t.Errorf("stepRef(-1) = %q, want \"\"", got)
	}
	if got := stepRef(7); got != model.FormatStepID(7) {
		t.Errorf("stepRef(7) = %q, want %q", got, model.FormatStepID(7))
	}
}

// TestCompletionGatesCarryTheStepID is the saga half: every completion gate of
// a step is dispatched with that step's own id, so the export above has
// something true to render.
func TestCompletionGatesCarryTheStepID(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	e := testEngine()
	captured := &baseCapture{seen: map[string]StepContext{}}
	e.Gates = captured

	want := stepIDByInstance(t, conn, "implement@0")
	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

	if len(captured.seen) == 0 {
		t.Fatal("no completion gate was dispatched")
	}
	for gate, sc := range captured.seen {
		if sc.StepID != want {
			t.Errorf("gate %s dispatched with StepID %d, want %d — the step it "+
				"is judging", gate, sc.StepID, want)
		}
	}
}

// TestPreGatesCarryTheStepID is the pre-claim half, and the one that matters
// most for the reported case: a pre-gate runs BEFORE the claim hands the
// context bundle over, so asking the engine by reference is its ONLY route to
// the artifacts an earlier step of the chain produced.
func TestPreGatesCarryTheStepID(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	e := testEngine()
	stepID := advanceToVerify(t, conn, e)

	captured := &baseCapture{seen: map[string]StepContext{}}
	e.Gates = captured

	_, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: "w", NowMS: nowMS,
	})
	testsupport.Must(t, err, "claiming the pre-gated step: %v", err)

	sc, ok := captured.seen["ac-commands"]
	if !ok {
		t.Fatalf("the ac-commands pre-gate was not dispatched; saw %v", captured.seen)
	}
	if sc.StepID != stepID {
		t.Errorf("the pre-gate was dispatched with StepID %d, want %d — the "+
			"step being claimed", sc.StepID, stepID)
	}
}
