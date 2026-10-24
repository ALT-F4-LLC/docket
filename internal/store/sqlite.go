package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// sqliteStore is the SQLite Store. Its SQL lives in internal/db; every
// method delegates to the helper of the same name.
//
// It holds two connections to the same file. writer is db.Open's
// single-connection pool, whose Begin is BEGIN IMMEDIATE. reader is a separate
// connection that BeginReadSnapshot uses: while a Tx holds writer's only
// connection, a snapshot opened on writer would wait for it, and in WAL mode a
// deferred read on its own connection needs no lock the writer holds.
type sqliteStore struct {
	writer *sql.DB
	reader *sql.DB
}

// sqliteTx is a transaction on either connection. A snapshot is handed out
// only as a ReadTx, so its write methods are unreachable.
type sqliteTx struct {
	tx *sql.Tx
}

var (
	_ Store = (*sqliteStore)(nil)
	_ Tx    = (*sqliteTx)(nil)
)

// OpenSQLite opens or creates the SQLite database at path. It does not
// migrate the schema; call Initialize and Migrate for that.
func OpenSQLite(path string) (Store, error) {
	writer, err := db.Open(path)
	if err != nil {
		return nil, err
	}
	reader, err := db.OpenReader(path)
	if err != nil {
		writer.Close()
		return nil, err
	}
	return &sqliteStore{writer: writer, reader: reader}, nil
}

func (s *sqliteStore) Close() error {
	return errors.Join(s.reader.Close(), s.writer.Close())
}

func (s *sqliteStore) Begin() (Tx, error) {
	tx, err := s.writer.Begin()
	if err != nil {
		return nil, err
	}
	return &sqliteTx{tx: tx}, nil
}

func (s *sqliteStore) BeginReadSnapshot() (ReadTx, error) {
	tx, err := db.BeginReadSnapshot(s.reader)
	if err != nil {
		return nil, err
	}
	return &sqliteTx{tx: tx}, nil
}

func (t *sqliteTx) Commit() error   { return t.tx.Commit() }
func (t *sqliteTx) Rollback() error { return t.tx.Rollback() }

func (s *sqliteStore) ActionOutputsFor(stepID int, action string) ([]string, error) {
	return db.ActionOutputsFor(s.writer, stepID, action)
}

func (s *sqliteStore) ActionRollup(runID int) ([]db.VerdictCount, error) {
	return db.ActionRollup(s.writer, runID)
}

func (s *sqliteStore) ActionTrail(runID int) ([]db.ResultTrailRow, error) {
	return db.ActionTrail(s.writer, runID)
}

func (s *sqliteStore) AddLabelsToIssue(issueID int, labelNames []string, color string, author string) error {
	return db.AddLabelsToIssue(s.writer, issueID, labelNames, color, author)
}

func (s *sqliteStore) AddRunIssue(runID, issueID int) error {
	return db.AddRunIssue(s.writer, runID, issueID)
}

func (s *sqliteStore) AllProjectPrefixes() ([]string, error) {
	return db.AllProjectPrefixes(s.writer)
}

func (s *sqliteStore) AttachFiles(issueID int, filePaths []string, changedBy string) error {
	return db.AttachFiles(s.writer, issueID, filePaths, changedBy)
}

func (s *sqliteStore) AuthorizeStepRead(id int, token string, nowMS int64) error {
	return db.AuthorizeStepRead(s.writer, id, token, nowMS)
}

func (s *sqliteStore) CascadeDeleteIssue(id int) error {
	return db.CascadeDeleteIssue(s.writer, id)
}

func (s *sqliteStore) CastVote(v *model.Vote) (*db.CastVoteResult, error) {
	return db.CastVote(s.writer, v)
}

func (s *sqliteStore) CastVoteWeighted(v *model.Vote, equal bool) (*db.CastVoteResult, error) {
	return db.CastVoteWeighted(s.writer, v, equal)
}

func (s *sqliteStore) ClaimIssue(id int, owner string, ttlMS int64, nowMS int64) (string, *model.Lease, error) {
	return db.ClaimIssue(s.writer, id, owner, ttlMS, nowMS)
}

func (s *sqliteStore) CloseProposal(id int, reason string) error {
	return db.CloseProposal(s.writer, id, reason)
}

func (s *sqliteStore) CommitProposal(id int, outcome string, escalationReason string) error {
	return db.CommitProposal(s.writer, id, outcome, escalationReason)
}

func (s *sqliteStore) CountActivity(issueID int) (int, error) {
	return db.CountActivity(s.writer, issueID)
}

func (s *sqliteStore) CountByPriority(projectID int) (map[string]int, error) {
	return db.CountByPriority(s.writer, projectID)
}

func (s *sqliteStore) CountByStatus(projectID int) (map[string]int, error) {
	return db.CountByStatus(s.writer, projectID)
}

func (s *sqliteStore) CountIssues(projectID int) (int, error) {
	return db.CountIssues(s.writer, projectID)
}

func (s *sqliteStore) CountRootIssues(projectID int) (int, error) {
	return db.CountRootIssues(s.writer, projectID)
}

func (s *sqliteStore) CreateCommentIdempotent(comment *model.Comment, idempotencyKey string) (int, error) {
	return db.CreateCommentIdempotent(s.writer, comment, idempotencyKey)
}

func (s *sqliteStore) CreateDocCommentIdempotent(c *model.DocComment, idempotencyKey string) (int, error) {
	return db.CreateDocCommentIdempotent(s.writer, c, idempotencyKey)
}

func (s *sqliteStore) CreateDocIdempotent(doc *model.Doc, idempotencyKey string) (int, error) {
	return db.CreateDocIdempotent(s.writer, doc, idempotencyKey)
}

func (s *sqliteStore) CreateIssueIdempotent(issue *model.Issue, labels []string, files []string, idempotencyKey string) (int, bool, error) {
	return db.CreateIssueIdempotent(s.writer, issue, labels, files, idempotencyKey)
}

func (s *sqliteStore) CreateProposalIdempotent(p *model.Proposal, idempotencyKey string) (int, error) {
	return db.CreateProposalIdempotent(s.writer, p, idempotencyKey)
}

func (s *sqliteStore) CreateRelation(rel *model.Relation) (int, error) {
	return db.CreateRelation(s.writer, rel)
}

func (s *sqliteStore) DeleteDoc(id int, cascade bool) error {
	return db.DeleteDoc(s.writer, id, cascade)
}

func (s *sqliteStore) DeleteIssue(id int) error {
	return db.DeleteIssue(s.writer, id)
}

func (s *sqliteStore) DeleteLabel(labelID int, name, author string) ([]int, error) {
	return db.DeleteLabel(s.writer, labelID, name, author)
}

