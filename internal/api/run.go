package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// RunAbandon abandons a run, or one issue's work in it, under the run's
// conductor capability.
func (s *Service) RunAbandon(ctx context.Context, req *connect.Request[docketv1.RunAbandonRequest]) (*connect.Response[docketv1.RunAbandonResponse], error) {
	return unary(ctx, s, req, decodeRunAbandon, verb.RunAbandon, encodeRunAbandon)
}

// RunActivate activates a run, or re-activates an active one.
func (s *Service) RunActivate(ctx context.Context, req *connect.Request[docketv1.RunActivateRequest]) (*connect.Response[docketv1.RunActivateResponse], error) {
	return unary(ctx, s, req, decodeRunActivate, verb.RunActivate, encodeRunActivate)
}

// RunBudget reads or sets a run's declared-cost cap.
func (s *Service) RunBudget(ctx context.Context, req *connect.Request[docketv1.RunBudgetRequest]) (*connect.Response[docketv1.RunBudgetResponse], error) {
	return unary(ctx, s, req, decodeRunBudget, verb.RunBudget, encodeRunBudget)
}

// RunConduct takes a run's conductor seat, minting a fresh capability.
func (s *Service) RunConduct(ctx context.Context, req *connect.Request[docketv1.RunConductRequest]) (*connect.Response[docketv1.RunConductResponse], error) {
	return unary(ctx, s, req, decodeRunConduct, verb.RunConduct, encodeRunConduct)
}

// RunFactAdd records a conductor-observed fact against a run under its
// conductor capability.
func (s *Service) RunFactAdd(ctx context.Context, req *connect.Request[docketv1.RunFactAddRequest]) (*connect.Response[docketv1.RunFactAddResponse], error) {
	return unary(ctx, s, req, decodeRunFactAdd, verb.RunFactAdd, encodeRunFactAdd)
}

// RunIssueAdd attaches issues to a planning or active run.
func (s *Service) RunIssueAdd(ctx context.Context, req *connect.Request[docketv1.RunIssueAddRequest]) (*connect.Response[docketv1.RunIssueAddResponse], error) {
	return unary(ctx, s, req, decodeRunIssueAdd, verb.RunIssueAdd, encodeRunIssueAdd)
}

// RunIssueRemove detaches issues from a planning run.
func (s *Service) RunIssueRemove(ctx context.Context, req *connect.Request[docketv1.RunIssueRemoveRequest]) (*connect.Response[docketv1.RunIssueRemoveResponse], error) {
	return unary(ctx, s, req, decodeRunIssueRemove, verb.RunIssueRemove, encodeRunIssueRemove)
}

// RunNoteAdd records a note for a run's later packets under its conductor
// capability.
func (s *Service) RunNoteAdd(ctx context.Context, req *connect.Request[docketv1.RunNoteAddRequest]) (*connect.Response[docketv1.RunNoteAddResponse], error) {
	return unary(ctx, s, req, decodeRunNoteAdd, verb.RunNoteAdd, encodeRunNoteAdd)
}

// RunNoteList lists a run's notes.
func (s *Service) RunNoteList(ctx context.Context, req *connect.Request[docketv1.RunNoteListRequest]) (*connect.Response[docketv1.RunNoteListResponse], error) {
	return unary(ctx, s, req, decodeRunNoteList, verb.RunNoteList, encodeRunNoteList)
}

// RunPause parks an active run at waiting-human under its conductor
// capability.
func (s *Service) RunPause(ctx context.Context, req *connect.Request[docketv1.RunPauseRequest]) (*connect.Response[docketv1.RunPauseResponse], error) {
	return unary(ctx, s, req, decodeRunPause, verb.RunPause, encodeRunPause)
}

