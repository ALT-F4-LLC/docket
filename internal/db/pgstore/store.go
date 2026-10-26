package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// pgStore is the PostgreSQL Store. Like the SQLite store, every method
// delegates to the internal/db helper of the same name; the helpers run on a
// pool of storeConns, which rebind their placeholders and give writes the
// global write lock while reads stay lock-free.
type pgStore struct {
	db *sql.DB
}

// pgTx is a transaction on the pool. A snapshot is handed out only as a
// ReadTx, and the server refuses a write inside it.
type pgTx struct {
	tx *sql.Tx
}

var (
	_ store.Store = (*pgStore)(nil)
	_ store.Tx    = (*pgTx)(nil)
)

// OpenStore connects to the PostgreSQL server the DSN names, under Open's
// DSN, password-file and server-version policy. A non-empty searchPath sets
// the session search_path, so the store's tables live in that schema. It does
// not migrate; call Initialize and Migrate for that.
func OpenStore(ctx context.Context, dsn, passwordFile, searchPath string) (store.Store, error) {
	pool, err := openPool(ctx, dsn, passwordFile, readServerVersionNum, func(cfg *pgx.ConnConfig) *sql.DB {
		if searchPath != "" {
			cfg.RuntimeParams["search_path"] = searchPath
		}
		return sql.OpenDB(newStoreConnector(cfg))
	})
	if err != nil {
		return nil, err
	}
	return &pgStore{db: pool}, nil
}

// initialize creates the version 1 schema on a database that has none, as the
// SQLite store's Initialize does; Migrate takes it from there.
func initialize(ctx context.Context, pool *sql.DB) error {
	version, err := schemaVersion(ctx, pool)
	if err != nil {
		return err
	}
	if version > 0 {
		return nil
	}
	return applyMigrations(ctx, pool, 0, 1)
}

func (s *pgStore) Close() error {
	return s.db.Close()
}

// Begin opens a read-write transaction, which takes the write lock before it
// returns.
func (s *pgStore) Begin() (store.Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	return &pgTx{tx: tx}, nil
}

// BeginReadSnapshot opens a READ ONLY transaction at REPEATABLE READ, so every
// read in it sees the state committed before its first statement. Like
// WithReadTx it takes no lock, so it returns while a writer holds one.
func (s *pgStore) BeginReadSnapshot() (store.ReadTx, error) {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	return &pgTx{tx: tx}, nil
}

func (t *pgTx) Commit() error   { return t.tx.Commit() }
func (t *pgTx) Rollback() error { return t.tx.Rollback() }

// advanceIdentity follows an explicit-id insert into table: when it inserted
// a row, it moves the table's id sequence past every id the table holds, so a
// later create does not reuse an imported id. SQLite's AUTOINCREMENT does
// this on its own. It never moves the sequence back.
func (t *pgTx) advanceIdentity(table string, inserted bool, err error) (bool, error) {
	if err != nil || !inserted {
		return inserted, err
	}
	if _, err := t.tx.Exec(`SELECT setval(seq, GREATEST(top, COALESCE(pg_sequence_last_value(seq), 0)))
		FROM (SELECT pg_get_serial_sequence('` + table + `', 'id')::regclass AS seq, MAX(id) AS top FROM ` + table + `) s`); err != nil {
		return true, fmt.Errorf("advancing the %s id sequence: %w", table, err)
	}
	return true, nil
}

func (s *pgStore) ActionOutputsFor(stepID int, action string) ([]string, error) {
	return db.ActionOutputsFor(s.db, stepID, action)
}

func (s *pgStore) ActionRollup(runID int) ([]db.VerdictCount, error) {
	return db.ActionRollup(s.db, runID)
}

func (s *pgStore) ActionTrail(runID int) ([]db.ResultTrailRow, error) {
	return db.ActionTrail(s.db, runID)
}

func (s *pgStore) AddLabelsToIssue(issueID int, labelNames []string, color string, author string) error {
	return db.AddLabelsToIssue(s.db, issueID, labelNames, color, author)
}

func (s *pgStore) AddRunIssue(runID, issueID int) error {
	return db.AddRunIssue(s.db, runID, issueID)
}

func (s *pgStore) AllProjectPrefixes() ([]string, error) {
	return db.AllProjectPrefixes(s.db)
}

func (s *pgStore) AttachFiles(issueID int, filePaths []string, changedBy string) error {
	return db.AttachFiles(s.db, issueID, filePaths, changedBy)
}

func (s *pgStore) AuthorizeStepRead(id int, token string, nowMS int64) error {
	return db.AuthorizeStepRead(s.db, id, token, nowMS)
}

func (s *pgStore) CascadeDeleteIssue(id int) error {
	return db.CascadeDeleteIssue(s.db, id)
}

func (s *pgStore) CastVote(v *model.Vote) (*db.CastVoteResult, error) {
	return db.CastVote(s.db, v)
}

func (s *pgStore) CastVoteWeighted(v *model.Vote, equal bool) (*db.CastVoteResult, error) {
	return db.CastVoteWeighted(s.db, v, equal)
}

func (s *pgStore) ClaimIssue(id int, owner string, ttlMS int64, nowMS int64) (string, *model.Lease, error) {
	return db.ClaimIssue(s.db, id, owner, ttlMS, nowMS)
}

func (s *pgStore) CloseProposal(id int, reason string) error {
	return db.CloseProposal(s.db, id, reason)
}

func (s *pgStore) CommitProposal(id int, outcome string, escalationReason string) error {
	return db.CommitProposal(s.db, id, outcome, escalationReason)
}

func (s *pgStore) CountActivity(issueID int) (int, error) {
	return db.CountActivity(s.db, issueID)
}

func (s *pgStore) CountByPriority(projectID int) (map[string]int, error) {
	return db.CountByPriority(s.db, projectID)
}

func (s *pgStore) CountByStatus(projectID int) (map[string]int, error) {
	return db.CountByStatus(s.db, projectID)
}

func (s *pgStore) CountIssues(projectID int) (int, error) {
	return db.CountIssues(s.db, projectID)
}

func (s *pgStore) CountRootIssues(projectID int) (int, error) {
	return db.CountRootIssues(s.db, projectID)
}

func (s *pgStore) CreateCommentIdempotent(comment *model.Comment, idempotencyKey string) (int, error) {
	return db.CreateCommentIdempotent(s.db, comment, idempotencyKey)
}

func (s *pgStore) CreateDocCommentIdempotent(c *model.DocComment, idempotencyKey string) (int, error) {
	return db.CreateDocCommentIdempotent(s.db, c, idempotencyKey)
}

func (s *pgStore) CreateDocIdempotent(doc *model.Doc, idempotencyKey string) (int, error) {
	return db.CreateDocIdempotent(s.db, doc, idempotencyKey)
}

