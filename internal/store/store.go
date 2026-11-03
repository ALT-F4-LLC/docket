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

// Row types returned by the engine issue queries, aliased as in aliases.go.
type (
	FrozenIssueBinding = db.FrozenIssueBinding
	PinnedArtifact     = db.PinnedArtifact
)

// The event and run-fact query inputs, aliased as in aliases.go so callers
// can build them without importing internal/db.
type (
	NewEvent           = db.NewEvent
	ChainDeferralQuery = db.ChainDeferralQuery
)

// The routing and loop query inputs and rows, aliased as in aliases.go so
// callers can build and read them without importing internal/db.
type (
	DispatchDeferralQuery = db.DispatchDeferralQuery
	OrdinalPayload        = db.OrdinalPayload
	RunStepRollup         = db.RunStepRollup
	SourcedDeferralQuery  = db.SourcedDeferralQuery
	StepArtifact          = db.StepArtifact
	StepArtifactRange     = db.StepArtifactRange
	StepInstanceRef       = db.StepInstanceRef
	StepStatusRouting     = db.StepStatusRouting
)

// The saga query inputs and rows, aliased as in aliases.go so callers can
// build and read them without importing internal/db.
type (
	FenceCommand            = db.FenceCommand
	FirstRecorderAfterQuery = db.FirstRecorderAfterQuery
)

// The report and ledger query rows, aliased as in aliases.go so callers can
// read them without importing internal/db.
type (
	IssueEvent   = db.IssueEvent
	StepEvent    = db.StepEvent
	StepProducer = db.StepProducer
)

// The run lifecycle and repin query rows, aliased as in aliases.go so callers
// can read them without importing internal/db.
type (
	RunEvent       = db.RunEvent
	StepEventSeq   = db.StepEventSeq
	UnfinishedStep = db.UnfinishedStep
)

// The claim and context query inputs, aliased as in aliases.go so callers can
// build them without importing internal/db.
type (
	ClaimEventsPrunedQuery = db.ClaimEventsPrunedQuery
	IssueEventAfterQuery   = db.IssueEventAfterQuery
)

// The dispatch and guard query inputs and rows, aliased as in aliases.go so
// callers can build and read them without importing internal/db.
type (
	GateStepQuery = db.GateStepQuery
	GateStepState = db.GateStepState
)

// The hold, activation and definition query rows, aliased as in aliases.go so
// callers can read them without importing internal/db.
type (
	InstanceStatusRouting = db.InstanceStatusRouting
	ParsedWorkflow        = db.ParsedWorkflow
	StepArtifactRef       = db.StepArtifactRef
)

// The closed set of tables the import id probes accept, aliased as in
// aliases.go so callers can name them without importing internal/db.
type ImportTable = db.ImportTable

