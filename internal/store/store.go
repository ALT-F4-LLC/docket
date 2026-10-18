// Package store is the storage seam between Docket's engine and CLI and the
// database that holds their state. Each implementation owns its SQL; callers
// see only the Store interface and the transaction types it hands out, none of
// which names a database/sql type.
//
// Store carries the operations that run on a pooled connection. Begin opens a
// write transaction (Tx); BeginReadSnapshot opens a read-only snapshot
// (ReadTx) that never takes the write lock, so it neither waits on nor blocks
// an open Tx. Tx embeds ReadTx, so every read is available inside a write.
//
// Method names match the internal/db helpers the SQLite implementation
// delegates to, including their Tx suffix; their contracts are documented
// there.
package store

import (
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Store is a handle on one Docket database. It is safe for concurrent use.
type Store interface {
	// Begin opens a write transaction. The caller must Commit or Rollback it.
	Begin() (Tx, error)
	// BeginReadSnapshot opens a consistent read-only view of committed state.
	// It takes no write lock, so it returns while a Tx is open on the same
	// database. The caller must Rollback it.
	BeginReadSnapshot() (ReadTx, error)
	// Close releases every connection the store holds.
	Close() error

	ActionOutputsFor(stepID int, action string) ([]string, error)
	ActionRollup(runID int) ([]db.VerdictCount, error)
	ActionTrail(runID int) ([]db.ResultTrailRow, error)
	AddLabelsToIssue(issueID int, labelNames []string, color string, author string) error
	AddRunIssue(runID, issueID int) error
	AllProjectPrefixes() ([]string, error)
	AttachFiles(issueID int, filePaths []string, changedBy string) error
	AuthorizeStepRead(id int, token string, nowMS int64) error
	CascadeDeleteIssue(id int) error
	CastVote(v *model.Vote) (*db.CastVoteResult, error)
	CastVoteWeighted(v *model.Vote, equal bool) (*db.CastVoteResult, error)
	ClaimIssue(id int, owner string, ttlMS int64, nowMS int64) (token string, lease *model.Lease, err error)
	CloseProposal(id int, reason string) error
	CommitProposal(id int, outcome string, escalationReason string) error
	CountActivity(issueID int) (int, error)
	CountByPriority(projectID int) (map[string]int, error)
	CountByStatus(projectID int) (map[string]int, error)
	CountIssues(projectID int) (int, error)
	CountRootIssues(projectID int) (int, error)
	CreateCommentIdempotent(comment *model.Comment, idempotencyKey string) (int, error)
	CreateDocCommentIdempotent(c *model.DocComment, idempotencyKey string) (int, error)
	CreateDocIdempotent(doc *model.Doc, idempotencyKey string) (int, error)
	CreateIssueIdempotent(issue *model.Issue, labels []string, files []string, idempotencyKey string) (int, bool, error)
	CreateProposalIdempotent(p *model.Proposal, idempotencyKey string) (int, error)
	CreateRelation(rel *model.Relation) (int, error)
	DeleteDoc(id int, cascade bool) error
	DeleteIssue(id int) error
	DeleteLabel(labelID int, name, author string) ([]int, error)
	DeleteProject(id int) error
	DeleteRelation(sourceID, targetID int, relType string) error
	DeprecateSchema(projectID int, name string, version int, nowMS int64) (*model.Schema, error)
	DeprecateWorkflow(projectID int, name string, version int, nowMS int64) (*model.Workflow, error)
	DetachFiles(issueID int, filePaths []string, changedBy string) error
	EnsureProject(identity, name string, nowMS int64) (int, error)
	EnsureProjectCreated(identity, name string, nowMS int64) (int, bool, error)
	EventsRetain() (time.Duration, error)
	GateOverrideGrantsForRun(runID int) ([]db.GateOverrideGrant, error)
	GateResultsForStep(stepID int) ([]db.GateResultRow, error)
	GateRollup(runID int) ([]db.VerdictCount, error)
	GateTrail(runID int) ([]db.ResultTrailRow, error)
	GetActivity(issueID int, limit int) ([]model.Activity, error)
	GetAllDirectionalRelations() ([]model.Relation, error)
	GetAllRelations(projectID int) ([]model.Relation, error)
	GetBatchSubIssueProgress(parentIDs []int) (map[int][2]int, error)
	GetComment(id int) (*model.Comment, error)
	GetConfig(projectID int, key string) (db.ConfigEntry, error)
	GetDoc(id int) (*model.Doc, error)
	GetDocComment(id int) (*model.DocComment, error)
	GetDocProposals(docID int) ([]int, error)
	GetDocRevision(docID, rev int) (*model.DocRevision, error)
	GetIssue(id int) (*model.Issue, error)
	GetIssueFiles(issueID int) ([]string, error)
	GetIssueLabelObjects(issueID int) ([]*model.Label, error)
	GetIssueLabels(issueID int) ([]string, error)
	GetIssueLease(id int) (*model.Lease, error)
	GetIssueProposals(issueID int) ([]model.Proposal, error)
	GetIssueRelations(issueID int) ([]model.Relation, error)
	GetIssuesByIDs(ids []int) (map[int]*model.Issue, error)
	GetLabelByName(projectID int, name string) (*model.LabelWithCount, error)
	GetProject(id int) (*model.Project, error)
	GetProposal(id int) (*model.Proposal, error)
	GetProposalDocs(proposalID int) ([]int, error)
	GetProposalIssues(proposalID int) ([]int, error)
	GetProposalVotes(proposalID int) ([]*model.Vote, error)
	GetRun(id int) (*model.Run, error)
	GetSchema(projectID int, name string, version int) (*model.Schema, error)
	GetStep(id int) (*db.Step, error)
	GetSubIssues(parentID int) ([]*model.Issue, error)
	GetVersion(table string, id int) (int, error)
	GetWorkflow(projectID int, name string, version int) (*model.Workflow, error)
	HasGateResult(stepID int, gate string) (bool, error)
	HeartbeatIssue(id int, token string, ttlMS int64, nowMS int64) (*model.Lease, error)
	HeartbeatStep(id int, token string, ttlMS, nowMS int64) (*model.Lease, error)
	HydrateDocs(issues []*model.Issue) error
	HydrateLinkedIssues(docIDs []int) (map[int][]model.IssueRef, error)
	IdempotencyKeyOf(scope string, entityID int) (string, bool, error)
	Initialize() error
	InsertRunWithContextIdempotent(projectID int, request string, budget float64, nowMS int64, ctx db.RunContext, idempotencyKey string, issueIDs ...int) (run *model.Run, inserted bool, err error)
	InsertSchema(schema *model.Schema, nowMS int64) (stored *model.Schema, created bool, err error)
	InsertWorkflow(wf *model.Workflow, nowMS int64) (stored *model.Workflow, created bool, err error)
	IsDescendant(issueID, potentialDescendantID int) (bool, error)
	IssueExists(issueID int) (bool, error)
	IssueOwnerPrefix(issueID int) (string, error)
	IssueProjectID(issueID int) (int, error)
	IssueScopeGlobs(issueID int) (string, error)
	IssueStepRuns(issueID int) ([]int, error)
	LeaseTTL(projectID int, class string) (time.Duration, error)
	LinkDocIssue(docID, issueID int) error
	LinkProposalIssue(proposalID, issueID int) error
	ListAllActivity(projectID int) ([]*model.Activity, error)
	ListAllComments(projectID int) ([]*model.Comment, error)
	ListAllDocComments(projectID int) ([]*model.DocComment, error)
	ListAllDocIssueLinks(projectID int) ([]model.DocIssueLink, error)
	ListAllDocRevisions(projectID int) ([]*model.DocRevision, error)
	ListAllDocs(projectID int) ([]*model.Doc, error)
	ListAllIssueFileMappings(projectID int) ([]model.IssueFileMapping, error)
	ListAllIssueLabelMappings(projectID int) ([]model.IssueLabelMapping, error)
	ListAllIssues(projectID int) ([]*model.Issue, error)
	ListAllLabels(projectID int) ([]*model.LabelWithCount, error)
	ListAllLabelsRaw(projectID int) ([]*model.Label, error)
	ListAllProposalDocs(projectID int) ([]model.ProposalDocLink, error)
	ListAllProposalIssues(projectID int) ([]model.ProposalIssueLink, error)
	ListAllProposals(projectID int) ([]*model.Proposal, error)
	ListAllVotes(projectID int) ([]*model.Vote, error)
	ListComments(issueID int) ([]*model.Comment, error)
	ListConfig(projectID int) ([]db.ConfigEntry, error)
	ListDocComments(docID int) ([]*model.DocComment, error)
	ListDocRevisions(docID int) ([]*model.DocRevision, error)
	ListDocsWithCounts(opts db.DocListOptions) ([]*db.DocSummary, int, error)
	ListIssues(opts db.ListOptions) ([]*model.Issue, int, error)
	ListPins(runID int) ([]db.Pin, error)
	ListProjects() ([]*model.Project, error)
	ListProposals(projectID int, status string, criticality string, domainTag string, limit int) ([]*model.Proposal, int, error)
	ListRunArtifacts(runID int) ([]*db.Artifact, error)
	ListRunIssues(runID int) ([]*db.RunIssue, error)
	ListRunNotes(runID int) ([]db.RunNote, error)
	ListRuns(opts db.RunListOptions) ([]*model.Run, int, error)
	ListRunSteps(runID int) ([]*db.Step, error)
	ListSchemas(opts db.SchemaListOptions) ([]*model.Schema, int, error)
	ListStepArtifacts(stepID int) ([]*db.Artifact, error)
	ListWorkflows(opts db.WorkflowListOptions) ([]*model.Workflow, int, error)
	LookupIdempotencyKey(scope, key string) (int, bool, error)
	LookupIdempotencyKeys(scope, prefix string) (map[string]int, error)
	LookupProject(identity string) (int, bool, error)
	MetadataRollup(runID int) ([]db.MetadataKeyRollup, error)
	Migrate() error
	MoveIssueProject(issueID, targetProjectID int, author string, nowMS int64) ([]int, error)
	NextGateOrdinal(stepID int, gate string) (int, error)
	OrphanSubIssues(parentID int, author string) error
	PrefixHolder(prefix string, exclude int) (int, error)
	ProjectPrefix(id int) (string, error)
	ProjectRefCounts(id int) (map[string]int, error)
	ProposalIDsNaming(token string) ([]int, error)
	ProposalRunLinks(filter db.ProposalRunLinkFilter) ([]db.ProposalRunLink, error)
	RegisteredVoteRules(projectID int) ([]string, error)
	ReleaseIssue(id int, token string, nowMS int64) (*model.Lease, error)
	RemoveLabelsFromIssue(issueID int, labelNames []string, author string) error
	RemoveRunIssue(runID, issueID int) error
	RestoreSchema(projectID int, name string, version int) (*model.Schema, error)
	RestoreWorkflow(projectID int, name string, version int) (*model.Workflow, error)
	RunBudgetFactsFor(runID int) (db.RunBudgetFacts, error)
	RunConductorHash(id int) (string, error)
	RunProjectID(runID int) (int, error)
	SchemaVersion() (int, error)
	SetConfig(projectID int, key, value string) error
	SetIssueFiles(issueID int, filePaths []string, changedBy string) error
	SetIssueScopeGlobs(issueID int, globsJSON string) error
	SetProjectPrefix(id int, prefix string) error
	SilentVoteSeatsFor(scope, prefix string, extraIDs ...int) ([]db.SilentVoteSeatRow, error)
	StaleTargetWaiversForRun(runID int) ([]db.StaleTargetWaiver, error)
	StepInputCount(stepID int) (int, error)
	StepStatusCounts(runID int) ([]model.StatusCount, error)
	UnlinkDocIssue(docID, issueID int) error
	UnlinkProposalIssue(proposalID, issueID int) error
	UpdateDoc(id int, upd db.DocUpdate) (int, error)
	UpdateIssueCAS(id int, updates map[string]interface{}, changedBy string, ifVersion *int) error
	UpdateIssueCASLease(id int, updates map[string]interface{}, changedBy string, ifVersion *int, token string) error
	UpdateIssueCASNote(id int, updates map[string]interface{}, changedBy string, ifVersion *int, note string) error
	UsageByStep(runID int) ([]db.StepUsageRow, error)
	UsageByUnit(runID int) ([]db.UnitTotal, error)
	VoteMetadataRollup(scope, prefix string) ([]db.MetadataKeyRollup, error)
	VoteRuleExists(projectID int, rule string) (bool, error)
	VoteRuleSetElsewhere(projectID int, rule string) (projects int, value string, err error)
	VoteUsageCoverageFor(scope, prefix string, extraIDs ...int) (db.VoteUsageCoverage, error)
	VoteUsageRollup(scope, prefix string, extraIDs ...int) ([]db.UnitTotal, error)
	WorkflowVersionsFor(projectID int, names []string) ([]db.WorkflowVersion, error)
}

// ReadTx is a transaction's read surface. BeginReadSnapshot returns one on
// its own; every Tx is also one.
type ReadTx interface {
	// Rollback ends the transaction without committing.
	Rollback() error

	AutoRegisterEnabledTx(projectID int) (bool, error)
	BudgetDefaultTx(projectID int) (float64, error)
	BudgetUnitTx(projectID int) (string, error)
	ClassMaxOverridesTx(projectID int) (map[string]db.ClassMaxOverride, error)
	DispatchGraceTx(projectID int) (time.Duration, error)
	DispatchTTLTx(projectID int) (time.Duration, error)
	GateResultsForStepTx(stepID int) ([]db.GateResultRow, error)
	GetArtifactTx(id int) (*db.Artifact, error)
	GetConfigTx(projectID int, key string) (db.ConfigEntry, error)
	GetDispatchTx(id int) (*db.Dispatch, error)
	GetIssueFilesTx(issueID int) ([]string, error)
	GetProposalTx(id int) (*model.Proposal, error)
	GetProposalVotesTx(proposalID int) ([]*model.Vote, error)
	GetRunIssueTx(runID, issueID int) (*db.RunIssue, error)
	GetRunTx(id int) (*model.Run, error)
	GetSchemaTx(projectID int, name string, version int) (*model.Schema, error)
	GetStepTx(id int) (*db.Step, error)
	IssueScopeGlobsTx(issueID int) (string, error)
	IssueStatusResolutionTx(issueID int) (model.Status, string, error)
	ListDispatchRowsTx(dispatchID int) ([]db.DispatchRow, error)
	ListIssueStepsTx(runID, issueID int) ([]*db.Step, error)
	ListPinsTx(runID int) ([]db.Pin, error)
	ListRunArtifactsTx(runID int) ([]*db.Artifact, error)
	ListRunIssuesTx(runID int) ([]*db.RunIssue, error)
	ListRunNotesTx(runID int) ([]db.RunNote, error)
	ListRunStepsTx(runID int) ([]*db.Step, error)
	ListStepInputArtifactsTx(stepID int) ([]*db.Artifact, error)
	LookupIdempotencyKeysTx(scope, prefix string) (map[string]int, error)
	LookupIdempotencyKeyTx(scope, key string) (int, bool, error)
	LoopGrantsTx(runID, issueID int) (int, error)
	NextActionOrdinalTx(stepID int, action string) (int, error)
	OpenDispatchTx(runID int) (*db.Dispatch, error)
	ProposalStatusesTx(ids []int) (map[int]model.ProposalStatus, error)
	RegisteredVoteRulesTx(projectID int) ([]string, error)
	ReportedUsageTx(runID int, unit string) (float64, error)
	RunBudgetFactsTx(runID int) (db.RunBudgetFacts, error)
	RunConductorHashTx(id int) (string, error)
	RunEverDispatchedTx(runID int) (bool, error)
	RunPauseOriginTx(id int) (model.RunPauseOrigin, error)
	RunProjectIDTx(runID int) (int, error)
	UnacknowledgedReapsTx(runID int) ([]db.ReapAck, error)
	UsageBudgetUnitTx(projectID int) (string, error)
	VoteRuleExistsTx(projectID int, rule string) (bool, error)
	VoteRuleSetElsewhereTx(projectID int, rule string) (projects int, value string, err error)
}

// Tx is a write transaction: the read surface plus every write.
type Tx interface {
	ReadTx

	// Commit makes the transaction's writes durable.
	Commit() error

	AckReapTx(runID int, seq int64, ackedBy string, nowMS int64) (db.AckReapResult, error)
	AdvanceSagaTx(id int, from, to string, nowMS int64) error
	AuthorizeStepTx(id int, token string, nowMS int64) (*model.Lease, error)
	BindRunIssueTx(ri *db.RunIssue) error
	BreachRunBudgetTx(runID int, reason string, nowMS int64) (bool, error)
	CacheRunFloorTx(runID int, floor float64) error
	ClaimStepWithRecoveryTx(id int, owner, recoverySecret string, ttlMS, nowMS int64) (token string, lease *model.Lease, err error)
	ClearProjectDataTx(projectID int) error
	ClearRunBreachTx(runID int, newReason string, nowMS int64) error
	ClearStaleBudgetReasonTx(runID int, newReason string, nowMS int64) error
	ClearStepInputsTx(stepID int) error
	ClearStepStartTx(id int) error
	CloseDispatchTx(id int, status, reason string, nowMS int64) (bool, error)
	CloseOpenProposalsTx(ids []int, reason string) (int, error)
	CoverGateOverrideGrantsTx(ids []int) error
	ExemptStepAttemptFromBudgetTx(id int, nowMS int64) error
	ExtendDispatchTx(id int, expiresMS, extendedSeq int64) (bool, error)
	GrantLoopTx(runID, issueID int) (int, error)
	IncrementLoopCountTx(runID, issueID int) (int, error)
	InsertActionResultTx(r db.ActionResultRow) error
	InsertActivityWithID(a *model.Activity) (bool, error)
	InsertArtifactTx(a db.Artifact, nowMS int64) (int, error)
	InsertCommentWithID(comment *model.Comment) (bool, error)
	InsertDispatchRowTx(dispatchID int, row db.DispatchRow) error
	InsertDispatchTx(runID int, openedSeq, expiresMS, nowMS int64) (int, error)
	InsertDocCommentWithID(c *model.DocComment) (bool, error)
	InsertDocIssueLink(docID, issueID int, createdAt string) (bool, error)
	InsertDocRevisionWithID(r *model.DocRevision) (bool, error)
	InsertDocWithID(doc *model.Doc) (bool, error)
	InsertEngineComment(issueID int, body string, nowMS int64) (int, error)
	InsertFenceTx(f db.RunFence) error
	InsertGapIssueTx(projectID int, gap db.GapIssue, relatedIssueID int) (int, error)
	InsertGateOverrideGrantTx(g db.GateOverrideGrant) (int, error)
	InsertGateResultTx(r db.GateResultRow) error
	InsertIssueFileMapping(issueID int, filePath string) (bool, error)
	InsertIssueLabelMapping(issueID, labelID int) (bool, error)
	InsertIssueWithID(issue *model.Issue) (bool, error)
	InsertLabelWithID(label *model.Label) (bool, error)
	InsertPinTx(p db.Pin) error
	InsertProposalDocLink(proposalID, docID int, createdAt string) (bool, error)
	InsertProposalIssueLink(proposalID, issueID int) (bool, error)
	InsertProposalWithID(p *model.Proposal) (bool, error)
	InsertReapAckTx(ack db.ReapAck, nowMS int64) error
	InsertRelationWithID(rel *model.Relation) (bool, error)
	InsertRunNoteTx(n db.RunNote) (int, error)
	InsertSchemaTx(schema *model.Schema, nowMS int64) (stored *model.Schema, created bool, err error)
	InsertStaleTargetWaiverTx(w db.StaleTargetWaiver) (int, error)
	InsertStepInputTx(stepID, position, artifactID int) error
	InsertStepTx(step db.StepRow, nowMS int64) error
	InsertTrustCacheTx(runID int, kind, gate, argvSHA256, entryName string, matched, prefix bool, atMS int64) error
	InsertUsageRowTx(row db.UsageRow, nowMS int64) error
	InsertVoteUsageTx(voteID int64, unit string, quantity float64, source string, nowMS int64) error
	InsertVoteWithID(v *model.Vote) (bool, error)
	InsertWorkflowTx(wf *model.Workflow, nowMS int64) (stored *model.Workflow, created bool, err error)
	MarkExpandedTx(runID, issueID int, nowMS int64) error
	MarkStepAttemptFailedTx(id int, nowMS int64) error
	MarkStepClaimReapedTx(id int, nowMS int64) error
	MarkStepUsageRecordedTx(stepID int) error
	ReapStepTx(id int, nowMS int64) error
	RefreshClaimLeaseTx(id int, tokenHash string, ttlMS, nowMS int64) (bool, error)
	ReleaseStepLeaseTx(id int, nowMS int64) error
	ReMintStepTokenTx(id int, owner string, nowMS int64) (token string, err error)
	ResetStepRetryBudgetTx(id int, nowMS int64) error
	RetireStepTokenTx(id int) error
	SetConfigTx(projectID int, key, value string) (prior string, err error)
	SetIssueResolutionTx(issueID int, resolution string) error
	SetRunBudgetTx(runID int, budget float64, ifVersion *int, nowMS int64) error
	SetRunConductorHashTx(id int, hash string) error
	SetRunIssueBodyTx(runID, issueID int, body, sha string) error
	SetRunIssueSnapshotTx(runID, issueID int, snapshot string) error
	SetRunPauseOriginTx(id int, origin model.RunPauseOrigin) error
	SetRunStatusTx(id int, status model.RunStatus, reason string, nowMS int64) error
	SetStepAuthorityTx(id int, authority, authorityRef string, nowMS int64) error
	SetStepClaimPhaseTx(id int, phase string, nowMS int64) error
	SetStepLoopHistoryWithCapTx(id, roundsRun, loopCap int, triggerStep, latestVerdict string, nowMS int64) error
	SetStepMetadataTx(id int, metadata string, nowMS int64) error
	SetStepRoutingTx(id int, routing, reason, status string, class db.ParkClass, nowMS int64) error
	SetStepRoutingWithParkReasonTx(id int, routing, reason, status string, class db.ParkClass, parkReason string, nowMS int64) error
	SetStepStatusTx(id int, status string, nowMS, activityMS int64) error
	StartStepTx(id int, nowMS int64) error
}