func (s *sqliteStore) DeleteProject(id int) error {
	return db.DeleteProject(s.writer, id)
}

func (s *sqliteStore) DeleteRelation(sourceID, targetID int, relType string) error {
	return db.DeleteRelation(s.writer, sourceID, targetID, relType)
}

func (s *sqliteStore) DeprecateSchema(projectID int, name string, version int, nowMS int64) (*model.Schema, error) {
	return db.DeprecateSchema(s.writer, projectID, name, version, nowMS)
}

func (s *sqliteStore) DeprecateWorkflow(projectID int, name string, version int, nowMS int64) (*model.Workflow, error) {
	return db.DeprecateWorkflow(s.writer, projectID, name, version, nowMS)
}

func (s *sqliteStore) DetachFiles(issueID int, filePaths []string, changedBy string) error {
	return db.DetachFiles(s.writer, issueID, filePaths, changedBy)
}

func (s *sqliteStore) EnsureProject(identity, name string, nowMS int64) (int, error) {
	return db.EnsureProject(s.writer, identity, name, nowMS)
}

func (s *sqliteStore) EnsureProjectCreated(identity, name string, nowMS int64) (int, bool, error) {
	return db.EnsureProjectCreated(s.writer, identity, name, nowMS)
}

func (s *sqliteStore) EventsRetain() (time.Duration, error) {
	return db.EventsRetain(s.writer)
}

func (s *sqliteStore) GateOverrideGrantsForRun(runID int) ([]db.GateOverrideGrant, error) {
	return db.GateOverrideGrantsForRun(s.writer, runID)
}

func (s *sqliteStore) GateResultsForStep(stepID int) ([]db.GateResultRow, error) {
	return db.GateResultsForStep(s.writer, stepID)
}

func (s *sqliteStore) GateRollup(runID int) ([]db.VerdictCount, error) {
	return db.GateRollup(s.writer, runID)
}

func (s *sqliteStore) GateTrail(runID int) ([]db.ResultTrailRow, error) {
	return db.GateTrail(s.writer, runID)
}

func (s *sqliteStore) GetActivity(issueID int, limit int) ([]model.Activity, error) {
	return db.GetActivity(s.writer, issueID, limit)
}

func (s *sqliteStore) GetAllDirectionalRelations() ([]model.Relation, error) {
	return db.GetAllDirectionalRelations(s.writer)
}

func (s *sqliteStore) GetAllRelations(projectID int) ([]model.Relation, error) {
	return db.GetAllRelations(s.writer, projectID)
}

func (s *sqliteStore) GetBatchSubIssueProgress(parentIDs []int) (map[int][2]int, error) {
	return db.GetBatchSubIssueProgress(s.writer, parentIDs)
}

func (s *sqliteStore) GetComment(id int) (*model.Comment, error) {
	return db.GetComment(s.writer, id)
}

func (s *sqliteStore) GetConfig(projectID int, key string) (db.ConfigEntry, error) {
	return db.GetConfig(s.writer, projectID, key)
}

func (s *sqliteStore) GetDoc(id int) (*model.Doc, error) {
	return db.GetDoc(s.writer, id)
}

func (s *sqliteStore) GetDocComment(id int) (*model.DocComment, error) {
	return db.GetDocComment(s.writer, id)
}

func (s *sqliteStore) GetDocProposals(docID int) ([]int, error) {
	return db.GetDocProposals(s.writer, docID)
}

func (s *sqliteStore) GetDocRevision(docID, rev int) (*model.DocRevision, error) {
	return db.GetDocRevision(s.writer, docID, rev)
}

func (s *sqliteStore) GetIssue(id int) (*model.Issue, error) {
	return db.GetIssue(s.writer, id)
}

func (s *sqliteStore) GetIssueFiles(issueID int) ([]string, error) {
	return db.GetIssueFiles(s.writer, issueID)
}

func (s *sqliteStore) GetIssueLabelObjects(issueID int) ([]*model.Label, error) {
	return db.GetIssueLabelObjects(s.writer, issueID)
}

func (s *sqliteStore) GetIssueLabels(issueID int) ([]string, error) {
	return db.GetIssueLabels(s.writer, issueID)
}

func (s *sqliteStore) GetIssueLease(id int) (*model.Lease, error) {
	return db.GetIssueLease(s.writer, id)
}

func (s *sqliteStore) GetIssueProposals(issueID int) ([]model.Proposal, error) {
	return db.GetIssueProposals(s.writer, issueID)
}

func (s *sqliteStore) GetIssueRelations(issueID int) ([]model.Relation, error) {
	return db.GetIssueRelations(s.writer, issueID)
}

func (s *sqliteStore) GetIssuesByIDs(ids []int) (map[int]*model.Issue, error) {
	return db.GetIssuesByIDs(s.writer, ids)
}

func (s *sqliteStore) GetLabelByName(projectID int, name string) (*model.LabelWithCount, error) {
	return db.GetLabelByName(s.writer, projectID, name)
}

func (s *sqliteStore) GetProject(id int) (*model.Project, error) {
	return db.GetProject(s.writer, id)
}

func (s *sqliteStore) GetProposal(id int) (*model.Proposal, error) {
	return db.GetProposal(s.writer, id)
}

func (s *sqliteStore) GetProposalDocs(proposalID int) ([]int, error) {
	return db.GetProposalDocs(s.writer, proposalID)
}

func (s *sqliteStore) GetProposalIssues(proposalID int) ([]int, error) {
	return db.GetProposalIssues(s.writer, proposalID)
}

func (s *sqliteStore) GetProposalVotes(proposalID int) ([]*model.Vote, error) {
	return db.GetProposalVotes(s.writer, proposalID)
}

func (s *sqliteStore) GetRun(id int) (*model.Run, error) {
	return db.GetRun(s.writer, id)
}

func (s *sqliteStore) GetSchema(projectID int, name string, version int) (*model.Schema, error) {
	return db.GetSchema(s.writer, projectID, name, version)
}

func (s *sqliteStore) GetStep(id int) (*db.Step, error) {
	return db.GetStep(s.writer, id)
}

func (s *sqliteStore) GetSubIssues(parentID int) ([]*model.Issue, error) {
	return db.GetSubIssues(s.writer, parentID)
}

func (s *sqliteStore) GetVersion(table string, id int) (int, error) {
	return db.GetVersion(s.writer, table, id)
}

func (s *sqliteStore) GetWorkflow(projectID int, name string, version int) (*model.Workflow, error) {
	return db.GetWorkflow(s.writer, projectID, name, version)
}

func (s *sqliteStore) HasGateResult(stepID int, gate string) (bool, error) {
	return db.HasGateResult(s.writer, stepID, gate)
}

