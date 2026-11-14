package verb_test

import (
	"context"
	"encoding/json"
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
	steps, _ := activatedRun(t, st, src)
	id, ok := steps["first@0"]
	if !ok {
		t.Fatalf("no first@0 step among %v", steps)
	}
	return id
}

// activatedRun activates a run of the workflow source src in the default
// project and returns its step ids by instance and the conductor capability
// activation minted.
func activatedRun(t *testing.T, st store.Store, src string) (map[string]int, string) {
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
	activated, err := engine.ActivateStore(st, run.ID, engine.ActivateOptions{NowMS: model.NowMS()})
	if err != nil {
		t.Fatalf("ActivateStore: %v", err)
	}

	steps, err := st.ListRunSteps(run.ID)
	if err != nil {
		t.Fatalf("ListRunSteps: %v", err)
	}
	ids := make(map[string]int, len(steps))
	for _, s := range steps {
		ids[s.Instance] = s.ID
	}
	return ids, activated.ConductorToken
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

// rulingWorkflow is an executor step that parks on its first failure,
// followed by a human gate.
const rulingWorkflow = `
[pipeline]
name = "unit-ruling"
version = 1
[match]
kind = ["task"]
[[step]]
name = "first"
after = []
executor = "someone"
emits = "result"
max_attempts = 1
on_fail = "waiting-human"
[[step]]
name = "second"
after = ["first"]
type = "human"
on_fail = "skip"
`

// rulerEnv is projectEnv with the author a ruling is attributed to.
func rulerEnv(st store.Store, identity string, anchored bool) verb.Env {
	env := projectEnv(st, identity, anchored)
	env.Author = "operator"
	return env
}

// rulingVerb is one conductor-gated step ruling: setup readies the step the
// verb rules on in a run of rulingWorkflow and returns its id, and call runs
// the verb on ref with the conductor token source token.
type rulingVerb struct {
	setup func(t *testing.T, st store.Store, steps map[string]int) int
	call  func(env verb.Env, ref string, token func() string) (model.StepRow, *engine.ProjectRegistration, error)
	// ruled is the status the step reads after the ruling.
	ruled string
	// event is the kind of the event the ruling records on the step.
	event string
}

// readyGateOf completes first@0 so its human gate second@0 is ready.
func readyGateOf(t *testing.T, st store.Store, steps map[string]int) int {
	t.Helper()
	env := projectEnv(st, "", false)
	token := claimStep(t, env, steps["first@0"]).Claim.Token
	if _, err := verb.StepComplete(context.Background(), env, verb.StepCompleteInput{
		Step: model.FormatStepID(steps["first@0"]), Token: token, Artifact: []byte("result"),
	}); err != nil {
		t.Fatalf("StepComplete: %v", err)
	}
	return steps["second@0"]
}

var stepRulingVerbs = map[string]rulingVerb{
	"step reap": {
		setup: func(t *testing.T, st store.Store, steps map[string]int) int {
			claimStep(t, projectEnv(st, "", false), steps["first@0"])
			return steps["first@0"]
		},
		call: func(env verb.Env, ref string, token func() string) (model.StepRow, *engine.ProjectRegistration, error) {
			res, err := verb.StepReap(context.Background(), env, verb.StepReapInput{
				Step: ref, Reason: "the holder died", ConductorToken: token,
			})
			return res.Step, res.Registered, err
		},
		ruled: store.StepReady,
		event: engine.EventLeaseReaped,
	},
	"step hold": {
		setup: func(t *testing.T, st store.Store, steps map[string]int) int {
			return steps["first@0"]
		},
		call: func(env verb.Env, ref string, token func() string) (model.StepRow, *engine.ProjectRegistration, error) {
			res, err := verb.StepHold(context.Background(), env, verb.StepHoldInput{
				Step: ref, Reason: "for the operator", ConductorToken: token,
			})
			return res.Step, res.Registered, err
		},
		ruled: store.StepWaitingHuman,
		event: engine.EventStepHeld,
	},
	"step approve": {
		setup: readyGateOf,
		call: func(env verb.Env, ref string, token func() string) (model.StepRow, *engine.ProjectRegistration, error) {
			res, err := verb.StepApprove(context.Background(), env, verb.StepApproveInput{
				Step: ref, Note: "looks right", Authority: engine.AuthorityOperator, ConductorToken: token,
			})
			return res.Step, res.Registered, err
		},
		ruled: store.StepDone,
		event: engine.EventStepApproved,
	},
	"step reject": {
		setup: readyGateOf,
		call: func(env verb.Env, ref string, token func() string) (model.StepRow, *engine.ProjectRegistration, error) {
			res, err := verb.StepReject(context.Background(), env, verb.StepRejectInput{
				Step: ref, Note: "not yet", Authority: engine.AuthorityOperator, ConductorToken: token,
			})
			return res.Step, res.Registered, err
		},
		ruled: store.StepSkipped,
		event: engine.EventStepRejected,
	},
	"step resolve": {
		setup: func(t *testing.T, st store.Store, steps map[string]int) int {
			env := projectEnv(st, "", false)
			id := steps["first@0"]
			token := claimStep(t, env, id).Claim.Token
			if _, err := verb.StepFail(context.Background(), env, verb.StepFailInput{
				Step: model.FormatStepID(id), Token: token, Note: "gave up",
			}); err != nil {
				t.Fatalf("StepFail: %v", err)
			}
			return id
		},
		call: func(env verb.Env, ref string, token func() string) (model.StepRow, *engine.ProjectRegistration, error) {
			res, err := verb.StepResolve(context.Background(), env, verb.StepResolveInput{
				Step: ref, As: engine.ResolveSkip, Authority: engine.AuthorityOperator, ConductorToken: token,
			})
			return res.Step, res.Registered, err
		},
		ruled: store.StepSkipped,
		event: engine.EventStepResolved,
	},
}

// readyRuling activates a run of rulingWorkflow, readies the step v rules on,
// and returns its id, its status before the ruling, and the run's conductor
// capability.
func readyRuling(t *testing.T, st store.Store, v rulingVerb) (int, string, string) {
	t.Helper()
	steps, conductor := activatedRun(t, st, rulingWorkflow)
	if conductor == "" {
		t.Fatal("premise: activation minted no conductor capability")
	}
	id := v.setup(t, st, steps)
	return id, effectiveStatus(t, st, id), conductor
}

// effectiveStatus is step id's status as every step read reports it.
func effectiveStatus(t *testing.T, st store.Store, id int) string {
	t.Helper()
	view, err := engine.LoadStepViewStore(st, id, model.NowMS())
	if err != nil {
		t.Fatalf("LoadStepViewStore: %v", err)
	}
	return view.Row.Status
}

func requireStatus(t *testing.T, st store.Store, id int, want string) {
	t.Helper()
	if got := effectiveStatus(t, st, id); got != want {
		t.Errorf("%s status = %s, want %s", model.FormatStepID(id), got, want)
	}
}

func TestStepRulingVerbsRuleUnderTheConductorCapability(t *testing.T) {
	for name, v := range stepRulingVerbs {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			id, _, conductor := readyRuling(t, st, v)

			row, _, err := v.call(rulerEnv(st, "", false), model.FormatStepID(id),
				func() string { return conductor })
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if row.Step != model.FormatStepID(id) || row.Status != v.ruled {
				t.Errorf("row = %s (%s), want %s %s", row.Step, row.Status, model.FormatStepID(id), v.ruled)
			}
			requireStatus(t, st, id, v.ruled)
		})
	}
}

