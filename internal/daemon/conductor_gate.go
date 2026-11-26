package daemon

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// The engine lets any caller rule on a run that holds no conductor
// capability. The daemon keeps that only for the Unix socket: over its HTTP
// listener every RPC whose verb checks the capability is refused with
// AUTH_ERROR on such a run until someone takes the seat with `run conduct`,
// which ConductRun's transport rule permits there. The seat records who took
// it, so an HTTP ruling is never unattributed.
//
// Each such RPC is overridden below and resolves the run its verb authorizes,
// with the parser that verb uses, before calling requireConducted. A form of
// an RPC that skips the verb's capability check passes through unresolved.
//
// A planning run holds no capability either and is refused the same way:
// conducting it first is how `run abandon` or `run note add` reaches it over
// HTTP, and its first activation then keeps that capability.

// runResolver names the run a request rules on. ok is false for a form that
// is not a ruling and for a reference the verb itself refuses (malformed, or
// naming no step or proposal), so the wrapped service answers it as before.
type runResolver func(st store.Store) (runID int, ok bool, err error)

func ungated(store.Store) (int, bool, error) { return 0, false, nil }

func byRun(ref string) runResolver {
	return func(store.Store) (int, bool, error) {
		runID, err := model.ParseRunID(ref)
		return runID, err == nil, nil
	}
}

func byStep(ref string) runResolver {
	return func(st store.Store) (int, bool, error) {
		stepID, err := model.ParseStepID(ref)
		if err != nil {
			return 0, false, nil
		}
		step, err := st.GetStep(stepID)
		switch {
		case errors.Is(err, store.ErrStepNotFound):
			return 0, false, nil
		case err != nil:
			return 0, false, err
		}
		return step.RunID, true, nil
	}
}

// byVoteStepProposal names the run of the vote step that owns the proposal;
// committing any other proposal is not a ruling.
func byVoteStepProposal(ref string) runResolver {
	return func(st store.Store) (int, bool, error) {
		proposalID, err := model.ParseProposalID(ref)
		if err != nil {
			return 0, false, nil
		}
		return engine.VoteProposalRunStore(st, proposalID)
	}
}

// requireConducted refuses verb with AUTH_ERROR when the request arrived over
// HTTP and resolve names a run that holds no conductor capability. A
// resolution error refuses too, so a lookup that fails never reads as a run
// that needs no seat. A daemon serving no store has no run to check.
func (c conductorRule) requireConducted(ctx context.Context, verb string, resolve runResolver) error {
	if api.TransportOf(ctx) != TransportHTTP || c.store == nil {
		return nil
	}
	runID, ok, err := resolve(c.store)
	if err != nil {
		return output.NewConnectError(output.ErrGeneral, err)
	}
	if !ok {
		return nil
	}
	switch hash, err := c.store.RunConductorHash(runID); {
	case errors.Is(err, store.ErrRunNotFound):
		// The verb reports the missing run as NOT_FOUND.
	case err != nil:
		return output.NewConnectError(output.ErrGeneral, err)
	case hash == "":
		ref := model.FormatRunID(runID)
		return output.NewConnectError(output.ErrAuth, &engine.Error{
			Code: engine.CodeAuth, Err: engine.ErrNotConductor, Message: fmt.Sprintf(
				"%s holds no conductor capability, and over the daemon's HTTP "+
					"listener %s requires one: take the seat with `docket run "+
					"conduct %s` against the HTTP URL and supply the token it returns",
				ref, verb, ref)})
	}
	return nil
}

// gated runs next unless requireConducted refuses the request.
func gated[Req, Res any](
	c conductorRule, ctx context.Context, req *connect.Request[Req], verb string, resolve runResolver,
	next func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error),
) (*connect.Response[Res], error) {
	if err := c.requireConducted(ctx, verb, resolve); err != nil {
		return nil, err
	}
	return next(ctx, req)
}

func (c conductorRule) DispatchAbandon(ctx context.Context, req *connect.Request[docketv1.DispatchAbandonRequest]) (*connect.Response[docketv1.DispatchAbandonResponse], error) {
	return gated(c, ctx, req, "dispatch abandon", byRun(req.Msg.GetRun()), c.DocketServiceHandler.DispatchAbandon)
}

func (c conductorRule) DispatchClose(ctx context.Context, req *connect.Request[docketv1.DispatchCloseRequest]) (*connect.Response[docketv1.DispatchCloseResponse], error) {
	resolve := runResolver(ungated)
	if req.Msg.GetSkipIntegrationCheck() != "" {
		resolve = byRun(req.Msg.GetRun())
	}
	return gated(c, ctx, req, "dispatch close --skip-integration-check", resolve, c.DocketServiceHandler.DispatchClose)
}

func (c conductorRule) DispatchWaiveTarget(ctx context.Context, req *connect.Request[docketv1.DispatchWaiveTargetRequest]) (*connect.Response[docketv1.DispatchWaiveTargetResponse], error) {
	return gated(c, ctx, req, "dispatch waive-target", byRun(req.Msg.GetRun()), c.DocketServiceHandler.DispatchWaiveTarget)
}

func (c conductorRule) RunAbandon(ctx context.Context, req *connect.Request[docketv1.RunAbandonRequest]) (*connect.Response[docketv1.RunAbandonResponse], error) {
	return gated(c, ctx, req, "run abandon", byRun(req.Msg.GetRun()), c.DocketServiceHandler.RunAbandon)
}

