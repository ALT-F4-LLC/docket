package verb_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// dispatchRun activates a one-step run and returns its run reference.
func dispatchRun(t *testing.T, st store.Store) (string, int) {
	t.Helper()
	stepID := activatedStep(t, st)
	step, err := st.GetStep(stepID)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	return model.FormatRunID(step.RunID), stepID
}

func openDispatch(t *testing.T, env verb.Env, run string) verb.DispatchOpenResult {
	t.Helper()
	res, err := verb.DispatchOpen(context.Background(), env, verb.DispatchOpenInput{Run: run})
	if err != nil {
		t.Fatalf("DispatchOpen: %v", err)
	}
	return res
}

func TestDispatchLifecycleThroughTheVerbs(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	run, _ := dispatchRun(t, st)
	ctx := context.Background()

	opened := openDispatch(t, env, run)
	if opened.Manifest == nil || len(opened.Manifest.Rows) != 1 || opened.Manifest.Run != run {
		t.Fatalf("manifest = %+v, want the run's one ready row", opened.Manifest)
	}

	extended, err := verb.DispatchExtend(ctx, env, verb.DispatchExtendInput{Run: run})
	if err != nil {
		t.Fatalf("DispatchExtend: %v", err)
	}
	if extended.Extension.Dispatch != opened.Manifest.Dispatch || len(extended.Extension.Rows) != 0 {
		t.Errorf("extension = %+v, want no new rows on %s", extended.Extension, opened.Manifest.Dispatch)
	}

	verified, err := verb.DispatchVerify(ctx, env, verb.DispatchVerifyInput{Run: run})
	if err != nil {
		t.Fatalf("DispatchVerify: %v", err)
	}
	if verified.Mismatch != nil || !verified.Verify.Verified {
		t.Errorf("verify = %+v, mismatch %+v, want an untouched manifest verified",
			verified.Verify, verified.Mismatch)
	}

	closed, err := verb.DispatchClose(ctx, env, verb.DispatchCloseInput{Run: run})
	if err != nil {
		t.Fatalf("DispatchClose: %v", err)
	}
	if closed.Close == nil || closed.Close.Dispatch != opened.Manifest.Dispatch || closed.Reconcile != nil {
		t.Errorf("close = %+v, reconcile %+v, want a plain close of %s",
			closed.Close, closed.Reconcile, opened.Manifest.Dispatch)
	}
}

// A mismatch is the verify's answer, not a failure: the result carries the
// stored and recomputed rows for the transport to render.
func TestDispatchVerifyReturnsAMismatchAsItsResult(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	run, stepID := dispatchRun(t, st)
	openDispatch(t, env, run)
	claimStep(t, env, stepID)

	res, err := verb.DispatchVerify(context.Background(), env, verb.DispatchVerifyInput{Run: run})
	if err != nil {
		t.Fatalf("DispatchVerify: %v", err)
	}
	if res.Mismatch == nil || res.Verify == nil {
		t.Fatalf("verify = %+v, mismatch %+v, want the claimed row reported", res.Verify, res.Mismatch)
	}
	if res.Mismatch.Stored == "" || res.Mismatch.Computed != "" {
		t.Errorf("mismatch = %+v, want the stored row and no recomputed one", res.Mismatch)
	}
}

func TestDispatchVerbsRefuseBadInputBeforeTheStore(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "/repo/unseen", true)
	run, _ := dispatchRun(t, st)
	ctx := context.Background()

	cases := map[string]func() error{
		"open malformed run": func() error {
			_, err := verb.DispatchOpen(ctx, env, verb.DispatchOpenInput{Run: "STEP-1"})
			return err
		},
		"open zero ack seq": func() error {
			_, err := verb.DispatchOpen(ctx, env, verb.DispatchOpenInput{Run: run, AckReap: []int64{0}})
			return err
		},
		"extend malformed run": func() error {
			_, err := verb.DispatchExtend(ctx, env, verb.DispatchExtendInput{Run: "nope"})
			return err
		},
		"verify malformed run": func() error {
			_, err := verb.DispatchVerify(ctx, env, verb.DispatchVerifyInput{Run: "nope"})
			return err
		},
		"close malformed run": func() error {
			_, err := verb.DispatchClose(ctx, env, verb.DispatchCloseInput{Run: "nope"})
			return err
		},
		"backfill-usage malformed run": func() error {
			_, err := verb.DispatchBackfillUsage(ctx, env, verb.DispatchBackfillUsageInput{Run: "nope"})
			return err
		},
		"abandon malformed run": func() error {
			_, err := verb.DispatchAbandon(ctx, env, verb.DispatchAbandonInput{Run: "nope"})
			return err
		},
		"waive-target malformed run": func() error {
			_, err := verb.DispatchWaiveTarget(ctx, env, verb.DispatchWaiveTargetInput{Run: "nope"})
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			requireCode(t, call(), output.ErrValidation)
		})
	}
	requireUnregistered(t, st, "/repo/unseen")
}

