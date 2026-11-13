package verb_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

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

// gatedStepWorkflow declares a completion gate no trust entry matches, so the
// saga records it unmatched and parks the step.
const gatedStepWorkflow = `
[pipeline]
name = "unit-gated-step"
version = 1
[match]
kind = ["task"]
[[step]]
name = "first"
after = []
executor = "someone"
emits = "result"
gates = ["verb-test-untrusted-gate"]
on_fail = "waiting-human"
`

// activatedStep activates a one-step run in the default project and returns
// its executor step's id. DOCKET_PATH is pinned to an empty store, so
// activation does not read the operator's or the repository's shipped
// workflows.
func activatedStep(t *testing.T, st store.Store) int {
	t.Helper()
	return activatedStepOf(t, st, stepWorkflow)
}

// activatedStepOf is activatedStep over the workflow source src.
func activatedStepOf(t *testing.T, st store.Store, src string) int {
	t.Helper()
	t.Setenv("DOCKET_PATH", filepath.Join(t.TempDir(), ".docket"))
	def, err := workflow.Load([]byte(src))
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
		SourceSHA256: workflow.SHA256([]byte(src)),
		Body:         src, Parsed: string(parsed),
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
	negativeTTL := -time.Second

	cases := map[string]verb.StepClaimInput{
		"malformed ref":            {Step: "ISSUE-1", Owner: "worker-1"},
		"no owner":                 {Step: ref},
		"negative ttl":             {Step: ref, Owner: "worker-1", TTL: &negativeTTL},
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

// leaseVerbCall runs one step lease verb on ref under env. token is the live
// lease token, or "" for step claim, which takes the lease itself.
type leaseVerbCall func(env verb.Env, ref, token string) (*engine.ProjectRegistration, error)

var stepLeaseVerbs = map[string]leaseVerbCall{
	"step claim": func(env verb.Env, ref, _ string) (*engine.ProjectRegistration, error) {
		res, err := verb.StepClaim(context.Background(), env, verb.StepClaimInput{Step: ref, Owner: "worker-1"})
		return res.Registered, err
	},
	"step heartbeat": func(env verb.Env, ref, token string) (*engine.ProjectRegistration, error) {
		res, err := verb.StepHeartbeat(context.Background(), env, verb.StepHeartbeatInput{Step: ref, Token: token})
		return res.Registered, err
	},
	"step complete": func(env verb.Env, ref, token string) (*engine.ProjectRegistration, error) {
		res, err := verb.StepComplete(context.Background(), env, verb.StepCompleteInput{
			Step: ref, Token: token, Artifact: []byte("result"),
		})
		return res.Registered, err
	},
	"step fail": func(env verb.Env, ref, token string) (*engine.ProjectRegistration, error) {
		res, err := verb.StepFail(context.Background(), env, verb.StepFailInput{Step: ref, Token: token})
		return res.Registered, err
	},
}

// TestStepLeaseVerbsAreRunAddressed pins the policy each step lease verb
// declares: an anchored identity is registered and reported, as under
// ProjectMayRegister and unlike ProjectReadOnly, and an unanchored one runs
// without registering, unlike ProjectMayRegister.
func TestStepLeaseVerbsAreRunAddressed(t *testing.T) {
	// leased activates a step and, for every verb but claim, claims it in the
	// default project, returning the step reference and the token.
	leased := func(t *testing.T, st store.Store, name string) (string, string) {
		id := activatedStep(t, st)
		if name == "step claim" {
			return model.FormatStepID(id), ""
		}
		return model.FormatStepID(id), claimStep(t, projectEnv(st, "", false), id).Claim.Token
	}

	for name, call := range stepLeaseVerbs {
		t.Run(name+" registers an anchored identity", func(t *testing.T) {
			st := openStore(t)
			ref, token := leased(t, st, name)
			const identity = "/work/repo"

			registered, err := call(projectEnv(st, identity, true), ref, token)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if registered == nil {
				t.Fatal("Registered = nil, want the registration this call made")
			}
			if registered.Identity != identity || registered.Verb != name {
				t.Errorf("Registered = %+v, want %s attributed to %s", *registered, identity, name)
			}
			if _, found, err := st.LookupProject(identity); err != nil || !found {
				t.Errorf("LookupProject(%s) = found %v, err %v; want registered", identity, found, err)
			}
		})

		t.Run(name+" runs for an unanchored identity without registering", func(t *testing.T) {
			st := openStore(t)
			ref, token := leased(t, st, name)
			const identity = "/work/scratch"

			registered, err := call(projectEnv(st, identity, false), ref, token)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if registered != nil {
				t.Errorf("Registered = %+v, want none for an unanchored identity", *registered)
			}
			requireUnregistered(t, st, identity)
		})
	}
}

func TestStepCompleteReturnsTheRecordedStep(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	id := activatedStepOf(t, st, gatedStepWorkflow)
	token := claimStep(t, env, id).Claim.Token

	res, err := verb.StepComplete(context.Background(), env, verb.StepCompleteInput{
		Step: model.FormatStepID(id), Token: token, Artifact: []byte("result"),
		Gaps: [][]byte{[]byte("A defect the work surfaced\nHome: THIS repository\n")},
	})
	if err != nil {
		t.Fatalf("StepComplete: %v", err)
	}

	if res.Detached {
		t.Error("Detached = true with no launcher installed, want the saga run inline")
	}
	if res.Step.Step != model.FormatStepID(id) || res.Step.Status != store.StepWaitingHuman {
		t.Errorf("step = %s (%s), want %s parked %s by its unmatched gate",
			res.Step.Step, res.Step.Status, model.FormatStepID(id), store.StepWaitingHuman)
	}
	if len(res.FailedGates) != 1 || res.FailedGates[0].Gate != "verb-test-untrusted-gate" ||
		res.FailedGates[0].Verdict != store.GateVerdictUnmatched {
		t.Errorf("FailedGates = %+v, want the one unmatched gate", res.FailedGates)
	}
	if len(res.GapIssues) != 1 {
		t.Fatalf("GapIssues = %v, want the one issue the gap filed", res.GapIssues)
	}
	gapID, err := model.ParseID(res.GapIssues[0])
	if err != nil {
		t.Fatalf("GapIssues[0] = %q is not an issue id: %v", res.GapIssues[0], err)
	}
	if gap, err := st.GetIssue(gapID); err != nil || gap.Title != "A defect the work surfaced" {
		t.Errorf("gap issue %s = %+v, err %v; want the filed gap", res.GapIssues[0], gap, err)
	}
}

// TestStepClaimHandsBackTheTokenOfAnIncompleteClaim fails the post-claim
// render: the template indexes past the packet's files only once the step
// reads claimed, so the preflight render passes and the authoritative one
// fails after the lease committed.
func TestStepClaimHandsBackTheTokenOfAnIncompleteClaim(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	id := activatedStep(t, st)
	ref := model.FormatStepID(id)
	template := filepath.Join(t.TempDir(), "packet.tmpl")
	if err := os.WriteFile(template, []byte(strandingTemplate), 0o600); err != nil {
		t.Fatalf("writing the template: %v", err)
	}

	res, err := verb.StepClaim(context.Background(), env, verb.StepClaimInput{
		Step: ref, Owner: "worker-1", Render: true, Template: template,
	})

	var incomplete *engine.IncompleteClaimError
	if !errors.As(err, &incomplete) {
		t.Fatalf("err = %v (%T), want one wrapping an *engine.IncompleteClaimError", err, err)
	}
	requireCode(t, err, output.ErrGeneral)
	if res.Claim == nil || res.Claim.Token == "" {
		t.Fatalf("Claim = %+v, want the committed claim carrying its token", res.Claim)
	}
	if _, err := verb.StepHeartbeat(context.Background(), env, verb.StepHeartbeatInput{
		Step: ref, Token: res.Claim.Token,
	}); err != nil {
		t.Errorf("the returned token does not hold the lease: %v", err)
	}
}

// strandingTemplate renders before the claim and fails after it.
const strandingTemplate = `{{.Step.Step}}{{if eq .Step.Status "claimed"}}{{index .Files 99}}{{end}}`
