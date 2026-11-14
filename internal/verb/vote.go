package verb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// voteStepKeyPrefix is the head of the engine's vote-step idempotency keys
// (engine.voteStepScopePrefix). The engine opens a vote step's proposal under
// such a key and adopts whatever proposal already holds it, so a caller key in
// this family would let the caller pre-seed the step's ballot.
const voteStepKeyPrefix = "vote-step:"

// ParseProposalRef parses a proposal reference ("DKT-V5" or a bare number)
// and reports a malformed one as VALIDATION_ERROR.
func ParseProposalRef(ref string) (int, error) {
	id, err := model.ParseProposalID(ref)
	if err != nil {
		return 0, newError(fmt.Errorf("invalid proposal ID: %w", err), output.ErrValidation)
	}
	return id, nil
}

// VoteProjectResult reports the project VoteProject registered. Registered is
// as in DocCreateResult.
type VoteProjectResult struct {
	Registered *model.Project
}

// VoteProject resolves the project as VoteCreate, VoteCast and VoteClose do,
// so it may register the caller's anchored project. A caller that asks a
// person for the vote's fields calls it first, so an unanchored caller is
// refused before anyone types.
func VoteProject(ctx context.Context, env Env) (VoteProjectResult, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil {
		return VoteProjectResult{}, err
	}
	var result VoteProjectResult
	result.Registered, err = registeredProject(env, projectID, created)
	return result, err
}

// VoteCreateInput is the proposal to open. CreatedBy defaults to env.Author.
// A replayed IdempotencyKey returns the proposal it first created.
type VoteCreateInput struct {
	Description      string
	Rationale        string
	Criticality      string
	Voters           int
	Threshold        float64
	CreatedBy        string
	DomainTags       []string
	FilesChanged     []string
	EscalationReason string
	Sealed           bool
	IdempotencyKey   string
}

// VoteCreateResult is the created proposal. Registered is as in
// DocCreateResult.
type VoteCreateResult struct {
	Proposal   *model.Proposal
	Registered *model.Project
}

// Validate runs the checks VoteCreate makes on its input alone, reporting the
// first failure as VALIDATION_ERROR. A caller whose own input checks must
// report after these calls it before them.
func (in VoteCreateInput) Validate() error {
	if in.Voters < 1 {
		return newError(fmt.Errorf("--voters must be >= 1"), output.ErrValidation)
	}
	if in.Threshold <= 0.0 || in.Threshold > 1.0 {
		return newError(fmt.Errorf("--threshold must be in (0.0, 1.0]"), output.ErrValidation)
	}
	if err := model.ValidateCriticality(model.Criticality(in.Criticality)); err != nil {
		return newError(err, output.ErrValidation)
	}
	if strings.HasPrefix(in.IdempotencyKey, voteStepKeyPrefix) {
		return newError(fmt.Errorf(
			"--idempotency-key must not begin with %q: that key family links workflow vote steps to their proposals",
			voteStepKeyPrefix), output.ErrValidation)
	}
	return nil
}

// VoteCreate opens a proposal in the caller's project. It writes into that
// project, so it may register an anchored one.
func VoteCreate(ctx context.Context, env Env, in VoteCreateInput) (VoteCreateResult, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil {
		return VoteCreateResult{}, err
	}
	var result VoteCreateResult
	if result.Registered, err = registeredProject(env, projectID, created); err != nil {
		return result, err
	}
	if err := in.Validate(); err != nil {
		return result, err
	}

	createdBy := in.CreatedBy
	if createdBy == "" {
		createdBy = env.Author
	}
	var escalationReason *string
	if in.EscalationReason != "" {
		escalationReason = &in.EscalationReason
	}

	id, err := env.Store.CreateProposalIdempotent(&model.Proposal{
		ProjectID:        projectID,
		Description:      in.Description,
		Rationale:        in.Rationale,
		Criticality:      model.Criticality(in.Criticality),
		Status:           model.ProposalStatusOpen,
		RequiredVoters:   in.Voters,
		Threshold:        in.Threshold,
		CreatedBy:        createdBy,
		DomainTags:       in.DomainTags,
		FilesChanged:     in.FilesChanged,
		EscalationReason: escalationReason,
		Sealed:           in.Sealed,
	}, in.IdempotencyKey)
	if errors.Is(err, store.ErrConflict) {
		return result, newError(err, output.ErrConflict)
	}
	if err != nil {
		return result, newError(fmt.Errorf("creating proposal: %w", err), output.ErrGeneral)
	}

	if result.Proposal, err = env.Store.GetProposal(id); err != nil {
		return result, newError(fmt.Errorf("fetching created proposal: %w", err), output.ErrGeneral)
	}
	return result, nil
}

