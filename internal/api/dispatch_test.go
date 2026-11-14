package api

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// dispatchTarget is a bound run with its manifest open: the run reference,
// the open manifest's id, and the run's conductor capability.
type dispatchTarget struct {
	run       string
	runID     int
	dispatch  int
	conductor string
}

// openBoundDispatch activates a bound run of stepRulingWorkflow and opens its
// manifest over Connect.
func (f *stepFixture) openBoundDispatch(t *testing.T) dispatchTarget {
	t.Helper()
	steps, conductor := f.boundRun(t)
	step, err := f.st.GetStep(steps["first@0"])
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	run := model.FormatRunID(step.RunID)
	_, err = f.c.DispatchOpen(context.Background(), connect.NewRequest(&docketv1.DispatchOpenRequest{Run: run}))
	requireServed(t, "DispatchOpen", err)

	snap, err := f.st.BeginReadSnapshot()
	if err != nil {
		t.Fatalf("BeginReadSnapshot: %v", err)
	}
	defer snap.Rollback()
	open, err := snap.OpenDispatchTx(step.RunID)
	if err != nil || open == nil {
		t.Fatalf("OpenDispatchTx = %v (err %v), want the manifest just opened", open, err)
	}
	return dispatchTarget{run: run, runID: step.RunID, dispatch: open.ID, conductor: conductor}
}

// dispatchState is what a refused conductor ruling must leave unchanged: the
// manifest row and the run's stale-target waivers.
type dispatchState struct {
	status  string
	version int
	waivers int
}

func (f *stepFixture) dispatchState(t *testing.T, target dispatchTarget) dispatchState {
	t.Helper()
	snap, err := f.st.BeginReadSnapshot()
	if err != nil {
		t.Fatalf("BeginReadSnapshot: %v", err)
	}
	defer snap.Rollback()
	row, err := snap.GetDispatchTx(target.dispatch)
	if err != nil {
		t.Fatalf("GetDispatchTx(%d): %v", target.dispatch, err)
	}
	waivers, err := f.st.StaleTargetWaiversForRun(target.runID)
	if err != nil {
		t.Fatalf("StaleTargetWaiversForRun: %v", err)
	}
	return dispatchState{status: row.Status, version: row.RowVersion, waivers: len(waivers)}
}

// dispatchRulingRPCs are the conductor-token dispatch RPCs, each sent with
// token against an open manifest of a bound run. done reports whether the
// state a call left is the ruling's effect.
var dispatchRulingRPCs = []struct {
	name string
	call func(f *stepFixture, target dispatchTarget, token string) error
	done func(after dispatchState) bool
}{
	{
		name: "DispatchClose",
		call: func(f *stepFixture, target dispatchTarget, token string) error {
			_, err := f.c.DispatchClose(context.Background(), connect.NewRequest(&docketv1.DispatchCloseRequest{
				Run: target.run, SkipIntegrationCheck: "integrated by hand", Token: token,
			}))
			return err
		},
		done: func(after dispatchState) bool { return after.status == "closed" },
	},
	{
		name: "DispatchAbandon",
		call: func(f *stepFixture, target dispatchTarget, token string) error {
			_, err := f.c.DispatchAbandon(context.Background(), connect.NewRequest(&docketv1.DispatchAbandonRequest{
				Run: target.run, Reason: "the relay crashed", Token: token,
			}))
			return err
		},
		done: func(after dispatchState) bool { return after.status == "abandoned" },
	},
	{
		name: "DispatchWaiveTarget",
		call: func(f *stepFixture, target dispatchTarget, token string) error {
			_, err := f.c.DispatchWaiveTarget(context.Background(), connect.NewRequest(&docketv1.DispatchWaiveTargetRequest{
				Run: target.run, Step: []string{"first@0"}, Target: "cafe1234cafe", Note: "adjudicated", Token: token,
			}))
			return err
		},
		done: func(after dispatchState) bool { return after.waivers == 1 },
	},
}