func (s *pgStore) CreateIssueIdempotent(issue *model.Issue, labels []string, files []string, idempotencyKey string) (int, bool, error) {
	return db.CreateIssueIdempotent(s.db, issue, labels, files, idempotencyKey)
}

func (s *pgStore) CreateProposalIdempotent(p *model.Proposal, idempotencyKey string) (int, error) {
	return db.CreateProposalIdempotent(s.db, p, idempotencyKey)
}

func (s *pgStore) CreateRelation(rel *model.Relation) (int, error) {
	return db.CreateRelation(s.db, rel)
}

func (s *pgStore) DeleteDoc(id int, cascade bool) error {
	return db.DeleteDoc(s.db, id, cascade)
}

func (s *pgStore) DeleteIssue(id int) error {
	return db.DeleteIssue(s.db, id)
}

func (s *pgStore) DeleteLabel(labelID int, name, author string) ([]int, error) {
	return db.DeleteLabel(s.db, labelID, name, author)
}

func (s *pgStore) DeleteProject(id int) error {
	return db.DeleteProject(s.db, id)
}

func (s *pgStore) DeleteRelation(sourceID, targetID int, relType string) error {
	return db.DeleteRelation(s.db, sourceID, targetID, relType)
}

func (s *pgStore) DeprecateSchema(projectID int, name string, version int, nowMS int64) (*model.Schema, error) {
	return db.DeprecateSchema(s.db, projectID, name, version, nowMS)
}

func (s *pgStore) DeprecateWorkflow(projectID int, name string, version int, nowMS int64) (*model.Workflow, error) {
	return db.DeprecateWorkflow(s.db, projectID, name, version, nowMS)
}

func (s *pgStore) DetachFiles(issueID int, filePaths []string, changedBy string) error {
	return db.DetachFiles(s.db, issueID, filePaths, changedBy)
}

func (s *pgStore) EnsureProject(identity, name string, nowMS int64) (int, error) {
	return db.EnsureProject(s.db, identity, name, nowMS)
}

func (s *pgStore) EnsureProjectCreated(identity, name string, nowMS int64) (int, bool, error) {
	return db.EnsureProjectCreated(s.db, identity, name, nowMS)
}

func (s *pgStore) EventsRetain() (time.Duration, error) {
	return db.EventsRetain(s.db)
}

func (s *pgStore) GateOverrideGrantsForRun(runID int) ([]db.GateOverrideGrant, error) {
	return db.GateOverrideGrantsForRun(s.db, runID)
}

func (s *pgStore) GateResultsForStep(stepID int) ([]db.GateResultRow, error) {
	return db.GateResultsForStep(s.db, stepID)
}

func (s *pgStore) GateRollup(runID int) ([]db.VerdictCount, error) {
	return db.GateRollup(s.db, runID)
}

func (s *pgStore) GateTrail(runID int) ([]db.ResultTrailRow, error) {
	return db.GateTrail(s.db, runID)
}

func (s *pgStore) GetActivity(issueID int, limit int) ([]model.Activity, error) {
	return db.GetActivity(s.db, issueID, limit)
}

func (s *pgStore) GetAllDirectionalRelations() ([]model.Relation, error) {
	return db.GetAllDirectionalRelations(s.db)
}

func (s *pgStore) GetAllRelations(projectID int) ([]model.Relation, error) {
	return db.GetAllRelations(s.db, projectID)
}

func (s *pgStore) GetBatchSubIssueProgress(parentIDs []int) (map[int][2]int, error) {
	return db.GetBatchSubIssueProgress(s.db, parentIDs)
}

func (s *pgStore) GetComment(id int) (*model.Comment, error) {
	return db.GetComment(s.db, id)
}

func (s *pgStore) GetConfig(projectID int, key string) (db.ConfigEntry, error) {
	return db.GetConfig(s.db, projectID, key)
}

func (s *pgStore) GetDoc(id int) (*model.Doc, error) {
	return db.GetDoc(s.db, id)
}

func (s *pgStore) GetDocComment(id int) (*model.DocComment, error) {
	return db.GetDocComment(s.db, id)
}

func (s *pgStore) GetDocProposals(docID int) ([]int, error) {
	return db.GetDocProposals(s.db, docID)
}

func (s *pgStore) GetDocRevision(docID, rev int) (*model.DocRevision, error) {
	return db.GetDocRevision(s.db, docID, rev)
}

func (s *pgStore) GetIssue(id int) (*model.Issue, error) {
	return db.GetIssue(s.db, id)
}

func (s *pgStore) GetIssueFiles(issueID int) ([]string, error) {
	return db.GetIssueFiles(s.db, issueID)
}

func (s *pgStore) GetIssueLabelObjects(issueID int) ([]*model.Label, error) {
	return db.GetIssueLabelObjects(s.db, issueID)
}

func (s *pgStore) GetIssueLabels(issueID int) ([]string, error) {
	return db.GetIssueLabels(s.db, issueID)
}

func (s *pgStore) GetIssueLease(id int) (*model.Lease, error) {
	return db.GetIssueLease(s.db, id)
}

func (s *pgStore) GetIssueProposals(issueID int) ([]model.Proposal, error) {
	return db.GetIssueProposals(s.db, issueID)
}

func (s *pgStore) GetIssueRelations(issueID int) ([]model.Relation, error) {
	return db.GetIssueRelations(s.db, issueID)
}

func (s *pgStore) GetIssuesByIDs(ids []int) (map[int]*model.Issue, error) {
	return db.GetIssuesByIDs(s.db, ids)
}

func (s *pgStore) GetLabelByName(projectID int, name string) (*model.LabelWithCount, error) {
	return db.GetLabelByName(s.db, projectID, name)
}

func (s *pgStore) GetProject(id int) (*model.Project, error) {
	return db.GetProject(s.db, id)
}

func (s *pgStore) GetProposal(id int) (*model.Proposal, error) {
	return db.GetProposal(s.db, id)
}

func (s *pgStore) GetProposalDocs(proposalID int) ([]int, error) {
	return db.GetProposalDocs(s.db, proposalID)
}

func (s *pgStore) GetProposalIssues(proposalID int) ([]int, error) {
	return db.GetProposalIssues(s.db, proposalID)
}

func (s *pgStore) GetProposalVotes(proposalID int) ([]*model.Vote, error) {
	return db.GetProposalVotes(s.db, proposalID)
}

func (s *pgStore) GetRun(id int) (*model.Run, error) {
	return db.GetRun(s.db, id)
}

func (s *pgStore) GetSchema(projectID int, name string, version int) (*model.Schema, error) {
	return db.GetSchema(s.db, projectID, name, version)
}