// VoteCastInput is one seat's ballot. Voter defaults to env.Author.
// FindingsJSON, Metadata and Usage are JSON documents, empty when the seat
// sends none.
type VoteCastInput struct {
	Proposal        string
	Voter           string
	Role            string
	Verdict         string
	Confidence      float64
	DomainRelevance float64
	Findings        string
	FindingsJSON    string
	Summary         string
	Metadata        string
	Usage           string
}

// VoteCastResult is the recorded cast and the tally after it. Registered is
// as in DocCreateResult.
//
// Attempted reports that the ballot passed every check here and went to the
// engine, whether or not the engine then recorded it.
//
// Cast beside an error is a cast that committed whose panel-event write then
// failed: the caller reports the cast together with the error, since a retry
// would be refused. A cast that committed and decided the proposal but whose
// vote step failed to route returns no Cast, only the error.
type VoteCastResult struct {
	ProposalID int
	Attempted  bool
	Cast       *store.CastVoteResult
	Registered *model.Project
}

// VoteCast records a ballot on a proposal, and routes the vote step behind it
// when the ballot decides it. It is a write, so it may register the caller's
// anchored project.
//
// Every refusal here comes before the cast, so a refused ballot writes no
// vote: a cast has no amend path.
func VoteCast(ctx context.Context, env Env, in VoteCastInput) (VoteCastResult, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil {
		return VoteCastResult{}, err
	}
	var result VoteCastResult
	if result.Registered, err = registeredProject(env, projectID, created); err != nil {
		return result, err
	}

	proposalID, err := ParseProposalRef(in.Proposal)
	if err != nil {
		return result, err
	}
	result.ProposalID = proposalID

	var findingsJSON *model.Findings
	if in.FindingsJSON != "" {
		var f model.Findings
		if err := json.Unmarshal([]byte(in.FindingsJSON), &f); err != nil {
			return result, newError(fmt.Errorf("--findings-json is not valid JSON: %w", err), output.ErrValidation)
		}
		findingsJSON = &f
	}

	// Evidence references are resolved against the proposal's run before the
	// cast records (DKT-2451): a reference that resolves against nothing is
	// refused by name rather than landing as a citation nobody can follow.
	if err := engine.ValidateCastEvidence(env.Store, proposalID, findingsJSON); err != nil {
		return result, engineError(err)
	}

	if in.Confidence < 0.0 || in.Confidence > 1.0 {
		return result, newError(fmt.Errorf("--confidence must be in [0.0, 1.0]"), output.ErrValidation)
	}
	if in.DomainRelevance < 0.0 || in.DomainRelevance > 1.0 {
		return result, newError(fmt.Errorf("--domain-relevance must be in [0.0, 1.0]"), output.ErrValidation)
	}
	if err := model.ValidateVerdict(model.Verdict(in.Verdict)); err != nil {
		return result, newError(err, output.ErrValidation)
	}
	metadata, err := parseVoteMetadata(in.Metadata)
	if err != nil {
		return result, newError(err, output.ErrValidation)
	}
	usage, err := parseVoteUsage(in.Usage)
	if err != nil {
		return result, newError(err, output.ErrValidation)
	}

	voter := in.Voter
	if voter == "" {
		voter = env.Author
	}

	// The engine's cast path (engine/vote_cast.go) reads the vote step's
	// pinned `roster`, `weighting` and `recuse` before the tally runs: a
	// name off a strict roster or the reviewed step's own executor is refused
	// as VALIDATION_ERROR with no vote written.
	result.Attempted = true
	cast, err := engine.CastVoteStore(env.Store, &model.Vote{
		ProposalID:      proposalID,
		VoterName:       voter,
		VoterRole:       in.Role,
		Verdict:         model.Verdict(in.Verdict),
		Confidence:      in.Confidence,
		DomainRelevance: in.DomainRelevance,
		Findings:        in.Findings,
		FindingsJSON:    findingsJSON,
		Summary:         in.Summary,
		Metadata:        metadata,
		Usage:           usage,
	})
	// A result beside the error is a cast that committed, followed by a
	// panel-event write that failed (engine.CastVoteStore).
	var afterCommit error
	if err != nil && cast != nil {
		afterCommit, err = err, nil
	}
	if err != nil {
		return result, castError(err, proposalID, voter)
	}

	// The cast that reaches quorum finalizes the proposal, and the vote step
	// standing behind it must then route: `next` refuses while a dispatch is
	// open, so the deciding cast is the observing invocation.
	if cast.QuorumReached {
		if err := engine.NewEngine().DriveVoteProposalStore(env.Store, proposalID, model.NowMS()); err != nil {
			if afterCommit == nil {
				return result, newError(fmt.Errorf(
					"the vote recorded and the proposal decided, but "+
						"routing the vote step failed: %w", err), output.ErrGeneral)
			}
			afterCommit = fmt.Errorf("%w; routing the vote step also failed: %v", afterCommit, err)
		}
	}

	result.Cast = cast
	if afterCommit != nil {
		return result, newError(afterCommit, output.ErrGeneral)
	}
	return result, nil
}

