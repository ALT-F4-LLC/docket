package api

import (
	"context"
	"errors"
	"maps"
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

// The bytes driftPin pins and the bytes it leaves on disk.
var (
	pinnedBytes = []byte("BEFORE\n")
	diskBytes   = []byte("AFTER\n")
)

// driftPin pins ref on target at bytes other than the ones on disk under the
// store's config root, so a repin has a change to authorize.
//
// driftPin and pinFile stay copies of pinDriftedFile and insertFilePin in
// internal/verb's tests rather than moving into internal/testsupport. Each
// pair is a few store calls, the assertions here read pinnedBytes and
// diskBytes by name, and a shared helper would make testsupport import store
// and workflow for that small saving.
func (f *stepFixture) driftPin(t *testing.T, target runTarget, ref string) {
	t.Helper()
	path := filepath.Join(os.Getenv("DOCKET_PATH"), "config", ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, diskBytes, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.pinFile(t, target, ref)
}

// pinFile pins ref on target at pinnedBytes without touching disk, so a ref
// with no file under the config root reads as missing.
func (f *stepFixture) pinFile(t *testing.T, target runTarget, ref string) {
	t.Helper()
	tx, err := f.st.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.InsertPinTx(store.Pin{
		RunID: target.runID, Kind: store.PinKindFile, Ref: ref, SHA256: workflow.SHA256(pinnedBytes),
	}); err != nil {
		tx.Rollback()
		t.Fatalf("InsertPinTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// pinSHA returns the hash target's file pin on ref records, and whether the
// run holds that pin at all.
func (f *stepFixture) pinSHA(t *testing.T, target runTarget, ref string) (string, bool) {
	t.Helper()
	pins, err := f.st.ListPins(target.runID)
	if err != nil {
		t.Fatalf("ListPins(%s): %v", target.run, err)
	}
	for _, p := range pins {
		if p.Kind == store.PinKindFile && p.Ref == ref {
			return p.SHA256, true
		}
	}
	return "", false
}

// recordUsage has runID's project count tokens against the declared-cost cap
// and seconds against the measured cap, and records each quantity on the
// run's first step.
func (f *stepFixture) recordUsage(t *testing.T, runID int, tokens, seconds float64) {
	t.Helper()
	run, err := f.st.GetRun(runID)
	if err != nil {
		t.Fatalf("GetRun(%d): %v", runID, err)
	}
	if err := f.st.SetConfig(run.ProjectID, store.KeyBudgetUnit, "tokens"); err != nil {
		t.Fatalf("SetConfig(%s): %v", store.KeyBudgetUnit, err)
	}
	if err := f.st.SetConfig(run.ProjectID, store.KeyUsageBudgetUnit, "seconds"); err != nil {
		t.Fatalf("SetConfig(%s): %v", store.KeyUsageBudgetUnit, err)
	}
	steps, err := f.st.ListRunSteps(runID)
	if err != nil || len(steps) == 0 {
		t.Fatalf("ListRunSteps = %v, %v; want the run's steps", steps, err)
	}
	tx, err := f.st.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for unit, quantity := range map[string]float64{"tokens": tokens, "seconds": seconds} {
		if err := tx.InsertUsageRowTx(store.UsageRow{
			RunID: runID, StepID: steps[0].ID, Attempt: 1, Unit: unit, Quantity: quantity,
		}, model.NowMS()); err != nil {
			tx.Rollback()
			t.Fatalf("InsertUsageRowTx(%s): %v", unit, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// pauseRun pauses target on the operator's authority through the verb layer,
// so a test that needs a paused run does not depend on the RunPause RPC.
func pauseRun(t *testing.T, f *stepFixture, target runTarget) {
	t.Helper()
	if _, err := verb.RunPause(context.Background(), verb.Env{Store: f.st}, verb.RunPauseInput{
		Run: target.run, Authority: engine.AuthorityOperator, ConductorToken: conductorToken(target.conductor),
	}); err != nil {
		t.Fatalf("pausing %s: %v", target.run, err)
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
		setup: pauseRun,
		call: func(f *stepFixture, target runTarget, token string) error {
			_, err := f.c.RunResume(context.Background(), connect.NewRequest(&docketv1.RunResumeRequest{
				Run: target.run, Reason: "the operator answered", Token: token,
			}))
			return err
		},
	},
	{
		name:  "RunPause",
		event: engine.EventRunPaused,
		call: func(f *stepFixture, target runTarget, token string) error {
			_, err := f.c.RunPause(context.Background(), connect.NewRequest(&docketv1.RunPauseRequest{
				Run: target.run, Reason: "waiting on the operator", Authority: engine.AuthorityOperator, Token: token,
			}))
			return err
		},
	},
	{
		name:  "RunAbandon",
		event: engine.EventRunAbandoned,
		call: func(f *stepFixture, target runTarget, token string) error {
			_, err := f.c.RunAbandon(context.Background(), connect.NewRequest(&docketv1.RunAbandonRequest{
				Run: target.run, Reason: "superseded", Authority: engine.AuthorityOperator, Token: token,
			}))
			return err
		},
	},
	{
		name:  "RunAbandonIssue",
		event: engine.EventIssueAbandoned,
		call: func(f *stepFixture, target runTarget, token string) error {
			_, err := f.c.RunAbandon(context.Background(), connect.NewRequest(&docketv1.RunAbandonRequest{
				Run: target.run, Issue: model.FormatID(target.issue), Reason: "superseded",
				Authority: engine.AuthorityOperator, Token: token,
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

	budget, usage := 12.5, 4000.0
	started, err := f.c.RunStart(ctx, connect.NewRequest(&docketv1.RunStartRequest{
		Request: "serve the run noun", Issues: issues[:1], Budget: &budget, UsageBudget: &usage,
	}))
	requireServed(t, "RunStart", err)
	run := started.Msg.GetRun().GetId()
	if got := started.Msg.GetRun(); run == "" || got.GetStatus() != string(model.RunPlanning) ||
		got.GetRequest() != "serve the run noun" || got.GetBudget() != budget || got.GetUsageBudget() != usage {
		t.Fatalf("RunStart run = %v, want a planning run with the request and both caps sent", got)
	}
	runID, err := model.ParseRunID(run)
	if err != nil {
		t.Fatalf("ParseRunID(%q): %v", run, err)
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

	// A dry run computes the activation and writes nothing.
	spare, err := f.c.RunStart(ctx, connect.NewRequest(&docketv1.RunStartRequest{
		Request: "dry-run subject", Issues: issues[1:],
	}))
	requireServed(t, "RunStart", err)
	spareRun := spare.Msg.GetRun().GetId()
	spareID, err := model.ParseRunID(spareRun)
	if err != nil {
		t.Fatalf("ParseRunID(%q): %v", spareRun, err)
	}
	_, err = f.c.RunActivate(ctx, connect.NewRequest(&docketv1.RunActivateRequest{Run: spareRun, DryRun: true}))
	requireServed(t, "RunActivate dry run", err)
	if row, err := f.st.GetRun(spareID); err != nil || row.Status != model.RunPlanning {
		t.Errorf("after a dry-run RunActivate, %s = %+v, %v; want it still planning", spareRun, row, err)
	}
	if steps, err := f.st.ListRunSteps(spareID); err != nil || len(steps) != 0 {
		t.Errorf("after a dry-run RunActivate, %s has steps %v, %v; want none", spareRun, steps, err)
	}

	pinPath := filepath.Join(t.TempDir(), "pinned.md")
	if err := os.WriteFile(pinPath, []byte("pinned by path\n"), 0o600); err != nil {
		t.Fatalf("writing the pin file: %v", err)
	}
	activated, err := f.c.RunActivate(ctx, connect.NewRequest(&docketv1.RunActivateRequest{Run: run, Pin: []string{pinPath}}))
	requireServed(t, "RunActivate", err)
	if got := activated.Msg; got.GetRun().GetStatus() != string(model.RunActive) || got.GetSteps() != 2 {
		t.Errorf("RunActivate = %v, want %s active with its two steps", got, run)
	}
	if sha, ok := f.pinSHA(t, runTarget{run: run, runID: runID}, pinPath); !ok ||
		sha != workflow.SHA256([]byte("pinned by path\n")) {
		t.Errorf("RunActivate pinned %s at %q (held %v), want the file's hash", pinPath, sha, ok)
	}

	listed, err := f.c.RunStatus(ctx, connect.NewRequest(&docketv1.RunStatusRequest{}))
	requireServed(t, "RunStatus list", err)
	if got := listed.Msg; len(got.GetItems()) != 2 || got.GetTotal() != 2 || got.GetTruncated() {
		t.Errorf("RunStatus list = %v, want the two live runs %s and %s", got, run, spareRun)
	}
	limited, err := f.c.RunStatus(ctx, connect.NewRequest(&docketv1.RunStatusRequest{Limit: 1}))
	requireServed(t, "RunStatus limit", err)
	if got := limited.Msg; len(got.GetItems()) != 1 || got.GetTotal() != 2 || !got.GetTruncated() {
		t.Errorf("RunStatus limit 1 = %v, want one of two runs, truncated", got)
	}
	if _, err := verb.RunAbandon(ctx, verb.Env{Store: f.st}, verb.RunAbandonInput{
		Run: spareRun, Reason: "only the dry run", Authority: engine.AuthorityOperator,
	}); err != nil {
		t.Fatalf("abandoning %s: %v", spareRun, err)
	}
	live, err := f.c.RunStatus(ctx, connect.NewRequest(&docketv1.RunStatusRequest{}))
	requireServed(t, "RunStatus list", err)
	if items := live.Msg.GetItems(); len(items) != 1 || items[0].GetId() != run || live.Msg.GetTotal() != 1 {
		t.Errorf("RunStatus list = %v, want the one live run %s", live.Msg, run)
	}
	all, err := f.c.RunStatus(ctx, connect.NewRequest(&docketv1.RunStatusRequest{All: true}))
	requireServed(t, "RunStatus all", err)
	if got := all.Msg; len(got.GetItems()) != 2 || got.GetTotal() != 2 {
		t.Errorf("RunStatus all = %v, want %s and the abandoned %s", got, run, spareRun)
	}
	status, err := f.c.RunStatus(ctx, connect.NewRequest(&docketv1.RunStatusRequest{Run: run}))
	requireServed(t, "RunStatus", err)
	if items := status.Msg.GetItems(); len(items) != 1 || items[0].GetId() != run || len(items[0].GetSteps()) == 0 {
		t.Errorf("RunStatus = %v, want %s with its step counts", status.Msg, run)
	}

	// One ledger row in the unit each cap counts, at different quantities, so
	// the declared-cost spend and the measured usage read back apart.
	const spentTokens, measuredSeconds = 7.0, 3.0
	f.recordUsage(t, runID, spentTokens, measuredSeconds)

	read, err := f.c.RunBudget(ctx, connect.NewRequest(&docketv1.RunBudgetRequest{Run: run}))
	requireServed(t, "RunBudget", err)
	if got := read.Msg; got.GetRun() != run || got.GetBudget() != budget || got.GetUsageBudget() != usage ||
		got.GetAccrued() != spentTokens {
		t.Errorf("RunBudget = %v, want %s capped at %g and %g with %g accrued", got, run, budget, usage, spentTokens)
	}
	raised := 20.0
	set, err := f.c.RunBudget(ctx, connect.NewRequest(&docketv1.RunBudgetRequest{Run: run, Set: &raised, Reason: "more room"}))
	requireServed(t, "RunBudget set", err)
	if got := set.Msg.GetBudget(); got != raised {
		t.Errorf("RunBudget set = %g, want %g", got, raised)
	}
	if row, err := f.st.GetRun(runID); err != nil || row.Budget != raised {
		t.Errorf("after RunBudget set, %s = %+v, %v; want its cap stored as %g", run, row, err, raised)
	}

	notes, err := f.c.RunNoteList(ctx, connect.NewRequest(&docketv1.RunNoteListRequest{Run: run}))
	requireServed(t, "RunNoteList", err)
	if len(notes.Msg.GetItems()) != 0 || notes.Msg.GetTotal() != 0 {
		t.Errorf("RunNoteList = %v, want no notes", notes.Msg)
	}

	report, err := f.c.RunReport(ctx, connect.NewRequest(&docketv1.RunReportRequest{Run: run}))
	requireServed(t, "RunReport", err)
	if got := report.Msg; got.GetRun().GetId() != run || len(got.GetSteps()) != 2 ||
		got.GetAccruedCost() != spentTokens || got.GetMeasuredUsage() != measuredSeconds {
		t.Errorf("RunReport = %v, want %s with its two steps' attempts, %g accrued and %g measured",
			got, run, spentTokens, measuredSeconds)
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

	// The capability RunConduct answers with is the one the run now accepts,
	// and the one it replaced is refused.
	t.Run("RunConduct capability", func(t *testing.T) {
		target := f.boundRunTarget(t)
		conducted, err := f.c.RunConduct(ctx, connect.NewRequest(&docketv1.RunConductRequest{Run: target.run}))
		requireServed(t, "RunConduct", err)
		token := conducted.Msg.GetToken()
		if token == "" || token == target.conductor {
			t.Fatalf("RunConduct token = %q, want a capability other than the one it replaces", token)
		}
		addNote := func(token string) (*connect.Response[docketv1.RunNoteAddResponse], error) {
			return f.c.RunNoteAdd(ctx, connect.NewRequest(&docketv1.RunNoteAddRequest{
				Run: target.run, Authority: engine.AuthorityOperator, Text: "keep the scope narrow", Token: token,
			}))
		}
		_, err = addNote(target.conductor)
		requireRefusal(t, "RunNoteAdd with the replaced token", err, connect.CodePermissionDenied, output.ErrAuth)

		added, err := addNote(token)
		requireServed(t, "RunNoteAdd with the conducted token", err)
		note := added.Msg.GetNote()
		if note.GetText() != "keep the scope narrow" || note.GetAuthority() != engine.AuthorityOperator {
			t.Errorf("RunNoteAdd note = %v, want the text and authority sent", note)
		}
		notes, err := f.c.RunNoteList(ctx, connect.NewRequest(&docketv1.RunNoteListRequest{Run: target.run}))
		requireServed(t, "RunNoteList", err)
		if items := notes.Msg.GetItems(); len(items) != 1 || notes.Msg.GetTotal() != 1 ||
			items[0].GetText() != note.GetText() || items[0].GetAuthority() != note.GetAuthority() ||
			items[0].GetOrdinal() != note.GetOrdinal() {
			t.Errorf("RunNoteList = %v, want the one note %v", notes.Msg, note)
		}
	})

	// An unsound pin set is refused with its report attached, so the caller
	// still reads which pin drifted.
	t.Run("RunVerifyPins drift", func(t *testing.T) {
		target := f.boundRunTarget(t)
		f.driftPin(t, target, "contracts/drifted.md")
		_, err := f.c.RunVerifyPins(ctx, connect.NewRequest(&docketv1.RunVerifyPinsRequest{Run: target.run}))
		requireRefusal(t, "RunVerifyPins on a drifted run", err, connect.CodeAborted, output.ErrConflict)
		var connectErr *connect.Error
		if !errors.As(err, &connectErr) || len(connectErr.Details()) != 2 {
			t.Fatalf("RunVerifyPins refusal = %v, want the ErrorDetail and the report", err)
		}
		value, err := connectErr.Details()[1].Value()
		if err != nil {
			t.Fatalf("report detail: %v", err)
		}
		report, ok := value.(*docketv1.RunVerifyPinsResponse)
		if !ok {
			t.Fatalf("report detail is %T, want *docketv1.RunVerifyPinsResponse", value)
		}
		if report.GetRun() != target.run || report.GetClean() {
			t.Errorf("report = %v, want %s not clean", report, target.run)
		}
		var drifted *docketv1.PinVerdict
		for _, p := range report.GetPins() {
			if p.GetPath() == "contracts/drifted.md" {
				drifted = p
			}
		}
		if drifted == nil || drifted.GetVerdict() != engine.PinChanged ||
			drifted.GetPinnedSha256() != workflow.SHA256(pinnedBytes) ||
			drifted.GetCurrentSha256() != workflow.SHA256(diskBytes) {
			t.Errorf("drifted pin verdict = %v, want changed from the pinned to the disk bytes", drifted)
		}
	})

	// Repin adopts a drifted pin's disk bytes and retires a pin whose ref no
	// longer resolves, by name or as every unresolvable one.
	t.Run("RunRepin dispositions", func(t *testing.T) {
		target := f.boundRunTarget(t)
		f.driftPin(t, target, "contracts/drifted.md")
		f.pinFile(t, target, "contracts/gone.md")
		repinned, err := f.c.RunRepin(ctx, connect.NewRequest(&docketv1.RunRepinRequest{
			Run: target.run, Reason: "adopt and retire", Drop: []string{"contracts/gone.md"}, Token: target.conductor,
		}))
		requireServed(t, "RunRepin with a named drop", err)
		got := repinned.Msg
		if len(got.GetRepinned()) != 1 || got.GetRepinned()[0].GetPath() != "contracts/drifted.md" ||
			got.GetRepinned()[0].GetPinnedSha256() != workflow.SHA256(pinnedBytes) ||
			got.GetRepinned()[0].GetCurrentSha256() != workflow.SHA256(diskBytes) {
			t.Errorf("RunRepin repinned = %v, want contracts/drifted.md moved to the disk bytes", got.GetRepinned())
		}
		if strings.Join(got.GetDropped(), ",") != "contracts/gone.md" {
			t.Errorf("RunRepin dropped = %v, want [contracts/gone.md]", got.GetDropped())
		}
		if sha, ok := f.pinSHA(t, target, "contracts/drifted.md"); !ok || sha != workflow.SHA256(diskBytes) {
			t.Errorf("after RunRepin, contracts/drifted.md is pinned at %q (held %v), want the disk bytes", sha, ok)
		}
		if _, ok := f.pinSHA(t, target, "contracts/gone.md"); ok {
			t.Error("after RunRepin, contracts/gone.md is still pinned, want it retired")
		}

		other := f.boundRunTarget(t)
		f.pinFile(t, other, "contracts/gone.md")
		swept, err := f.c.RunRepin(ctx, connect.NewRequest(&docketv1.RunRepinRequest{
			Run: other.run, Reason: "retire the unresolvable", DropUnresolvable: true, Token: other.conductor,
		}))
		requireServed(t, "RunRepin dropping the unresolvable", err)
		if strings.Join(swept.Msg.GetDropped(), ",") != "contracts/gone.md" {
			t.Errorf("RunRepin dropped = %v, want [contracts/gone.md]", swept.Msg.GetDropped())
		}
		if _, ok := f.pinSHA(t, other, "contracts/gone.md"); ok {
			t.Error("after RunRepin, contracts/gone.md is still pinned, want it retired")
		}

		// A plain repin retires nothing it was not asked to: an unresolvable
		// pin refuses the whole repin and stays held.
		kept := f.boundRunTarget(t)
		f.pinFile(t, kept, "contracts/gone.md")
		before := f.runState(t, kept)
		_, err = f.c.RunRepin(ctx, connect.NewRequest(&docketv1.RunRepinRequest{
			Run: kept.run, Reason: "adopt the edit", Token: kept.conductor,
		}))
		requireRefusal(t, "RunRepin over an unresolvable pin", err, connect.CodeNotFound, output.ErrNotFound)
		if sha, ok := f.pinSHA(t, kept, "contracts/gone.md"); !ok || sha != workflow.SHA256(pinnedBytes) {
			t.Errorf("after a refused RunRepin, contracts/gone.md is pinned at %q (held %v), want the pinned bytes", sha, ok)
		}
		if after := f.runState(t, kept); after != before {
			t.Errorf("a refused RunRepin changed the run %+v -> %+v", before, after)
		}
	})

	// RunReport carries each step's attempt row, the run's per-status step
	// counts, and the artifacts the run recorded.
	t.Run("RunReport ledger", func(t *testing.T) {
		target := f.boundRunTarget(t)
		steps, err := f.st.ListRunSteps(target.runID)
		if err != nil {
			t.Fatalf("ListRunSteps: %v", err)
		}
		var first int
		for _, s := range steps {
			if s.Instance == "first@0" {
				first = s.ID
			}
		}
		f.complete(t, first, f.claim(t, first))

		// The stored step rows are the oracle for each reported attempt count.
		steps, err = f.st.ListRunSteps(target.runID)
		if err != nil {
			t.Fatalf("ListRunSteps: %v", err)
		}
		attempts := map[string]int{}
		for _, s := range steps {
			attempts[s.Instance] = s.Attempt
		}
		if attempts["first@0"] != 1 {
			t.Fatalf("first@0 attempt = %d after one claim, want 1", attempts["first@0"])
		}

		report, err := f.c.RunReport(ctx, connect.NewRequest(&docketv1.RunReportRequest{Run: target.run}))
		requireServed(t, "RunReport", err)
		got := report.Msg
		if len(got.GetSteps()) != len(steps) {
			t.Fatalf("RunReport steps = %v, want one per step of %s", got.GetSteps(), target.run)
		}
		issue := model.FormatID(target.issue)
		for _, s := range got.GetSteps() {
			instance := s.GetRow().GetInstance()
			if instance == "first@0" && s.GetStatus() != store.StepDone {
				t.Errorf("RunReport first@0 status = %q, want %q after its complete", s.GetStatus(), store.StepDone)
			}
			if s.GetStatus() == "" {
				t.Errorf("RunReport step %s carries no status", instance)
			}
			if want, ok := attempts[instance]; !ok || int(s.GetAttempt()) != want {
				t.Errorf("RunReport step %s attempt = %d, want the stored %d", instance, s.GetAttempt(), want)
			}
			if s.GetRow().GetIssue() != issue {
				t.Errorf("RunReport step %s issue = %q, want %s", instance, s.GetRow().GetIssue(), issue)
			}
		}
		// first@0 is done, and second@0, whose one predecessor is done, reads
		// as ready rather than its stored pending.
		counts := map[string]int{store.StepDone: 1, store.StepReady: 1}
		reported := map[string]int{}
		for _, c := range got.GetRun().GetSteps() {
			reported[c.GetStatus()] = int(c.GetCount())
		}
		if !maps.Equal(reported, counts) {
			t.Errorf("RunReport run step counts = %v, want %v", reported, counts)
		}
		var result *docketv1.Artifact
		for _, a := range got.GetArtifacts() {
			if a.GetKind() == "result" {
				result = a
			}
		}
		if result == nil || result.GetId() == "" || result.GetStep() != "first@0" {
			t.Errorf("RunReport artifacts = %v, want first@0's result", got.GetArtifacts())
		}
	})

	// RunResume records the reason it was sent on the run it returns to active.
	t.Run("RunResume reason", func(t *testing.T) {
		target := f.boundRunTarget(t)
		pauseRun(t, f, target)
		resumed, err := f.c.RunResume(ctx, connect.NewRequest(&docketv1.RunResumeRequest{
			Run: target.run, Reason: "the operator answered", Token: target.conductor,
		}))
		requireServed(t, "RunResume", err)
		if got := resumed.Msg.GetRun(); got.GetStatus() != string(model.RunActive) || got.GetReason() != "the operator answered" {
			t.Errorf("RunResume run = %v, want %s active for the reason sent", got, target.run)
		}
		if row, err := f.st.GetRun(target.runID); err != nil || row.Reason != "the operator answered" {
			t.Errorf("after RunResume, %s = %+v, %v; want the reason sent stored", target.run, row, err)
		}
	})

	// RunFactAdd names the event it recorded as EventsList serves it.
	t.Run("RunFactAdd event", func(t *testing.T) {
		target := f.boundRunTarget(t)
		steps, err := f.st.ListRunSteps(target.runID)
		if err != nil || len(steps) == 0 {
			t.Fatalf("ListRunSteps = %v, %v; want the run's steps", steps, err)
		}
		added, err := f.c.RunFactAdd(ctx, connect.NewRequest(&docketv1.RunFactAddRequest{
			Run: target.run, Kind: engine.RunFactStepDeferred, Step: model.FormatStepID(steps[0].ID),
			Cause: engine.DeferralChain, Reason: "behind its predecessor", Token: target.conductor,
		}))
		requireServed(t, "RunFactAdd", err)
		fact := added.Msg.GetEvent()
		listed, err := f.c.EventsList(ctx, connect.NewRequest(&docketv1.EventsListRequest{Run: target.run}))
		requireServed(t, "EventsList", err)
		var stored *docketv1.Event
		for _, e := range listed.Msg.GetItems() {
			if e.GetSeq() == fact.GetSeq() {
				stored = e
			}
		}
		if stored == nil {
			t.Fatalf("EventsList holds no event at RunFactAdd's seq %d", fact.GetSeq())
		}
		if fact.GetKind() != stored.GetKind() || fact.GetRun() != stored.GetRun() {
			t.Errorf("RunFactAdd event = %v, want the stored event %v", fact, stored)
		}
		// A slot RunFactAdd fills must hold what EventsList serves there.
		if s := fact.GetStep(); s != "" && s != stored.GetStep() {
			t.Errorf("RunFactAdd step = %q, want none or the stored step %q", s, stored.GetStep())
		}
		if p := fact.GetPayloadJson(); p != "" && p != stored.GetPayloadJson() {
			t.Errorf("RunFactAdd payload_json = %s, want none or the stored payload %s", p, stored.GetPayloadJson())
		}
	})

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

// TestRunActivateReturnsTheConductorToken pins that a Connect caller gets
// the conductor capability from the activation that mints it, without the
// RunConduct seat-taking that would retire it, and that a re-activation
// mints none.
func TestRunActivateReturnsTheConductorToken(t *testing.T) {
	f := newStepFixture(t)
	ctx := context.Background()
	issue, _, err := f.st.CreateIssueIdempotent(&model.Issue{
		Title: "activation subject", Status: model.StatusTodo, Priority: model.PriorityMedium, Kind: model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent: %v", err)
	}
	run, _, err := f.st.InsertRunWithContextIdempotent(
		store.DefaultProjectID, "", 0, model.NowMS(), store.RunContext{ExecRoot: f.execRoot}, "", issue)
	if err != nil {
		t.Fatalf("InsertRunWithContextIdempotent: %v", err)
	}
	target := runTarget{run: model.FormatRunID(run.ID), runID: run.ID}
	noteAdd := func(token string) error {
		_, err := f.c.RunNoteAdd(ctx, connect.NewRequest(&docketv1.RunNoteAddRequest{
			Run: target.run, Authority: engine.AuthorityOperator, Text: "keep the scope narrow", Token: token,
		}))
		return err
	}

	activated, err := f.c.RunActivate(ctx, connect.NewRequest(&docketv1.RunActivateRequest{Run: target.run}))
	requireServed(t, "RunActivate", err)
	token := activated.Msg.GetToken()
	if token == "" {
		t.Fatalf("RunActivate = %v, want the conductor token the first activation mints", activated.Msg)
	}
	if n := f.runEvents(t, target, engine.EventConductorSeated); n != 0 {
		t.Errorf("after the first activation %s holds %d %s events, want none", target.run, n, engine.EventConductorSeated)
	}
	requireServed(t, "RunNoteAdd with the activation's token", noteAdd(token))

	again, err := f.c.RunActivate(ctx, connect.NewRequest(&docketv1.RunActivateRequest{Run: target.run}))
	requireServed(t, "RunActivate again", err)
	if got := again.Msg.GetToken(); got != "" {
		t.Errorf("a second RunActivate answered token %q, want none", got)
	}
	requireServed(t, "RunNoteAdd with the activation's token after a re-activation", noteAdd(token))
}