// Open, extend and close are run-addressed: an anchored identity registers on
// first contact and the result names it for the caller's event, while an
// unanchored one carries on unregistered. Verify is a read and registers
// nothing.
func TestDispatchVerbsDeclareTheirProjectPolicy(t *testing.T) {
	st := openStore(t)
	run, _ := dispatchRun(t, st)
	ctx := context.Background()

	_, err := verb.DispatchVerify(ctx, projectEnv(st, "/repo/reader", true),
		verb.DispatchVerifyInput{Run: run})
	requireCode(t, err, output.ErrConflict)
	requireUnregistered(t, st, "/repo/reader")

	opened := openDispatch(t, projectEnv(st, "/repo/opener", true), run)
	if opened.Registered == nil || opened.Registered.Identity != "/repo/opener" ||
		opened.Registered.Verb != "dispatch open" {
		t.Errorf("registered = %+v, want /repo/opener attributed to dispatch open", opened.Registered)
	}
	if _, found, err := st.LookupProject("/repo/opener"); err != nil || !found {
		t.Errorf("LookupProject(/repo/opener) = found %v, err %v; want registered", found, err)
	}

	unanchored := projectEnv(st, "/scratch/elsewhere", false)
	if _, err := verb.DispatchExtend(ctx, unanchored, verb.DispatchExtendInput{Run: run}); err != nil {
		t.Fatalf("unanchored DispatchExtend: %v", err)
	}
	requireUnregistered(t, st, "/scratch/elsewhere")

	anchored := projectEnv(st, "/repo/writer", true)
	extended, err := verb.DispatchExtend(ctx, anchored, verb.DispatchExtendInput{Run: run})
	if err != nil {
		t.Fatalf("DispatchExtend: %v", err)
	}
	if extended.Registered == nil || extended.Registered.Identity != "/repo/writer" ||
		extended.Registered.Verb != "dispatch extend" {
		t.Errorf("registered = %+v, want /repo/writer attributed to dispatch extend", extended.Registered)
	}

	closed, err := verb.DispatchClose(ctx, projectEnv(st, "/repo/closer", true),
		verb.DispatchCloseInput{Run: run})
	if err != nil {
		t.Fatalf("DispatchClose: %v", err)
	}
	if closed.Registered == nil || closed.Registered.Verb != "dispatch close" {
		t.Errorf("registered = %+v, want the close's registration", closed.Registered)
	}

	// Back-fill, abandon and waive-target are run-addressed too. Each
	// registers before its engine call, so a refusal after registration still
	// names the project for the caller's event.
	backfilled, err := verb.DispatchBackfillUsage(ctx, projectEnv(st, "/repo/backfiller", true),
		verb.DispatchBackfillUsageInput{Run: run})
	requireCode(t, err, output.ErrValidation)
	if backfilled.Registered == nil || backfilled.Registered.Verb != "dispatch backfill-usage" {
		t.Errorf("registered = %+v, want the back-fill's registration", backfilled.Registered)
	}
	abandoned, err := verb.DispatchAbandon(ctx, projectEnv(st, "/repo/abandoner", true),
		verb.DispatchAbandonInput{Run: run})
	requireCode(t, err, output.ErrValidation)
	if abandoned.Registered == nil || abandoned.Registered.Verb != "dispatch abandon" {
		t.Errorf("registered = %+v, want the abandon's registration", abandoned.Registered)
	}
	waived, err := verb.DispatchWaiveTarget(ctx, projectEnv(st, "/repo/waiver", true),
		verb.DispatchWaiveTargetInput{Run: run})
	requireCode(t, err, output.ErrValidation)
	if waived.Registered == nil || waived.Registered.Verb != "dispatch waive-target" {
		t.Errorf("registered = %+v, want the waive-target's registration", waived.Registered)
	}
	unanchoredBackfill := projectEnv(st, "/scratch/backfill", false)
	if _, err := verb.DispatchBackfillUsage(ctx, unanchoredBackfill,
		verb.DispatchBackfillUsageInput{Run: run}); err == nil {
		t.Fatal("an empty back-fill batch was accepted")
	}
	requireUnregistered(t, st, "/scratch/backfill")
}

