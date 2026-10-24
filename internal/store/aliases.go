package store

import (
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// The row types, vocabularies and sentinels below are internal/db's own, so
// callers can name what Store returns without importing internal/db. They are
// aliases, not copies: a store value is a db value, and errors.Is matches a
// store sentinel against an error wrapped from its db counterpart.

type (
	Step                  = db.Step
	Artifact              = db.Artifact
	GateResultRow         = db.GateResultRow
	ActionResultRow       = db.ActionResultRow
	GapIssue              = db.GapIssue
	RunContext            = db.RunContext
	ParkClass             = db.ParkClass
	AckReapResult         = db.AckReapResult
	CastVoteResult        = db.CastVoteResult
	ClassMaxOverride      = db.ClassMaxOverride
	ConfigEntry           = db.ConfigEntry
	Dispatch              = db.Dispatch
	DispatchRow           = db.DispatchRow
	DocListOptions        = db.DocListOptions
	DocSummary            = db.DocSummary
	DocUpdate             = db.DocUpdate
	EventFilter           = db.EventFilter
	EventPruneSelection   = db.EventPruneSelection
	EventRow              = db.EventRow
	EventRunStatus        = db.EventRunStatus
	GateOverrideGrant     = db.GateOverrideGrant
	ListOptions           = db.ListOptions
	MetadataKeyRollup     = db.MetadataKeyRollup
	MetadataValueCount    = db.MetadataValueCount
	Pin                   = db.Pin
	ProposalRunLink       = db.ProposalRunLink
	ProposalRunLinkFilter = db.ProposalRunLinkFilter
	ReapAck               = db.ReapAck
	ResultTrailRow        = db.ResultTrailRow
	RunBudgetFacts        = db.RunBudgetFacts
	RunFence              = db.RunFence
	RunIssue              = db.RunIssue
	RunListOptions        = db.RunListOptions
	RunNote               = db.RunNote
	SchemaListOptions     = db.SchemaListOptions
	SilentVoteSeatRow     = db.SilentVoteSeatRow
	StaleTargetWaiver     = db.StaleTargetWaiver
	StepRow               = db.StepRow
	StepUsageRow          = db.StepUsageRow
	UnitTotal             = db.UnitTotal
	UsageRow              = db.UsageRow
	VerdictCount          = db.VerdictCount
	VoteUsageCoverage     = db.VoteUsageCoverage
	WorkflowListOptions   = db.WorkflowListOptions
	WorkflowVersion       = db.WorkflowVersion
)

var (
	ErrStepNotFound              = db.ErrStepNotFound
	ErrSagaStageMoved            = db.ErrSagaStageMoved
	ErrConflict                  = db.ErrConflict
	ErrCycleDetected             = db.ErrCycleDetected
	ErrDispatchAlreadyOpen       = db.ErrDispatchAlreadyOpen
	ErrDuplicateRelation         = db.ErrDuplicateRelation
	ErrIssueHasParent            = db.ErrIssueHasParent
	ErrIssueInRun                = db.ErrIssueInRun
	ErrLabelColorConflict        = db.ErrLabelColorConflict
	ErrLeaseExpired              = db.ErrLeaseExpired
	ErrLeaseHeld                 = db.ErrLeaseHeld
	ErrNoEventLowWatermark       = db.ErrNoEventLowWatermark
	ErrNoOpenDispatch            = db.ErrNoOpenDispatch
	ErrNotAttached               = db.ErrNotAttached
	ErrNotFound                  = db.ErrNotFound
	ErrNotHolder                 = db.ErrNotHolder
	ErrProjectInUse              = db.ErrProjectInUse
	ErrProjectIsDefault          = db.ErrProjectIsDefault
	ErrRunNotFound               = db.ErrRunNotFound
	ErrSchemaAlreadyDeprecated   = db.ErrSchemaAlreadyDeprecated
	ErrSchemaBuiltin             = db.ErrSchemaBuiltin
	ErrSchemaConflict            = db.ErrSchemaConflict
	ErrSchemaNotFound            = db.ErrSchemaNotFound
	ErrSelfRelation              = db.ErrSelfRelation
	ErrUnknownConfigKey          = db.ErrUnknownConfigKey
	ErrUsageAlreadyRecorded      = db.ErrUsageAlreadyRecorded
	ErrValidation                = db.ErrValidation
	ErrVersionConflict           = db.ErrVersionConflict
	ErrWorkflowAlreadyDeprecated = db.ErrWorkflowAlreadyDeprecated
	ErrWorkflowConflict          = db.ErrWorkflowConflict
	ErrWorkflowNotFound          = db.ErrWorkflowNotFound
)

const (
	StepPending      = db.StepPending
	StepClaimed      = db.StepClaimed
	StepRunning      = db.StepRunning
	StepGated        = db.StepGated
	StepDone         = db.StepDone
	StepWaitingHuman = db.StepWaitingHuman
	StepSkipped      = db.StepSkipped
	StepSuperseded   = db.StepSuperseded
	StepFailedRouted = db.StepFailedRouted
	StepReady        = db.StepReady
	StepStaged       = db.StepStaged
)

const (
	SagaRecorded   = db.SagaRecorded
	SagaRouting    = db.SagaRouting
	SagaHeld       = db.SagaHeld
	SagaGatePrefix = db.SagaGatePrefix
)

const (
	ParkClassGateFailed        = db.ParkClassGateFailed
	ParkClassGateUnmatched     = db.ParkClassGateUnmatched
	ParkClassGateSkipped       = db.ParkClassGateSkipped
	ParkClassThresholdRouted   = db.ParkClassThresholdRouted
	ParkClassLoopBound         = db.ParkClassLoopBound
	ParkClassLoopStalled       = db.ParkClassLoopStalled
	ParkClassVoteRejected      = db.ParkClassVoteRejected
	ParkClassDissentHeld       = db.ParkClassDissentHeld
	ParkClassHeldRejected      = db.ParkClassHeldRejected
	ParkClassGapOnly           = db.ParkClassGapOnly
	ParkClassUnchangedHandBack = db.ParkClassUnchangedHandBack
	ParkClassPassFloor         = db.ParkClassPassFloor
	ParkClassActionFailed      = db.ParkClassActionFailed
	ParkClassAttemptsExhausted = db.ParkClassAttemptsExhausted
	ParkClassJoinMissed        = db.ParkClassJoinMissed
	ParkClassTriageUndecided   = db.ParkClassTriageUndecided
	ParkClassHeld              = db.ParkClassHeld
)

const (
	GateVerdictPass      = db.GateVerdictPass
	GateVerdictFail      = db.GateVerdictFail
	GateVerdictUnmatched = db.GateVerdictUnmatched
	GateVerdictSkipped   = db.GateVerdictSkipped
)

const (
	ActionVerdictPass      = db.ActionVerdictPass
	ActionVerdictFail      = db.ActionVerdictFail
	ActionVerdictUnmatched = db.ActionVerdictUnmatched
)

const (
	TrustKindGate   = db.TrustKindGate
	TrustKindAction = db.TrustKindAction
)

const ArtifactMaxBytes = db.ArtifactMaxBytes

const (
	AckAlreadyDone     = db.AckAlreadyDone
	AckNoSuchReap      = db.AckNoSuchReap
	AckByGuardSpawn    = db.AckByGuardSpawn
	AckByDispatchOpen  = db.AckByDispatchOpen
	AckByDispatchClose = db.AckByDispatchClose
)

const (
	DispatchOpen      = db.DispatchOpen
	DispatchClosed    = db.DispatchClosed
	DispatchAbandoned = db.DispatchAbandoned

	CloseReasonTTL                  = db.CloseReasonTTL
	CloseReasonReconciled           = db.CloseReasonReconciled
	CloseReasonAcceptedMissingUsage = db.CloseReasonAcceptedMissingUsage
	CloseReasonOperator             = db.CloseReasonOperator
)

const (
	ClaimEndFailed     = db.ClaimEndFailed
	ClaimEndReaped     = db.ClaimEndReaped
	ClaimPhasePreGates = db.ClaimPhasePreGates
)

const (
	DefaultProjectID      = db.DefaultProjectID
	UnregisteredProjectID = db.UnregisteredProjectID
)

const (
	EngineAuthor             = db.EngineAuthor
	IssueResolutionAbandoned = db.IssueResolutionAbandoned
	ScopeVoteCreate          = db.ScopeVoteCreate
)

const (
	ClassMaxAuto          = db.ClassMaxAuto
	KeyAutoRegister       = db.KeyAutoRegister
	KeyBudgetDefault      = db.KeyBudgetDefault
	KeyBudgetUnit         = db.KeyBudgetUnit
	KeyClassMaxPrefix     = db.KeyClassMaxPrefix
	KeyContextErrorBytes  = db.KeyContextErrorBytes
	KeyContextWarnBytes   = db.KeyContextWarnBytes
	KeyEventsRetain       = db.KeyEventsRetain
	KeyLeaseTTLDefault    = db.KeyLeaseTTLDefault
	KeyLeaseTTLPrefix     = db.KeyLeaseTTLPrefix
	KeyUsageBudgetDefault = db.KeyUsageBudgetDefault
	KeyUsageBudgetUnit    = db.KeyUsageBudgetUnit
	KeyVoteHoldCost       = db.KeyVoteHoldCost
	KeyVoteHoldRule       = db.KeyVoteHoldRule
	KeyVoteHoldVoters     = db.KeyVoteHoldVoters
	KeyVoteRulePrefix     = db.KeyVoteRulePrefix
)

const (
	PinKindWorkflow = db.PinKindWorkflow
	PinKindFile     = db.PinKindFile
	PinKindSchema   = db.PinKindSchema
)

const (
	UnitNameMaxBytes     = db.UnitNameMaxBytes
	UsageSourceReported  = db.UsageSourceReported
	UsageUnitsMax        = db.UsageUnitsMax
	VoteMetadataMaxBytes = db.VoteMetadataMaxBytes
)

// The functions below forward to internal/db helpers that touch no database
// handle; their contracts are documented there. They are functions rather
// than func-typed variables so no importer can reassign them.

func DefaultProjectIDOr(id int) int { return db.DefaultProjectIDOr(id) }

func DoneTransitionClearsResolution(prior, target model.Status, explicitResolution bool) bool {
	return db.DoneTransitionClearsResolution(prior, target, explicitResolution)
}

func KnownConfigKeys() []string { return db.KnownConfigKeys() }

func ParseUsage(raw string) ([]UsageRow, error) { return db.ParseUsage(raw) }

func RenderMetadataValue(v any) string { return db.RenderMetadataValue(v) }

func RoutingRecord(routing, reason string) string { return db.RoutingRecord(routing, reason) }

func SplitNameList(value string) []string { return db.SplitNameList(value) }

func StepOffScheduler(status string) bool { return db.StepOffScheduler(status) }

func StepTerminal(status string) bool { return db.StepTerminal(status) }

func ValidateUnitName(name string) error { return db.ValidateUnitName(name) }

func VoteRuleCriticalityKey(rule string) string { return db.VoteRuleCriticalityKey(rule) }

func VoteRuleHoldOnDissentKey(rule string) string { return db.VoteRuleHoldOnDissentKey(rule) }

func VoteRuleSealedKey(rule string) string { return db.VoteRuleSealedKey(rule) }

func VoteRuleThresholdKey(rule string) string { return db.VoteRuleThresholdKey(rule) }
