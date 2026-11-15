package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// runTarget is a bound run of stepRulingWorkflow: its reference, its one
// issue, and its conductor capability.
type runTarget struct {
	run       string
	runID     int
	issue     int
	conductor string
}

func (f *stepFixture) boundRunTarget(t *testing.T) runTarget {
	t.Helper()
	steps, conductor := f.boundRun(t)
	step, err := f.st.GetStep(steps["first@0"])
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	issues, err := f.st.ListRunIssues(step.RunID)
	if err != nil || len(issues) != 1 {
		t.Fatalf("ListRunIssues = %v, %v; want the run's one issue", issues, err)
	}
	return runTarget{
		run: model.FormatRunID(step.RunID), runID: step.RunID, issue: issues[0].IssueID, conductor: conductor,
	}
}

// runState is what a refused conductor ruling must leave unchanged: the run
// row and the run's event trail.
type runState struct {
	status  model.RunStatus
	version int
	events  int
}

func (f *stepFixture) runState(t *testing.T, target runTarget) runState {
	t.Helper()
	run, err := f.st.GetRun(target.runID)
	if err != nil {
		t.Fatalf("GetRun(%s): %v", target.run, err)
	}
	return runState{status: run.Status, version: run.RowVersion, events: f.runEvents(t, target, "")}
}

// runEvents counts target's events of kind, or all of them for "".
func (f *stepFixture) runEvents(t *testing.T, target runTarget, kind string) int {
	t.Helper()
	page, err := engine.ListEventsStore(f.st, engine.EventQuery{RunID: target.runID, Kind: kind})
	if err != nil {
		t.Fatalf("ListEventsStore(%s, %q): %v", target.run, kind, err)
	}
	return page.Total
}