func (c conductorRule) RunFactAdd(ctx context.Context, req *connect.Request[docketv1.RunFactAddRequest]) (*connect.Response[docketv1.RunFactAddResponse], error) {
	return gated(c, ctx, req, "run fact add", byRun(req.Msg.GetRun()), c.DocketServiceHandler.RunFactAdd)
}

func (c conductorRule) RunNoteAdd(ctx context.Context, req *connect.Request[docketv1.RunNoteAddRequest]) (*connect.Response[docketv1.RunNoteAddResponse], error) {
	return gated(c, ctx, req, "run note add", byRun(req.Msg.GetRun()), c.DocketServiceHandler.RunNoteAdd)
}

func (c conductorRule) RunPause(ctx context.Context, req *connect.Request[docketv1.RunPauseRequest]) (*connect.Response[docketv1.RunPauseResponse], error) {
	return gated(c, ctx, req, "run pause", byRun(req.Msg.GetRun()), c.DocketServiceHandler.RunPause)
}

func (c conductorRule) RunRefreshBody(ctx context.Context, req *connect.Request[docketv1.RunRefreshBodyRequest]) (*connect.Response[docketv1.RunRefreshBodyResponse], error) {
	return gated(c, ctx, req, "run refresh-body", byRun(req.Msg.GetRun()), c.DocketServiceHandler.RunRefreshBody)
}

func (c conductorRule) RunRefreshFiles(ctx context.Context, req *connect.Request[docketv1.RunRefreshFilesRequest]) (*connect.Response[docketv1.RunRefreshFilesResponse], error) {
	return gated(c, ctx, req, "run refresh-files", byRun(req.Msg.GetRun()), c.DocketServiceHandler.RunRefreshFiles)
}

func (c conductorRule) RunRefreshScope(ctx context.Context, req *connect.Request[docketv1.RunRefreshScopeRequest]) (*connect.Response[docketv1.RunRefreshScopeResponse], error) {
	return gated(c, ctx, req, "run refresh-scope", byRun(req.Msg.GetRun()), c.DocketServiceHandler.RunRefreshScope)
}

func (c conductorRule) RunRepin(ctx context.Context, req *connect.Request[docketv1.RunRepinRequest]) (*connect.Response[docketv1.RunRepinResponse], error) {
	return gated(c, ctx, req, "run repin", byRun(req.Msg.GetRun()), c.DocketServiceHandler.RunRepin)
}

func (c conductorRule) RunResume(ctx context.Context, req *connect.Request[docketv1.RunResumeRequest]) (*connect.Response[docketv1.RunResumeResponse], error) {
	return gated(c, ctx, req, "run resume", byRun(req.Msg.GetRun()), c.DocketServiceHandler.RunResume)
}

func (c conductorRule) StepAnnotate(ctx context.Context, req *connect.Request[docketv1.StepAnnotateRequest]) (*connect.Response[docketv1.StepAnnotateResponse], error) {
	resolve := runResolver(ungated)
	if req.Msg.GetIntegratedSha() != "" {
		resolve = byStep(req.Msg.GetStep())
	}
	return gated(c, ctx, req, "step annotate --integrated-sha", resolve, c.DocketServiceHandler.StepAnnotate)
}

func (c conductorRule) StepApprove(ctx context.Context, req *connect.Request[docketv1.StepApproveRequest]) (*connect.Response[docketv1.StepApproveResponse], error) {
	return gated(c, ctx, req, "step approve", byStep(req.Msg.GetStep()), c.DocketServiceHandler.StepApprove)
}

func (c conductorRule) StepHold(ctx context.Context, req *connect.Request[docketv1.StepHoldRequest]) (*connect.Response[docketv1.StepHoldResponse], error) {
	return gated(c, ctx, req, "step hold", byStep(req.Msg.GetStep()), c.DocketServiceHandler.StepHold)
}

func (c conductorRule) StepReap(ctx context.Context, req *connect.Request[docketv1.StepReapRequest]) (*connect.Response[docketv1.StepReapResponse], error) {
	return gated(c, ctx, req, "step reap", byStep(req.Msg.GetStep()), c.DocketServiceHandler.StepReap)
}

func (c conductorRule) StepReject(ctx context.Context, req *connect.Request[docketv1.StepRejectRequest]) (*connect.Response[docketv1.StepRejectResponse], error) {
	return gated(c, ctx, req, "step reject", byStep(req.Msg.GetStep()), c.DocketServiceHandler.StepReject)
}

func (c conductorRule) StepResolve(ctx context.Context, req *connect.Request[docketv1.StepResolveRequest]) (*connect.Response[docketv1.StepResolveResponse], error) {
	return gated(c, ctx, req, "step resolve", byStep(req.Msg.GetStep()), c.DocketServiceHandler.StepResolve)
}

func (c conductorRule) VoteCommit(ctx context.Context, req *connect.Request[docketv1.VoteCommitRequest]) (*connect.Response[docketv1.VoteCommitResponse], error) {
	return gated(c, ctx, req, "vote commit", byVoteStepProposal(req.Msg.GetProposal()), c.DocketServiceHandler.VoteCommit)
}
