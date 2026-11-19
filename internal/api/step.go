package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// StepAnnotate merges metadata onto a finished step, or records a verified
// integration for it under the run's conductor capability.
func (s *Service) StepAnnotate(ctx context.Context, req *connect.Request[docketv1.StepAnnotateRequest]) (*connect.Response[docketv1.StepAnnotateResponse], error) {
	return unary(ctx, s, req, decodeStepAnnotate, verb.StepAnnotate, encodeStepAnnotate)
}

// StepApprove approves a gate step under the run's conductor capability.
func (s *Service) StepApprove(ctx context.Context, req *connect.Request[docketv1.StepApproveRequest]) (*connect.Response[docketv1.StepApproveResponse], error) {
	return unary(ctx, s, req, decodeStepApprove, verb.StepApprove, encodeStepApprove)
}

// StepArtifact reads one artifact in full.
func (s *Service) StepArtifact(ctx context.Context, req *connect.Request[docketv1.StepArtifactRequest]) (*connect.Response[docketv1.StepArtifactResponse], error) {
	return unary(ctx, s, req, decodeStepArtifact, verb.StepArtifact, encodeStepArtifact)
}

// StepArtifacts lists the artifacts a step produced, without their bodies.
func (s *Service) StepArtifacts(ctx context.Context, req *connect.Request[docketv1.StepArtifactsRequest]) (*connect.Response[docketv1.StepArtifactsResponse], error) {
	return unary(ctx, s, req, decodeStepArtifacts, verb.StepArtifacts, encodeStepArtifacts)
}

// StepClaim takes or re-mints a step's lease.
func (s *Service) StepClaim(ctx context.Context, req *connect.Request[docketv1.StepClaimRequest]) (*connect.Response[docketv1.StepClaimResponse], error) {
	return unary(ctx, s, req, decodeStepClaim, verb.StepClaim, encodeStepClaim)
}

// StepComplete records a step's artifact with the token holding its lease.
func (s *Service) StepComplete(ctx context.Context, req *connect.Request[docketv1.StepCompleteRequest]) (*connect.Response[docketv1.StepCompleteResponse], error) {
	return unary(ctx, s, req, decodeStepComplete, verb.StepComplete, encodeStepComplete)
}

// StepContext re-emits a step's context bundle.
func (s *Service) StepContext(ctx context.Context, req *connect.Request[docketv1.StepContextRequest]) (*connect.Response[docketv1.StepContextResponse], error) {
	return unary(ctx, s, req, decodeStepContext, verb.StepContext, encodeStepContext)
}

// StepFail reports a step failed with the token holding its lease.
func (s *Service) StepFail(ctx context.Context, req *connect.Request[docketv1.StepFailRequest]) (*connect.Response[docketv1.StepFailResponse], error) {
	return unary(ctx, s, req, decodeStepFail, verb.StepFail, encodeStepFail)
}

// StepGates lists a step's recorded gate results.
func (s *Service) StepGates(ctx context.Context, req *connect.Request[docketv1.StepGatesRequest]) (*connect.Response[docketv1.StepGatesResponse], error) {
	return unary(ctx, s, req, decodeStepGates, verb.StepGates, encodeStepGates)
}

// StepHeartbeat extends the lease the request's token holds.
func (s *Service) StepHeartbeat(ctx context.Context, req *connect.Request[docketv1.StepHeartbeatRequest]) (*connect.Response[docketv1.StepHeartbeatResponse], error) {
	return unary(ctx, s, req, decodeStepHeartbeat, verb.StepHeartbeat, encodeStepHeartbeat)
}

// StepHold parks one pending step for the operator under the run's conductor
// capability.
func (s *Service) StepHold(ctx context.Context, req *connect.Request[docketv1.StepHoldRequest]) (*connect.Response[docketv1.StepHoldResponse], error) {
	return unary(ctx, s, req, decodeStepHold, verb.StepHold, encodeStepHold)
}

// StepList lists a run's or an issue's steps.
func (s *Service) StepList(ctx context.Context, req *connect.Request[docketv1.StepListRequest]) (*connect.Response[docketv1.StepListResponse], error) {
	return unary(ctx, s, req, decodeStepList, verb.StepList, encodeStepList)
}