func (s *pgStore) GetStep(id int) (*db.Step, error) {
	return db.GetStep(s.db, id)
}

func (s *pgStore) GetSubIssues(parentID int) ([]*model.Issue, error) {
	return db.GetSubIssues(s.db, parentID)
}

func (s *pgStore) GetVersion(table string, id int) (int, error) {
	return db.GetVersion(s.db, table, id)
}

func (s *pgStore) GetWorkflow(projectID int, name string, version int) (*model.Workflow, error) {
	return db.GetWorkflow(s.db, projectID, name, version)
}

func (s *pgStore) HasGateResult(stepID int, gate string) (bool, error) {
	return db.HasGateResult(s.db, stepID, gate)
}

func (s *pgStore) HeartbeatIssue(id int, token string, ttlMS int64, nowMS int64) (*model.Lease, error) {
	return db.HeartbeatIssue(s.db, id, token, ttlMS, nowMS)
}

func (s *pgStore) HeartbeatStep(id int, token string, ttlMS, nowMS int64) (*model.Lease, error) {
	return db.HeartbeatStep(s.db, id, token, ttlMS, nowMS)
}

func (s *pgStore) HydrateDocs(issues []*model.Issue) error {
	return db.HydrateDocs(s.db, issues)
}

func (s *pgStore) HydrateLinkedIssues(docIDs []int) (map[int][]model.IssueRef, error) {
	return db.HydrateLinkedIssues(s.db, docIDs)
}

func (s *pgStore) IdempotencyKeyOf(scope string, entityID int) (string, bool, error) {
	return db.IdempotencyKeyOf(s.db, scope, entityID)
}

func (s *pgStore) Initialize() error {
	return initialize(context.Background(), s.db)
}

func (s *pgStore) InsertRunWithContextIdempotent(projectID int, request string, budget float64, nowMS int64, ctx db.RunContext, idempotencyKey string, issueIDs ...int) (*model.Run, bool, error) {
	return db.InsertRunWithContextIdempotent(s.db, projectID, request, budget, nowMS, ctx, idempotencyKey, issueIDs...)
}

func (s *pgStore) InsertSchema(schema *model.Schema, nowMS int64) (*model.Schema, bool, error) {
	return db.InsertSchema(s.db, schema, nowMS)
}

func (s *pgStore) InsertWorkflow(wf *model.Workflow, nowMS int64) (*model.Workflow, bool, error) {
	return db.InsertWorkflow(s.db, wf, nowMS)
}

func (s *pgStore) IsDescendant(issueID, potentialDescendantID int) (bool, error) {
	return db.IsDescendant(s.db, issueID, potentialDescendantID)
}

func (s *pgStore) IssueExists(issueID int) (bool, error) {
	return db.IssueExists(s.db, issueID)
}

func (s *pgStore) IssueOwnerPrefix(issueID int) (string, error) {
	return db.IssueOwnerPrefix(s.db, issueID)
}

func (s *pgStore) IssueProjectID(issueID int) (int, error) {
	return db.IssueProjectID(s.db, issueID)
}

func (s *pgStore) IssueScopeGlobs(issueID int) (string, error) {
	return db.IssueScopeGlobs(s.db, issueID)
}

func (s *pgStore) IssueStepRuns(issueID int) ([]int, error) {
	return db.IssueStepRuns(s.db, issueID)
}

func (s *pgStore) LeaseTTL(projectID int, class string) (time.Duration, error) {
	return db.LeaseTTL(s.db, projectID, class)
}

func (s *pgStore) LinkDocIssue(docID, issueID int) error {
	return db.LinkDocIssue(s.db, docID, issueID)
}

func (s *pgStore) LinkProposalIssue(proposalID, issueID int) error {
	return db.LinkProposalIssue(s.db, proposalID, issueID)
}

func (s *pgStore) ListAllActivity(projectID int) ([]*model.Activity, error) {
	return db.ListAllActivity(s.db, projectID)
}

func (s *pgStore) ListAllComments(projectID int) ([]*model.Comment, error) {
	return db.ListAllComments(s.db, projectID)
}

func (s *pgStore) ListAllDocComments(projectID int) ([]*model.DocComment, error) {
	return db.ListAllDocComments(s.db, projectID)
}

func (s *pgStore) ListAllDocIssueLinks(projectID int) ([]model.DocIssueLink, error) {
	return db.ListAllDocIssueLinks(s.db, projectID)
}

func (s *pgStore) ListAllDocRevisions(projectID int) ([]*model.DocRevision, error) {
	return db.ListAllDocRevisions(s.db, projectID)
}

func (s *pgStore) ListAllDocs(projectID int) ([]*model.Doc, error) {
	return db.ListAllDocs(s.db, projectID)
}

func (s *pgStore) ListAllIssueFileMappings(projectID int) ([]model.IssueFileMapping, error) {
	return db.ListAllIssueFileMappings(s.db, projectID)
}

func (s *pgStore) ListAllIssueLabelMappings(projectID int) ([]model.IssueLabelMapping, error) {
	return db.ListAllIssueLabelMappings(s.db, projectID)
}

func (s *pgStore) ListAllIssues(projectID int) ([]*model.Issue, error) {
	return db.ListAllIssues(s.db, projectID)
}

func (s *pgStore) ListAllLabels(projectID int) ([]*model.LabelWithCount, error) {
	return db.ListAllLabels(s.db, projectID)
}

func (s *pgStore) ListAllLabelsRaw(projectID int) ([]*model.Label, error) {
	return db.ListAllLabelsRaw(s.db, projectID)
}

func (s *pgStore) ListAllProposalDocs(projectID int) ([]model.ProposalDocLink, error) {
	return db.ListAllProposalDocs(s.db, projectID)
}

func (s *pgStore) ListAllProposalIssues(projectID int) ([]model.ProposalIssueLink, error) {
	return db.ListAllProposalIssues(s.db, projectID)
}

func (s *pgStore) ListAllProposals(projectID int) ([]*model.Proposal, error) {
	return db.ListAllProposals(s.db, projectID)
}

func (s *pgStore) ListAllVotes(projectID int) ([]*model.Vote, error) {
	return db.ListAllVotes(s.db, projectID)
}

func (s *pgStore) ListComments(issueID int) ([]*model.Comment, error) {
	return db.ListComments(s.db, issueID)
}

func (s *pgStore) ListConfig(projectID int) ([]db.ConfigEntry, error) {
	return db.ListConfig(s.db, projectID)
}

func (s *pgStore) ListDocComments(docID int) ([]*model.DocComment, error) {
	return db.ListDocComments(s.db, docID)
}

