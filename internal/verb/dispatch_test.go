package verb_test

import (
	"context"
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

	unanchored := projectEnv(st, "/scratch/elsewhere", false)
	openDispatch(t, unanchored, run)
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

// A non-nil batch makes the close the one-invocation reconcile, and an empty
// one is refused at the back-fill stage rather than closing quietly.
func TestDispatchCloseWithABatchReconciles(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	run, _ := dispatchRun(t, st)
	openDispatch(t, env, run)

	res, err := verb.DispatchClose(context.Background(), env, verb.DispatchCloseInput{
		Run: run, BackfillRows: []engine.BackfillRow{},
	})
	requireCode(t, err, output.ErrValidation)
	if res.Reconcile == nil || res.Reconcile.Close != nil {
		t.Errorf("reconcile = %+v, want the partial outcome with no close stage", res.Reconcile)
	}
}