func TestStepRulingVerbsRefuseAWrongConductorToken(t *testing.T) {
	for name, v := range stepRulingVerbs {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			id, before, _ := readyRuling(t, st, v)

			_, _, err := v.call(rulerEnv(st, "", false), model.FormatStepID(id),
				func() string { return "not-the-capability" })
			requireCode(t, err, output.ErrAuth)
			requireStatus(t, st, id, before)
		})
	}
}

// TestStepRulingVerbsReadNoTokenOnAnUnboundRun pins the blocking-channel
// hazard: a run bound to no capability never calls the token source.
func TestStepRulingVerbsReadNoTokenOnAnUnboundRun(t *testing.T) {
	for name, v := range stepRulingVerbs {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			id, _, _ := readyRuling(t, st, v)
			step, err := st.GetStep(id)
			if err != nil {
				t.Fatalf("GetStep: %v", err)
			}
			tx, err := st.Begin()
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if err := tx.SetRunConductorHashTx(step.RunID, ""); err != nil {
				_ = tx.Rollback()
				t.Fatalf("unbinding the run: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}

			_, _, err = v.call(rulerEnv(st, "", false), model.FormatStepID(id), func() string {
				t.Error("the token source was read for a run bound to no capability")
				return ""
			})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			requireStatus(t, st, id, v.ruled)
		})
	}
}