func (s *sqliteStore) HeartbeatIssue(id int, token string, ttlMS int64, nowMS int64) (*model.Lease, error) {
	return db.HeartbeatIssue(s.writer, id, token, ttlMS, nowMS)
}

func (s *sqliteStore) HeartbeatStep(id int, token string, ttlMS, nowMS int64) (*model.Lease, error) {
	return db.HeartbeatStep(s.writer, id, token, ttlMS, nowMS)
}

func (s *sqliteStore) HydrateDocs(issues []*model.Issue) error {
	return db.HydrateDocs(s.writer, issues)
}

func (s *sqliteStore) HydrateLinkedIssues(docIDs []int) (map[int][]model.IssueRef, error) {
	return db.HydrateLinkedIssues(s.writer, docIDs)
}

func (s *sqliteStore) IdempotencyKeyOf(scope string, entityID int) (string, bool, error) {
	return db.IdempotencyKeyOf(s.writer, scope, entityID)
}

func (s *sqliteStore) Initialize() error {
	return db.Initialize(s.writer)
}

func (s *sqliteStore) InsertRunWithContextIdempotent(projectID int, request string, budget float64, nowMS int64, ctx db.RunContext, idempotencyKey string, issueIDs ...int) (*model.Run, bool, error) {
	return db.InsertRunWithContextIdempotent(s.writer, projectID, request, budget, nowMS, ctx, idempotencyKey, issueIDs...)
}

func (s *sqliteStore) InsertSchema(schema *model.Schema, nowMS int64) (*model.Schema, bool, error) {
	return db.InsertSchema(s.writer, schema, nowMS)
}

func (s *sqliteStore) InsertWorkflow(wf *model.Workflow, nowMS int64) (*model.Workflow, bool, error) {
	return db.InsertWorkflow(s.writer, wf, nowMS)
}

func (s *sqliteStore) IsDescendant(issueID, potentialDescendantID int) (bool, error) {
	return db.IsDescendant(s.writer, issueID, potentialDescendantID)
}

func (s *sqliteStore) IssueExists(issueID int) (bool, error) {
	return db.IssueExists(s.writer, issueID)
}

func (s *sqliteStore) IssueOwnerPrefix(issueID int) (string, error) {
	return db.IssueOwnerPrefix(s.writer, issueID)
}

func (s *sqliteStore) IssueProjectID(issueID int) (int, error) {
	return db.IssueProjectID(s.writer, issueID)
}

func (s *sqliteStore) IssueScopeGlobs(issueID int) (string, error) {
	return db.IssueScopeGlobs(s.writer, issueID)
}

func (s *sqliteStore) IssueStepRuns(issueID int) ([]int, error) {
	return db.IssueStepRuns(s.writer, issueID)
}

func (s *sqliteStore) LeaseTTL(projectID int, class string) (time.Duration, error) {
	return db.LeaseTTL(s.writer, projectID, class)
}

func (s *sqliteStore) LinkDocIssue(docID, issueID int) error {
	return db.LinkDocIssue(s.writer, docID, issueID)
}

func (s *sqliteStore) LinkProposalIssue(proposalID, issueID int) error {
	return db.LinkProposalIssue(s.writer, proposalID, issueID)
}

func (s *sqliteStore) ListAllActivity(projectID int) ([]*model.Activity, error) {
	return db.ListAllActivity(s.writer, projectID)
}

func (s *sqliteStore) ListAllComments(projectID int) ([]*model.Comment, error) {
	return db.ListAllComments(s.writer, projectID)
}

func (s *sqliteStore) ListAllDocComments(projectID int) ([]*model.DocComment, error) {
	return db.ListAllDocComments(s.writer, projectID)
}

func (s *sqliteStore) ListAllDocIssueLinks(projectID int) ([]model.DocIssueLink, error) {
	return db.ListAllDocIssueLinks(s.writer, projectID)
}

func (s *sqliteStore) ListAllDocRevisions(projectID int) ([]*model.DocRevision, error) {
	return db.ListAllDocRevisions(s.writer, projectID)
}

func (s *sqliteStore) ListAllDocs(projectID int) ([]*model.Doc, error) {
	return db.ListAllDocs(s.writer, projectID)
}

func (s *sqliteStore) ListAllIssueFileMappings(projectID int) ([]model.IssueFileMapping, error) {
	return db.ListAllIssueFileMappings(s.writer, projectID)
}

func (s *sqliteStore) ListAllIssueLabelMappings(projectID int) ([]model.IssueLabelMapping, error) {
	return db.ListAllIssueLabelMappings(s.writer, projectID)
}

func (s *sqliteStore) ListAllIssues(projectID int) ([]*model.Issue, error) {
	return db.ListAllIssues(s.writer, projectID)
}

func (s *sqliteStore) ListAllLabels(projectID int) ([]*model.LabelWithCount, error) {
	return db.ListAllLabels(s.writer, projectID)
}

func (s *sqliteStore) ListAllLabelsRaw(projectID int) ([]*model.Label, error) {
	return db.ListAllLabelsRaw(s.writer, projectID)
}

func (s *sqliteStore) ListAllProposalDocs(projectID int) ([]model.ProposalDocLink, error) {
	return db.ListAllProposalDocs(s.writer, projectID)
}

func (s *sqliteStore) ListAllProposalIssues(projectID int) ([]model.ProposalIssueLink, error) {
	return db.ListAllProposalIssues(s.writer, projectID)
}

func (s *sqliteStore) ListAllProposals(projectID int) ([]*model.Proposal, error) {
	return db.ListAllProposals(s.writer, projectID)
}

func (s *sqliteStore) ListAllVotes(projectID int) ([]*model.Vote, error) {
	return db.ListAllVotes(s.writer, projectID)
}

func (s *sqliteStore) ListComments(issueID int) ([]*model.Comment, error) {
	return db.ListComments(s.writer, issueID)
}

func (s *sqliteStore) ListConfig(projectID int) ([]db.ConfigEntry, error) {
	return db.ListConfig(s.writer, projectID)
}

func (s *sqliteStore) ListDocComments(docID int) ([]*model.DocComment, error) {
	return db.ListDocComments(s.writer, docID)
}

func (s *sqliteStore) ListDocRevisions(docID int) ([]*model.DocRevision, error) {
	return db.ListDocRevisions(s.writer, docID)
}

func (s *sqliteStore) ListDocsWithCounts(opts db.DocListOptions) ([]*db.DocSummary, int, error) {
	return db.ListDocsWithCounts(s.writer, opts)
}

func (s *sqliteStore) ListIssues(opts db.ListOptions) ([]*model.Issue, int, error) {
	return db.ListIssues(s.writer, opts)
}

