package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
)

// TestAliasedTypesAreTheDBTypes assigns each re-exported type to its db
// counterpart and back with no conversion, which compiles only for an alias.
func TestAliasedTypesAreTheDBTypes(t *testing.T) {
	var step db.Step
	var storeStep Step = step
	step = storeStep

	var artifact db.Artifact
	var storeArtifact Artifact = artifact
	artifact = storeArtifact

	var gate db.GateResultRow
	var storeGate GateResultRow = gate
	gate = storeGate

	var action db.ActionResultRow
	var storeAction ActionResultRow = action
	action = storeAction

	var gap db.GapIssue
	var storeGap GapIssue = gap
	gap = storeGap

	var runCtx db.RunContext
	var storeRunCtx RunContext = runCtx
	runCtx = storeRunCtx

	var class db.ParkClass
	var storeClass ParkClass = class
	class = storeClass

	_, _, _, _, _, _, _ = step, artifact, gate, action, gap, runCtx, class
}

func TestAliasedConstantsEqualTheDBConstants(t *testing.T) {
	strs := []struct {
		name       string
		store, dbv string
	}{
		{"StepPending", StepPending, db.StepPending},
		{"StepClaimed", StepClaimed, db.StepClaimed},
		{"StepRunning", StepRunning, db.StepRunning},
		{"StepGated", StepGated, db.StepGated},
		{"StepDone", StepDone, db.StepDone},
		{"StepWaitingHuman", StepWaitingHuman, db.StepWaitingHuman},
		{"StepSkipped", StepSkipped, db.StepSkipped},
		{"StepSuperseded", StepSuperseded, db.StepSuperseded},
		{"StepFailedRouted", StepFailedRouted, db.StepFailedRouted},
		{"StepReady", StepReady, db.StepReady},
		{"StepStaged", StepStaged, db.StepStaged},
		{"SagaRecorded", SagaRecorded, db.SagaRecorded},
		{"SagaRouting", SagaRouting, db.SagaRouting},
		{"SagaHeld", SagaHeld, db.SagaHeld},
		{"SagaGatePrefix", SagaGatePrefix, db.SagaGatePrefix},
		{"GateVerdictPass", GateVerdictPass, db.GateVerdictPass},
		{"GateVerdictFail", GateVerdictFail, db.GateVerdictFail},
		{"GateVerdictUnmatched", GateVerdictUnmatched, db.GateVerdictUnmatched},
		{"GateVerdictSkipped", GateVerdictSkipped, db.GateVerdictSkipped},
		{"ActionVerdictPass", ActionVerdictPass, db.ActionVerdictPass},
		{"ActionVerdictFail", ActionVerdictFail, db.ActionVerdictFail},
		{"ActionVerdictUnmatched", ActionVerdictUnmatched, db.ActionVerdictUnmatched},
		{"TrustKindGate", TrustKindGate, db.TrustKindGate},
		{"TrustKindAction", TrustKindAction, db.TrustKindAction},
	}
	for _, c := range strs {
		if c.store != c.dbv {
			t.Errorf("store.%s = %q, db.%s = %q", c.name, c.store, c.name, c.dbv)
		}
	}

	classes := []struct {
		name       string
		store, dbv db.ParkClass
	}{
		{"ParkClassGateFailed", ParkClassGateFailed, db.ParkClassGateFailed},
		{"ParkClassGateUnmatched", ParkClassGateUnmatched, db.ParkClassGateUnmatched},
		{"ParkClassGateSkipped", ParkClassGateSkipped, db.ParkClassGateSkipped},
		{"ParkClassThresholdRouted", ParkClassThresholdRouted, db.ParkClassThresholdRouted},
		{"ParkClassLoopBound", ParkClassLoopBound, db.ParkClassLoopBound},
		{"ParkClassLoopStalled", ParkClassLoopStalled, db.ParkClassLoopStalled},
		{"ParkClassVoteRejected", ParkClassVoteRejected, db.ParkClassVoteRejected},
		{"ParkClassDissentHeld", ParkClassDissentHeld, db.ParkClassDissentHeld},
		{"ParkClassHeldRejected", ParkClassHeldRejected, db.ParkClassHeldRejected},
		{"ParkClassGapOnly", ParkClassGapOnly, db.ParkClassGapOnly},
		{"ParkClassUnchangedHandBack", ParkClassUnchangedHandBack, db.ParkClassUnchangedHandBack},
		{"ParkClassPassFloor", ParkClassPassFloor, db.ParkClassPassFloor},
		{"ParkClassActionFailed", ParkClassActionFailed, db.ParkClassActionFailed},
		{"ParkClassAttemptsExhausted", ParkClassAttemptsExhausted, db.ParkClassAttemptsExhausted},
		{"ParkClassJoinMissed", ParkClassJoinMissed, db.ParkClassJoinMissed},
		{"ParkClassTriageUndecided", ParkClassTriageUndecided, db.ParkClassTriageUndecided},
		{"ParkClassHeld", ParkClassHeld, db.ParkClassHeld},
	}
	for _, c := range classes {
		if c.store != c.dbv {
			t.Errorf("store.%s = %q, db.%s = %q", c.name, c.store, c.name, c.dbv)
		}
	}

	if ArtifactMaxBytes != db.ArtifactMaxBytes {
		t.Errorf("store.ArtifactMaxBytes = %d, db.ArtifactMaxBytes = %d", ArtifactMaxBytes, db.ArtifactMaxBytes)
	}
}

func TestAliasedSentinelsMatchTheDBErrors(t *testing.T) {
	for _, c := range []struct {
		name       string
		store, dbv error
	}{
		{"ErrStepNotFound", ErrStepNotFound, db.ErrStepNotFound},
		{"ErrSagaStageMoved", ErrSagaStageMoved, db.ErrSagaStageMoved},
	} {
		if !errors.Is(c.dbv, c.store) {
			t.Errorf("errors.Is(db.%s, store.%s) = false", c.name, c.name)
		}
		if wrapped := fmt.Errorf("reading step: %w", c.dbv); !errors.Is(wrapped, c.store) {
			t.Errorf("errors.Is(wrapped db.%s, store.%s) = false", c.name, c.name)
		}
	}
}