func (s *pgStore) ListDocRevisions(docID int) ([]*model.DocRevision, error) {
	return db.ListDocRevisions(s.db, docID)
}

func (s *pgStore) ListDocsWithCounts(opts db.DocListOptions) ([]*db.DocSummary, int, error) {
	return db.ListDocsWithCounts(s.db, opts)
}

func (s *pgStore) ListIssues(opts db.ListOptions) ([]*model.Issue, int, error) {
	return db.ListIssues(s.db, opts)
}

func (s *pgStore) ListPins(runID int) ([]db.Pin, error) {
	return db.ListPins(s.db, runID)
}

func (s *pgStore) ListProjects() ([]*model.Project, error) {
	return db.ListProjects(s.db)
}

func (s *pgStore) ListProposals(projectID int, status string, criticality string, domainTag string, limit int) ([]*model.Proposal, int, error) {
	return db.ListProposals(s.db, projectID, status, criticality, domainTag, limit)
}

func (s *pgStore) ListRunArtifacts(runID int) ([]*db.Artifact, error) {
	return db.ListRunArtifacts(s.db, runID)
}

func (s *pgStore) ListRunIssues(runID int) ([]*db.RunIssue, error) {
	return db.ListRunIssues(s.db, runID)
}

func (s *pgStore) ListRunNotes(runID int) ([]db.RunNote, error) {
	return db.ListRunNotes(s.db, runID)
}

func (s *pgStore) ListRuns(opts db.RunListOptions) ([]*model.Run, int, error) {
	return db.ListRuns(s.db, opts)
}

func (s *pgStore) ListRunSteps(runID int) ([]*db.Step, error) {
	return db.ListRunSteps(s.db, runID)
}

func (s *pgStore) ListSchemas(opts db.SchemaListOptions) ([]*model.Schema, int, error) {
	return db.ListSchemas(s.db, opts)
}

func (s *pgStore) ListStepArtifacts(stepID int) ([]*db.Artifact, error) {
	return db.ListStepArtifacts(s.db, stepID)
}

func (s *pgStore) ListWorkflows(opts db.WorkflowListOptions) ([]*model.Workflow, int, error) {
	return db.ListWorkflows(s.db, opts)
}

func (s *pgStore) LookupIdempotencyKey(scope, key string) (int, bool, error) {
	return db.LookupIdempotencyKey(s.db, scope, key)
}

func (s *pgStore) LookupIdempotencyKeys(scope, prefix string) (map[string]int, error) {
	return db.LookupIdempotencyKeys(s.db, scope, prefix)
}

func (s *pgStore) LookupProject(identity string) (int, bool, error) {
	return db.LookupProject(s.db, identity)
}

func (s *pgStore) MetadataRollup(runID int) ([]db.MetadataKeyRollup, error) {
	return db.MetadataRollup(s.db, runID)
}

func (s *pgStore) Migrate() error {
	return Migrate(context.Background(), s.db)
}

func (s *pgStore) MoveIssueProject(issueID, targetProjectID int, author string, nowMS int64) ([]int, error) {
	return db.MoveIssueProject(s.db, issueID, targetProjectID, author, nowMS)
}

func (s *pgStore) NextGateOrdinal(stepID int, gate string) (int, error) {
	return db.NextGateOrdinal(s.db, stepID, gate)
}

func (s *pgStore) OrphanSubIssues(parentID int, author string) error {
	return db.OrphanSubIssues(s.db, parentID, author)
}

func (s *pgStore) PrefixHolder(prefix string, exclude int) (int, error) {
	return db.PrefixHolder(s.db, prefix, exclude)
}

func (s *pgStore) ProjectPrefix(id int) (string, error) {
	return db.ProjectPrefix(s.db, id)
}

func (s *pgStore) ProjectRefCounts(id int) (map[string]int, error) {
	return db.ProjectRefCounts(s.db, id)
}

func (s *pgStore) ProposalIDsNaming(token string) ([]int, error) {
	return db.ProposalIDsNaming(s.db, token)
}

func (s *pgStore) ProposalRunLinks(filter db.ProposalRunLinkFilter) ([]db.ProposalRunLink, error) {
	return db.ProposalRunLinks(s.db, filter)
}

func (s *pgStore) RegisteredVoteRules(projectID int) ([]string, error) {
	return db.RegisteredVoteRules(s.db, projectID)
}

func (s *pgStore) ReleaseIssue(id int, token string, nowMS int64) (*model.Lease, error) {
	return db.ReleaseIssue(s.db, id, token, nowMS)
}

func (s *pgStore) RemoveLabelsFromIssue(issueID int, labelNames []string, author string) error {
	return db.RemoveLabelsFromIssue(s.db, issueID, labelNames, author)
}

func (s *pgStore) RemoveRunIssue(runID, issueID int) error {
	return db.RemoveRunIssue(s.db, runID, issueID)
}

func (s *pgStore) RestoreSchema(projectID int, name string, version int) (*model.Schema, error) {
	return db.RestoreSchema(s.db, projectID, name, version)
}

func (s *pgStore) RestoreWorkflow(projectID int, name string, version int) (*model.Workflow, error) {
	return db.RestoreWorkflow(s.db, projectID, name, version)
}

func (s *pgStore) RunBudgetFactsFor(runID int) (db.RunBudgetFacts, error) {
	return db.RunBudgetFactsFor(s.db, runID)
}

func (s *pgStore) RunConductorHash(id int) (string, error) {
	return db.RunConductorHash(s.db, id)
}

func (s *pgStore) RunProjectID(runID int) (int, error) {
	return db.RunProjectID(s.db, runID)
}

func (s *pgStore) SchemaVersion() (int, error) {
	return schemaVersion(context.Background(), s.db)
}

func (s *pgStore) SetConfig(projectID int, key, value string) error {
	return db.SetConfig(s.db, projectID, key, value)
}

func (s *pgStore) SetIssueFiles(issueID int, filePaths []string, changedBy string) error {
	return db.SetIssueFiles(s.db, issueID, filePaths, changedBy)
}

func (s *pgStore) SetIssueScopeGlobs(issueID int, globsJSON string) error {
	return db.SetIssueScopeGlobs(s.db, issueID, globsJSON)
}

func (s *pgStore) SetProjectPrefix(id int, prefix string) error {
	return db.SetProjectPrefix(s.db, id, prefix)
}

func (s *pgStore) SilentVoteSeatsFor(scope, prefix string, extraIDs ...int) ([]db.SilentVoteSeatRow, error) {
	return db.SilentVoteSeatsFor(s.db, scope, prefix, extraIDs...)
}

func (s *pgStore) StaleTargetWaiversForRun(runID int) ([]db.StaleTargetWaiver, error) {
	return db.StaleTargetWaiversForRun(s.db, runID)
}