func (s *sqliteStore) ListPins(runID int) ([]db.Pin, error) {
	return db.ListPins(s.writer, runID)
}

func (s *sqliteStore) ListProjects() ([]*model.Project, error) {
	return db.ListProjects(s.writer)
}

func (s *sqliteStore) ListProposals(projectID int, status string, criticality string, domainTag string, limit int) ([]*model.Proposal, int, error) {
	return db.ListProposals(s.writer, projectID, status, criticality, domainTag, limit)
}

func (s *sqliteStore) ListRunArtifacts(runID int) ([]*db.Artifact, error) {
	return db.ListRunArtifacts(s.writer, runID)
}

func (s *sqliteStore) ListRunIssues(runID int) ([]*db.RunIssue, error) {
	return db.ListRunIssues(s.writer, runID)
}

func (s *sqliteStore) ListRunNotes(runID int) ([]db.RunNote, error) {
	return db.ListRunNotes(s.writer, runID)
}

func (s *sqliteStore) ListRuns(opts db.RunListOptions) ([]*model.Run, int, error) {
	return db.ListRuns(s.writer, opts)
}

func (s *sqliteStore) ListRunSteps(runID int) ([]*db.Step, error) {
	return db.ListRunSteps(s.writer, runID)
}

func (s *sqliteStore) ListSchemas(opts db.SchemaListOptions) ([]*model.Schema, int, error) {
	return db.ListSchemas(s.writer, opts)
}

func (s *sqliteStore) ListStepArtifacts(stepID int) ([]*db.Artifact, error) {
	return db.ListStepArtifacts(s.writer, stepID)
}

func (s *sqliteStore) ListWorkflows(opts db.WorkflowListOptions) ([]*model.Workflow, int, error) {
	return db.ListWorkflows(s.writer, opts)
}

func (s *sqliteStore) LookupIdempotencyKey(scope, key string) (int, bool, error) {
	return db.LookupIdempotencyKey(s.writer, scope, key)
}

func (s *sqliteStore) LookupIdempotencyKeys(scope, prefix string) (map[string]int, error) {
	return db.LookupIdempotencyKeys(s.writer, scope, prefix)
}

func (s *sqliteStore) LookupProject(identity string) (int, bool, error) {
	return db.LookupProject(s.writer, identity)
}

func (s *sqliteStore) MetadataRollup(runID int) ([]db.MetadataKeyRollup, error) {
	return db.MetadataRollup(s.writer, runID)
}

func (s *sqliteStore) Migrate() error {
	return db.Migrate(s.writer)
}

func (s *sqliteStore) MoveIssueProject(issueID, targetProjectID int, author string, nowMS int64) ([]int, error) {
	return db.MoveIssueProject(s.writer, issueID, targetProjectID, author, nowMS)
}

func (s *sqliteStore) NextGateOrdinal(stepID int, gate string) (int, error) {
	return db.NextGateOrdinal(s.writer, stepID, gate)
}

func (s *sqliteStore) OrphanSubIssues(parentID int, author string) error {
	return db.OrphanSubIssues(s.writer, parentID, author)
}

func (s *sqliteStore) PrefixHolder(prefix string, exclude int) (int, error) {
	return db.PrefixHolder(s.writer, prefix, exclude)
}

func (s *sqliteStore) ProjectPrefix(id int) (string, error) {
	return db.ProjectPrefix(s.writer, id)
}

func (s *sqliteStore) ProjectRefCounts(id int) (map[string]int, error) {
	return db.ProjectRefCounts(s.writer, id)
}

func (s *sqliteStore) ProposalIDsNaming(token string) ([]int, error) {
	return db.ProposalIDsNaming(s.writer, token)
}

func (s *sqliteStore) ProposalRunLinks(filter db.ProposalRunLinkFilter) ([]db.ProposalRunLink, error) {
	return db.ProposalRunLinks(s.writer, filter)
}

func (s *sqliteStore) RegisteredVoteRules(projectID int) ([]string, error) {
	return db.RegisteredVoteRules(s.writer, projectID)
}

func (s *sqliteStore) ReleaseIssue(id int, token string, nowMS int64) (*model.Lease, error) {
	return db.ReleaseIssue(s.writer, id, token, nowMS)
}

func (s *sqliteStore) RemoveLabelsFromIssue(issueID int, labelNames []string, author string) error {
	return db.RemoveLabelsFromIssue(s.writer, issueID, labelNames, author)
}

func (s *sqliteStore) RemoveRunIssue(runID, issueID int) error {
	return db.RemoveRunIssue(s.writer, runID, issueID)
}

func (s *sqliteStore) RestoreSchema(projectID int, name string, version int) (*model.Schema, error) {
	return db.RestoreSchema(s.writer, projectID, name, version)
}

func (s *sqliteStore) RestoreWorkflow(projectID int, name string, version int) (*model.Workflow, error) {
	return db.RestoreWorkflow(s.writer, projectID, name, version)
}

func (s *sqliteStore) RunBudgetFactsFor(runID int) (db.RunBudgetFacts, error) {
	return db.RunBudgetFactsFor(s.writer, runID)
}

func (s *sqliteStore) RunConductorHash(id int) (string, error) {
	return db.RunConductorHash(s.writer, id)
}

func (s *sqliteStore) RunProjectID(runID int) (int, error) {
	return db.RunProjectID(s.writer, runID)
}

func (s *sqliteStore) SchemaVersion() (int, error) {
	return db.SchemaVersion(s.writer)
}

func (s *sqliteStore) SetConfig(projectID int, key, value string) error {
	return db.SetConfig(s.writer, projectID, key, value)
}

func (s *sqliteStore) SetIssueFiles(issueID int, filePaths []string, changedBy string) error {
	return db.SetIssueFiles(s.writer, issueID, filePaths, changedBy)
}

func (s *sqliteStore) SetIssueScopeGlobs(issueID int, globsJSON string) error {
	return db.SetIssueScopeGlobs(s.writer, issueID, globsJSON)
}

func (s *sqliteStore) SetProjectPrefix(id int, prefix string) error {
	return db.SetProjectPrefix(s.writer, id, prefix)
}

func (s *sqliteStore) SilentVoteSeatsFor(scope, prefix string, extraIDs ...int) ([]db.SilentVoteSeatRow, error) {
	return db.SilentVoteSeatsFor(s.writer, scope, prefix, extraIDs...)
}

func (s *sqliteStore) StaleTargetWaiversForRun(runID int) ([]db.StaleTargetWaiver, error) {
	return db.StaleTargetWaiversForRun(s.writer, runID)
}

func (s *sqliteStore) StepInputCount(stepID int) (int, error) {
	return db.StepInputCount(s.writer, stepID)
}

