package verb_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

const stepWorkflow = `
[pipeline]
name = "unit-step"
version = 1
[match]
kind = ["task"]
[[step]]
name = "first"
after = []
executor = "someone"
emits = "result"
max_attempts = 3
`

// activatedStep activates a one-step run in the default project and returns
// its executor step's id. DOCKET_PATH is pinned to an empty store, so
// activation does not read the operator's or the repository's shipped
// workflows.
func activatedStep(t *testing.T, st store.Store) int {
	t.Helper()
	t.Setenv("DOCKET_PATH", filepath.Join(t.TempDir(), ".docket"))
	def, err := workflow.Load([]byte(stepWorkflow))
	if err != nil {
		t.Fatalf("workflow.Load: %v", err)
	}
	parsed, err := workflow.Canonical(def)
	if err != nil {
		t.Fatalf("workflow.Canonical: %v", err)
	}
	if _, _, err := st.InsertWorkflow(&model.Workflow{
		ProjectID: store.DefaultProjectID,
		Name:      def.Pipeline.Name, Version: def.Pipeline.Version,
		SourceSHA256: workflow.SHA256([]byte(stepWorkflow)),
		Body:         stepWorkflow, Parsed: string(parsed),
	}, model.NowMS()); err != nil {
		t.Fatalf("InsertWorkflow: %v", err)
	}

	issue := createIssue(t, st)
	run, _, err := st.InsertRunWithContextIdempotent(
		store.DefaultProjectID, "", 0, model.NowMS(), store.RunContext{}, "", issue)
	if err != nil {
		t.Fatalf("InsertRunWithContextIdempotent: %v", err)
	}
	if _, err := engine.ActivateStore(st, run.ID, engine.ActivateOptions{NowMS: model.NowMS()}); err != nil {
		t.Fatalf("ActivateStore: %v", err)
	}

	steps, err := st.ListRunSteps(run.ID)
	if err != nil {
		t.Fatalf("ListRunSteps: %v", err)
	}
	for _, s := range steps {
		if s.Instance == "first@0" {
			return s.ID
		}
	}
	t.Fatalf("no first@0 step among %d steps", len(steps))
	return 0
}

func claimStep(t *testing.T, env verb.Env, id int) verb.StepClaimResult {
	t.Helper()
	res, err := verb.StepClaim(context.Background(), env, verb.StepClaimInput{
		Step: model.FormatStepID(id), Owner: "worker-1",
	})
	if err != nil {
		t.Fatalf("StepClaim: %v", err)
	}
	return res
}

func TestStepLeaseVerbsRefuseEmptyToken(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	id := activatedStep(t, st)
	claimStep(t, env, id)
	before, err := st.GetStep(id)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}

	ctx := context.Background()
	ref := model.FormatStepID(id)
	_, heartbeatErr := verb.StepHeartbeat(ctx, env, verb.StepHeartbeatInput{Step: ref})
	_, completeErr := verb.StepComplete(ctx, env, verb.StepCompleteInput{
		Step: ref, Artifact: []byte("result"),
	})
	_, failErr := verb.StepFail(ctx, env, verb.StepFailInput{Step: ref, Note: "gave up"})

	for name, err := range map[string]error{
		"heartbeat": heartbeatErr, "complete": completeErr, "fail": failErr,
	} {
		t.Run(name, func(t *testing.T) {
			requireCode(t, err, output.ErrValidation)
		})
	}
	after, err := st.GetStep(id)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Errorf("step = %+v after empty-token calls, want unchanged %+v", *after, *before)
	}
}

func TestStepClaimValidatesBeforeTheStore(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	id := activatedStep(t, st)
	ref := model.FormatStepID(id)
	zero, negative := 0.0, -1.0

	cases := map[string]verb.StepClaimInput{
		"malformed ref":            {Step: "ISSUE-1", Owner: "worker-1"},
		"no owner":                 {Step: ref},
		"zero cost multiplier":     {Step: ref, Owner: "worker-1", CostMultiplier: &zero},
		"negative cost multiplier": {Step: ref, Owner: "worker-1", CostMultiplier: &negative},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := verb.StepClaim(context.Background(), env, in)
			requireCode(t, err, output.ErrValidation)
		})
	}

	step, err := st.GetStep(id)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if step.Status != store.StepPending && step.Status != store.StepReady {
		t.Errorf("status = %s after refused claims, want it still unclaimed", step.Status)
	}
}