func (s *pgStore) StepInputCount(stepID int) (int, error) {
	return db.StepInputCount(s.db, stepID)
}

func (s *pgStore) StepStatusCounts(runID int) ([]model.StatusCount, error) {
	return db.StepStatusCounts(s.db, runID)
}

func (s *pgStore) UnlinkDocIssue(docID, issueID int) error {
	return db.UnlinkDocIssue(s.db, docID, issueID)
}

func (s *pgStore) UnlinkProposalIssue(proposalID, issueID int) error {
	return db.UnlinkProposalIssue(s.db, proposalID, issueID)
}

func (s *pgStore) UpdateDoc(id int, upd db.DocUpdate) (int, error) {
	return db.UpdateDoc(s.db, id, upd)
}

func (s *pgStore) UpdateIssueCAS(id int, updates map[string]interface{}, changedBy string, ifVersion *int) error {
	return db.UpdateIssueCAS(s.db, id, updates, changedBy, ifVersion)
}

func (s *pgStore) UpdateIssueCASLease(id int, updates map[string]interface{}, changedBy string, ifVersion *int, token string) error {
	return db.UpdateIssueCASLease(s.db, id, updates, changedBy, ifVersion, token)
}

func (s *pgStore) UpdateIssueCASNote(id int, updates map[string]interface{}, changedBy string, ifVersion *int, note string) error {
	return db.UpdateIssueCASNote(s.db, id, updates, changedBy, ifVersion, note)
}

func (s *pgStore) UsageByStep(runID int) ([]db.StepUsageRow, error) {
	return db.UsageByStep(s.db, runID)
}

func (s *pgStore) UsageByUnit(runID int) ([]db.UnitTotal, error) {
	return db.UsageByUnit(s.db, runID)
}

func (s *pgStore) VoteMetadataRollup(scope, prefix string) ([]db.MetadataKeyRollup, error) {
	return db.VoteMetadataRollup(s.db, scope, prefix)
}

func (s *pgStore) VoteRuleExists(projectID int, rule string) (bool, error) {
	return db.VoteRuleExists(s.db, projectID, rule)
}

func (s *pgStore) VoteRuleSetElsewhere(projectID int, rule string) (int, string, error) {
	return db.VoteRuleSetElsewhere(s.db, projectID, rule)
}

func (s *pgStore) VoteUsageCoverageFor(scope, prefix string, extraIDs ...int) (db.VoteUsageCoverage, error) {
	return db.VoteUsageCoverageFor(s.db, scope, prefix, extraIDs...)
}

func (s *pgStore) VoteUsageRollup(scope, prefix string, extraIDs ...int) ([]db.UnitTotal, error) {
	return db.VoteUsageRollup(s.db, scope, prefix, extraIDs...)
}

func (s *pgStore) WorkflowVersionsFor(projectID int, names []string) ([]db.WorkflowVersion, error) {
	return db.WorkflowVersionsFor(s.db, projectID, names)
}

func (t *pgTx) AutoRegisterEnabledTx(projectID int) (bool, error) {
	return db.AutoRegisterEnabledTx(t.tx, projectID)
}

func (t *pgTx) BudgetDefaultTx(projectID int) (float64, error) {
	return db.BudgetDefaultTx(t.tx, projectID)
}

func (t *pgTx) BudgetUnitTx(projectID int) (string, error) {
	return db.BudgetUnitTx(t.tx, projectID)
}

func (t *pgTx) ClassMaxOverridesTx(projectID int) (map[string]db.ClassMaxOverride, error) {
	return db.ClassMaxOverridesTx(t.tx, projectID)
}

func (t *pgTx) CountEventsHeldByRetentionTx(sel db.EventPruneSelection, cutoffMS int64) (int, error) {
	return db.CountEventsHeldByRetentionTx(t.tx, sel, cutoffMS)
}

func (t *pgTx) CountEventsTx(f db.EventFilter) (int, error) {
	return db.CountEventsTx(t.tx, f)
}

func (t *pgTx) CountPrunableEventsTx(sel db.EventPruneSelection) (int, error) {
	return db.CountPrunableEventsTx(t.tx, sel)
}

func (t *pgTx) DispatchGraceTx(projectID int) (time.Duration, error) {
	return db.DispatchGraceTx(t.tx, projectID)
}

func (t *pgTx) DispatchTTLTx(projectID int) (time.Duration, error) {
	return db.DispatchTTLTx(t.tx, projectID)
}

func (t *pgTx) EventKindCountsTx(runID int) (map[string]int, error) {
	return db.EventKindCountsTx(t.tx, runID)
}

func (t *pgTx) GateResultsForStepTx(stepID int) ([]db.GateResultRow, error) {
	return db.GateResultsForStepTx(t.tx, stepID)
}

func (t *pgTx) GetArtifactTx(id int) (*db.Artifact, error) {
	return db.GetArtifactTx(t.tx, id)
}

func (t *pgTx) GetConfigTx(projectID int, key string) (db.ConfigEntry, error) {
	return db.GetConfigTx(t.tx, projectID, key)
}

func (t *pgTx) GetDispatchTx(id int) (*db.Dispatch, error) {
	return db.GetDispatchTx(t.tx, id)
}

func (t *pgTx) GetIssueFilesTx(issueID int) ([]string, error) {
	return db.GetIssueFilesTx(t.tx, issueID)
}

func (t *pgTx) GetProposalTx(id int) (*model.Proposal, error) {
	return db.GetProposalTx(t.tx, id)
}

func (t *pgTx) GetProposalVotesTx(proposalID int) ([]*model.Vote, error) {
	return db.GetProposalVotesTx(t.tx, proposalID)
}

func (t *pgTx) GetRunIssueTx(runID, issueID int) (*db.RunIssue, error) {
	return db.GetRunIssueTx(t.tx, runID, issueID)
}

func (t *pgTx) GetRunTx(id int) (*model.Run, error) {
	return db.GetRunTx(t.tx, id)
}

func (t *pgTx) GetSchemaTx(projectID int, name string, version int) (*model.Schema, error) {
	return db.GetSchemaTx(t.tx, projectID, name, version)
}

func (t *pgTx) GetStepTx(id int) (*db.Step, error) {
	return db.GetStepTx(t.tx, id)
}

func (t *pgTx) IssueScopeGlobsTx(issueID int) (string, error) {
	return db.IssueScopeGlobsTx(t.tx, issueID)
}

func (t *pgTx) IssueStatusResolutionTx(issueID int) (model.Status, string, error) {
	return db.IssueStatusResolutionTx(t.tx, issueID)
}