func (s *sqliteStore) StepStatusCounts(runID int) ([]model.StatusCount, error) {
	return db.StepStatusCounts(s.writer, runID)
}

func (s *sqliteStore) UnlinkDocIssue(docID, issueID int) error {
	return db.UnlinkDocIssue(s.writer, docID, issueID)
}

func (s *sqliteStore) UnlinkProposalIssue(proposalID, issueID int) error {
	return db.UnlinkProposalIssue(s.writer, proposalID, issueID)
}

func (s *sqliteStore) UpdateDoc(id int, upd db.DocUpdate) (int, error) {
	return db.UpdateDoc(s.writer, id, upd)
}

func (s *sqliteStore) UpdateIssueCAS(id int, updates map[string]interface{}, changedBy string, ifVersion *int) error {
	return db.UpdateIssueCAS(s.writer, id, updates, changedBy, ifVersion)
}

func (s *sqliteStore) UpdateIssueCASLease(id int, updates map[string]interface{}, changedBy string, ifVersion *int, token string) error {
	return db.UpdateIssueCASLease(s.writer, id, updates, changedBy, ifVersion, token)
}

func (s *sqliteStore) UpdateIssueCASNote(id int, updates map[string]interface{}, changedBy string, ifVersion *int, note string) error {
	return db.UpdateIssueCASNote(s.writer, id, updates, changedBy, ifVersion, note)
}

func (s *sqliteStore) UsageByStep(runID int) ([]db.StepUsageRow, error) {
	return db.UsageByStep(s.writer, runID)
}

func (s *sqliteStore) UsageByUnit(runID int) ([]db.UnitTotal, error) {
	return db.UsageByUnit(s.writer, runID)
}

func (s *sqliteStore) VoteMetadataRollup(scope, prefix string) ([]db.MetadataKeyRollup, error) {
	return db.VoteMetadataRollup(s.writer, scope, prefix)
}

func (s *sqliteStore) VoteRuleExists(projectID int, rule string) (bool, error) {
	return db.VoteRuleExists(s.writer, projectID, rule)
}

func (s *sqliteStore) VoteRuleSetElsewhere(projectID int, rule string) (int, string, error) {
	return db.VoteRuleSetElsewhere(s.writer, projectID, rule)
}

func (s *sqliteStore) VoteUsageCoverageFor(scope, prefix string, extraIDs ...int) (db.VoteUsageCoverage, error) {
	return db.VoteUsageCoverageFor(s.writer, scope, prefix, extraIDs...)
}

func (s *sqliteStore) VoteUsageRollup(scope, prefix string, extraIDs ...int) ([]db.UnitTotal, error) {
	return db.VoteUsageRollup(s.writer, scope, prefix, extraIDs...)
}

func (s *sqliteStore) WorkflowVersionsFor(projectID int, names []string) ([]db.WorkflowVersion, error) {
	return db.WorkflowVersionsFor(s.writer, projectID, names)
}

func (t *sqliteTx) AutoRegisterEnabledTx(projectID int) (bool, error) {
	return db.AutoRegisterEnabledTx(t.tx, projectID)
}

func (t *sqliteTx) BudgetDefaultTx(projectID int) (float64, error) {
	return db.BudgetDefaultTx(t.tx, projectID)
}

func (t *sqliteTx) BudgetUnitTx(projectID int) (string, error) {
	return db.BudgetUnitTx(t.tx, projectID)
}

func (t *sqliteTx) ClassMaxOverridesTx(projectID int) (map[string]db.ClassMaxOverride, error) {
	return db.ClassMaxOverridesTx(t.tx, projectID)
}

func (t *sqliteTx) CountEventsHeldByRetentionTx(sel db.EventPruneSelection, cutoffMS int64) (int, error) {
	return db.CountEventsHeldByRetentionTx(t.tx, sel, cutoffMS)
}

func (t *sqliteTx) CountEventsTx(f db.EventFilter) (int, error) {
	return db.CountEventsTx(t.tx, f)
}

func (t *sqliteTx) CountPrunableEventsTx(sel db.EventPruneSelection) (int, error) {
	return db.CountPrunableEventsTx(t.tx, sel)
}

func (t *sqliteTx) DispatchGraceTx(projectID int) (time.Duration, error) {
	return db.DispatchGraceTx(t.tx, projectID)
}

func (t *sqliteTx) DispatchTTLTx(projectID int) (time.Duration, error) {
	return db.DispatchTTLTx(t.tx, projectID)
}

func (t *sqliteTx) EventKindCountsTx(runID int) (map[string]int, error) {
	return db.EventKindCountsTx(t.tx, runID)
}

func (t *sqliteTx) EventLowWatermarkTx() (int64, error) {
	return db.EventLowWatermarkTx(t.tx)
}

func (t *sqliteTx) GateResultsForStepTx(stepID int) ([]db.GateResultRow, error) {
	return db.GateResultsForStepTx(t.tx, stepID)
}

func (t *sqliteTx) GetArtifactTx(id int) (*db.Artifact, error) {
	return db.GetArtifactTx(t.tx, id)
}

func (t *sqliteTx) GetConfigTx(projectID int, key string) (db.ConfigEntry, error) {
	return db.GetConfigTx(t.tx, projectID, key)
}

func (t *sqliteTx) GetDispatchTx(id int) (*db.Dispatch, error) {
	return db.GetDispatchTx(t.tx, id)
}

func (t *sqliteTx) GetIssueFilesTx(issueID int) ([]string, error) {
	return db.GetIssueFilesTx(t.tx, issueID)
}

func (t *sqliteTx) GetProposalTx(id int) (*model.Proposal, error) {
	return db.GetProposalTx(t.tx, id)
}

func (t *sqliteTx) GetProposalVotesTx(proposalID int) ([]*model.Vote, error) {
	return db.GetProposalVotesTx(t.tx, proposalID)
}

func (t *sqliteTx) GetRunIssueTx(runID, issueID int) (*db.RunIssue, error) {
	return db.GetRunIssueTx(t.tx, runID, issueID)
}

func (t *sqliteTx) GetRunTx(id int) (*model.Run, error) {
	return db.GetRunTx(t.tx, id)
}

func (t *sqliteTx) GetSchemaTx(projectID int, name string, version int) (*model.Schema, error) {
	return db.GetSchemaTx(t.tx, projectID, name, version)
}

func (t *sqliteTx) GetStepTx(id int) (*db.Step, error) {
	return db.GetStepTx(t.tx, id)
}

func (t *sqliteTx) IssueScopeGlobsTx(issueID int) (string, error) {
	return db.IssueScopeGlobsTx(t.tx, issueID)
}