const (
	ImportIssues         = db.ImportIssues
	ImportLabels         = db.ImportLabels
	ImportComments       = db.ImportComments
	ImportIssueRelations = db.ImportIssueRelations
	ImportActivityLog    = db.ImportActivityLog
	ImportProposals      = db.ImportProposals
	ImportVotes          = db.ImportVotes
	ImportDocs           = db.ImportDocs
	ImportDocRevisions   = db.ImportDocRevisions
	ImportDocComments    = db.ImportDocComments
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
	ActiveGateSteps(q db.GateStepQuery) ([]db.GateStepState, error)
	ActiveStartedRunIDs(projectID int) ([]int, error)
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
	FirstIssueEventDataAfter(q db.IssueEventAfterQuery) (data string, found bool, err error)
	FirstRecorderAfterMetadata(q db.FirstRecorderAfterQuery) (metadata string, found bool, err error)
	FirstStepEventData(stepID int, kind string) (data string, found bool, err error)
	FrozenIssueBindings(issueID int) ([]db.FrozenIssueBinding, error)
	GateOverrideGrantsForRun(runID int) ([]db.GateOverrideGrant, error)
	GateResultsForStep(stepID int) ([]db.GateResultRow, error)
	GateRollup(runID int) ([]db.VerdictCount, error)
	GateTrail(runID int) ([]db.ResultTrailRow, error)
	GatedStepIDAtOrdinal(runID, issueID int, stepName string, ordinal int) (id int, found bool, err error)
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
	HoldInstances(runID, issueID int, holdInstance string) ([]db.InstanceStatusRouting, error)
	HydrateDocs(issues []*model.Issue) error
	HydrateLinkedIssues(docIDs []int) (map[int][]model.IssueRef, error)
	IdempotencyKeyOf(scope string, entityID int) (string, bool, error)
	Initialize() error
	InsertRunWithContextIdempotent(projectID int, request string, budget float64, nowMS int64, ctx db.RunContext, idempotencyKey string, issueIDs ...int) (run *model.Run, inserted bool, err error)
	InsertSchema(schema *model.Schema, nowMS int64) (stored *model.Schema, created bool, err error)
	InsertWorkflow(wf *model.Workflow, nowMS int64) (stored *model.Workflow, created bool, err error)
	IsDescendant(issueID, potentialDescendantID int) (bool, error)
	IssueDeclaration(issueID int) (description, scope string, err error)
	IssueExists(issueID int) (bool, error)
	IssueOwnerPrefix(issueID int) (string, error)
	IssueProjectID(issueID int) (int, error)
	IssueScopeGlobs(issueID int) (string, error)
	IssueStepRuns(issueID int) ([]int, error)
	LatestIssueEvent(issueID int, kind string) (event db.IssueEvent, found bool, err error)
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
	NewestIssueArtifact(runID, issueID int, kind string) (body, payload string, found bool, err error)
	NewestIssueArtifactBodies(runID, issueID int, kind string, limit int) ([]string, error)
	NewestIssueArtifactConsumedByDoneStep(runID, issueID int, kind string) (artifactID int, payload string, found bool, err error)
	NewestIssueArtifactPayloadExcept(runID, issueID int, kind string, exceptStepID int) (payload string, found bool, err error)
	NewestStepArtifactBody(stepID int, kind string) (body string, found bool, err error)
	NewestStepNamePayloadBelowOrdinal(runID, issueID int, stepName string, ordinal int, kind string) (payload string, found bool, err error)
	NewestStepPayload(stepID int, kind string) (payload string, found bool, err error)
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
	RunBoundWorkflows(runID int) ([]db.ParsedWorkflow, error)
	RunBudgetFactsFor(runID int) (db.RunBudgetFacts, error)
	RunConductorHash(id int) (string, error)
	RunEventSpanMS(runID int, kinds ...string) (first, last int64, found bool, err error)
	RunFenceCommands(runID, issueID int, tag string) (commands []db.FenceCommand, found bool, err error)
	RunIssueSnapshot(runID, issueID int) (string, error)
	RunProjectID(runID int) (int, error)
	RunStepEvents(runID int, kinds ...string) ([]db.StepEvent, error)
	RunStepProducers(runID int) ([]db.StepProducer, error)
	RunStepWorkflows(runID int) ([]db.ParsedWorkflow, error)
	SchemaVersion() (int, error)
	SetConfig(projectID int, key, value string) error
	SetIssueFiles(issueID int, filePaths []string, changedBy string) error
	SetIssueScopeGlobs(issueID int, globsJSON string) error
	SetProjectPrefix(id int, prefix string) error
	SilentVoteSeatsFor(scope, prefix string, extraIDs ...int) ([]db.SilentVoteSeatRow, error)
	StaleTargetWaiversForRun(runID int) ([]db.StaleTargetWaiver, error)
	StepArtifactCount(stepID int, kind string) (int, error)
	StepEventDataContains(runID, stepID int, kind, text string) (bool, error)
	StepIDAtOrdinal(runID, issueID int, stepName string, ordinal int) (id int, found bool, err error)
	StepInputCount(stepID int) (int, error)
	StepStatusCounts(runID int) ([]model.StatusCount, error)
	StrandedSagaStepIDs(runID int, excludedKind string, updatedBeforeMS int64) ([]int, error)
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

	ActivationIssueTx(id int) (*model.Issue, error)
	AutoRegisterEnabledTx(projectID int) (bool, error)
	BudgetDefaultTx(projectID int) (float64, error)
	BudgetUnitTx(projectID int) (string, error)
	ClaimAdmittedByDispatchTx(dispatchID int, openedSeq int64, stepID int, claimKind string, reapedSeq int64) (bool, error)
	ClaimEventsPrunedTx(q db.ClaimEventsPrunedQuery) (bool, error)
	ClassMaxOverridesTx(projectID int) (map[string]db.ClassMaxOverride, error)
	ConductorChainDeferralTx(q db.ChainDeferralQuery) (seq int64, data string, found bool, err error)
	CountEventsHeldByRetentionTx(sel db.EventPruneSelection, cutoffMS int64) (int, error)
	CountEventsTx(f db.EventFilter) (int, error)
	CountPrunableEventsTx(sel db.EventPruneSelection) (int, error)
	DeferralRecordedForDispatchTx(q db.DispatchDeferralQuery) (bool, error)
	DispatchGraceTx(projectID int) (time.Duration, error)
	DispatchTTLTx(projectID int) (time.Duration, error)
	EventDataTx(seq int64) (string, error)
	EventKindCountsTx(runID int) (map[string]int, error)
	EventLowWatermarkTx() (int64, error)
	FirstIssueEventDataAfterTx(q db.IssueEventAfterQuery) (data string, found bool, err error)
	ForeignHoldingStepsTx(projectID, runID int) ([]*db.Step, error)
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
	HighestOrdinalsTx(runID, issueID int) (map[string]int, error)
	HoldClusterInstancesTx(runID, issueID int, holdInstance string) ([]db.InstanceStatusRouting, error)
	ImportMaxIDTx(table db.ImportTable) (int, error)
	ImportRowExistsTx(table db.ImportTable, id int) (bool, error)
	ImportRowOwnedTx(table db.ImportTable, id, projectID int) (bool, error)
	IssueDependenciesDoneTx(issueID int) (bool, error)
	IssueDescriptionTx(issueID int) (string, error)
	IssueHasParkedOrHeldStepTx(runID, issueID int) (bool, error)
	IssueLabelsTx(issueID int) ([]string, error)
	IssueLatestInstancesTerminalTx(runID, issueID int) (bool, error)
	IssueSchedulingFactsTx(issueID int) (priority model.Priority, scope string, err error)
	IssueScopeGlobsTx(issueID int) (string, error)
	IssueStatusResolutionTx(issueID int) (model.Status, string, error)
	IssueStatusTx(issueID int) (model.Status, error)
	IssueStepsBelowOrdinalTx(runID, issueID, ordinal int) ([]db.StepInstanceRef, error)
	IssueUnfinishedStepsTx(runID, issueID int) ([]db.UnfinishedStep, error)
	LabelIDByNameTx(projectID int, name string) (id int, found bool, err error)
	LastDispatchDeferralTx(q db.SourcedDeferralQuery) (seq int64, data string, found bool, err error)
	LastEventAtMSTx() (atMS int64, found bool, err error)
	LastEventSeqTx() (int64, error)
	LatestEventDataTx(runID int, kind string) (data string, found bool, err error)
	LatestRecordedArtifactIDTx(issueID int, kind string) (id int, found bool, err error)
	ListDispatchRowsTx(dispatchID int) ([]db.DispatchRow, error)
	ListEventsTx(f db.EventFilter, limit, tail int) ([]db.EventRow, error)
	ListFencesTx(runID int) ([]db.RunFence, error)
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
	MaxEventSeqTx() (seq int64, found bool, err error)
	NewestIssueArtifactUpToTx(runID, issueID int, kind string, ordinal int) (hash, body string, found bool, err error)
	NewestStepArtifactExceptKindsTx(stepID int, exceptKinds []string) (db.StepArtifactRef, bool, error)
	NewestStepNameArtifactTx(r db.StepArtifactRange) (db.StepArtifact, bool, error)
	NewestStepPayloadTx(stepID int, kind string) (payload string, found bool, err error)
	NextActionOrdinalTx(stepID int, action string) (int, error)
	NextGateOrdinalTx(stepID int, gate string) (int, error)
	OpenDispatchTx(runID int) (*db.Dispatch, error)
	PendingDeclaredStepCountTx(runID, issueID int, kinds []string) (int, error)
	PendingInstancesUpToTx(runID, issueID int, stepName string, ordinal int) ([]db.StepInstanceRef, error)
	PinnedArtifactTx(id int) (db.PinnedArtifact, error)
	PriorDispatchClosesTx(runID int, closedKind string) ([]string, error)
	ProjectIdentityTx(projectID int) (string, error)
	ProjectWorkflowsTx(projectID int) ([]*model.Workflow, error)
	PrunableEventRunsTx(sel db.EventPruneSelection) ([]db.EventRunStatus, error)
	ProposalExistsTx(proposalID int) (bool, error)
	ProposalStatusesTx(ids []int) (map[int]model.ProposalStatus, error)
	ProposalStatusTx(id int) (status model.ProposalStatus, found bool, err error)
	RegisteredVoteRulesTx(projectID int) ([]string, error)
	RelationSourcesTx(targetID int, rt model.RelationType) ([]int, error)
	RelationTargetsTx(sourceID int, rt model.RelationType) ([]int, error)
	ReportedUsageTx(runID int, unit string) (float64, error)
	RunBudgetFactsTx(runID int) (db.RunBudgetFacts, error)
	RunConductorHashTx(id int) (string, error)
	RunCostFloorTx(runID int, claimEventKind, voteStepKind string) (float64, error)
	RunEventDataTx(runID int, kinds ...string) ([]string, error)
	RunEventDetailExistsTx(runID int, kind, detail string) (bool, error)
	RunEventsTx(runID int, kinds ...string) ([]db.RunEvent, error)
	RunEventIssueIDsTx(runID int, kind string) (map[int]bool, error)
	RunEverDispatchedTx(runID int) (bool, error)
	RunExpectedCostTx(runID int) (float64, error)
	RunHasIssueTx(runID, issueID int) (bool, error)
	RunIssueEventsTx(runID int, kind string) ([]db.IssueEvent, error)
	RunIssueSnapshotsTx(runID, issueID int) (body, snapshot string, err error)
	RunPauseOriginTx(id int) (model.RunPauseOrigin, error)
	RunProjectIDTx(runID int) (int, error)
	RunStatusTx(runID int) (model.RunStatus, error)
	RunStepEventSeqsTx(runID int) ([]db.StepEventSeq, error)
	RunStepEventsTx(runID int, kinds ...string) ([]db.StepEvent, error)
	RunStepRollupTx(runID int) (db.RunStepRollup, error)
	RunStepWorkflowsTx(runID int) ([]db.ParsedWorkflow, error)
	RunUnfinishedStepCountTx(runID int) (int, error)
	RunWorktreesTx(runID, issueID int) ([]string, error)
	StepArtifactPayloadsTx(r db.StepArtifactRange) ([]db.OrdinalPayload, error)
	StepByInstanceTx(runID, issueID int, instance string) (db.StepInstanceRef, error)
	StepHasLaterInstanceTx(runID, issueID int, stepName string, ordinal int) (bool, error)
	StepIDByInstanceTx(runID, issueID int, instance string) (id int, found bool, err error)
	StepInstancesAtOrdinalTx(runID, issueID int, stepName string, ordinal int) ([]db.StepStatusRouting, error)
	StepNamesAtOrdinalTx(runID, issueID, ordinal int) (map[string]bool, error)
	StepRecoveryHashTx(stepID int) (hash string, found bool, err error)
	StepRoundCountTx(runID, issueID int, names []string) (int, error)
	StepRowVersionTx(stepID int) (int, error)
	StepsWithEventKindsSinceTx(runID int, afterSeq int64, kinds []string) (map[int]bool, error)
	UnacknowledgedReapsTx(runID int) ([]db.ReapAck, error)
	UnexpandedRunIssueCountTx(runID int) (int, error)
	UsageBudgetUnitTx(projectID int) (string, error)
	VoteIDTx(proposalID int, voter string) (id int64, found bool, err error)
	VoteRuleExistsTx(projectID int, rule string) (bool, error)
	VoteRuleSetElsewhereTx(projectID int, rule string) (projects int, value string, err error)
	WorkGraphRelationsTx(issueIDs []int) ([]model.Relation, error)
}