// driftPin pins ref on target at bytes other than the ones on disk under the
// store's config root, so a repin has a change to authorize.
func (f *stepFixture) driftPin(t *testing.T, target runTarget, ref string) {
	t.Helper()
	path := filepath.Join(os.Getenv("DOCKET_PATH"), "config", ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("AFTER\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	tx, err := f.st.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.InsertPinTx(store.Pin{
		RunID: target.runID, Kind: store.PinKindFile, Ref: ref, SHA256: workflow.SHA256([]byte("BEFORE\n")),
	}); err != nil {
		tx.Rollback()
		t.Fatalf("InsertPinTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// runRulingRPCs are the conductor-token run RPCs whose request carries every
// input the verb requires. setup readies a fresh bound run, call sends the
// RPC with token, and event is the event kind the ruling records.
var runRulingRPCs = []struct {
	name  string
	event string
	setup func(t *testing.T, f *stepFixture, target runTarget)
	call  func(f *stepFixture, target runTarget, token string) error
}{
	{
		name:  "RunResume",
		event: engine.EventRunResumed,
		setup: func(t *testing.T, f *stepFixture, target runTarget) {
			t.Helper()
			if _, err := verb.RunPause(context.Background(), verb.Env{Store: f.st}, verb.RunPauseInput{
				Run: target.run, Authority: engine.AuthorityOperator, ConductorToken: conductorToken(target.conductor),
			}); err != nil {
				t.Fatalf("pausing %s: %v", target.run, err)
			}
		},
		call: func(f *stepFixture, target runTarget, token string) error {
			_, err := f.c.RunResume(context.Background(), connect.NewRequest(&docketv1.RunResumeRequest{
				Run: target.run, Reason: "the operator answered", Token: token,
			}))
			return err
		},
	},
	{
		name:  "RunNoteAdd",
		event: engine.EventRunNoteAdded,
		call: func(f *stepFixture, target runTarget, token string) error {
			_, err := f.c.RunNoteAdd(context.Background(), connect.NewRequest(&docketv1.RunNoteAddRequest{
				Run: target.run, Authority: engine.AuthorityOperator, Text: "keep the scope narrow", Token: token,
			}))
			return err
		},
	},
	{
		name:  "RunFactAdd",
		event: engine.RunFactStepDeferred,
		call: func(f *stepFixture, target runTarget, token string) error {
			steps, err := f.st.ListRunSteps(target.runID)
			if err != nil || len(steps) == 0 {
				return errors.New("the run has no steps to defer")
			}
			_, err = f.c.RunFactAdd(context.Background(), connect.NewRequest(&docketv1.RunFactAddRequest{
				Run: target.run, Kind: engine.RunFactStepDeferred, Step: model.FormatStepID(steps[0].ID),
				Cause: engine.DeferralChain, Reason: "behind its predecessor", Token: token,
			}))
			return err
		},
	},
	{
		name:  "RunRefreshBody",
		event: engine.EventIssueBodyRefreshed,
		setup: func(t *testing.T, f *stepFixture, target runTarget) {
			t.Helper()
			if err := f.st.UpdateIssueCAS(target.issue, map[string]interface{}{"description": "AC1: amended"}, "operator", nil); err != nil {
				t.Fatalf("UpdateIssueCAS: %v", err)
			}
		},
		call: func(f *stepFixture, target runTarget, token string) error {
			_, err := f.c.RunRefreshBody(context.Background(), connect.NewRequest(&docketv1.RunRefreshBodyRequest{
				Run: target.run, Issue: model.FormatID(target.issue), Reason: "amended", Token: token,
			}))
			return err
		},
	},
	{
		name:  "RunRefreshFiles",
		event: engine.EventIssueFilesRefreshed,
		setup: func(t *testing.T, f *stepFixture, target runTarget) {
			t.Helper()
			if err := f.st.AttachFiles(target.issue, []string{"docs/attached.md"}, "operator"); err != nil {
				t.Fatalf("AttachFiles: %v", err)
			}
		},
		call: func(f *stepFixture, target runTarget, token string) error {
			_, err := f.c.RunRefreshFiles(context.Background(), connect.NewRequest(&docketv1.RunRefreshFilesRequest{
				Run: target.run, Issue: model.FormatID(target.issue), Reason: "attached", Token: token,
			}))
			return err
		},
	},
	{
		name:  "RunRefreshScope",
		event: engine.EventIssueScopeRefreshed,
		setup: func(t *testing.T, f *stepFixture, target runTarget) {
			t.Helper()
			if err := f.st.SetIssueScopeGlobs(target.issue, `["a/**"]`); err != nil {
				t.Fatalf("SetIssueScopeGlobs: %v", err)
			}
		},
		call: func(f *stepFixture, target runTarget, token string) error {
			_, err := f.c.RunRefreshScope(context.Background(), connect.NewRequest(&docketv1.RunRefreshScopeRequest{
				Run: target.run, Issue: model.FormatID(target.issue), Reason: "widened", Token: token,
			}))
			return err
		},
	},
	{
		name:  "RunRepin",
		event: engine.EventRunRepinned,
		setup: func(t *testing.T, f *stepFixture, target runTarget) {
			t.Helper()
			f.driftPin(t, target, "contracts/drifted.md")
		},
		call: func(f *stepFixture, target runTarget, token string) error {
			_, err := f.c.RunRepin(context.Background(), connect.NewRequest(&docketv1.RunRepinRequest{
				Run: target.run, Reason: "adopt the edit", Token: token,
			}))
			return err
		},
	},
}

// TestRunHandlersBindVerbLayer calls every run RPC over Connect, then runs
// the threat model's token rows against each conductor-token run RPC on a
// bound run.
func TestRunHandlersBindVerbLayer(t *testing.T) {
	f := newStepFixture(t)
	ctx := context.Background()
	var issues []string
	for _, title := range []string{"run subject", "run extra"} {
		id, _, err := f.st.CreateIssueIdempotent(&model.Issue{
			Title: title, Status: model.StatusTodo, Priority: model.PriorityMedium, Kind: model.IssueKindTask,
		}, nil, nil, "")
		if err != nil {
			t.Fatalf("CreateIssueIdempotent: %v", err)
		}
		issues = append(issues, model.FormatID(id))
	}

	budget := 12.5
	started, err := f.c.RunStart(ctx, connect.NewRequest(&docketv1.RunStartRequest{
		Request: "serve the run noun", Issues: issues[:1], Budget: &budget,
	}))
	requireServed(t, "RunStart", err)
	run := started.Msg.GetRun().GetId()
	if got := started.Msg.GetRun(); run == "" || got.GetStatus() != string(model.RunPlanning) ||
		got.GetRequest() != "serve the run noun" || got.GetBudget() != budget {
		t.Fatalf("RunStart run = %v, want a planning run with the request and budget sent", got)
	}

	added, err := f.c.RunIssueAdd(ctx, connect.NewRequest(&docketv1.RunIssueAddRequest{Run: run, Issues: issues[1:]}))
	requireServed(t, "RunIssueAdd", err)
	if got := added.Msg.GetRun().GetIssues(); strings.Join(got, ",") != strings.Join(issues, ",") {
		t.Errorf("RunIssueAdd issues = %v, want %v", got, issues)
	}
	removed, err := f.c.RunIssueRemove(ctx, connect.NewRequest(&docketv1.RunIssueRemoveRequest{Run: run, Issues: issues[1:]}))
	requireServed(t, "RunIssueRemove", err)
	if got := removed.Msg.GetRun().GetIssues(); strings.Join(got, ",") != issues[0] {
		t.Errorf("RunIssueRemove issues = %v, want [%s]", got, issues[0])
	}

	activated, err := f.c.RunActivate(ctx, connect.NewRequest(&docketv1.RunActivateRequest{Run: run}))
	requireServed(t, "RunActivate", err)
	if got := activated.Msg; got.GetRun().GetStatus() != string(model.RunActive) || got.GetSteps() != 2 {
		t.Errorf("RunActivate = %v, want %s active with its two steps", got, run)
	}

	listed, err := f.c.RunStatus(ctx, connect.NewRequest(&docketv1.RunStatusRequest{}))
	requireServed(t, "RunStatus list", err)
	if items := listed.Msg.GetItems(); len(items) != 1 || items[0].GetId() != run || listed.Msg.GetTotal() != 1 {
		t.Errorf("RunStatus list = %v, want the one active run %s", listed.Msg, run)
	}
	status, err := f.c.RunStatus(ctx, connect.NewRequest(&docketv1.RunStatusRequest{Run: run}))
	requireServed(t, "RunStatus", err)
	if items := status.Msg.GetItems(); len(items) != 1 || items[0].GetId() != run || len(items[0].GetSteps()) == 0 {
		t.Errorf("RunStatus = %v, want %s with its step counts", status.Msg, run)
	}

	read, err := f.c.RunBudget(ctx, connect.NewRequest(&docketv1.RunBudgetRequest{Run: run}))
	requireServed(t, "RunBudget", err)
	if got := read.Msg; got.GetRun() != run || got.GetBudget() != budget {
		t.Errorf("RunBudget = %v, want %s capped at %g", got, run, budget)
	}

	notes, err := f.c.RunNoteList(ctx, connect.NewRequest(&docketv1.RunNoteListRequest{Run: run}))
	requireServed(t, "RunNoteList", err)
	if len(notes.Msg.GetItems()) != 0 || notes.Msg.GetTotal() != 0 {
		t.Errorf("RunNoteList = %v, want no notes", notes.Msg)
	}

	report, err := f.c.RunReport(ctx, connect.NewRequest(&docketv1.RunReportRequest{Run: run}))
	requireServed(t, "RunReport", err)
	if got := report.Msg; got.GetRun().GetId() != run || len(got.GetSteps()) != 2 {
		t.Errorf("RunReport = %v, want %s with its two steps' attempts", got, run)
	}

	pins, err := f.c.RunVerifyPins(ctx, connect.NewRequest(&docketv1.RunVerifyPinsRequest{Run: run}))
	requireServed(t, "RunVerifyPins", err)
	if got := pins.Msg; got.GetRun() != run || !got.GetClean() || len(got.GetPins()) == 0 {
		t.Errorf("RunVerifyPins = %v, want %s clean over its pins", got, run)
	}

	conducted, err := f.c.RunConduct(ctx, connect.NewRequest(&docketv1.RunConductRequest{Run: run}))
	requireServed(t, "RunConduct", err)
	if got := conducted.Msg; got.GetRun() != run || got.GetToken() == "" {
		t.Errorf("RunConduct = %v, want a fresh capability on %s", got, run)
	}

	// RunPause and RunAbandon reach the verb, which refuses each for the
	// authority the request has no field for, even with the run's own token.
	for _, rpc := range []struct {
		name string
		call func(target runTarget) error
	}{
		{"RunPause", func(target runTarget) error {
			_, err := f.c.RunPause(ctx, connect.NewRequest(&docketv1.RunPauseRequest{
				Run: target.run, Reason: "waiting on the operator", Token: target.conductor,
			}))
			return err
		}},
		{"RunAbandon", func(target runTarget) error {
			_, err := f.c.RunAbandon(ctx, connect.NewRequest(&docketv1.RunAbandonRequest{
				Run: target.run, Reason: "superseded", Token: target.conductor,
			}))
			return err
		}},
	} {
		t.Run(rpc.name, func(t *testing.T) {
			target := f.boundRunTarget(t)
			before := f.runState(t, target)
			err := rpc.call(target)
			requireRefusal(t, rpc.name, err, connect.CodeInvalidArgument, output.ErrValidation)
			if err != nil && !strings.Contains(err.Error(), "--authority is required") {
				t.Errorf("%s refusal = %v, want the verb's missing-authority refusal", rpc.name, err)
			}
			if after := f.runState(t, target); after != before {
				t.Errorf("%s changed the run %+v -> %+v", rpc.name, before, after)
			}
		})
	}

	for _, rpc := range runRulingRPCs {
		t.Run(rpc.name, func(t *testing.T) {
			target := f.boundRunTarget(t)
			if rpc.setup != nil {
				rpc.setup(t, f, target)
			}
			before := f.runState(t, target)

			err := rpc.call(f, target, "not-the-token")
			requireRefusal(t, rpc.name+" with a wrong token", err, connect.CodePermissionDenied, output.ErrAuth)
			if after := f.runState(t, target); after != before {
				t.Errorf("%s with a wrong token changed the run %+v -> %+v", rpc.name, before, after)
			}

			err = rpc.call(f, target, "")
			requireRefusal(t, rpc.name+" with no token", err, connect.CodeInvalidArgument, output.ErrValidation)
			if after := f.runState(t, target); after != before {
				t.Errorf("%s with no token changed the run %+v -> %+v", rpc.name, before, after)
			}

			requireServed(t, rpc.name+" with the correct token", rpc.call(f, target, target.conductor))
			if n := f.runEvents(t, target, rpc.event); n != 1 {
				t.Errorf("%s with the correct token left %d %s events, want one", rpc.name, n, rpc.event)
			}
		})
	}
}
