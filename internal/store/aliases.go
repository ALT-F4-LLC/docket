package store

import "github.com/ALT-F4-LLC/docket/internal/db"

// The row types, vocabularies and sentinels below are internal/db's own, so
// callers can name what Store returns without importing internal/db. They are
// aliases, not copies: a store value is a db value, and errors.Is matches a
// store sentinel against an error wrapped from its db counterpart.

type (
	Step            = db.Step
	Artifact        = db.Artifact
	GateResultRow   = db.GateResultRow
	ActionResultRow = db.ActionResultRow
	GapIssue        = db.GapIssue
	RunContext      = db.RunContext
	ParkClass       = db.ParkClass
)

var (
	ErrStepNotFound   = db.ErrStepNotFound
	ErrSagaStageMoved = db.ErrSagaStageMoved
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