func (t *sqliteTx) IssueStatusResolutionTx(issueID int) (model.Status, string, error) {
	return db.IssueStatusResolutionTx(t.tx, issueID)
}

func (t *sqliteTx) LatestEventDataTx(runID int, kind string) (string, bool, error) {
	return db.LatestEventDataTx(t.tx, runID, kind)
}

func (t *sqliteTx) ListDispatchRowsTx(dispatchID int) ([]db.DispatchRow, error) {
	return db.ListDispatchRowsTx(t.tx, dispatchID)
}

func (t *sqliteTx) ListEventsTx(f db.EventFilter, limit, tail int) ([]db.EventRow, error) {
	return db.ListEventsTx(t.tx, f, limit, tail)
}

func (t *sqliteTx) ListIssueStepsTx(runID, issueID int) ([]*db.Step, error) {
	return db.ListIssueStepsTx(t.tx, runID, issueID)
}

func (t *sqliteTx) ListPinsTx(runID int) ([]db.Pin, error) {
	return db.ListPinsTx(t.tx, runID)
}

func (t *sqliteTx) ListRunArtifactsTx(runID int) ([]*db.Artifact, error) {
	return db.ListRunArtifactsTx(t.tx, runID)
}

func (t *sqliteTx) ListRunIssuesTx(runID int) ([]*db.RunIssue, error) {
	return db.ListRunIssuesTx(t.tx, runID)
}

func (t *sqliteTx) ListRunNotesTx(runID int) ([]db.RunNote, error) {
	return db.ListRunNotesTx(t.tx, runID)
}

func (t *sqliteTx) ListRunStepsTx(runID int) ([]*db.Step, error) {
	return db.ListRunStepsTx(t.tx, runID)
}

func (t *sqliteTx) ListStepInputArtifactsTx(stepID int) ([]*db.Artifact, error) {
	return db.ListStepInputArtifactsTx(t.tx, stepID)
}

func (t *sqliteTx) LookupIdempotencyKeysTx(scope, prefix string) (map[string]int, error) {
	return db.LookupIdempotencyKeysTx(t.tx, scope, prefix)
}

func (t *sqliteTx) LookupIdempotencyKeyTx(scope, key string) (int, bool, error) {
	return db.LookupIdempotencyKeyTx(t.tx, scope, key)
}

func (t *sqliteTx) LoopGrantsTx(runID, issueID int) (int, error) {
	return db.LoopGrantsTx(t.tx, runID, issueID)
}

func (t *sqliteTx) NextActionOrdinalTx(stepID int, action string) (int, error) {
	return db.NextActionOrdinalTx(t.tx, stepID, action)
}

func (t *sqliteTx) OpenDispatchTx(runID int) (*db.Dispatch, error) {
	return db.OpenDispatchTx(t.tx, runID)
}

func (t *sqliteTx) PrunableEventRunsTx(sel db.EventPruneSelection) ([]db.EventRunStatus, error) {
	return db.PrunableEventRunsTx(t.tx, sel)
}

func (t *sqliteTx) ProposalStatusesTx(ids []int) (map[int]model.ProposalStatus, error) {
	return db.ProposalStatusesTx(t.tx, ids)
}

func (t *sqliteTx) RegisteredVoteRulesTx(projectID int) ([]string, error) {
	return db.RegisteredVoteRulesTx(t.tx, projectID)
}

func (t *sqliteTx) ReportedUsageTx(runID int, unit string) (float64, error) {
	return db.ReportedUsageTx(t.tx, runID, unit)
}

func (t *sqliteTx) RunBudgetFactsTx(runID int) (db.RunBudgetFacts, error) {
	return db.RunBudgetFactsTx(t.tx, runID)
}

func (t *sqliteTx) RunConductorHashTx(id int) (string, error) {
	return db.RunConductorHashTx(t.tx, id)
}

func (t *sqliteTx) RunEverDispatchedTx(runID int) (bool, error) {
	return db.RunEverDispatchedTx(t.tx, runID)
}

func (t *sqliteTx) RunPauseOriginTx(id int) (model.RunPauseOrigin, error) {
	return db.RunPauseOriginTx(t.tx, id)
}

func (t *sqliteTx) RunProjectIDTx(runID int) (int, error) {
	return db.RunProjectIDTx(t.tx, runID)
}

func (t *sqliteTx) UnacknowledgedReapsTx(runID int) ([]db.ReapAck, error) {
	return db.UnacknowledgedReapsTx(t.tx, runID)
}

func (t *sqliteTx) UsageBudgetUnitTx(projectID int) (string, error) {
	return db.UsageBudgetUnitTx(t.tx, projectID)
}

func (t *sqliteTx) VoteRuleExistsTx(projectID int, rule string) (bool, error) {
	return db.VoteRuleExistsTx(t.tx, projectID, rule)
}

func (t *sqliteTx) VoteRuleSetElsewhereTx(projectID int, rule string) (int, string, error) {
	return db.VoteRuleSetElsewhereTx(t.tx, projectID, rule)
}

func (t *sqliteTx) AckReapTx(runID int, seq int64, ackedBy string, nowMS int64) (db.AckReapResult, error) {
	return db.AckReapTx(t.tx, runID, seq, ackedBy, nowMS)
}

func (t *sqliteTx) AdvanceSagaTx(id int, from, to string, nowMS int64) error {
	return db.AdvanceSagaTx(t.tx, id, from, to, nowMS)
}

func (t *sqliteTx) AuthorizeStepTx(id int, token string, nowMS int64) (*model.Lease, error) {
	return db.AuthorizeStepTx(t.tx, id, token, nowMS)
}

func (t *sqliteTx) BindRunIssueTx(ri *db.RunIssue) error {
	return db.BindRunIssueTx(t.tx, ri)
}

func (t *sqliteTx) BreachRunBudgetTx(runID int, reason string, nowMS int64) (bool, error) {
	return db.BreachRunBudgetTx(t.tx, runID, reason, nowMS)
}

func (t *sqliteTx) CacheRunFloorTx(runID int, floor float64) error {
	return db.CacheRunFloorTx(t.tx, runID, floor)
}

func (t *sqliteTx) ClaimStepWithRecoveryTx(id int, owner, recoverySecret string, ttlMS, nowMS int64) (string, *model.Lease, error) {
	return db.ClaimStepWithRecoveryTx(t.tx, id, owner, recoverySecret, ttlMS, nowMS)
}

func (t *sqliteTx) ClearProjectDataTx(projectID int) error {
	return db.ClearProjectDataTx(t.tx, projectID)
}

func (t *sqliteTx) ClearRunBreachTx(runID int, newReason string, nowMS int64) error {
	return db.ClearRunBreachTx(t.tx, runID, newReason, nowMS)
}

