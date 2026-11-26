package api

import (
	"context"
	"encoding/json"
	"fmt"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// defaultVoteThreshold is the approval threshold VoteCreate records when the
// request leaves threshold unset, the default of `vote create --threshold`.
const defaultVoteThreshold = 0.67

func (s *Service) VoteBackfillUsage(ctx context.Context, req *connect.Request[docketv1.VoteBackfillUsageRequest]) (*connect.Response[docketv1.VoteBackfillUsageResponse], error) {
	return unary(ctx, s, req, decodeVoteBackfillUsage, verb.VoteBackfillUsage, encodeVoteBackfillUsage)
}

func (s *Service) VoteCast(ctx context.Context, req *connect.Request[docketv1.VoteCastRequest]) (*connect.Response[docketv1.VoteCastResponse], error) {
	return unary(ctx, s, req, decodeVoteCast, verb.VoteCast, encodeVoteCast)
}

func (s *Service) VoteClose(ctx context.Context, req *connect.Request[docketv1.VoteCloseRequest]) (*connect.Response[docketv1.VoteCloseResponse], error) {
	return unary(ctx, s, req, decodeVoteClose, verb.VoteClose, encodeVoteClose)
}

func (s *Service) VoteCommit(ctx context.Context, req *connect.Request[docketv1.VoteCommitRequest]) (*connect.Response[docketv1.VoteCommitResponse], error) {
	return unary(ctx, s, req, decodeVoteCommit, verb.VoteCommit, encodeVoteCommit)
}

func (s *Service) VoteCreate(ctx context.Context, req *connect.Request[docketv1.VoteCreateRequest]) (*connect.Response[docketv1.VoteCreateResponse], error) {
	return unary(ctx, s, req, decodeVoteCreate, verb.VoteCreate, encodeVoteCreate)
}

func (s *Service) VoteLink(ctx context.Context, req *connect.Request[docketv1.VoteLinkRequest]) (*connect.Response[docketv1.VoteLinkResponse], error) {
	return unary(ctx, s, req, decodeVoteLink, verb.VoteLink, encodeVoteLink)
}

func (s *Service) VoteList(ctx context.Context, req *connect.Request[docketv1.VoteListRequest]) (*connect.Response[docketv1.VoteListResponse], error) {
	return unary(ctx, s, req, decodeVoteList, verb.VoteList, encodeVoteList(int(req.Msg.GetLimit())))
}

func (s *Service) VoteResult(ctx context.Context, req *connect.Request[docketv1.VoteResultRequest]) (*connect.Response[docketv1.VoteResultResponse], error) {
	return unary(ctx, s, req, decodeVoteResult, verb.VoteResult, encodeVoteResult)
}

func (s *Service) VoteShow(ctx context.Context, req *connect.Request[docketv1.VoteShowRequest]) (*connect.Response[docketv1.VoteShowResponse], error) {
	return unary(ctx, s, req, decodeVoteShow, verb.VoteShow, encodeVoteShow)
}

func (s *Service) VoteUnlink(ctx context.Context, req *connect.Request[docketv1.VoteUnlinkRequest]) (*connect.Response[docketv1.VoteUnlinkResponse], error) {
	return unary(ctx, s, req, decodeVoteUnlink, verb.VoteUnlink, encodeVoteUnlink)
}

func decodeVoteBackfillUsage(m *docketv1.VoteBackfillUsageRequest) (verb.VoteBackfillUsageInput, error) {
	in := verb.VoteBackfillUsageInput{Proposal: m.GetProposal(), Source: m.GetSource()}
	if err := setBackfillRows(&in.Rows, m.GetRows()); err != nil {
		return verb.VoteBackfillUsageInput{}, err
	}
	return in, nil
}

// backfillRow is the shape of the verb's backfill row type. Row is inferred
// from VoteBackfillUsageInput.Rows, so this file names no engine type, and a
// change to the row's fields fails the build here.
type backfillRow = struct {
	Voter    string
	Unit     string
	Quantity float64
}

// setBackfillRows sets *dst to a batch source returning rows. A vote's spend
// attaches to a seat's cast, never to a step, so a row naming a step is
// refused rather than recorded without it.
func setBackfillRows[Row ~backfillRow](dst *func() ([]Row, error), rows []*docketv1.UsageRow) error {
	batch := make([]Row, 0, len(rows))
	for i, r := range rows {
		if r.GetStep() != "" {
			return fmt.Errorf("rows[%d].step is set: vote backfill-usage attributes spend to a seat, not a step", i)
		}
		batch = append(batch, Row(backfillRow{Voter: r.GetVoter(), Unit: r.GetUnit(), Quantity: r.GetQuantity()}))
	}
	*dst = func() ([]Row, error) { return batch, nil }
	return nil
}

// encodeVoteBackfillUsage reports every row recorded: the verb records the
// whole batch in one transaction or none of it, so it never skips a row.
func encodeVoteBackfillUsage(r verb.VoteBackfillUsageResult) *docketv1.VoteBackfillUsageResponse {
	return &docketv1.VoteBackfillUsageResponse{Recorded: int32(r.Rows)}
}

func decodeVoteCast(m *docketv1.VoteCastRequest) (verb.VoteCastInput, error) {
	return verb.VoteCastInput{
		Proposal:        m.GetProposal(),
		Voter:           m.GetVoter(),
		Role:            m.GetRole(),
		Verdict:         m.GetVerdict(),
		Confidence:      m.GetConfidence(),
		DomainRelevance: m.GetDomainRelevance(),
		Findings:        m.GetFindings(),
		FindingsJSON:    m.GetFindingsJson(),
		Summary:         m.GetSummary(),
		Metadata:        m.GetMetadataJson(),
		Usage:           m.GetUsageJson(),
	}, nil
}

// encodeVoteCast returns the recorded vote and the proposal's tally after it.
// A refused cast carries only the proposal id.
func encodeVoteCast(r verb.VoteCastResult) *docketv1.VoteCastResponse {
	res := &docketv1.VoteCastResponse{Proposal: proposalRef(r.ProposalID)}
	if r.Cast == nil || res.Proposal == nil {
		return res
	}
	res.Vote = voteMessage(r.Cast.Vote)
	res.Proposal.Status = string(r.Cast.ProposalStatus)
	res.Proposal.RequiredVoters = int32(r.Cast.VotesRequired)
	res.Proposal.WeightedScore = r.Cast.WeightedScore
	return res
}

func decodeVoteClose(m *docketv1.VoteCloseRequest) (verb.VoteCloseInput, error) {
	return verb.VoteCloseInput{Proposal: m.GetProposal(), Reason: m.GetReason()}, nil
}

func encodeVoteClose(r verb.VoteCloseResult) *docketv1.VoteCloseResponse {
	return &docketv1.VoteCloseResponse{Proposal: proposalRef(r.ProposalID)}
}

// decodeVoteCommit hands the verb the request's token as the run's conductor
// capability; the verb reads it only for a vote step's proposal on a bound
// run.
func decodeVoteCommit(m *docketv1.VoteCommitRequest) (verb.VoteCommitInput, error) {
	token := m.GetToken()
	return verb.VoteCommitInput{
		Proposal:         m.GetProposal(),
		Outcome:          m.GetOutcome(),
		EscalationReason: m.GetEscalationReason(),
		ConductorToken:   func() string { return token },
	}, nil
}

func encodeVoteCommit(r verb.VoteCommitResult) *docketv1.VoteCommitResponse {
	return &docketv1.VoteCommitResponse{Proposal: proposalRef(r.ProposalID)}
}

// decodeVoteCreate leaves the creator empty when the request does, so the
// verb records the Docket-Author header instead.
func decodeVoteCreate(m *docketv1.VoteCreateRequest) (verb.VoteCreateInput, error) {
	threshold := defaultVoteThreshold
	if m.Threshold != nil {
		threshold = m.GetThreshold()
	}
	return verb.VoteCreateInput{
		Description:      m.GetDescription(),
		Rationale:        m.GetRationale(),
		Criticality:      m.GetCriticality(),
		Voters:           int(m.GetVoters()),
		Threshold:        threshold,
		CreatedBy:        m.GetCreatedBy(),
		DomainTags:       m.GetDomainTags(),
		FilesChanged:     m.GetFilesChanged(),
		EscalationReason: m.GetEscalationReason(),
		Sealed:           m.GetSealed(),
	}, nil
}

func encodeVoteCreate(r verb.VoteCreateResult) *docketv1.VoteCreateResponse {
	return &docketv1.VoteCreateResponse{Proposal: proposalMessage(r.Proposal, nil)}
}

func decodeVoteLink(m *docketv1.VoteLinkRequest) (verb.VoteLinkInput, error) {
	return verb.VoteLinkInput{Proposal: m.GetProposal(), Issue: m.GetIssue()}, nil
}

func encodeVoteLink(r verb.VoteLinkResult) *docketv1.VoteLinkResponse {
	res := &docketv1.VoteLinkResponse{}
	if r.ProposalID != 0 {
		res.Proposal, res.Issue = model.FormatProposalID(r.ProposalID), model.FormatID(r.IssueID)
	}
	return res
}

func decodeVoteList(m *docketv1.VoteListRequest) (verb.VoteListInput, error) {
	return verb.VoteListInput{
		Status:      m.GetStatus(),
		Criticality: m.GetCriticality(),
		DomainTag:   m.GetDomainTag(),
		All:         m.GetAll(),
		Limit:       int(m.GetLimit()),
	}, nil
}

// encodeVoteList returns the encoder for one VoteList request, whose limit
// decides truncated.
func encodeVoteList(limit int) func(verb.VoteListResult) *docketv1.VoteListResponse {
	return func(r verb.VoteListResult) *docketv1.VoteListResponse {
		items := make([]*docketv1.Proposal, 0, len(r.Proposals))
		for _, p := range r.Proposals {
			items = append(items, proposalMessage(p, nil))
		}
		return &docketv1.VoteListResponse{
			Items:     items,
			Total:     int32(r.Total),
			Truncated: output.IsTruncated(limit, r.Total, len(items)),
		}
	}
}

func decodeVoteResult(m *docketv1.VoteResultRequest) (verb.VoteResultInput, error) {
	return verb.VoteResultInput{Proposal: m.GetProposal()}, nil
}

// encodeVoteResult reports the tally: outcome is the proposal's status, the
// decision the tally reached, and cast counts every vote recorded.
func encodeVoteResult(r verb.VoteResultResult) *docketv1.VoteResultResponse {
	p := r.Proposal
	if p == nil {
		return &docketv1.VoteResultResponse{}
	}
	return &docketv1.VoteResultResponse{
		Proposal:      model.FormatProposalID(p.ID),
		Outcome:       string(p.Status),
		WeightedScore: p.WeightedScore,
		Threshold:     p.Threshold,
		Cast:          int32(len(r.Votes)),
		Required:      int32(p.RequiredVoters),
	}
}

func decodeVoteShow(m *docketv1.VoteShowRequest) (verb.VoteShowInput, error) {
	return verb.VoteShowInput{Proposal: m.GetProposal()}, nil
}

func encodeVoteShow(r verb.VoteShowResult) *docketv1.VoteShowResponse {
	p := proposalMessage(r.Proposal, r.Votes)
	if p != nil {
		for _, id := range r.LinkedIssues {
			p.LinkedIssues = append(p.LinkedIssues, model.FormatID(id))
		}
	}
	return &docketv1.VoteShowResponse{Proposal: p}
}

func decodeVoteUnlink(m *docketv1.VoteUnlinkRequest) (verb.VoteUnlinkInput, error) {
	return verb.VoteUnlinkInput{Proposal: m.GetProposal(), Issue: m.GetIssue()}, nil
}

func encodeVoteUnlink(r verb.VoteUnlinkResult) *docketv1.VoteUnlinkResponse {
	res := &docketv1.VoteUnlinkResponse{}
	if r.ProposalID != 0 {
		res.Proposal, res.Issue = model.FormatProposalID(r.ProposalID), model.FormatID(r.IssueID)
	}
	return res
}

// proposalRef is a proposal carrying only its id, or nil for a result that
// names none.
func proposalRef(id int) *docketv1.Proposal {
	if id == 0 {
		return nil
	}
	return &docketv1.Proposal{Id: model.FormatProposalID(id)}
}

// proposalMessage renders p with votes as model.Proposal's JSON marshaler
// does. While p is sealed and open, each vote carries only who cast and when,
// as the CLI's read verbs project it. A nil proposal is nil.
func proposalMessage(p *model.Proposal, votes []*model.Vote) *docketv1.Proposal {
	if p == nil {
		return nil
	}
	m := &docketv1.Proposal{
		Id:               model.FormatProposalID(p.ID),
		Description:      p.Description,
		Criticality:      string(p.Criticality),
		Status:           string(p.Status),
		RequiredVoters:   int32(p.RequiredVoters),
		Threshold:        p.Threshold,
		WeightedScore:    p.WeightedScore,
		CreatedBy:        p.CreatedBy,
		CreatedAt:        wireTime(p.CreatedAt),
		UpdatedAt:        wireTime(p.UpdatedAt),
		Rationale:        p.Rationale,
		DomainTags:       p.DomainTags,
		FilesChanged:     p.FilesChanged,
		FinalOutcome:     p.FinalOutcome,
		EscalationReason: p.EscalationReason,
		Sealed:           p.Sealed,
	}
	for _, v := range votes {
		if p.SealedOpen() {
			m.Votes = append(m.Votes, &docketv1.Vote{VoterName: v.VoterName, CreatedAt: wireTime(v.CreatedAt)})
			continue
		}
		m.Votes = append(m.Votes, voteMessage(v))
	}
	return m
}

// voteMessage renders v as model.Vote's JSON marshaler does. Each structured
// finding carries its list as severity and its text as title; the proto
// Finding has no field for its evidence references.
func voteMessage(v *model.Vote) *docketv1.Vote {
	if v == nil {
		return nil
	}
	m := &docketv1.Vote{
		Id:              int32(v.ID),
		ProposalId:      model.FormatProposalID(v.ProposalID),
		VoterName:       v.VoterName,
		VoterRole:       v.VoterRole,
		Verdict:         string(v.Verdict),
		Confidence:      v.Confidence,
		DomainRelevance: v.DomainRelevance,
		Findings:        v.Findings,
		Summary:         v.Summary,
		CreatedAt:       wireTime(v.CreatedAt),
	}
	if f := v.FindingsJSON; f != nil {
		for _, list := range []struct {
			severity string
			entries  []model.Finding
		}{{"blocker", f.Blockers}, {"concern", f.Concerns}, {"suggestion", f.Suggestions}} {
			for _, e := range list.entries {
				m.FindingsJson = append(m.FindingsJson, &docketv1.Finding{Severity: list.severity, Title: e.Text})
			}
		}
	}
	// Metadata is a bag decoded from a JSON object, so it always re-encodes.
	if v.Metadata != nil {
		if bag, err := json.Marshal(v.Metadata); err == nil {
			m.MetadataJson = string(bag)
		}
	}
	return m
}