// castError maps a refused cast to its error code.
func castError(err error, proposalID int, voter string) error {
	if _, ok := engine.CodeOf(err); ok {
		return engineError(err)
	}
	label := model.FormatProposalID(proposalID)
	if e := notFound(err, "proposal "+label); e != nil {
		return e
	}
	if errors.Is(err, store.ErrConflict) {
		return newError(fmt.Errorf(
			"conflict: voter %q has already voted on %s, or proposal is already finalized",
			voter, label), output.ErrConflict)
	}
	return newError(fmt.Errorf("casting vote: %w", err), output.ErrGeneral)
}

// parseVoteMetadata decodes a vote's opaque metadata bag: an empty input is
// nil, and anything but a JSON object is refused. The bag is free text that
// core stores whole and never reads a key out of.
//
// The size cap is the storage invariant store.VoteMetadataMaxBytes, applied
// here to the ENCODED bag, the quantity the column measures, so the refusal
// arrives as VALIDATION_ERROR before any transaction opens. The caller's raw
// text differs from the encoding in both directions (escaping of `<`, `>`
// and `&`; dropped whitespace), so a raw-text cap would disagree with the
// column.
func parseVoteMetadata(raw string) (map[string]any, error) {
	bag, err := engine.DecodeMetadataBag(raw, "--metadata")
	if err != nil {
		return nil, err
	}
	if bag == nil {
		return nil, nil
	}

	encoded, err := json.Marshal(bag)
	if err != nil {
		return nil, fmt.Errorf("--metadata cannot be re-encoded: %w", err)
	}
	if len(encoded) > store.VoteMetadataMaxBytes {
		return nil, fmt.Errorf(
			"--metadata encodes to %d bytes, over the %d-byte cap; record the detail in --findings instead",
			len(encoded), store.VoteMetadataMaxBytes)
	}
	return bag, nil
}