// TestDispatchHandlersBindVerbLayer calls every dispatch RPC over Connect,
// then runs the threat model's token rows against each conductor-token RPC
// on a bound run.
func TestDispatchHandlersBindVerbLayer(t *testing.T) {
	f := newStepFixture(t)
	ctx := context.Background()
	steps, _ := f.boundRun(t)
	first := model.FormatStepID(steps["first@0"])
	step, err := f.st.GetStep(steps["first@0"])
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	run := model.FormatRunID(step.RunID)

	opened, err := f.c.DispatchOpen(ctx, connect.NewRequest(&docketv1.DispatchOpenRequest{Run: run}))
	requireServed(t, "DispatchOpen", err)
	manifest := opened.Msg.GetManifest()
	if manifest.GetId() == "" || manifest.GetRun() != run || manifest.GetStatus() != "open" ||
		len(manifest.GetRows()) != 1 || manifest.GetRows()[0].GetStep() != first {
		t.Fatalf("DispatchOpen manifest = %v, want %s open with its one ready row %s", manifest, run, first)
	}

	extended, err := f.c.DispatchExtend(ctx, connect.NewRequest(&docketv1.DispatchExtendRequest{Run: run}))
	requireServed(t, "DispatchExtend", err)
	if got := extended.Msg.GetManifest().GetId(); got != manifest.GetId() || len(extended.Msg.GetAdded()) != 0 {
		t.Errorf("DispatchExtend = %v, want no rows added to %s", extended.Msg, manifest.GetId())
	}

	verified, err := f.c.DispatchVerify(ctx, connect.NewRequest(&docketv1.DispatchVerifyRequest{Run: run}))
	requireServed(t, "DispatchVerify", err)
	if rows := verified.Msg.GetRows(); !verified.Msg.GetMatch() || len(rows) != 1 ||
		rows[0].GetStep() != first || rows[0].GetVerdict() != "matched" {
		t.Errorf("DispatchVerify = %v, want %s matched", verified.Msg, first)
	}

	backfilled, err := f.c.DispatchBackfillUsage(ctx, connect.NewRequest(&docketv1.DispatchBackfillUsageRequest{
		Run: run, Source: "relay-journal", Rows: []*docketv1.UsageRow{{Step: first, Unit: "tokens", Quantity: 42}},
	}))
	requireServed(t, "DispatchBackfillUsage", err)
	if backfilled.Msg.GetRecorded() != 1 || backfilled.Msg.GetSkipped() != 0 {
		t.Errorf("DispatchBackfillUsage = %v, want one row recorded", backfilled.Msg)
	}

	closed, err := f.c.DispatchClose(ctx, connect.NewRequest(&docketv1.DispatchCloseRequest{Run: run}))
	requireServed(t, "DispatchClose", err)
	if got := closed.Msg.GetManifest(); got.GetId() != manifest.GetId() || got.GetStatus() != "closed" {
		t.Errorf("DispatchClose manifest = %v, want %s closed", got, manifest.GetId())
	}

	for _, rpc := range dispatchRulingRPCs {
		t.Run(rpc.name, func(t *testing.T) {
			target := f.openBoundDispatch(t)
			before := f.dispatchState(t, target)

			err := rpc.call(f, target, "not-the-token")
			requireRefusal(t, rpc.name+" with a wrong token", err, connect.CodePermissionDenied, output.ErrAuth)
			if after := f.dispatchState(t, target); after != before {
				t.Errorf("%s with a wrong token changed the state %+v -> %+v", rpc.name, before, after)
			}

			err = rpc.call(f, target, "")
			requireRefusal(t, rpc.name+" with no token", err, connect.CodeInvalidArgument, output.ErrValidation)
			if after := f.dispatchState(t, target); after != before {
				t.Errorf("%s with no token changed the state %+v -> %+v", rpc.name, before, after)
			}

			requireServed(t, rpc.name+" with the correct token", rpc.call(f, target, target.conductor))
			if after := f.dispatchState(t, target); !rpc.done(after) {
				t.Errorf("%s with the correct token left the state %+v", rpc.name, after)
			}
		})
	}
}