// Tx is a write transaction: the read surface plus every write.
type Tx interface {
	ReadTx

	// Commit makes the transaction's writes durable.
	Commit() error

	AckReapTx(runID int, seq int64, ackedBy string, nowMS int64) (db.AckReapResult, error)
	AddPinTx(p db.Pin) (int, error)
	AdvanceSagaTx(id int, from, to string, nowMS int64) error
	AuthorizeStepTx(id int, token string, nowMS int64) (*model.Lease, error)
	BindRunIssueTx(ri *db.RunIssue) error
	BreachRunBudgetTx(runID int, reason string, nowMS int64) (bool, error)
	CacheRunFloorTx(runID int, floor float64) error
	CascadeFailIssueStepsTx(runID, issueID, exceptStepID int, routing string, nowMS int64) (int, error)
	ClaimStepWithRecoveryTx(id int, owner, recoverySecret string, ttlMS, nowMS int64) (token string, lease *model.Lease, err error)
	ClearProjectDataTx(projectID int) error
	ClearRunBreachTx(runID int, newReason string, nowMS int64) error
	ClearStaleBudgetReasonTx(runID int, newReason string, nowMS int64) error
	ClearStepInputsTx(stepID int) error
	ClearStepStartTx(id int) error
	CloseDispatchTx(id int, status, reason string, nowMS int64) (bool, error)
	CloseOpenProposalsTx(ids []int, reason string) (int, error)
	CompleteIssueTx(issueID int, prior model.Status, resolution string, nowMS int64) (bool, error)
	CoverGateOverrideGrantsTx(ids []int) error
	DeletePrunableEventsTx(sel db.EventPruneSelection) (int, error)
	DropPinTx(runID int, kind, ref, oldSHA string) (int, error)
	ExemptStepAttemptFromBudgetTx(id int, nowMS int64) error
	ExtendDispatchTx(id int, expiresMS, extendedSeq int64) (bool, error)
	FailRouteIssueStepsTx(runID, issueID int, nowMS int64) (int, error)
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
	InsertEventTx(e db.NewEvent) error
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
	MoveIssueStatusTx(issueID int, from, to model.Status, nowMS int64) (moved bool, err error)
	RaiseEventLowWatermarkTx() error
	ReapStepTx(id int, nowMS int64) error
	RefreshClaimLeaseTx(id int, tokenHash string, ttlMS, nowMS int64) (bool, error)
	ReleaseStepLeaseTx(id int, nowMS int64) error
	ReMintStepTokenTx(id int, owner string, nowMS int64) (token string, err error)
	RepinPinTx(runID int, kind, ref, oldSHA, newSHA string) (int, error)
	RepinStepWorkRootTx(stepID int, worktree string) error
	ResetStepRetryBudgetTx(id int, nowMS int64) error
	RestoreIssueParentTx(issueID, parentID int) error
	RetireStepTokenTx(id int) error
	SetConfigTx(projectID int, key, value string) (prior string, err error)
	SetIssueResolutionTx(issueID int, resolution string) error
	SetRunActiveTx(runID int, reactivation bool, nowMS int64) error
	SetRunBudgetTx(runID int, budget float64, ifVersion *int, nowMS int64) error
	SetRunConductorHashTx(id int, hash string) error
	SetRunIssueBodyTx(runID, issueID int, body, sha string) error
	SetRunIssueLoopCountTx(runID, issueID, count int) error
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
	SetStepWorkRootTx(stepID int, workRoot string) error
	StartStepTx(id int, nowMS int64) error
	TransitionRunStatusTx(runID int, to model.RunStatus, nowMS int64) (from model.RunStatus, moved bool, err error)
}