// parseVoteUsage decodes a seat's spend report through store.ParseUsage, the
// rules the step ledger's writer applies, into the map model.Vote carries.
// Empty means the seat reports nothing.
func parseVoteUsage(raw string) (map[string]float64, error) {
	rows, err := store.ParseUsage(raw)
	if err != nil {
		return nil, fmt.Errorf("--usage: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		out[r.Unit] = r.Quantity
	}
	return out, nil
}

// VoteCloseInput names the open proposal to close and why.
type VoteCloseInput struct {
	Proposal string
	Reason   string
}

// VoteCloseResult is the closed proposal's id. Registered is as in
// DocCreateResult.
type VoteCloseResult struct {
	ProposalID int
	Registered *model.Project
}

// VoteClose closes an open proposal without a tally (DKT-114). A decided
// proposal, and one an engine vote step opened, are refused. It is a write,
// so it may register the caller's anchored project.
func VoteClose(ctx context.Context, env Env, in VoteCloseInput) (VoteCloseResult, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil {
		return VoteCloseResult{}, err
	}
	var result VoteCloseResult
	if result.Registered, err = registeredProject(env, projectID, created); err != nil {
		return result, err
	}

	proposalID, err := ParseProposalRef(in.Proposal)
	if err != nil {
		return result, err
	}
	label := model.FormatProposalID(proposalID)
	if in.Reason == "" {
		return result, newError(
			fmt.Errorf("--reason is required: a closure without a why is the audit gap this verb exists to close"),
			output.ErrValidation)
	}

	stepOwned, err := engine.IsVoteStepProposalStore(env.Store, proposalID)
	if err != nil {
		return result, newError(err, output.ErrGeneral)
	}
	if stepOwned {
		return result, newError(fmt.Errorf(
			"proposal %s belongs to a vote step; move the run past it with `docket step resolve` — closing the proposal underneath the step would not route it",
			label), output.ErrConflict)
	}

	if err := env.Store.CloseProposal(proposalID, in.Reason); err != nil {
		if e := notFound(err, "proposal "+label); e != nil {
			return result, e
		}
		if errors.Is(err, store.ErrConflict) {
			return result, newError(err, output.ErrConflict)
		}
		return result, newError(fmt.Errorf("closing proposal: %w", err), output.ErrGeneral)
	}
	result.ProposalID = proposalID
	return result, nil
}

// VoteCommitInput names the approved proposal to commit and the outcome to
// record on it.
type VoteCommitInput struct {
	Proposal         string
	Outcome          string
	EscalationReason string
	// ConductorToken returns the run's conductor capability. It is called
	// only when the proposal belongs to a vote step whose run is bound to
	// one, so a transport whose token channel can block is never read for a
	// commit that needs no token.
	ConductorToken func() string
}

// VoteCommitResult is the committed proposal's id. Registered is as in
// DocCreateResult.
type VoteCommitResult struct {
	ProposalID int
	Registered *model.Project
}

// VoteCommit commits an approved proposal. A vote step's proposal is routed
// by its run, so committing it out of band is a conductor's ruling: a bound
// run requires its capability. It is a write, so it may register the
// caller's anchored project.
func VoteCommit(ctx context.Context, env Env, in VoteCommitInput) (VoteCommitResult, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil {
		return VoteCommitResult{}, err
	}
	var result VoteCommitResult
	if result.Registered, err = registeredProject(env, projectID, created); err != nil {
		return result, err
	}

	proposalID, err := ParseProposalRef(in.Proposal)
	if err != nil {
		return result, err
	}

	runID, stepOwned, err := engine.VoteProposalRunStore(env.Store, proposalID)
	if err != nil {
		return result, newError(err, output.ErrGeneral)
	}
	if stepOwned {
		// The binding look is advisory: AuthorizeConductorStore re-reads the
		// capability and refuses an empty token on a bound run.
		var token string
		if in.ConductorToken != nil {
			if hash, err := env.Store.RunConductorHash(runID); err == nil && hash != "" {
				token = in.ConductorToken()
			}
		}
		if err := engine.AuthorizeConductorStore(env.Store, runID, token, "vote commit"); err != nil {
			return result, engineError(err)
		}
	}

	if err := env.Store.CommitProposal(proposalID, in.Outcome, in.EscalationReason); err != nil {
		if e := notFound(err, "proposal "+model.FormatProposalID(proposalID)); e != nil {
			return result, e
		}
		if errors.Is(err, store.ErrConflict) {
			return result, newError(err, output.ErrConflict)
		}
		return result, newError(fmt.Errorf("committing proposal: %w", err), output.ErrGeneral)
	}
	result.ProposalID = proposalID
	return result, nil
}

// VoteResultInput names the proposal whose tally to report.
type VoteResultInput struct {
	Proposal string
}

// VoteResultResult is the proposal and every vote cast on it, in full: a
// transport projects a sealed open proposal's votes before reporting them.
type VoteResultResult struct {
	Proposal *model.Proposal
	Votes    []*model.Vote
}

// VoteResult reads a proposal's tally. It is a read, so it never registers a
// project.
func VoteResult(ctx context.Context, env Env, in VoteResultInput) (VoteResultResult, error) {
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return VoteResultResult{}, err
	}
	proposal, votes, err := proposalWithVotes(env, in.Proposal)
	return VoteResultResult{Proposal: proposal, Votes: votes}, err
}

// VoteShowInput names the proposal to show.
type VoteShowInput struct {
	Proposal string
}

// VoteShowResult is the proposal, its votes in full as on VoteResultResult,
// and the ids of the issues and docs linked to it.
type VoteShowResult struct {
	Proposal     *model.Proposal
	Votes        []*model.Vote
	LinkedIssues []int
	LinkedDocs   []int
}