// The conductor token is read only when an override needs it, so a plain
// close never touches the caller's token channel.
func TestDispatchCloseReadsTheConductorTokenOnlyForAnOverride(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	run, _ := dispatchRun(t, st)
	ctx := context.Background()

	reads := 0
	token := func() string {
		reads++
		return "not-the-conductor-token"
	}

	openDispatch(t, env, run)
	_, err := verb.DispatchClose(ctx, env, verb.DispatchCloseInput{
		Run: run, SkipIntegrationCheck: "hand-integrated", ConductorToken: token,
	})
	requireCode(t, err, output.ErrAuth)
	if reads != 1 {
		t.Errorf("token reads = %d on an override, want 1", reads)
	}

	reads = 0
	if _, err := verb.DispatchClose(ctx, env, verb.DispatchCloseInput{
		Run: run, ConductorToken: token,
	}); err != nil {
		t.Fatalf("plain DispatchClose: %v", err)
	}
	if reads != 0 {
		t.Errorf("token reads = %d on a plain close, want 0", reads)
	}
}

// An override on a run bound to no capability needs no token, so the token
// channel is never read: a caller whose channel never closes would block.
func TestDispatchCloseOverrideOnAnUnboundRunLeavesTheTokenUnread(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	run, _ := dispatchRun(t, st)
	bindConductor(t, st, run, "")
	openDispatch(t, env, run)

	reads := 0
	_, err := verb.DispatchClose(context.Background(), env, verb.DispatchCloseInput{
		Run: run, SkipIntegrationCheck: "hand-integrated",
		ConductorToken: func() string { reads++; return "" },
	})
	if err != nil {
		t.Fatalf("DispatchClose with an override on an unbound run: %v", err)
	}
	if reads != 0 {
		t.Errorf("token reads = %d on an unbound run, want 0", reads)
	}
}

// --no-conditions is a conductor ruling: on a bound run it takes the run's
// token, and its run note is attributed to the Env's author and working
// directory.
func TestDispatchCloseNoConditionsWaivesUnderTheConductor(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	env.Author = "tester"
	run, stepID := dispatchRun(t, st)
	const conductor = "the-conductor-token"
	bindConductor(t, st, run, conductor)
	openDispatch(t, env, run)
	proposal := sealedConcernsVote(t, st, stepID)
	ctx := context.Background()

	waive := func(token string) (verb.DispatchCloseResult, error) {
		return verb.DispatchClose(ctx, env, verb.DispatchCloseInput{
			Run: run, NoConditions: "tracked elsewhere",
			ConductorToken: func() string { return token },
		})
	}

	_, err := waive("not-the-conductor-token")
	requireCode(t, err, output.ErrAuth)
	runID, _ := verb.ParseRunRef(run)
	if notes, err := st.ListRunNotes(runID); err != nil || len(notes) != 0 {
		t.Fatalf("run notes after a refused waiver = %+v (err %v), want none", notes, err)
	}

	res, err := waive(conductor)
	if err != nil {
		t.Fatalf("DispatchClose --no-conditions with the conductor token: %v", err)
	}
	if res.Close == nil || len(res.Close.ConditionsWaived) != 1 ||
		res.Close.ConditionsWaived[0].Proposal != model.FormatProposalID(proposal) {
		t.Errorf("close = %+v, want %s waived", res.Close, model.FormatProposalID(proposal))
	}

	snap, err := st.BeginReadSnapshot()
	if err != nil {
		t.Fatalf("BeginReadSnapshot: %v", err)
	}
	defer snap.Rollback()
	data, found, err := snap.LatestEventDataTx(runID, engine.EventRunNoteAdded)
	if err != nil || !found {
		t.Fatalf("run-note-added event: found %v, err %v", found, err)
	}
	var by struct {
		Actor string `json:"actor"`
		Cwd   string `json:"cwd"`
	}
	if err := json.Unmarshal([]byte(data), &by); err != nil {
		t.Fatalf("run-note-added data %q: %v", data, err)
	}
	if by.Actor != "tester" || by.Cwd != "/work/scratch" {
		t.Errorf("waiver attributed to %+v, want actor tester and cwd /work/scratch", by)
	}
}