func (t *sqliteTx) ClearStaleBudgetReasonTx(runID int, newReason string, nowMS int64) error {
	return db.ClearStaleBudgetReasonTx(t.tx, runID, newReason, nowMS)
}

func (t *sqliteTx) ClearStepInputsTx(stepID int) error {
	return db.ClearStepInputsTx(t.tx, stepID)
}

func (t *sqliteTx) ClearStepStartTx(id int) error {
	return db.ClearStepStartTx(t.tx, id)
}

func (t *sqliteTx) CloseDispatchTx(id int, status, reason string, nowMS int64) (bool, error) {
	return db.CloseDispatchTx(t.tx, id, status, reason, nowMS)
}

func (t *sqliteTx) CloseOpenProposalsTx(ids []int, reason string) (int, error) {
	return db.CloseOpenProposalsTx(t.tx, ids, reason)
}

func (t *sqliteTx) CoverGateOverrideGrantsTx(ids []int) error {
	return db.CoverGateOverrideGrantsTx(t.tx, ids)
}

func (t *sqliteTx) DeletePrunableEventsTx(sel db.EventPruneSelection) (int, error) {
	return db.DeletePrunableEventsTx(t.tx, sel)
}

func (t *sqliteTx) ExemptStepAttemptFromBudgetTx(id int, nowMS int64) error {
	return db.ExemptStepAttemptFromBudgetTx(t.tx, id, nowMS)
}

func (t *sqliteTx) ExtendDispatchTx(id int, expiresMS, extendedSeq int64) (bool, error) {
	return db.ExtendDispatchTx(t.tx, id, expiresMS, extendedSeq)
}

func (t *sqliteTx) GrantLoopTx(runID, issueID int) (int, error) {
	return db.GrantLoopTx(t.tx, runID, issueID)
}

func (t *sqliteTx) IncrementLoopCountTx(runID, issueID int) (int, error) {
	return db.IncrementLoopCountTx(t.tx, runID, issueID)
}

func (t *sqliteTx) InsertActionResultTx(r db.ActionResultRow) error {
	return db.InsertActionResultTx(t.tx, r)
}

func (t *sqliteTx) InsertActivityWithID(a *model.Activity) (bool, error) {
	return db.InsertActivityWithID(t.tx, a)
}

func (t *sqliteTx) InsertArtifactTx(a db.Artifact, nowMS int64) (int, error) {
	return db.InsertArtifactTx(t.tx, a, nowMS)
}

func (t *sqliteTx) InsertCommentWithID(comment *model.Comment) (bool, error) {
	return db.InsertCommentWithID(t.tx, comment)
}

func (t *sqliteTx) InsertDispatchRowTx(dispatchID int, row db.DispatchRow) error {
	return db.InsertDispatchRowTx(t.tx, dispatchID, row)
}

func (t *sqliteTx) InsertDispatchTx(runID int, openedSeq, expiresMS, nowMS int64) (int, error) {
	return db.InsertDispatchTx(t.tx, runID, openedSeq, expiresMS, nowMS)
}

func (t *sqliteTx) InsertDocCommentWithID(c *model.DocComment) (bool, error) {
	return db.InsertDocCommentWithID(t.tx, c)
}

func (t *sqliteTx) InsertDocIssueLink(docID, issueID int, createdAt string) (bool, error) {
	return db.InsertDocIssueLink(t.tx, docID, issueID, createdAt)
}

func (t *sqliteTx) InsertDocRevisionWithID(r *model.DocRevision) (bool, error) {
	return db.InsertDocRevisionWithID(t.tx, r)
}

func (t *sqliteTx) InsertDocWithID(doc *model.Doc) (bool, error) {
	return db.InsertDocWithID(t.tx, doc)
}

func (t *sqliteTx) InsertEngineComment(issueID int, body string, nowMS int64) (int, error) {
	return db.InsertEngineComment(t.tx, issueID, body, nowMS)
}

func (t *sqliteTx) InsertFenceTx(f db.RunFence) error {
	return db.InsertFenceTx(t.tx, f)
}

func (t *sqliteTx) InsertGapIssueTx(projectID int, gap db.GapIssue, relatedIssueID int) (int, error) {
	return db.InsertGapIssueTx(t.tx, projectID, gap, relatedIssueID)
}

func (t *sqliteTx) InsertGateOverrideGrantTx(g db.GateOverrideGrant) (int, error) {
	return db.InsertGateOverrideGrantTx(t.tx, g)
}

func (t *sqliteTx) InsertGateResultTx(r db.GateResultRow) error {
	return db.InsertGateResultTx(t.tx, r)
}

func (t *sqliteTx) InsertIssueFileMapping(issueID int, filePath string) (bool, error) {
	return db.InsertIssueFileMapping(t.tx, issueID, filePath)
}

func (t *sqliteTx) InsertIssueLabelMapping(issueID, labelID int) (bool, error) {
	return db.InsertIssueLabelMapping(t.tx, issueID, labelID)
}

func (t *sqliteTx) InsertIssueWithID(issue *model.Issue) (bool, error) {
	return db.InsertIssueWithID(t.tx, issue)
}

func (t *sqliteTx) InsertLabelWithID(label *model.Label) (bool, error) {
	return db.InsertLabelWithID(t.tx, label)
}

func (t *sqliteTx) InsertPinTx(p db.Pin) error {
	return db.InsertPinTx(t.tx, p)
}

func (t *sqliteTx) InsertProposalDocLink(proposalID, docID int, createdAt string) (bool, error) {
	return db.InsertProposalDocLink(t.tx, proposalID, docID, createdAt)
}

func (t *sqliteTx) InsertProposalIssueLink(proposalID, issueID int) (bool, error) {
	return db.InsertProposalIssueLink(t.tx, proposalID, issueID)
}

func (t *sqliteTx) InsertProposalWithID(p *model.Proposal) (bool, error) {
	return db.InsertProposalWithID(t.tx, p)
}

func (t *sqliteTx) InsertReapAckTx(ack db.ReapAck, nowMS int64) error {
	return db.InsertReapAckTx(t.tx, ack, nowMS)
}

func (t *sqliteTx) InsertRelationWithID(rel *model.Relation) (bool, error) {
	return db.InsertRelationWithID(t.tx, rel)
}

func (t *sqliteTx) InsertRunNoteTx(n db.RunNote) (int, error) {
	return db.InsertRunNoteTx(t.tx, n)
}

func (t *sqliteTx) InsertSchemaTx(schema *model.Schema, nowMS int64) (*model.Schema, bool, error) {
	return db.InsertSchemaTx(t.tx, schema, nowMS)
}

func (t *sqliteTx) InsertStaleTargetWaiverTx(w db.StaleTargetWaiver) (int, error) {
	return db.InsertStaleTargetWaiverTx(t.tx, w)
}