// VoteShow reads a proposal with its votes and links. It is a read, so it
// never registers a project.
func VoteShow(ctx context.Context, env Env, in VoteShowInput) (VoteShowResult, error) {
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return VoteShowResult{}, err
	}
	proposal, votes, err := proposalWithVotes(env, in.Proposal)
	if err != nil {
		return VoteShowResult{}, err
	}
	result := VoteShowResult{Proposal: proposal, Votes: votes}

	if result.LinkedIssues, err = env.Store.GetProposalIssues(proposal.ID); err != nil {
		return VoteShowResult{}, newError(fmt.Errorf("fetching linked issues: %w", err), output.ErrGeneral)
	}
	if result.LinkedDocs, err = env.Store.GetProposalDocs(proposal.ID); err != nil {
		return VoteShowResult{}, newError(fmt.Errorf("fetching linked docs: %w", err), output.ErrGeneral)
	}
	return result, nil
}

// proposalWithVotes reads the proposal ref names and its votes. A missing
// proposal is reported by ref as the caller gave it.
func proposalWithVotes(env Env, ref string) (*model.Proposal, []*model.Vote, error) {
	id, err := ParseProposalRef(ref)
	if err != nil {
		return nil, nil, err
	}
	proposal, err := env.Store.GetProposal(id)
	if err != nil {
		if e := notFound(err, "proposal "+ref); e != nil {
			return nil, nil, e
		}
		return nil, nil, newError(fmt.Errorf("fetching proposal: %w", err), output.ErrGeneral)
	}
	votes, err := env.Store.GetProposalVotes(id)
	if err != nil {
		return nil, nil, newError(fmt.Errorf("fetching votes: %w", err), output.ErrGeneral)
	}
	return proposal, votes, nil
}

// VoteListInput filters the caller's proposals. An empty Status lists open
// proposals only, unless All is set. VoteCounts asks for each listed
// proposal's cast count, which costs one read per proposal.
type VoteListInput struct {
	Status      string
	Criticality string
	DomainTag   string
	All         bool
	Limit       int
	VoteCounts  bool
}

// VoteListResult is the listed proposals and the pre-limit count of every
// match. VoteCounts maps each listed proposal's id to its cast count, and is
// nil unless the input asked for it.
type VoteListResult struct {
	Proposals  []*model.Proposal
	Total      int
	VoteCounts map[int]int
}

// VoteList lists the caller's proposals. It is a read, so it never registers
// a project.
func VoteList(ctx context.Context, env Env, in VoteListInput) (VoteListResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return VoteListResult{}, err
	}
	if in.Status != "" {
		if err := model.ValidateProposalStatus(model.ProposalStatus(in.Status)); err != nil {
			return VoteListResult{}, newError(err, output.ErrValidation)
		}
	}
	if in.Criticality != "" {
		if err := model.ValidateCriticality(model.Criticality(in.Criticality)); err != nil {
			return VoteListResult{}, newError(err, output.ErrValidation)
		}
	}
	status := in.Status
	if !in.All && status == "" {
		status = string(model.ProposalStatusOpen)
	}

	proposals, total, err := env.Store.ListProposals(projectID, status, in.Criticality, in.DomainTag, in.Limit)
	if err != nil {
		return VoteListResult{}, newError(fmt.Errorf("listing proposals: %w", err), output.ErrGeneral)
	}
	if proposals == nil {
		proposals = []*model.Proposal{}
	}
	result := VoteListResult{Proposals: proposals, Total: total}
	if !in.VoteCounts {
		return result, nil
	}

	result.VoteCounts = make(map[int]int, len(proposals))
	for _, p := range proposals {
		votes, err := env.Store.GetProposalVotes(p.ID)
		if err != nil {
			return VoteListResult{}, newError(fmt.Errorf("fetching vote counts: fetching votes for %s: %w",
				model.FormatProposalID(p.ID), err), output.ErrGeneral)
		}
		result.VoteCounts[p.ID] = len(votes)
	}
	return result, nil
}

// VoteLinkInput names the proposal and the issue to link.
type VoteLinkInput struct {
	Proposal string
	Issue    string
}

// VoteLinkResult is the linked pair. Registered is as in DocCreateResult.
type VoteLinkResult struct {
	ProposalID int
	IssueID    int
	Registered *model.Project
}