// TestStepRulingVerbsRefuseAnUnattributedRuling: a transport that supplies no
// author or no working directory gets a refusal, never a ruling recorded
// without one.
func TestStepRulingVerbsRefuseAnUnattributedRuling(t *testing.T) {
	unattributed := map[string]func(env *verb.Env){
		"no working directory": func(env *verb.Env) { env.Cwd = "" },
		"no author":            func(env *verb.Env) { env.Author = "" },
	}
	for name, v := range stepRulingVerbs {
		for missing, strip := range unattributed {
			t.Run(name+" with "+missing, func(t *testing.T) {
				st := openStore(t)
				id, before, conductor := readyRuling(t, st, v)
				env := rulerEnv(st, "", false)
				strip(&env)

				if _, _, err := v.call(env, model.FormatStepID(id), func() string { return conductor }); err == nil {
					t.Fatalf("a ruling with %s was accepted", missing)
				}
				requireStatus(t, st, id, before)
			})
		}
	}
}

// TestStepRulingVerbsRecordWhoRuled: the event each ruling writes names
// env.Author as its actor and env.Cwd as its working directory.
func TestStepRulingVerbsRecordWhoRuled(t *testing.T) {
	for name, v := range stepRulingVerbs {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			id, _, conductor := readyRuling(t, st, v)
			env := rulerEnv(st, "", false)

			if _, _, err := v.call(env, model.FormatStepID(id), func() string { return conductor }); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			raw, found, err := st.FirstStepEventData(id, v.event)
			if err != nil || !found {
				t.Fatalf("reading the %s event: found=%v err=%v", v.event, found, err)
			}
			var data struct {
				Actor string `json:"actor"`
				Cwd   string `json:"cwd"`
			}
			if err := json.Unmarshal([]byte(raw), &data); err != nil {
				t.Fatalf("decoding the %s event %s: %v", v.event, raw, err)
			}
			if data.Actor != env.Author || data.Cwd != env.Cwd {
				t.Errorf("%s event actor=%q cwd=%q, want actor=%q cwd=%q",
					v.event, data.Actor, data.Cwd, env.Author, env.Cwd)
			}
		})
	}
}