// RunRefreshBody re-snapshots one issue's description in a live run.
func (s *Service) RunRefreshBody(ctx context.Context, req *connect.Request[docketv1.RunRefreshBodyRequest]) (*connect.Response[docketv1.RunRefreshBodyResponse], error) {
	return unary(ctx, s, req, decodeRunRefreshBody, verb.RunRefreshBody, encodeRunRefreshBody)
}

// RunRefreshFiles re-snapshots one issue's attached files in a live run.
func (s *Service) RunRefreshFiles(ctx context.Context, req *connect.Request[docketv1.RunRefreshFilesRequest]) (*connect.Response[docketv1.RunRefreshFilesResponse], error) {
	return unary(ctx, s, req, decodeRunRefreshFiles, verb.RunRefreshFiles, encodeRunRefreshFiles)
}

// RunRefreshScope re-snapshots one issue's declared scope in a live run.
func (s *Service) RunRefreshScope(ctx context.Context, req *connect.Request[docketv1.RunRefreshScopeRequest]) (*connect.Response[docketv1.RunRefreshScopeResponse], error) {
	return unary(ctx, s, req, decodeRunRefreshScope, verb.RunRefreshScope, encodeRunRefreshScope)
}

// RunRepin adopts current disk bytes for a run's drifted pins under its
// conductor capability.
func (s *Service) RunRepin(ctx context.Context, req *connect.Request[docketv1.RunRepinRequest]) (*connect.Response[docketv1.RunRepinResponse], error) {
	return unary(ctx, s, req, decodeRunRepin, verb.RunRepin, encodeRunRepin)
}

// RunReport rolls up a run's ledger.
func (s *Service) RunReport(ctx context.Context, req *connect.Request[docketv1.RunReportRequest]) (*connect.Response[docketv1.RunReportResponse], error) {
	return unary(ctx, s, req, decodeRunReport, verb.RunReport, encodeRunReport)
}

// RunResume returns a parked run to active under its conductor capability.
func (s *Service) RunResume(ctx context.Context, req *connect.Request[docketv1.RunResumeRequest]) (*connect.Response[docketv1.RunResumeResponse], error) {
	return unary(ctx, s, req, decodeRunResume, verb.RunResume, encodeRunResume)
}

// RunStart creates a run in planning with its issues attached.
func (s *Service) RunStart(ctx context.Context, req *connect.Request[docketv1.RunStartRequest]) (*connect.Response[docketv1.RunStartResponse], error) {
	return unary(ctx, s, req, decodeRunStart, verb.RunStart, encodeRunStart)
}

// RunStatus lists the caller's runs, or reads one run's status.
func (s *Service) RunStatus(ctx context.Context, req *connect.Request[docketv1.RunStatusRequest]) (*connect.Response[docketv1.RunStatusResponse], error) {
	return unary(ctx, s, req, decodeRunStatus, verb.RunStatus, encodeRunStatus)
}

// RunVerifyPins checks a run's pin set against what each ref resolves to now.
func (s *Service) RunVerifyPins(ctx context.Context, req *connect.Request[docketv1.RunVerifyPinsRequest]) (*connect.Response[docketv1.RunVerifyPinsResponse], error) {
	return unary(ctx, s, req, decodeRunVerifyPins, verb.RunVerifyPins, encodeRunVerifyPins)
}