func (t *sqliteTx) InsertStepInputTx(stepID, position, artifactID int) error {
	return db.InsertStepInputTx(t.tx, stepID, position, artifactID)
}

func (t *sqliteTx) InsertStepTx(step db.StepRow, nowMS int64) error {
	return db.InsertStepTx(t.tx, step, nowMS)
}

func (t *sqliteTx) InsertTrustCacheTx(runID int, kind, gate, argvSHA256, entryName string, matched, prefix bool, atMS int64) error {
	return db.InsertTrustCacheTx(t.tx, runID, kind, gate, argvSHA256, entryName, matched, prefix, atMS)
}

func (t *sqliteTx) InsertUsageRowTx(row db.UsageRow, nowMS int64) error {
	return db.InsertUsageRowTx(t.tx, row, nowMS)
}

func (t *sqliteTx) InsertVoteUsageTx(voteID int64, unit string, quantity float64, source string, nowMS int64) error {
	return db.InsertVoteUsageTx(t.tx, voteID, unit, quantity, source, nowMS)
}

func (t *sqliteTx) InsertVoteWithID(v *model.Vote) (bool, error) {
	return db.InsertVoteWithID(t.tx, v)
}

func (t *sqliteTx) InsertWorkflowTx(wf *model.Workflow, nowMS int64) (*model.Workflow, bool, error) {
	return db.InsertWorkflowTx(t.tx, wf, nowMS)
}

func (t *sqliteTx) MarkExpandedTx(runID, issueID int, nowMS int64) error {
	return db.MarkExpandedTx(t.tx, runID, issueID, nowMS)
}

func (t *sqliteTx) MarkStepAttemptFailedTx(id int, nowMS int64) error {
	return db.MarkStepAttemptFailedTx(t.tx, id, nowMS)
}

func (t *sqliteTx) MarkStepClaimReapedTx(id int, nowMS int64) error {
	return db.MarkStepClaimReapedTx(t.tx, id, nowMS)
}

func (t *sqliteTx) MarkStepUsageRecordedTx(stepID int) error {
	return db.MarkStepUsageRecordedTx(t.tx, stepID)
}

func (t *sqliteTx) RaiseEventLowWatermarkTx() error {
	return db.RaiseEventLowWatermarkTx(t.tx)
}

func (t *sqliteTx) ReapStepTx(id int, nowMS int64) error {
	return db.ReapStepTx(t.tx, id, nowMS)
}

func (t *sqliteTx) RefreshClaimLeaseTx(id int, tokenHash string, ttlMS, nowMS int64) (bool, error) {
	return db.RefreshClaimLeaseTx(t.tx, id, tokenHash, ttlMS, nowMS)
}

func (t *sqliteTx) ReleaseStepLeaseTx(id int, nowMS int64) error {
	return db.ReleaseStepLeaseTx(t.tx, id, nowMS)
}

func (t *sqliteTx) ReMintStepTokenTx(id int, owner string, nowMS int64) (string, error) {
	return db.ReMintStepTokenTx(t.tx, id, owner, nowMS)
}

func (t *sqliteTx) ResetStepRetryBudgetTx(id int, nowMS int64) error {
	return db.ResetStepRetryBudgetTx(t.tx, id, nowMS)
}

func (t *sqliteTx) RetireStepTokenTx(id int) error {
	return db.RetireStepTokenTx(t.tx, id)
}

func (t *sqliteTx) SetConfigTx(projectID int, key, value string) (string, error) {
	return db.SetConfigTx(t.tx, projectID, key, value)
}

func (t *sqliteTx) SetIssueResolutionTx(issueID int, resolution string) error {
	return db.SetIssueResolutionTx(t.tx, issueID, resolution)
}

func (t *sqliteTx) SetRunBudgetTx(runID int, budget float64, ifVersion *int, nowMS int64) error {
	return db.SetRunBudgetTx(t.tx, runID, budget, ifVersion, nowMS)
}

func (t *sqliteTx) SetRunConductorHashTx(id int, hash string) error {
	return db.SetRunConductorHashTx(t.tx, id, hash)
}

func (t *sqliteTx) SetRunIssueBodyTx(runID, issueID int, body, sha string) error {
	return db.SetRunIssueBodyTx(t.tx, runID, issueID, body, sha)
}

func (t *sqliteTx) SetRunIssueSnapshotTx(runID, issueID int, snapshot string) error {
	return db.SetRunIssueSnapshotTx(t.tx, runID, issueID, snapshot)
}

func (t *sqliteTx) SetRunPauseOriginTx(id int, origin model.RunPauseOrigin) error {
	return db.SetRunPauseOriginTx(t.tx, id, origin)
}

func (t *sqliteTx) SetRunStatusTx(id int, status model.RunStatus, reason string, nowMS int64) error {
	return db.SetRunStatusTx(t.tx, id, status, reason, nowMS)
}

func (t *sqliteTx) SetStepAuthorityTx(id int, authority, authorityRef string, nowMS int64) error {
	return db.SetStepAuthorityTx(t.tx, id, authority, authorityRef, nowMS)
}

func (t *sqliteTx) SetStepClaimPhaseTx(id int, phase string, nowMS int64) error {
	return db.SetStepClaimPhaseTx(t.tx, id, phase, nowMS)
}

func (t *sqliteTx) SetStepLoopHistoryWithCapTx(id, roundsRun, loopCap int, triggerStep, latestVerdict string, nowMS int64) error {
	return db.SetStepLoopHistoryWithCapTx(t.tx, id, roundsRun, loopCap, triggerStep, latestVerdict, nowMS)
}

func (t *sqliteTx) SetStepMetadataTx(id int, metadata string, nowMS int64) error {
	return db.SetStepMetadataTx(t.tx, id, metadata, nowMS)
}

func (t *sqliteTx) SetStepRoutingTx(id int, routing, reason, status string, class db.ParkClass, nowMS int64) error {
	return db.SetStepRoutingTx(t.tx, id, routing, reason, status, class, nowMS)
}

func (t *sqliteTx) SetStepRoutingWithParkReasonTx(id int, routing, reason, status string, class db.ParkClass, parkReason string, nowMS int64) error {
	return db.SetStepRoutingWithParkReasonTx(t.tx, id, routing, reason, status, class, parkReason, nowMS)
}

func (t *sqliteTx) SetStepStatusTx(id int, status string, nowMS, activityMS int64) error {
	return db.SetStepStatusTx(t.tx, id, status, nowMS, activityMS)
}

func (t *sqliteTx) StartStepTx(id int, nowMS int64) error {
	return db.StartStepTx(t.tx, id, nowMS)
}