// TestStepRulingVerbsAreRunAddressed pins the policy each ruling declares,
// as TestStepLeaseVerbsAreRunAddressed does for the lease verbs.
func TestStepRulingVerbsAreRunAddressed(t *testing.T) {
	for name, v := range stepRulingVerbs {
		t.Run(name+" registers an anchored identity", func(t *testing.T) {
			st := openStore(t)
			id, _, conductor := readyRuling(t, st, v)
			const identity = "/work/repo"

			_, registered, err := v.call(rulerEnv(st, identity, true), model.FormatStepID(id),
				func() string { return conductor })
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if registered == nil {
				t.Fatal("Registered = nil, want the registration this call made")
			}
			if registered.Identity != identity || registered.Verb != name {
				t.Errorf("Registered = %+v, want %s attributed to %s", *registered, identity, name)
			}
		})

		t.Run(name+" runs for an unanchored identity without registering", func(t *testing.T) {
			st := openStore(t)
			id, _, conductor := readyRuling(t, st, v)
			const identity = "/work/scratch"

			_, registered, err := v.call(rulerEnv(st, identity, false), model.FormatStepID(id),
				func() string { return conductor })
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

func TestStepRulingVerbsValidateBeforeTheStore(t *testing.T) {
	st := openStore(t)
	env := rulerEnv(st, "/work/repo", true)
	ctx := context.Background()
	const ref = "STEP-1"

	_, malformedErr := verb.StepReap(ctx, env, verb.StepReapInput{Step: "ISSUE-1", Reason: "dead"})
	_, approveErr := verb.StepApprove(ctx, env, verb.StepApproveInput{Step: ref})
	_, rejectErr := verb.StepReject(ctx, env, verb.StepRejectInput{
		Step: ref, Authority: engine.AuthorityStandingGrant,
	})
	_, noAsErr := verb.StepResolve(ctx, env, verb.StepResolveInput{
		Step: ref, Authority: engine.AuthorityOperator,
	})
	_, unknownAuthorityErr := verb.StepResolve(ctx, env, verb.StepResolveInput{
		Step: ref, As: engine.ResolveSkip, Authority: "someone",
	})
	_, relativeWorktreeErr := verb.StepResolve(ctx, env, verb.StepResolveInput{
		Step: ref, As: engine.ResolveOverridePass, Authority: engine.AuthorityOperator,
		Worktree: "relative/checkout",
	})

	for name, err := range map[string]error{
		"malformed ref":                      malformedErr,
		"approve without authority":          approveErr,
		"standing grant without its ref":     rejectErr,
		"resolve without --as":               noAsErr,
		"resolve under an unknown authority": unknownAuthorityErr,
		"resolve with a relative worktree":   relativeWorktreeErr,
	} {
		t.Run(name, func(t *testing.T) {
			requireCode(t, err, output.ErrValidation)
		})
	}
	requireUnregistered(t, st, "/work/repo")
}

// TestStepRulingInputsValidateTheRequest pins what each ruling input's
// Validate refuses, which a transport checks before resolving its
// attribution, and that a well-formed request passes.
func TestStepRulingInputsValidateTheRequest(t *testing.T) {
	const ref = "STEP-1"
	refused := map[string]error{
		"reap with a malformed ref":    verb.StepReapInput{Step: "ISSUE-1"}.Validate(),
		"hold with a malformed ref":    verb.StepHoldInput{Step: "ISSUE-1"}.Validate(),
		"approve with a malformed ref": verb.StepApproveInput{Step: "ISSUE-1"}.Validate(),
		"reject with a malformed ref":  verb.StepRejectInput{Step: "ISSUE-1"}.Validate(),
		"resolve with a malformed ref": verb.StepResolveInput{Step: "ISSUE-1", As: engine.ResolveSkip}.Validate(),
		"resolve without --as":         verb.StepResolveInput{Step: ref}.Validate(),
	}
	for name, err := range refused {
		t.Run(name, func(t *testing.T) {
			requireCode(t, err, output.ErrValidation)
		})
	}

	accepted := map[string]error{
		"reap":    verb.StepReapInput{Step: ref}.Validate(),
		"hold":    verb.StepHoldInput{Step: ref}.Validate(),
		"approve": verb.StepApproveInput{Step: ref}.Validate(),
		"reject":  verb.StepRejectInput{Step: ref}.Validate(),
		"resolve": verb.StepResolveInput{Step: ref, As: engine.ResolveSkip}.Validate(),
	}
	for name, err := range accepted {
		if err != nil {
			t.Errorf("%s: Validate = %v, want nil for a well-formed request", name, err)
		}
	}
}