// decodeRunAbandon leaves the authority unset: the request has no authority
// field, and the daemon never asserts one for the caller, so the verb refuses
// the abandon as VALIDATION_ERROR.
func decodeRunAbandon(req *docketv1.RunAbandonRequest) (verb.RunAbandonInput, error) {
	return verb.RunAbandonInput{
		Run: req.GetRun(), Issue: req.GetIssue(), Reason: req.GetReason(),
		ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

// encodeRunAbandon answers the issue form with the run's reference and the
// status the rollup left it in.
func encodeRunAbandon(res verb.RunAbandonResult) *docketv1.RunAbandonResponse {
	if o := res.IssueOutcome; o != nil {
		return &docketv1.RunAbandonResponse{Run: &docketv1.Run{Id: o.Run, Status: o.RunStatus}, Worktrees: o.Worktrees}
	}
	return &docketv1.RunAbandonResponse{Run: runMessage(res.Run), Worktrees: res.Worktrees}
}

func decodeRunActivate(req *docketv1.RunActivateRequest) (verb.RunActivateInput, error) {
	return verb.RunActivateInput{
		Run: req.GetRun(), Pins: req.GetPin(), DryRun: req.GetDryRun(), Reason: req.GetReason(),
	}, nil
}

// encodeRunActivate leaves pins empty: activation reports how many pins it
// recorded, not their verdicts.
func encodeRunActivate(res verb.RunActivateResult) *docketv1.RunActivateResponse {
	a := res.Activation
	if a == nil {
		return &docketv1.RunActivateResponse{}
	}
	return &docketv1.RunActivateResponse{Run: runMessage(a.Run), Steps: int32(a.StepsCreated), Token: a.ConductorToken}
}

func decodeRunBudget(req *docketv1.RunBudgetRequest) (verb.RunBudgetInput, error) {
	in := verb.RunBudgetInput{Run: req.GetRun(), Reason: req.GetReason()}
	if req.Set != nil {
		set := req.GetSet()
		in.Set = &set
	}
	return in, nil
}

// encodeRunBudget reports as accrued the spend the cap is compared against.
func encodeRunBudget(res verb.RunBudgetResult) *docketv1.RunBudgetResponse {
	b := res.Budget
	if b == nil {
		return &docketv1.RunBudgetResponse{}
	}
	return &docketv1.RunBudgetResponse{Run: b.Run, Budget: b.Budget, Accrued: b.Spend, UsageBudget: b.UsageBudget}
}

func decodeRunConduct(req *docketv1.RunConductRequest) (verb.RunConductInput, error) {
	return verb.RunConductInput{Run: req.GetRun()}, nil
}

func encodeRunConduct(res verb.RunConductResult) *docketv1.RunConductResponse {
	c := res.Conduct
	if c == nil {
		return &docketv1.RunConductResponse{}
	}
	return &docketv1.RunConductResponse{Run: c.Run.Ref(), Token: c.Token}
}

func decodeRunFactAdd(req *docketv1.RunFactAddRequest) (verb.RunFactAddInput, error) {
	return verb.RunFactAddInput{
		Run: req.GetRun(), Kind: req.GetKind(), Reason: req.GetReason(),
		Proposal: req.GetProposal(), Voter: req.GetVoter(), Step: req.GetStep(), Cause: req.GetCause(),
		ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

// encodeRunFactAdd names the recorded event by seq, kind and run, and leaves
// step and payload_json empty: the fact holds a step id and its own fields,
// where the stored event holds the step's instance and the ruling's data.
// EventsList serves the event itself at that seq.
func encodeRunFactAdd(res verb.RunFactAddResult) *docketv1.RunFactAddResponse {
	f := res.Fact
	if f == nil {
		return &docketv1.RunFactAddResponse{}
	}
	return &docketv1.RunFactAddResponse{Event: &docketv1.Event{Seq: f.Seq, Kind: f.Kind, Run: f.Run}}
}

func decodeRunIssueAdd(req *docketv1.RunIssueAddRequest) (verb.RunIssueAddInput, error) {
	return verb.RunIssueAddInput{Run: req.GetRun(), Issues: req.GetIssues()}, nil
}

func encodeRunIssueAdd(res verb.RunIssueAddResult) *docketv1.RunIssueAddResponse {
	return &docketv1.RunIssueAddResponse{Run: runWithIssues(res.Run, res.Issues)}
}

func decodeRunIssueRemove(req *docketv1.RunIssueRemoveRequest) (verb.RunIssueRemoveInput, error) {
	return verb.RunIssueRemoveInput{Run: req.GetRun(), Issues: req.GetIssues()}, nil
}

func encodeRunIssueRemove(res verb.RunIssueRemoveResult) *docketv1.RunIssueRemoveResponse {
	return &docketv1.RunIssueRemoveResponse{Run: runWithIssues(res.Run, res.Issues)}
}

func decodeRunNoteAdd(req *docketv1.RunNoteAddRequest) (verb.RunNoteAddInput, error) {
	text := req.GetText()
	return verb.RunNoteAddInput{
		Run: req.GetRun(), Authority: req.GetAuthority(),
		Text:           func() (string, error) { return text, nil },
		ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

func encodeRunNoteAdd(res verb.RunNoteAddResult) *docketv1.RunNoteAddResponse {
	n := res.Note
	if n == nil {
		return &docketv1.RunNoteAddResponse{}
	}
	return &docketv1.RunNoteAddResponse{Note: runNoteMessage(*n)}
}

func decodeRunNoteList(req *docketv1.RunNoteListRequest) (verb.RunNoteListInput, error) {
	return verb.RunNoteListInput{Run: req.GetRun()}, nil
}

func encodeRunNoteList(res verb.RunNoteListResult) *docketv1.RunNoteListResponse {
	items := make([]*docketv1.RunNote, 0, len(res.Notes))
	for _, n := range res.Notes {
		items = append(items, runNoteMessage(n))
	}
	return &docketv1.RunNoteListResponse{Items: items, Total: int32(len(items))}
}

// decodeRunPause leaves the authority unset, as decodeRunAbandon does.
func decodeRunPause(req *docketv1.RunPauseRequest) (verb.RunPauseInput, error) {
	return verb.RunPauseInput{
		Run: req.GetRun(), Reason: req.GetReason(), ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

func encodeRunPause(res verb.RunPauseResult) *docketv1.RunPauseResponse {
	return &docketv1.RunPauseResponse{Run: runMessage(res.Run)}
}

func decodeRunRefreshBody(req *docketv1.RunRefreshBodyRequest) (verb.RunRefreshBodyInput, error) {
	return verb.RunRefreshBodyInput{
		Run: req.GetRun(), Issue: req.GetIssue(), Reason: req.GetReason(),
		ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

func encodeRunRefreshBody(res verb.RunRefreshBodyResult) *docketv1.RunRefreshBodyResponse {
	o := res.Outcome
	if o == nil {
		return &docketv1.RunRefreshBodyResponse{}
	}
	return &docketv1.RunRefreshBodyResponse{Run: o.Run, Issue: o.Issue, Steps: o.Steps}
}

func decodeRunRefreshFiles(req *docketv1.RunRefreshFilesRequest) (verb.RunRefreshFilesInput, error) {
	return verb.RunRefreshFilesInput{
		Run: req.GetRun(), Issue: req.GetIssue(), Reason: req.GetReason(),
		ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

func encodeRunRefreshFiles(res verb.RunRefreshFilesResult) *docketv1.RunRefreshFilesResponse {
	o := res.Outcome
	if o == nil {
		return &docketv1.RunRefreshFilesResponse{}
	}
	return &docketv1.RunRefreshFilesResponse{Run: o.Run, Issue: o.Issue, Steps: o.Steps}
}

func decodeRunRefreshScope(req *docketv1.RunRefreshScopeRequest) (verb.RunRefreshScopeInput, error) {
	return verb.RunRefreshScopeInput{
		Run: req.GetRun(), Issue: req.GetIssue(), Reason: req.GetReason(),
		ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

func encodeRunRefreshScope(res verb.RunRefreshScopeResult) *docketv1.RunRefreshScopeResponse {
	o := res.Outcome
	if o == nil {
		return &docketv1.RunRefreshScopeResponse{}
	}
	return &docketv1.RunRefreshScopeResponse{Run: o.Run, Issue: o.Issue, Steps: o.Steps}
}

func decodeRunRepin(req *docketv1.RunRepinRequest) (verb.RunRepinInput, error) {
	return verb.RunRepinInput{
		Run: req.GetRun(), Reason: req.GetReason(), Drop: req.GetDrop(), DropUnresolvable: req.GetDropUnresolvable(),
		ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

// encodeRunRepin lists the pins the closure added among the repinned, each
// with no pinned hash, and names each dropped pin by its ref.
func encodeRunRepin(res verb.RunRepinResult) *docketv1.RunRepinResponse {
	o := res.Outcome
	if o == nil {
		return &docketv1.RunRepinResponse{}
	}
	msg := &docketv1.RunRepinResponse{Run: o.Run}
	repinned := func(ref, old, adopted string) {
		msg.Repinned = append(msg.Repinned, &docketv1.PinVerdict{Path: ref, PinnedSha256: old, CurrentSha256: adopted})
	}
	for _, c := range o.Repinned {
		repinned(c.Ref, c.OldSHA256, c.NewSHA256)
	}
	for _, c := range o.Added {
		repinned(c.Ref, c.OldSHA256, c.NewSHA256)
	}
	for _, c := range o.Dropped {
		msg.Dropped = append(msg.Dropped, c.Ref)
	}
	return msg
}

func decodeRunReport(req *docketv1.RunReportRequest) (verb.RunReportInput, error) {
	return verb.RunReportInput{Run: req.GetRun()}, nil
}

// encodeRunReport carries the run with its step counts, each step's attempt
// row, the artifact index and the spend. gates stays empty: the report rolls
// gate verdicts up per gate, and GateResult has no slot for the counts.
func encodeRunReport(res verb.RunReportResult) *docketv1.RunReportResponse {
	r := res.Report
	if r == nil {
		return &docketv1.RunReportResponse{}
	}
	run := runMessage(r.Run)
	if run != nil {
		run.Steps = statusCountMessages(r.Steps)
	}
	msg := &docketv1.RunReportResponse{Run: run, AccruedCost: r.Budget.Spend, MeasuredUsage: r.Budget.UsageSpend}
	for _, a := range r.Attempts {
		msg.Steps = append(msg.Steps, &docketv1.Step{
			Row:    &docketv1.StepRow{Step: a.Step, Instance: a.Instance, Issue: a.Issue},
			Status: a.Status, Attempt: int32(a.Attempts),
		})
	}
	for _, a := range r.Artifacts {
		msg.Artifacts = append(msg.Artifacts, &docketv1.Artifact{Id: a.Artifact, Step: a.Producer, Kind: a.Kind})
	}
	return msg
}

func decodeRunResume(req *docketv1.RunResumeRequest) (verb.RunResumeInput, error) {
	return verb.RunResumeInput{
		Run: req.GetRun(), Reason: req.GetReason(), ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

// encodeRunResume carries the run row alone: the response has no slot for the
// pin drift the verb read after the resume, which RunVerifyPins reports.
func encodeRunResume(res verb.RunResumeResult) *docketv1.RunResumeResponse {
	return &docketv1.RunResumeResponse{Run: runMessage(res.Run)}
}

func decodeRunStart(req *docketv1.RunStartRequest) (verb.RunStartInput, error) {
	in := verb.RunStartInput{Request: req.GetRequest(), Issues: req.GetIssues()}
	if req.Budget != nil {
		budget := req.GetBudget()
		in.Budget = &budget
	}
	if req.UsageBudget != nil {
		usage := req.GetUsageBudget()
		in.UsageBudget = &usage
	}
	return in, nil
}

func encodeRunStart(res verb.RunStartResult) *docketv1.RunStartResponse {
	return &docketv1.RunStartResponse{Run: runMessage(res.Run)}
}

func decodeRunStatus(req *docketv1.RunStatusRequest) (verb.RunStatusInput, error) {
	return verb.RunStatusInput{Run: req.GetRun(), All: req.GetAll(), Limit: int(req.GetLimit())}, nil
}

// encodeRunStatus answers one run's read as a one-item list carrying the
// run's step counts. The Run message has no slot for the rest of the detail
// (pins, pin drift, activation head, unexpanded issues, retry-eligible steps),
// so a client reads drift from RunVerifyPins. A list is truncated when it
// holds fewer runs than it matched, which only a limit causes.
func encodeRunStatus(res verb.RunStatusResult) *docketv1.RunStatusResponse {
	if d := res.Detail; d != nil {
		run := runMessage(d.Run)
		if run == nil {
			return &docketv1.RunStatusResponse{}
		}
		run.Steps = statusCountMessages(d.Steps)
		return &docketv1.RunStatusResponse{Items: []*docketv1.Run{run}, Total: 1}
	}
	items := make([]*docketv1.Run, 0, len(res.Runs))
	for _, r := range res.Runs {
		items = append(items, runMessage(r))
	}
	return &docketv1.RunStatusResponse{
		Items: items, Total: int32(res.Total), Truncated: res.Total > len(items),
	}
}

func decodeRunVerifyPins(req *docketv1.RunVerifyPinsRequest) (verb.RunVerifyPinsInput, error) {
	return verb.RunVerifyPinsInput{Run: req.GetRun()}, nil
}

// encodeRunVerifyPins also encodes the report an unsound refusal carries.
func encodeRunVerifyPins(res verb.RunVerifyPinsResult) *docketv1.RunVerifyPinsResponse {
	r := res.Report
	if r == nil {
		return &docketv1.RunVerifyPinsResponse{}
	}
	msg := &docketv1.RunVerifyPinsResponse{Run: r.Run, Clean: r.Sound()}
	for _, p := range r.Pins {
		msg.Pins = append(msg.Pins, pinVerdictMessage(p))
	}
	return msg
}

// pinVerdictMessage is a checked pin on the wire. It names the pin by its
// ref, which every pin kind carries.
func pinVerdictMessage(p engine.PinVerdict) *docketv1.PinVerdict {
	return &docketv1.PinVerdict{
		Path: p.Ref, PinnedSha256: p.Pinned, CurrentSha256: p.Found, Verdict: p.Status,
	}
}

func runNoteMessage(n engine.RunNote) *docketv1.RunNote {
	return &docketv1.RunNote{
		Ordinal: int32(n.ID), Authority: n.Authority, Text: n.Text, CreatedAtMs: n.RecordedAtMS,
	}
}

// runMessage is a run row on the wire, nil for no run. It carries no issues
// or step counts, which the row does not hold.
func runMessage(r *model.Run) *docketv1.Run {
	if r == nil {
		return nil
	}
	return &docketv1.Run{
		Id: r.Ref(), Request: r.Request, Status: string(r.Status), Reason: r.Reason,
		Budget: r.Budget, UsageBudget: r.UsageBudget, ActivatedAtMs: r.ActivatedAtMS,
		CreatedAtMs: r.CreatedAtMS, UpdatedAtMs: r.UpdatedAtMS, Version: int32(r.RowVersion),
		ExecRoot: r.ExecRoot, Branch: r.Branch, CommitSha: r.CommitSHA, Hostname: r.Hostname,
	}
}

// runWithIssues is r with the ids of the issues attached to it.
func runWithIssues(r *model.Run, issues []int) *docketv1.Run {
	msg := runMessage(r)
	if msg == nil {
		return nil
	}
	for _, id := range issues {
		msg.Issues = append(msg.Issues, model.FormatID(id))
	}
	return msg
}

func statusCountMessages(counts []model.StatusCount) []*docketv1.StatusCount {
	msgs := make([]*docketv1.StatusCount, 0, len(counts))
	for _, c := range counts {
		msgs = append(msgs, &docketv1.StatusCount{Status: c.Status, Count: int32(c.Count)})
	}
	return msgs
}