// A ruling must record where it was made from, so an Env with no working
// directory is refused before the close writes anything.
func TestDispatchCloseNoConditionsRefusesAnEmptyWorkingDirectory(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	run, _ := dispatchRun(t, st)
	openDispatch(t, env, run)

	unrooted := env
	unrooted.Cwd = ""
	_, err := verb.DispatchClose(context.Background(), unrooted, verb.DispatchCloseInput{
		Run: run, NoConditions: "tracked elsewhere",
	})
	requireCode(t, err, output.ErrGeneral)

	// The dispatch is still open: a plain close finds it to close.
	if _, err := verb.DispatchClose(context.Background(), env, verb.DispatchCloseInput{Run: run}); err != nil {
		t.Fatalf("plain DispatchClose after the refusal: %v", err)
	}
}

// Reconcile makes the close the one-invocation reconcile, and an empty batch,
// absent or not, is refused at the back-fill stage rather than closing
// quietly.
func TestDispatchCloseWithReconcileReconciles(t *testing.T) {
	for name, rows := range map[string][]engine.BackfillRow{
		"empty batch":  {},
		"absent batch": nil,
	} {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			env := projectEnv(st, "", false)
			run, _ := dispatchRun(t, st)
			openDispatch(t, env, run)

			res, err := verb.DispatchClose(context.Background(), env, verb.DispatchCloseInput{
				Run: run, Reconcile: true, BackfillRows: rows,
			})
			requireCode(t, err, output.ErrValidation)
			if res.Reconcile == nil || res.Reconcile.Close != nil || res.Close != nil {
				t.Errorf("reconcile = %+v, close %+v, want the partial reconcile with no close stage",
					res.Reconcile, res.Close)
			}
		})
	}
}

func TestDispatchBackfillAbandonAndWaiveThroughTheVerbs(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	env.Author = "tester"
	run, stepID := dispatchRun(t, st)
	const conductor = "the-conductor-token"
	bindConductor(t, st, run, conductor)
	token := func() string { return conductor }
	ctx := context.Background()

	backfilled, err := verb.DispatchBackfillUsage(ctx, env, verb.DispatchBackfillUsageInput{
		Run:  run,
		Rows: []engine.BackfillRow{{Step: stepID, Unit: "tokens", Quantity: 42}},
	})
	if err != nil {
		t.Fatalf("DispatchBackfillUsage: %v", err)
	}
	if b := backfilled.Backfill; b == nil || b.Written != 1 || b.Source != engine.UsageSourceBackfilled ||
		len(b.Unclaimed) != 1 {
		t.Errorf("backfill = %+v, want one backfilled row on the never-claimed step", b)
	}

	waived, err := verb.DispatchWaiveTarget(ctx, env, verb.DispatchWaiveTargetInput{
		Run: run, Steps: []string{"review@0#0"}, Target: "cafe1234cafe",
		Note: "adjudicated", ConductorToken: token,
	})
	if err != nil {
		t.Fatalf("DispatchWaiveTarget: %v", err)
	}
	if len(waived.Waivers) != 1 || waived.Waivers[0].Instance != "review@0#0" || waived.Waivers[0].ID == 0 {
		t.Errorf("waivers = %+v, want one recorded waiver on review@0#0", waived.Waivers)
	}

	opened := openDispatch(t, env, run)
	abandoned, err := verb.DispatchAbandon(ctx, env, verb.DispatchAbandonInput{
		Run: run, Reason: "relay crashed", ConductorToken: token,
	})
	if err != nil {
		t.Fatalf("DispatchAbandon: %v", err)
	}
	if a := abandoned.Abandon; a == nil || a.Dispatch != opened.Manifest.Dispatch || a.Status != "abandoned" {
		t.Errorf("abandon = %+v, want %s abandoned", a, opened.Manifest.Dispatch)
	}
}