// VoteLink links a proposal to an issue, then records the panel's
// vote-opened event on every run the link makes attribute it. It is a
// write, so it may register the caller's anchored project.
func VoteLink(ctx context.Context, env Env, in VoteLinkInput) (VoteLinkResult, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil {
		return VoteLinkResult{}, err
	}
	var result VoteLinkResult
	if result.Registered, err = registeredProject(env, projectID, created); err != nil {
		return result, err
	}
	proposalID, issueID, err := parseProposalLink(in.Proposal, in.Issue)
	if err != nil {
		return result, err
	}

	if err := env.Store.LinkProposalIssue(proposalID, issueID); err != nil {
		if e := notFound(err, "proposal or issue"); e != nil {
			return result, e
		}
		if errors.Is(err, store.ErrConflict) {
			return result, newError(fmt.Errorf("link already exists"), output.ErrConflict)
		}
		return result, newError(fmt.Errorf("linking proposal to issue: %w", err), output.ErrGeneral)
	}
	if err := engine.RecordConductorPanelOpenedStore(env.Store, proposalID); err != nil {
		return result, newError(fmt.Errorf("the link recorded, but recording %s's run events failed: %w",
			model.FormatProposalID(proposalID), err), output.ErrGeneral)
	}
	result.ProposalID, result.IssueID = proposalID, issueID
	return result, nil
}

// VoteUnlinkInput names the proposal and the issue to unlink.
type VoteUnlinkInput struct {
	Proposal string
	Issue    string
}

// VoteUnlinkResult is the unlinked pair. Registered is as in
// DocCreateResult.
type VoteUnlinkResult struct {
	ProposalID int
	IssueID    int
	Registered *model.Project
}

// VoteUnlink removes a proposal-issue link. It is a write, so it may
// register the caller's anchored project.
func VoteUnlink(ctx context.Context, env Env, in VoteUnlinkInput) (VoteUnlinkResult, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil {
		return VoteUnlinkResult{}, err
	}
	var result VoteUnlinkResult
	if result.Registered, err = registeredProject(env, projectID, created); err != nil {
		return result, err
	}
	proposalID, issueID, err := parseProposalLink(in.Proposal, in.Issue)
	if err != nil {
		return result, err
	}

	if err := env.Store.UnlinkProposalIssue(proposalID, issueID); err != nil {
		if e := notFound(err, "link"); e != nil {
			return result, e
		}
		return result, newError(fmt.Errorf("unlinking proposal from issue: %w", err), output.ErrGeneral)
	}
	result.ProposalID, result.IssueID = proposalID, issueID
	return result, nil
}

// parseProposalLink parses the two ends of a proposal-issue link, the
// proposal first.
func parseProposalLink(proposal, issue string) (proposalID, issueID int, err error) {
	if proposalID, err = ParseProposalRef(proposal); err != nil {
		return 0, 0, err
	}
	if issueID, err = ParseIssueRef(issue); err != nil {
		return 0, 0, err
	}
	return proposalID, issueID, nil
}

// VoteBackfillUsageInput is a relay's measured spend for a proposal's seats.
// An empty Source records the rows as engine.UsageSourceBackfilled.
type VoteBackfillUsageInput struct {
	Proposal string
	Rows     []engine.VoteBackfillRow
	Source   string
}

// VoteBackfillUsageResult counts what was recorded: the rows, the distinct
// seats they cover, and the source every row carries. Registered is as in
// DocCreateResult.
type VoteBackfillUsageResult struct {
	ProposalID int
	Rows       int
	Seats      int
	Source     string
	Registered *model.Project
}

// VoteBackfillUsage records seat spend a relay measured after the casts
// landed (DKT-115), in one transaction. Each row attaches to its seat's
// cast, so a seat that never cast refuses the batch. It is a write, so it
// may register the caller's anchored project.
func VoteBackfillUsage(ctx context.Context, env Env, in VoteBackfillUsageInput) (VoteBackfillUsageResult, error) {
	projectID, created, err := env.ResolveProject(ProjectMayRegister)
	if err != nil {
		return VoteBackfillUsageResult{}, err
	}
	var result VoteBackfillUsageResult
	if result.Registered, err = registeredProject(env, projectID, created); err != nil {
		return result, err
	}
	proposalID, err := ParseProposalRef(in.Proposal)
	if err != nil {
		return result, err
	}

	if err := engine.NewEngine().BackfillVoteUsageStore(
		env.Store, proposalID, in.Rows, in.Source, model.NowMS()); err != nil {
		return result, engineError(err)
	}

	seats := make(map[string]bool, len(in.Rows))
	for _, r := range in.Rows {
		seats[r.Voter] = true
	}
	result.ProposalID, result.Rows, result.Seats = proposalID, len(in.Rows), len(seats)
	result.Source = in.Source
	if result.Source == "" {
		result.Source = engine.UsageSourceBackfilled
	}
	return result, nil
}