func (t *pgTx) LatestEventDataTx(runID int, kind string) (string, bool, error) {
	return db.LatestEventDataTx(t.tx, runID, kind)
}

func (t *pgTx) ListDispatchRowsTx(dispatchID int) ([]db.DispatchRow, error) {
	return db.ListDispatchRowsTx(t.tx, dispatchID)
}

func (t *pgTx) ListEventsTx(f db.EventFilter, limit, tail int) ([]db.EventRow, error) {
	return db.ListEventsTx(t.tx, f, limit, tail)
}

func (t *pgTx) ListIssueStepsTx(runID, issueID int) ([]*db.Step, error) {
	return db.ListIssueStepsTx(t.tx, runID, issueID)
}

func (t *pgTx) ListPinsTx(runID int) ([]db.Pin, error) {
	return db.ListPinsTx(t.tx, runID)
}

func (t *pgTx) ListRunArtifactsTx(runID int) ([]*db.Artifact, error) {
	return db.ListRunArtifactsTx(t.tx, runID)
}

func (t *pgTx) ListRunIssuesTx(runID int) ([]*db.RunIssue, error) {
	return db.ListRunIssuesTx(t.tx, runID)
}

func (t *pgTx) ListRunNotesTx(runID int) ([]db.RunNote, error) {
	return db.ListRunNotesTx(t.tx, runID)
}

func (t *pgTx) ListRunStepsTx(runID int) ([]*db.Step, error) {
	return db.ListRunStepsTx(t.tx, runID)
}

func (t *pgTx) ListStepInputArtifactsTx(stepID int) ([]*db.Artifact, error) {
	return db.ListStepInputArtifactsTx(t.tx, stepID)
}

func (t *pgTx) LookupIdempotencyKeysTx(scope, prefix string) (map[string]int, error) {
	return db.LookupIdempotencyKeysTx(t.tx, scope, prefix)
}

func (t *pgTx) LookupIdempotencyKeyTx(scope, key string) (int, bool, error) {
	return db.LookupIdempotencyKeyTx(t.tx, scope, key)
}

func (t *pgTx) LoopGrantsTx(runID, issueID int) (int, error) {
	return db.LoopGrantsTx(t.tx, runID, issueID)
}

func (t *pgTx) NextActionOrdinalTx(stepID int, action string) (int, error) {
	return db.NextActionOrdinalTx(t.tx, stepID, action)
}

func (t *pgTx) OpenDispatchTx(runID int) (*db.Dispatch, error) {
	return db.OpenDispatchTx(t.tx, runID)
}

func (t *pgTx) PrunableEventRunsTx(sel db.EventPruneSelection) ([]db.EventRunStatus, error) {
	return db.PrunableEventRunsTx(t.tx, sel)
}

func (t *pgTx) ProposalStatusesTx(ids []int) (map[int]model.ProposalStatus, error) {
	return db.ProposalStatusesTx(t.tx, ids)
}

func (t *pgTx) RegisteredVoteRulesTx(projectID int) ([]string, error) {
	return db.RegisteredVoteRulesTx(t.tx, projectID)
}

func (t *pgTx) ReportedUsageTx(runID int, unit string) (float64, error) {
	return db.ReportedUsageTx(t.tx, runID, unit)
}

func (t *pgTx) RunBudgetFactsTx(runID int) (db.RunBudgetFacts, error) {
	return db.RunBudgetFactsTx(t.tx, runID)
}

func (t *pgTx) RunConductorHashTx(id int) (string, error) {
	return db.RunConductorHashTx(t.tx, id)
}

func (t *pgTx) RunEverDispatchedTx(runID int) (bool, error) {
	return db.RunEverDispatchedTx(t.tx, runID)
}

func (t *pgTx) RunPauseOriginTx(id int) (model.RunPauseOrigin, error) {
	return db.RunPauseOriginTx(t.tx, id)
}

func (t *pgTx) RunProjectIDTx(runID int) (int, error) {
	return db.RunProjectIDTx(t.tx, runID)
}

func (t *pgTx) UnacknowledgedReapsTx(runID int) ([]db.ReapAck, error) {
	return db.UnacknowledgedReapsTx(t.tx, runID)
}

func (t *pgTx) UsageBudgetUnitTx(projectID int) (string, error) {
	return db.UsageBudgetUnitTx(t.tx, projectID)
}

func (t *pgTx) VoteRuleExistsTx(projectID int, rule string) (bool, error) {
	return db.VoteRuleExistsTx(t.tx, projectID, rule)
}

func (t *pgTx) VoteRuleSetElsewhereTx(projectID int, rule string) (int, string, error) {
	return db.VoteRuleSetElsewhereTx(t.tx, projectID, rule)
}

func (t *pgTx) AckReapTx(runID int, seq int64, ackedBy string, nowMS int64) (db.AckReapResult, error) {
	return db.AckReapTx(t.tx, runID, seq, ackedBy, nowMS)
}

func (t *pgTx) AdvanceSagaTx(id int, from, to string, nowMS int64) error {
	return db.AdvanceSagaTx(t.tx, id, from, to, nowMS)
}

func (t *pgTx) AuthorizeStepTx(id int, token string, nowMS int64) (*model.Lease, error) {
	return db.AuthorizeStepTx(t.tx, id, token, nowMS)
}

func (t *pgTx) BindRunIssueTx(ri *db.RunIssue) error {
	return db.BindRunIssueTx(t.tx, ri)
}

func (t *pgTx) BreachRunBudgetTx(runID int, reason string, nowMS int64) (bool, error) {
	return db.BreachRunBudgetTx(t.tx, runID, reason, nowMS)
}

func (t *pgTx) CacheRunFloorTx(runID int, floor float64) error {
	return db.CacheRunFloorTx(t.tx, runID, floor)
}

func (t *pgTx) ClaimStepWithRecoveryTx(id int, owner, recoverySecret string, ttlMS, nowMS int64) (string, *model.Lease, error) {
	return db.ClaimStepWithRecoveryTx(t.tx, id, owner, recoverySecret, ttlMS, nowMS)
}

func (t *pgTx) ClearProjectDataTx(projectID int) error {
	return db.ClearProjectDataTx(t.tx, projectID)
}

func (t *pgTx) ClearRunBreachTx(runID int, newReason string, nowMS int64) error {
	return db.ClearRunBreachTx(t.tx, runID, newReason, nowMS)
}

func (t *pgTx) ClearStaleBudgetReasonTx(runID int, newReason string, nowMS int64) error {
	return db.ClearStaleBudgetReasonTx(t.tx, runID, newReason, nowMS)
}

func (t *pgTx) ClearStepInputsTx(stepID int) error {
	return db.ClearStepInputsTx(t.tx, stepID)
}