// StepReap clears a claimed step's lease under the run's conductor
// capability.
func (s *Service) StepReap(ctx context.Context, req *connect.Request[docketv1.StepReapRequest]) (*connect.Response[docketv1.StepReapResponse], error) {
	return unary(ctx, s, req, decodeStepReap, verb.StepReap, encodeStepReap)
}

// StepReject rejects a gate step under the run's conductor capability.
func (s *Service) StepReject(ctx context.Context, req *connect.Request[docketv1.StepRejectRequest]) (*connect.Response[docketv1.StepRejectResponse], error) {
	return unary(ctx, s, req, decodeStepReject, verb.StepReject, encodeStepReject)
}

// StepRender renders a step's work packet from the shipped template.
func (s *Service) StepRender(ctx context.Context, req *connect.Request[docketv1.StepRenderRequest]) (*connect.Response[docketv1.StepRenderResponse], error) {
	return unary(ctx, s, req, decodeStepRender, verb.StepRender, encodeStepRender)
}

// StepResolve resolves a parked step under the run's conductor capability.
func (s *Service) StepResolve(ctx context.Context, req *connect.Request[docketv1.StepResolveRequest]) (*connect.Response[docketv1.StepResolveResponse], error) {
	return unary(ctx, s, req, decodeStepResolve, verb.StepResolve, encodeStepResolve)
}

// StepShow reads steps at their effective status.
func (s *Service) StepShow(ctx context.Context, req *connect.Request[docketv1.StepShowRequest]) (*connect.Response[docketv1.StepShowResponse], error) {
	return unary(ctx, s, req, decodeStepShow, verb.StepShow, encodeStepShow)
}

// conductorToken hands a ruling verb the request's token as its conductor
// capability source. The verb calls it only for a run bound to a capability.
func conductorToken(token string) func() string {
	return func() string { return token }
}

// errClientTemplate refuses a client-named template: the daemon renders the
// shipped template only, so it never opens a path a client chose.
var errClientTemplate = errors.New("template is not accepted: the daemon renders the default template only")

