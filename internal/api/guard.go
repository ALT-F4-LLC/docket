package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// GuardGate answers whether a human or vote gate step passed by a decision.
func (s *Service) GuardGate(ctx context.Context, req *connect.Request[docketv1.GuardGateRequest]) (*connect.Response[docketv1.GuardGateResponse], error) {
	return unary(ctx, s, req, decodeGuardGate, verb.GuardGate, encodeGuardGate)
}

// GuardRecord answers whether no open dispatch and no discrepancy exists.
func (s *Service) GuardRecord(ctx context.Context, req *connect.Request[docketv1.GuardRecordRequest]) (*connect.Response[docketv1.GuardRecordResponse], error) {
	return unary(ctx, s, req, decodeGuardRecord, verb.GuardRecord, encodeGuardRecord)
}

// GuardSpawn answers whether a relay may spawn the proposed batch.
func (s *Service) GuardSpawn(ctx context.Context, req *connect.Request[docketv1.GuardSpawnRequest]) (*connect.Response[docketv1.GuardSpawnResponse], error) {
	return unary(ctx, s, req, decodeGuardSpawn, verb.GuardSpawn, encodeGuardSpawn)
}

// GuardStop answers whether no work of the caller's project is still pending.
func (s *Service) GuardStop(ctx context.Context, req *connect.Request[docketv1.GuardStopRequest]) (*connect.Response[docketv1.GuardStopResponse], error) {
	return unary(ctx, s, req, decodeGuardStop, verb.GuardStop, encodeGuardStop)
}

func decodeGuardGate(req *docketv1.GuardGateRequest) (verb.GuardGateInput, error) {
	return verb.GuardGateInput{
		Step:        req.GetStep(),
		Run:         req.GetRun(),
		AllProjects: req.GetAllProjects(),
	}, nil
}

func encodeGuardGate(res verb.GuardGateResult) *docketv1.GuardGateResponse {
	return &docketv1.GuardGateResponse{Decision: guardDecisionMessage(res)}
}

func decodeGuardRecord(req *docketv1.GuardRecordRequest) (verb.GuardRecordInput, error) {
	return verb.GuardRecordInput{Run: req.GetRun(), AllProjects: req.GetAllProjects()}, nil
}

func encodeGuardRecord(res verb.GuardRecordResult) *docketv1.GuardRecordResponse {
	return &docketv1.GuardRecordResponse{Decision: guardDecisionMessage(res)}
}

// decodeGuardSpawn hands the verb the proposed rows as the JSON array it
// parses. A repeated field cannot tell an empty list from an absent one, so
// no rows reads as none proposed.
func decodeGuardSpawn(req *docketv1.GuardSpawnRequest) (verb.GuardSpawnInput, error) {
	in := verb.GuardSpawnInput{
		Run:          req.GetRun(),
		Active:       req.GetActive(),
		AckReap:      req.GetAckReap(),
		DecidingVote: req.GetDecidingVote(),
		UsageJoin:    req.GetUsageJoin(),
		AllProjects:  req.GetAllProjects(),
	}
	if len(req.GetRows()) == 0 {
		return in, nil
	}
	rows := make([]model.StepRow, 0, len(req.GetRows()))
	for _, row := range req.GetRows() {
		rows = append(rows, stepRowModel(row))
	}
	body, err := json.Marshal(rows)
	if err != nil {
		return verb.GuardSpawnInput{}, fmt.Errorf("encoding the proposed rows: %w", err)
	}
	in.Rows = func() ([]byte, error) { return body, nil }
	return in, nil
}

func encodeGuardSpawn(res verb.GuardSpawnResult) *docketv1.GuardSpawnResponse {
	return &docketv1.GuardSpawnResponse{Decision: guardDecisionMessage(res)}
}

// decodeGuardStop refuses a run: the verb answers over the caller's project
// and has no per-run form, so honoring the field would mean ignoring it.
func decodeGuardStop(req *docketv1.GuardStopRequest) (verb.GuardStopInput, error) {
	if req.GetRun() != "" {
		return verb.GuardStopInput{}, errors.New(
			"GuardStop takes no run: it answers over the caller's project, or every project with all_projects")
	}
	return verb.GuardStopInput{AllProjects: req.GetAllProjects()}, nil
}

func encodeGuardStop(res verb.GuardStopResult) *docketv1.GuardStopResponse {
	return &docketv1.GuardStopResponse{Decision: guardDecisionMessage(res)}
}

func guardDecisionMessage(d verb.GuardDecision) *docketv1.GuardDecision {
	return &docketv1.GuardDecision{Allow: d.Allowed, Reason: d.Reason}
}

// stepRowModel is the inverse of stepRowMessage.
func stepRowModel(msg *docketv1.StepRow) model.StepRow {
	row := model.StepRow{
		Step:     msg.GetStep(),
		Instance: msg.GetInstance(),
		Issue:    msg.GetIssue(),
		Run:      msg.GetRun(),
		Kind:     msg.GetKind(),
		Executor: msg.GetExecutor(),
		Class:    msg.GetClass(),
		Labels:   msg.GetLabels(),
		Size:     msg.GetSize(),
		Voters:   msg.GetVoters(),
		Model:    msg.GetModel(),
		Effort:   msg.GetEffort(),
		Variant:  msg.GetVariant(),
		Proposal: msg.GetProposal(),
	}
	for _, v := range msg.GetVoterAssignments() {
		row.VoterAssignments = append(row.VoterAssignments, model.VoterAssignment{
			Voter: v.GetVoter(), Model: v.GetModel(), Effort: v.GetEffort(), Variant: v.GetVariant(),
		})
	}
	return row
}