func (t *pgTx) ClearStepStartTx(id int) error {
	return db.ClearStepStartTx(t.tx, id)
}

func (t *pgTx) CloseDispatchTx(id int, status, reason string, nowMS int64) (bool, error) {
	return db.CloseDispatchTx(t.tx, id, status, reason, nowMS)
}

func (t *pgTx) CloseOpenProposalsTx(ids []int, reason string) (int, error) {
	return db.CloseOpenProposalsTx(t.tx, ids, reason)
}

func (t *pgTx) CoverGateOverrideGrantsTx(ids []int) error {
	return db.CoverGateOverrideGrantsTx(t.tx, ids)
}

func (t *pgTx) DeletePrunableEventsTx(sel db.EventPruneSelection) (int, error) {
	return db.DeletePrunableEventsTx(t.tx, sel)
}

func (t *pgTx) ExemptStepAttemptFromBudgetTx(id int, nowMS int64) error {
	return db.ExemptStepAttemptFromBudgetTx(t.tx, id, nowMS)
}

func (t *pgTx) ExtendDispatchTx(id int, expiresMS, extendedSeq int64) (bool, error) {
	return db.ExtendDispatchTx(t.tx, id, expiresMS, extendedSeq)
}

func (t *pgTx) GrantLoopTx(runID, issueID int) (int, error) {
	return db.GrantLoopTx(t.tx, runID, issueID)
}

func (t *pgTx) IncrementLoopCountTx(runID, issueID int) (int, error) {
	return db.IncrementLoopCountTx(t.tx, runID, issueID)
}

func (t *pgTx) InsertActionResultTx(r db.ActionResultRow) error {
	return db.InsertActionResultTx(t.tx, r)
}

func (t *pgTx) InsertActivityWithID(a *model.Activity) (bool, error) {
	inserted, err := db.InsertActivityWithID(t.tx, a)
	return t.advanceIdentity("activity_log", inserted, err)
}

func (t *pgTx) InsertArtifactTx(a db.Artifact, nowMS int64) (int, error) {
	return db.InsertArtifactTx(t.tx, a, nowMS)
}

func (t *pgTx) InsertCommentWithID(comment *model.Comment) (bool, error) {
	inserted, err := db.InsertCommentWithID(t.tx, comment)
	return t.advanceIdentity("comments", inserted, err)
}

func (t *pgTx) InsertDispatchRowTx(dispatchID int, row db.DispatchRow) error {
	return db.InsertDispatchRowTx(t.tx, dispatchID, row)
}

func (t *pgTx) InsertDispatchTx(runID int, openedSeq, expiresMS, nowMS int64) (int, error) {
	return db.InsertDispatchTx(t.tx, runID, openedSeq, expiresMS, nowMS)
}

func (t *pgTx) InsertDocCommentWithID(c *model.DocComment) (bool, error) {
	inserted, err := db.InsertDocCommentWithID(t.tx, c)
	return t.advanceIdentity("doc_comments", inserted, err)
}

func (t *pgTx) InsertDocIssueLink(docID, issueID int, createdAt string) (bool, error) {
	return db.InsertDocIssueLink(t.tx, docID, issueID, createdAt)
}

func (t *pgTx) InsertDocRevisionWithID(r *model.DocRevision) (bool, error) {
	inserted, err := db.InsertDocRevisionWithID(t.tx, r)
	return t.advanceIdentity("doc_revisions", inserted, err)
}

func (t *pgTx) InsertDocWithID(doc *model.Doc) (bool, error) {
	inserted, err := db.InsertDocWithID(t.tx, doc)
	return t.advanceIdentity("docs", inserted, err)
}

func (t *pgTx) InsertEngineComment(issueID int, body string, nowMS int64) (int, error) {
	return db.InsertEngineComment(t.tx, issueID, body, nowMS)
}

func (t *pgTx) InsertFenceTx(f db.RunFence) error {
	return db.InsertFenceTx(t.tx, f)
}

func (t *pgTx) InsertGapIssueTx(projectID int, gap db.GapIssue, relatedIssueID int) (int, error) {
	return db.InsertGapIssueTx(t.tx, projectID, gap, relatedIssueID)
}

func (t *pgTx) InsertGateOverrideGrantTx(g db.GateOverrideGrant) (int, error) {
	return db.InsertGateOverrideGrantTx(t.tx, g)
}

func (t *pgTx) InsertGateResultTx(r db.GateResultRow) error {
	return db.InsertGateResultTx(t.tx, r)
}

func (t *pgTx) InsertIssueFileMapping(issueID int, filePath string) (bool, error) {
	return db.InsertIssueFileMapping(t.tx, issueID, filePath)
}

func (t *pgTx) InsertIssueLabelMapping(issueID, labelID int) (bool, error) {
	return db.InsertIssueLabelMapping(t.tx, issueID, labelID)
}

func (t *pgTx) InsertIssueWithID(issue *model.Issue) (bool, error) {
	inserted, err := db.InsertIssueWithID(t.tx, issue)
	return t.advanceIdentity("issues", inserted, err)
}

func (t *pgTx) InsertLabelWithID(label *model.Label) (bool, error) {
	inserted, err := db.InsertLabelWithID(t.tx, label)
	return t.advanceIdentity("labels", inserted, err)
}

func (t *pgTx) InsertPinTx(p db.Pin) error {
	return db.InsertPinTx(t.tx, p)
}

func (t *pgTx) InsertProposalDocLink(proposalID, docID int, createdAt string) (bool, error) {
	return db.InsertProposalDocLink(t.tx, proposalID, docID, createdAt)
}

func (t *pgTx) InsertProposalIssueLink(proposalID, issueID int) (bool, error) {
	return db.InsertProposalIssueLink(t.tx, proposalID, issueID)
}

func (t *pgTx) InsertProposalWithID(p *model.Proposal) (bool, error) {
	inserted, err := db.InsertProposalWithID(t.tx, p)
	return t.advanceIdentity("proposals", inserted, err)
}

func (t *pgTx) InsertReapAckTx(ack db.ReapAck, nowMS int64) error {
	return db.InsertReapAckTx(t.tx, ack, nowMS)
}

func (t *pgTx) InsertRelationWithID(rel *model.Relation) (bool, error) {
	inserted, err := db.InsertRelationWithID(t.tx, rel)
	return t.advanceIdentity("issue_relations", inserted, err)
}

func (t *pgTx) InsertRunNoteTx(n db.RunNote) (int, error) {
	return db.InsertRunNoteTx(t.tx, n)
}

func (t *pgTx) InsertSchemaTx(schema *model.Schema, nowMS int64) (*model.Schema, bool, error) {
	return db.InsertSchemaTx(t.tx, schema, nowMS)
}