// Abandon and waive-target are conductor rulings: on a bound run a wrong
// token is refused AUTH, and on an unbound run the token channel is never
// read, since a caller whose channel never closes would block.
func TestDispatchConductorVerbsReadTheTokenOnlyOnABoundRun(t *testing.T) {
	cases := map[string]func(env verb.Env, run string, token func() string) error{
		"abandon": func(env verb.Env, run string, token func() string) error {
			_, err := verb.DispatchAbandon(context.Background(), env, verb.DispatchAbandonInput{
				Run: run, ConductorToken: token,
			})
			return err
		},
		"waive-target": func(env verb.Env, run string, token func() string) error {
			_, err := verb.DispatchWaiveTarget(context.Background(), env, verb.DispatchWaiveTargetInput{
				Run: run, Steps: []string{"review@0#0"}, Target: "cafe1234cafe", ConductorToken: token,
			})
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			env := projectEnv(st, "", false)
			env.Author = "tester"
			run, _ := dispatchRun(t, st)
			bindConductor(t, st, run, "the-conductor-token")
			openDispatch(t, env, run)

			reads := 0
			wrong := func() string { reads++; return "not-the-conductor-token" }
			requireCode(t, call(env, run, wrong), output.ErrAuth)
			if reads != 1 {
				t.Errorf("token reads = %d on a bound run, want 1", reads)
			}

			bindConductor(t, st, run, "")
			reads = 0
			if err := call(env, run, wrong); err != nil {
				t.Fatalf("%s on an unbound run: %v", name, err)
			}
			if reads != 0 {
				t.Errorf("token reads = %d on an unbound run, want 0", reads)
			}
		})
	}
}

// A waiver is a ruling, so it is attributed to the Env's author and working
// directory, and an Env with neither is refused before anything is written.
func TestDispatchWaiveTargetRefusesAnUnattributedRuling(t *testing.T) {
	st := openStore(t)
	run, _ := dispatchRun(t, st)
	bindConductor(t, st, run, "")

	_, err := verb.DispatchWaiveTarget(context.Background(), verb.Env{Store: st},
		verb.DispatchWaiveTargetInput{Run: run, Steps: []string{"review@0#0"}, Target: "cafe1234cafe"})
	if err == nil {
		t.Fatal("an unattributed waiver was accepted")
	}
	runID, _ := verb.ParseRunRef(run)
	if waivers, err := st.StaleTargetWaiversForRun(runID); err != nil || len(waivers) != 0 {
		t.Errorf("waivers after a refusal = %+v (err %v), want none", waivers, err)
	}
}

// bindConductor binds run to token's conductor capability, or unbinds it
// when token is empty.
func bindConductor(t *testing.T, st store.Store, run, token string) {
	t.Helper()
	runID, err := verb.ParseRunRef(run)
	if err != nil {
		t.Fatalf("ParseRunRef: %v", err)
	}
	var hash string
	if token != "" {
		hash = model.HashToken(token)
	}
	tx, err := st.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	if err := tx.SetRunConductorHashTx(runID, hash); err != nil {
		t.Fatalf("SetRunConductorHashTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// sealedConcernsVote seals a one-seat vote with an approve-with-concerns cast
// on the step's issue and no linked tribunal issue, and returns its proposal.
func sealedConcernsVote(t *testing.T, st store.Store, stepID int) int {
	t.Helper()
	step, err := st.GetStep(stepID)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	id, err := st.CreateProposalIdempotent(&model.Proposal{
		ProjectID: store.DefaultProjectID, Description: "ship it?",
		Criticality: model.CriticalityMedium, Threshold: 0.5, RequiredVoters: 1,
		Status: model.ProposalStatusOpen, CreatedBy: "conductor",
	}, "")
	if err != nil {
		t.Fatalf("CreateProposalIdempotent: %v", err)
	}
	if err := st.LinkProposalIssue(id, step.IssueID); err != nil {
		t.Fatalf("LinkProposalIssue: %v", err)
	}
	if _, err := st.CastVote(&model.Vote{
		ProposalID: id, VoterName: "seat-a", Verdict: model.VerdictApproveWithConcerns,
		Confidence: 0.9, DomainRelevance: 0.8,
	}); err != nil {
		t.Fatalf("CastVote: %v", err)
	}
	return id
}