func decodeStepAnnotate(req *docketv1.StepAnnotateRequest) (verb.StepAnnotateInput, error) {
	return verb.StepAnnotateInput{
		Step: req.GetStep(), Metadata: req.GetMetadataJson(),
		IntegratedSHA: req.GetIntegratedSha(), ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

func encodeStepAnnotate(res verb.StepAnnotateResult) *docketv1.StepAnnotateResponse {
	return &docketv1.StepAnnotateResponse{Step: stepMessage(res.Step)}
}

func decodeStepApprove(req *docketv1.StepApproveRequest) (verb.StepApproveInput, error) {
	return verb.StepApproveInput{
		Step: req.GetStep(), Note: req.GetNote(), Value: req.GetValue(),
		Authority: req.GetAuthority(), AuthorityRef: req.GetAuthorityRef(),
		ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

func encodeStepApprove(res verb.StepApproveResult) *docketv1.StepApproveResponse {
	return &docketv1.StepApproveResponse{Step: stepMessage(res.Step)}
}

// decodeStepArtifact ignores payload: the response carries both the body and
// the payload, and which to show is the client's choice.
func decodeStepArtifact(req *docketv1.StepArtifactRequest) (verb.StepArtifactInput, error) {
	return verb.StepArtifactInput{Artifact: req.GetArtifact()}, nil
}

func encodeStepArtifact(res verb.StepArtifactResult) *docketv1.StepArtifactResponse {
	a := res.Artifact
	if a == nil {
		return &docketv1.StepArtifactResponse{}
	}
	return &docketv1.StepArtifactResponse{Artifact: &docketv1.Artifact{
		Id: a.Artifact, Kind: a.Kind, Body: a.Body, PayloadJson: a.Payload, CreatedAtMs: a.CreatedAtMS,
	}}
}

func decodeStepArtifacts(req *docketv1.StepArtifactsRequest) (verb.StepArtifactsInput, error) {
	return verb.StepArtifactsInput{Step: req.GetStep()}, nil
}

func encodeStepArtifacts(res verb.StepArtifactsResult) *docketv1.StepArtifactsResponse {
	items := make([]*docketv1.Artifact, 0, len(res.Artifacts))
	for _, a := range res.Artifacts {
		items = append(items, &docketv1.Artifact{
			Id: a.Artifact, Step: res.Step, Kind: a.Kind, CreatedAtMs: a.CreatedAtMS,
		})
	}
	return &docketv1.StepArtifactsResponse{Items: items, Total: int32(len(items))}
}

// maxRecoverySecretBytes matches the CLI's cap on a recovery secret file.
const maxRecoverySecretBytes = 4096

// decodeStepClaim refuses what the wire admits and the verb does not check:
// a claim without a recovery secret, which the daemon requires so a re-mint is
// always provable; an oversize secret; and a client template path. The secret
// is forwarded byte for byte and trimmed only to test for emptiness, as the
// CLI does.
func decodeStepClaim(req *docketv1.StepClaimRequest) (verb.StepClaimInput, error) {
	secret := req.GetRecoverySecret()
	if strings.TrimSpace(secret) == "" {
		return verb.StepClaimInput{}, errors.New("recovery_secret is required")
	}
	if len(secret) > maxRecoverySecretBytes {
		return verb.StepClaimInput{}, fmt.Errorf("recovery_secret exceeds %d bytes", maxRecoverySecretBytes)
	}
	if req.GetTemplate() != "" {
		return verb.StepClaimInput{}, errClientTemplate
	}
	in := verb.StepClaimInput{
		Step: req.GetStep(), Owner: req.GetOwner(), Metadata: req.GetMetadataJson(),
		RecoverySecret: secret, Render: req.GetRender(), Executor: req.GetExecutor(),
	}
	if req.TtlMs != nil {
		ttl := time.Duration(req.GetTtlMs()) * time.Millisecond
		in.TTL = &ttl
	}
	if req.CostMultiplier != nil {
		m := req.GetCostMultiplier()
		in.CostMultiplier = &m
	}
	return in, nil
}

// encodeStepClaim carries a claim whose later stage failed too: the verb
// returns it beside its error, and the client reads the token from the error's
// second detail.
func encodeStepClaim(res verb.StepClaimResult) *docketv1.StepClaimResponse {
	c := res.Claim
	if c == nil {
		return &docketv1.StepClaimResponse{}
	}
	msg := &docketv1.StepClaimResponse{
		Step: c.Step, Token: c.Token, LeaseExpiresMs: c.LeaseExpiresMS,
		ReMinted: c.ReMinted, Attempt: int32(c.Attempt), Version: int32(c.RowVersion),
	}
	if c.Context != nil {
		msg.Context = mustBundle(ContextBundle(c.Context))
	}
	if res.Packet != nil {
		msg.Packet = res.Packet.Packet
	}
	return msg
}

// decodeStepComplete forwards worktree as given; the engine computes the diff
// and runs the gates there.
func decodeStepComplete(req *docketv1.StepCompleteRequest) (verb.StepCompleteInput, error) {
	in := verb.StepCompleteInput{
		Step: req.GetStep(), Token: req.GetToken(), WorkDir: req.GetWorktree(),
		Artifact: []byte(req.GetArtifact()), Usage: req.GetUsageJson(), Metadata: req.GetMetadataJson(),
	}
	if req.GetPayloadJson() != "" {
		in.Payload = []byte(req.GetPayloadJson())
	}
	for _, gap := range req.GetGaps() {
		in.Gaps = append(in.Gaps, []byte(gap))
	}
	return in, nil
}

func encodeStepComplete(res verb.StepCompleteResult) *docketv1.StepCompleteResponse {
	msg := &docketv1.StepCompleteResponse{Step: stepMessage(res.Step), Detached: res.Detached}
	for _, g := range res.FailedGates {
		gate := &docketv1.GateResult{Gate: g.Gate, Status: g.Verdict, Output: g.Reason}
		if g.Exit != nil {
			gate.ExitCode = int32(*g.Exit)
		}
		msg.FailedGates = append(msg.FailedGates, gate)
	}
	return msg
}

// decodeStepContext refuses meta: the response has no slot for the bundle's
// per-section byte counts, so honoring it would mean dropping them.
func decodeStepContext(req *docketv1.StepContextRequest) (verb.StepContextInput, error) {
	if req.GetMeta() {
		return verb.StepContextInput{}, errors.New("meta is not served: StepContextResponse has no slot for the byte counts")
	}
	return verb.StepContextInput{Step: req.GetStep(), Live: req.GetLive()}, nil
}

func encodeStepContext(res verb.StepContextResult) *docketv1.StepContextResponse {
	if res.Context == nil {
		return &docketv1.StepContextResponse{}
	}
	return &docketv1.StepContextResponse{Context: mustBundle(ContextBundle(res.Context))}
}

func decodeStepFail(req *docketv1.StepFailRequest) (verb.StepFailInput, error) {
	return verb.StepFailInput{
		Step: req.GetStep(), Token: req.GetToken(), Note: req.GetNote(), Metadata: req.GetMetadataJson(),
	}, nil
}

func encodeStepFail(res verb.StepFailResult) *docketv1.StepFailResponse {
	return &docketv1.StepFailResponse{Step: stepMessage(res.Step)}
}

// decodeStepGates refuses a gate filter, which the verb does not take. full is
// ignored: the response always carries each row's full capture, and how much
// to show is the client's choice.
func decodeStepGates(req *docketv1.StepGatesRequest) (verb.StepGatesInput, error) {
	if len(req.GetGate()) > 0 {
		return verb.StepGatesInput{}, errors.New("gate is not served: StepGates lists every recorded gate")
	}
	return verb.StepGatesInput{Step: req.GetStep()}, nil
}

func encodeStepGates(res verb.StepGatesResult) *docketv1.StepGatesResponse {
	items := make([]*docketv1.GateResult, 0, len(res.Gates))
	for _, g := range res.Gates {
		gate := &docketv1.GateResult{
			Gate: g.Gate, Command: strings.Join(g.Argv, " "), Status: g.Verdict,
			DurationMs: g.DurationMS, Output: g.Output, TargetSha: g.TargetSHA,
			Pre: g.Pre, RecordedAtMs: g.CreatedAtMS,
		}
		if g.Exit != nil {
			gate.ExitCode = int32(*g.Exit)
		}
		items = append(items, gate)
	}
	return &docketv1.StepGatesResponse{Items: items, Total: int32(len(items))}
}

func decodeStepHeartbeat(req *docketv1.StepHeartbeatRequest) (verb.StepHeartbeatInput, error) {
	return verb.StepHeartbeatInput{Step: req.GetStep(), Token: req.GetToken()}, nil
}

func encodeStepHeartbeat(res verb.StepHeartbeatResult) *docketv1.StepHeartbeatResponse {
	return &docketv1.StepHeartbeatResponse{Step: model.FormatStepID(res.Step), LeaseExpiresMs: res.Lease.ExpiresMS}
}

// decodeStepHold refuses an authority: a hold records none, so honoring the
// field would mean ignoring it.
func decodeStepHold(req *docketv1.StepHoldRequest) (verb.StepHoldInput, error) {
	if req.GetAuthority() != "" || req.GetAuthorityRef() != "" {
		return verb.StepHoldInput{}, errors.New("authority is not accepted: a hold records no authority")
	}
	return verb.StepHoldInput{
		Step: req.GetStep(), Reason: req.GetReason(), ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

func encodeStepHold(res verb.StepHoldResult) *docketv1.StepHoldResponse {
	return &docketv1.StepHoldResponse{Step: stepMessage(res.Step)}
}

func decodeStepList(req *docketv1.StepListRequest) (verb.StepListInput, error) {
	return verb.StepListInput{Run: req.GetRun(), Issue: req.GetIssue()}, nil
}

func encodeStepList(res verb.StepListResult) *docketv1.StepListResponse {
	items := make([]*docketv1.Step, 0, len(res.Steps))
	for _, e := range res.Steps {
		items = append(items, &docketv1.Step{
			Row:    &docketv1.StepRow{Step: e.Step, Instance: e.Instance, Issue: e.Issue, Run: e.Run, Kind: e.Kind},
			Status: e.Status, Attempt: int32(e.Attempt), FailedAttempts: int32(e.FailedAttempts),
			ExpectedCost: e.ExpectedCost,
		})
	}
	return &docketv1.StepListResponse{Items: items, Total: int32(len(items))}
}

func decodeStepReap(req *docketv1.StepReapRequest) (verb.StepReapInput, error) {
	return verb.StepReapInput{
		Step: req.GetStep(), Reason: req.GetReason(), ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

func encodeStepReap(res verb.StepReapResult) *docketv1.StepReapResponse {
	return &docketv1.StepReapResponse{Step: stepMessage(res.Step)}
}

func decodeStepReject(req *docketv1.StepRejectRequest) (verb.StepRejectInput, error) {
	return verb.StepRejectInput{
		Step: req.GetStep(), Note: req.GetNote(),
		Authority: req.GetAuthority(), AuthorityRef: req.GetAuthorityRef(),
		ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

func encodeStepReject(res verb.StepRejectResult) *docketv1.StepRejectResponse {
	return &docketv1.StepRejectResponse{Step: stepMessage(res.Step)}
}

func decodeStepRender(req *docketv1.StepRenderRequest) (verb.StepRenderInput, error) {
	if req.GetTemplate() != "" {
		return verb.StepRenderInput{}, errClientTemplate
	}
	return verb.StepRenderInput{Step: req.GetStep(), Executor: req.GetExecutor()}, nil
}

func encodeStepRender(res verb.StepRenderResult) *docketv1.StepRenderResponse {
	if res.Packet == nil {
		return &docketv1.StepRenderResponse{}
	}
	return &docketv1.StepRenderResponse{Packet: res.Packet.Packet}
}

// decodeStepResolve forwards worktree as given; the verb refuses a relative
// path.
func decodeStepResolve(req *docketv1.StepResolveRequest) (verb.StepResolveInput, error) {
	return verb.StepResolveInput{
		Step: req.GetStep(), As: req.GetAs(), Note: req.GetNote(),
		Batch: req.GetBatch(), DropInterposed: req.GetDropInterposed(), Worktree: req.GetWorktree(),
		Authority: req.GetAuthority(), AuthorityRef: req.GetAuthorityRef(),
		ConductorToken: conductorToken(req.GetToken()),
	}, nil
}

func encodeStepResolve(res verb.StepResolveResult) *docketv1.StepResolveResponse {
	if res.Step.Step == "" {
		return &docketv1.StepResolveResponse{}
	}
	return &docketv1.StepResolveResponse{Steps: []*docketv1.Step{stepMessage(res.Step)}}
}

func decodeStepShow(req *docketv1.StepShowRequest) (verb.StepShowInput, error) {
	return verb.StepShowInput{Steps: req.GetSteps()}, nil
}

// encodeStepShow adds what a view knows beyond its row: the stored lease, the
// recorded worktree, and the row version.
func encodeStepShow(res verb.StepShowResult) *docketv1.StepShowResponse {
	steps := make([]*docketv1.Step, 0, len(res.Views))
	for _, view := range res.Views {
		msg := stepMessage(view.Row)
		if view.Owner != "" {
			msg.Lease = &docketv1.Lease{Owner: view.Owner, ExpiresMs: view.ExpiresMS, Attempt: int32(view.Row.Attempt)}
		}
		if view.Step != nil {
			msg.Worktree, msg.Version = view.Step.WorkRoot, int32(view.Step.RowVersion)
		}
		steps = append(steps, msg)
	}
	return &docketv1.StepShowResponse{Steps: steps}
}

// stepMessage is a step row on the wire. A row carries no lease, worktree or
// version, so those stay unset; StepShow adds them from the view.
func stepMessage(row model.StepRow) *docketv1.Step {
	msg := &docketv1.Step{
		Row: stepRowMessage(row), Status: row.Status,
		Attempt: int32(row.Attempt), FailedAttempts: int32(row.FailedAttempts), ExpectedCost: row.ExpectedCost,
	}
	if len(row.Metadata) > 0 {
		msg.MetadataJson = mustJSON(row.Metadata)
	}
	return msg
}

// mustBundle and mustJSON re-encode values decoded from stored JSON, which
// always re-encode. A failure is a broken invariant: it panics rather than
// answer with the field silently dropped, and net/http confines the panic to
// the one request.
func mustBundle(bundle *docketv1.StepContextBundle, err error) *docketv1.StepContextBundle {
	if err != nil {
		panic(fmt.Sprintf("encoding a context bundle: %v", err))
	}
	return bundle
}

func mustJSON(v map[string]any) string {
	body, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("encoding step metadata: %v", err))
	}
	return string(body)
}