func (t *pgTx) InsertStaleTargetWaiverTx(w db.StaleTargetWaiver) (int, error) {
	return db.InsertStaleTargetWaiverTx(t.tx, w)
}

func (t *pgTx) InsertStepInputTx(stepID, position, artifactID int) error {
	return db.InsertStepInputTx(t.tx, stepID, position, artifactID)
}

func (t *pgTx) InsertStepTx(step db.StepRow, nowMS int64) error {
	return db.InsertStepTx(t.tx, step, nowMS)
}

func (t *pgTx) InsertTrustCacheTx(runID int, kind, gate, argvSHA256, entryName string, matched, prefix bool, atMS int64) error {
	return db.InsertTrustCacheTx(t.tx, runID, kind, gate, argvSHA256, entryName, matched, prefix, atMS)
}

func (t *pgTx) InsertUsageRowTx(row db.UsageRow, nowMS int64) error {
	return db.InsertUsageRowTx(t.tx, row, nowMS)
}

func (t *pgTx) InsertVoteUsageTx(voteID int64, unit string, quantity float64, source string, nowMS int64) error {
	return db.InsertVoteUsageTx(t.tx, voteID, unit, quantity, source, nowMS)
}

func (t *pgTx) InsertVoteWithID(v *model.Vote) (bool, error) {
	inserted, err := db.InsertVoteWithID(t.tx, v)
	return t.advanceIdentity("votes", inserted, err)
}

func (t *pgTx) InsertWorkflowTx(wf *model.Workflow, nowMS int64) (*model.Workflow, bool, error) {
	return db.InsertWorkflowTx(t.tx, wf, nowMS)
}

func (t *pgTx) MarkExpandedTx(runID, issueID int, nowMS int64) error {
	return db.MarkExpandedTx(t.tx, runID, issueID, nowMS)
}

func (t *pgTx) MarkStepAttemptFailedTx(id int, nowMS int64) error {
	return db.MarkStepAttemptFailedTx(t.tx, id, nowMS)
}

func (t *pgTx) MarkStepClaimReapedTx(id int, nowMS int64) error {
	return db.MarkStepClaimReapedTx(t.tx, id, nowMS)
}

func (t *pgTx) MarkStepUsageRecordedTx(stepID int) error {
	return db.MarkStepUsageRecordedTx(t.tx, stepID)
}

func (t *pgTx) RaiseEventLowWatermarkTx() error {
	return db.RaiseEventLowWatermarkTx(t.tx)
}

func (t *pgTx) ReapStepTx(id int, nowMS int64) error {
	return db.ReapStepTx(t.tx, id, nowMS)
}

func (t *pgTx) RefreshClaimLeaseTx(id int, tokenHash string, ttlMS, nowMS int64) (bool, error) {
	return db.RefreshClaimLeaseTx(t.tx, id, tokenHash, ttlMS, nowMS)
}

func (t *pgTx) ReleaseStepLeaseTx(id int, nowMS int64) error {
	return db.ReleaseStepLeaseTx(t.tx, id, nowMS)
}

func (t *pgTx) ReMintStepTokenTx(id int, owner string, nowMS int64) (string, error) {
	return db.ReMintStepTokenTx(t.tx, id, owner, nowMS)
}

func (t *pgTx) ResetStepRetryBudgetTx(id int, nowMS int64) error {
	return db.ResetStepRetryBudgetTx(t.tx, id, nowMS)
}

func (t *pgTx) RetireStepTokenTx(id int) error {
	return db.RetireStepTokenTx(t.tx, id)
}

func (t *pgTx) SetConfigTx(projectID int, key, value string) (string, error) {
	return db.SetConfigTx(t.tx, projectID, key, value)
}

func (t *pgTx) SetIssueResolutionTx(issueID int, resolution string) error {
	return db.SetIssueResolutionTx(t.tx, issueID, resolution)
}

func (t *pgTx) SetRunBudgetTx(runID int, budget float64, ifVersion *int, nowMS int64) error {
	return db.SetRunBudgetTx(t.tx, runID, budget, ifVersion, nowMS)
}

func (t *pgTx) SetRunConductorHashTx(id int, hash string) error {
	return db.SetRunConductorHashTx(t.tx, id, hash)
}

func (t *pgTx) SetRunIssueBodyTx(runID, issueID int, body, sha string) error {
	return db.SetRunIssueBodyTx(t.tx, runID, issueID, body, sha)
}

func (t *pgTx) SetRunIssueSnapshotTx(runID, issueID int, snapshot string) error {
	return db.SetRunIssueSnapshotTx(t.tx, runID, issueID, snapshot)
}

func (t *pgTx) SetRunPauseOriginTx(id int, origin model.RunPauseOrigin) error {
	return db.SetRunPauseOriginTx(t.tx, id, origin)
}

func (t *pgTx) SetRunStatusTx(id int, status model.RunStatus, reason string, nowMS int64) error {
	return db.SetRunStatusTx(t.tx, id, status, reason, nowMS)
}

func (t *pgTx) SetStepAuthorityTx(id int, authority, authorityRef string, nowMS int64) error {
	return db.SetStepAuthorityTx(t.tx, id, authority, authorityRef, nowMS)
}

func (t *pgTx) SetStepClaimPhaseTx(id int, phase string, nowMS int64) error {
	return db.SetStepClaimPhaseTx(t.tx, id, phase, nowMS)
}

func (t *pgTx) SetStepLoopHistoryWithCapTx(id, roundsRun, loopCap int, triggerStep, latestVerdict string, nowMS int64) error {
	return db.SetStepLoopHistoryWithCapTx(t.tx, id, roundsRun, loopCap, triggerStep, latestVerdict, nowMS)
}

func (t *pgTx) SetStepMetadataTx(id int, metadata string, nowMS int64) error {
	return db.SetStepMetadataTx(t.tx, id, metadata, nowMS)
}

func (t *pgTx) SetStepRoutingTx(id int, routing, reason, status string, class db.ParkClass, nowMS int64) error {
	return db.SetStepRoutingTx(t.tx, id, routing, reason, status, class, nowMS)
}

func (t *pgTx) SetStepRoutingWithParkReasonTx(id int, routing, reason, status string, class db.ParkClass, parkReason string, nowMS int64) error {
	return db.SetStepRoutingWithParkReasonTx(t.tx, id, routing, reason, status, class, parkReason, nowMS)
}

func (t *pgTx) SetStepStatusTx(id int, status string, nowMS, activityMS int64) error {
	return db.SetStepStatusTx(t.tx, id, status, nowMS, activityMS)
}

func (t *pgTx) StartStepTx(id int, nowMS int64) error {
	return db.StartStepTx(t.tx, id, nowMS)
}
