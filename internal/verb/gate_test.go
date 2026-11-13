package verb_test

import (
	"context"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// humanGateWorkflow's one step is a human gate, which no one has decided.
const humanGateWorkflow = `
[pipeline]
name = "unit-human-gate"
version = 1
[match]
kind = ["task"]
[[step]]
name = "first"
after = []
type = "human"
on_fail = "skip"
`

func TestGateStatusReportsAnUndecidedHumanGate(t *testing.T) {
	st := openStore(t)
	id := activatedStepOf(t, st, humanGateWorkflow)

	res, err := verb.GateStatus(context.Background(), projectEnv(st, "", false),
		verb.GateStatusInput{Step: model.FormatStepID(id)})
	if err != nil {
		t.Fatalf("GateStatus: %v", err)
	}
	if res.Outcome != engine.GateOutcomeOpen {
		t.Errorf("outcome = %q, want %q", res.Outcome, engine.GateOutcomeOpen)
	}
	if res.StepStatus == "" {
		t.Error("step status is empty, want the step's effective status")
	}
	if res.Proposal != "" || res.Tally != nil || res.Seats != nil {
		t.Errorf("a human gate reported vote state: %+v", res.GateStatusResult)
	}
}

func TestGateStatusRefusesAStepThatIsNotAGate(t *testing.T) {
	st := openStore(t)
	id := activatedStep(t, st)

	_, err := verb.GateStatus(context.Background(), projectEnv(st, "", false),
		verb.GateStatusInput{Step: model.FormatStepID(id)})
	requireCode(t, err, output.ErrValidation)
}

func TestGateStatusRefusesAMalformedOrMissingStep(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)

	_, err := verb.GateStatus(context.Background(), env, verb.GateStatusInput{Step: "not-a-step"})
	requireCode(t, err, output.ErrValidation)

	_, err = verb.GateStatus(context.Background(), env, verb.GateStatusInput{Step: "STEP-999"})
	requireCode(t, err, output.ErrNotFound)
}

func TestGateStatusNeverRegistersTheCaller(t *testing.T) {
	st := openStore(t)
	id := activatedStepOf(t, st, humanGateWorkflow)
	const identity = "/work/anchored-repo"

	if _, err := verb.GateStatus(context.Background(), projectEnv(st, identity, true),
		verb.GateStatusInput{Step: model.FormatStepID(id)}); err != nil {
		t.Fatalf("GateStatus: %v", err)
	}
	requireUnregistered(t, st, identity)
}