func TestStepLeaseVerbsCarryTheLeaseLifecycle(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	id := activatedStep(t, st)
	ctx := context.Background()
	ref := model.FormatStepID(id)

	claimed := claimStep(t, env, id)
	if claimed.Claim.Token == "" || claimed.Claim.Step != ref || claimed.Claim.Attempt != 1 {
		t.Fatalf("claim = %+v, want a token on %s at attempt 1", *claimed.Claim, ref)
	}
	if claimed.Claim.Context == nil || claimed.Packet != nil {
		t.Errorf("claim carries context %v and packet %v, want the bundle and no packet",
			claimed.Claim.Context, claimed.Packet)
	}
	token := claimed.Claim.Token

	_, err := verb.StepClaim(ctx, env, verb.StepClaimInput{Step: ref, Owner: "worker-2"})
	requireCode(t, err, output.ErrConflict)

	_, err = verb.StepHeartbeat(ctx, env, verb.StepHeartbeatInput{Step: ref, Token: "not-the-token"})
	requireCode(t, err, output.ErrAuth)

	beat, err := verb.StepHeartbeat(ctx, env, verb.StepHeartbeatInput{Step: ref, Token: token})
	if err != nil {
		t.Fatalf("StepHeartbeat: %v", err)
	}
	if beat.Step != id || beat.Lease.Attempt != 1 || beat.Lease.ExpiresMS < claimed.Claim.LeaseExpiresMS {
		t.Errorf("heartbeat = %+v, want %s's lease at attempt 1, no earlier than %d",
			beat, ref, claimed.Claim.LeaseExpiresMS)
	}

	failed, err := verb.StepFail(ctx, env, verb.StepFailInput{Step: ref, Token: token, Note: "gave up"})
	if err != nil {
		t.Fatalf("StepFail: %v", err)
	}
	if failed.Step.Step != ref || failed.Step.Status == store.StepClaimed {
		t.Errorf("fail row = %+v, want %s released from its claim", failed.Step, ref)
	}

	_, err = verb.StepFail(ctx, env, verb.StepFailInput{Step: ref, Token: token})
	requireCode(t, err, output.ErrAuth)
}

func TestStepLeaseVerbsOnAMissingStepAreNotFound(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	ctx := context.Background()
	const ref = "STEP-999"

	_, claimErr := verb.StepClaim(ctx, env, verb.StepClaimInput{Step: ref, Owner: "worker-1"})
	_, heartbeatErr := verb.StepHeartbeat(ctx, env, verb.StepHeartbeatInput{Step: ref, Token: "t"})
	_, completeErr := verb.StepComplete(ctx, env, verb.StepCompleteInput{Step: ref, Token: "t"})
	_, failErr := verb.StepFail(ctx, env, verb.StepFailInput{Step: ref, Token: "t"})

	for name, err := range map[string]error{
		"claim": claimErr, "heartbeat": heartbeatErr, "complete": completeErr, "fail": failErr,
	} {
		t.Run(name, func(t *testing.T) {
			requireCode(t, err, output.ErrNotFound)
		})
	}
}

func TestStepClaimIsRunAddressed(t *testing.T) {
	t.Run("an anchored identity is registered and reported", func(t *testing.T) {
		st := openStore(t)
		id := activatedStep(t, st)
		const identity = "/work/repo"

		res := claimStep(t, projectEnv(st, identity, true), id)

		if res.Registered == nil {
			t.Fatal("Registered = nil, want the registration this claim made")
		}
		if res.Registered.Identity != identity || res.Registered.Verb != "step claim" {
			t.Errorf("Registered = %+v, want %s attributed to step claim", *res.Registered, identity)
		}
		if _, found, err := st.LookupProject(identity); err != nil || !found {
			t.Errorf("LookupProject(%s) = found %v, err %v; want registered", identity, found, err)
		}
	})

	t.Run("an unanchored identity claims without registering", func(t *testing.T) {
		st := openStore(t)
		id := activatedStep(t, st)
		const identity = "/work/scratch"

		res := claimStep(t, projectEnv(st, identity, false), id)

		if res.Registered != nil {
			t.Errorf("Registered = %+v, want none for an unanchored identity", *res.Registered)
		}
		requireUnregistered(t, st, identity)
	})
}
