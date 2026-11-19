package store

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// TestStoreSignaturesNameOnlyReExportedDBTypes reads store.go and requires a
// store export for every db.X it names, so a caller can spell every Store and
// Tx signature without importing internal/db.
func TestStoreSignaturesNameOnlyReExportedDBTypes(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{}
	for _, m := range regexp.MustCompile(`\bdb\.([A-Z][A-Za-z0-9_]*)`).FindAllSubmatch(src, -1) {
		named[string(m[1])] = true
	}
	if len(named) == 0 {
		t.Fatal("store.go names no db.X; the scan pattern no longer matches its signatures")
	}

	exported := storeExports(t)
	var missing []string
	for name := range named {
		if !exported[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("store.go names db types with no store export: %s", strings.Join(missing, ", "))
	}
}

// storeExports lists the exported top-level names declared in this package's
// non-test sources.
func storeExports(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	exported := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.IsExported() {
					exported[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							exported[s.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								exported[n.Name] = true
							}
						}
					}
				}
			}
		}
	}
	return exported
}

// TestAliasedTypesAreTheDBTypes assigns a db value to each store type and the
// store value back to the db type with no conversion, which compiles only for
// an alias.
func TestAliasedTypesAreTheDBTypes(t *testing.T) {
	var step Step = db.Step{}
	var artifact Artifact = db.Artifact{}
	var gate GateResultRow = db.GateResultRow{}
	var action ActionResultRow = db.ActionResultRow{}
	var gap GapIssue = db.GapIssue{}
	var runCtx RunContext = db.RunContext{}
	var class ParkClass = db.ParkClass("")
	var ackResult AckReapResult = db.AckReapResult(0)
	var vote CastVoteResult = db.CastVoteResult{}
	var classMax ClassMaxOverride = db.ClassMaxOverride{}
	var config ConfigEntry = db.ConfigEntry{}
	var dispatch Dispatch = db.Dispatch{}
	var dispatchRow DispatchRow = db.DispatchRow{}
	var docList DocListOptions = db.DocListOptions{}
	var docSummary DocSummary = db.DocSummary{}
	var docUpdate DocUpdate = db.DocUpdate{}
	var eventFilter EventFilter = db.EventFilter{}
	var pruneSel EventPruneSelection = db.EventPruneSelection{}
	var eventRow EventRow = db.EventRow{}
	var eventRun EventRunStatus = db.EventRunStatus{}
	var grant GateOverrideGrant = db.GateOverrideGrant{}
	var list ListOptions = db.ListOptions{}
	var metaRollup MetadataKeyRollup = db.MetadataKeyRollup{}
	var metaCount MetadataValueCount = db.MetadataValueCount{}
	var pin Pin = db.Pin{}
	var link ProposalRunLink = db.ProposalRunLink{}
	var linkFilter ProposalRunLinkFilter = db.ProposalRunLinkFilter{}
	var ack ReapAck = db.ReapAck{}
	var trail ResultTrailRow = db.ResultTrailRow{}
	var budget RunBudgetFacts = db.RunBudgetFacts{}
	var fence RunFence = db.RunFence{}
	var runIssue RunIssue = db.RunIssue{}
	var runList RunListOptions = db.RunListOptions{}
	var note RunNote = db.RunNote{}
	var schemaList SchemaListOptions = db.SchemaListOptions{}
	var seat SilentVoteSeatRow = db.SilentVoteSeatRow{}
	var waiver StaleTargetWaiver = db.StaleTargetWaiver{}
	var stepRow StepRow = db.StepRow{}
	var stepUsage StepUsageRow = db.StepUsageRow{}
	var unitTotal UnitTotal = db.UnitTotal{}
	var usage UsageRow = db.UsageRow{}
	var verdicts VerdictCount = db.VerdictCount{}
	var coverage VoteUsageCoverage = db.VoteUsageCoverage{}
	var wfList WorkflowListOptions = db.WorkflowListOptions{}
	var wfVersion WorkflowVersion = db.WorkflowVersion{}

	var _ db.Step = step
	var _ db.Artifact = artifact
	var _ db.GateResultRow = gate
	var _ db.ActionResultRow = action
	var _ db.GapIssue = gap
	var _ db.RunContext = runCtx
	var _ db.ParkClass = class
	var _ db.AckReapResult = ackResult
	var _ db.CastVoteResult = vote
	var _ db.ClassMaxOverride = classMax
	var _ db.ConfigEntry = config
	var _ db.Dispatch = dispatch
	var _ db.DispatchRow = dispatchRow
	var _ db.DocListOptions = docList
	var _ db.DocSummary = docSummary
	var _ db.DocUpdate = docUpdate
	var _ db.EventFilter = eventFilter
	var _ db.EventPruneSelection = pruneSel
	var _ db.EventRow = eventRow
	var _ db.EventRunStatus = eventRun
	var _ db.GateOverrideGrant = grant
	var _ db.ListOptions = list
	var _ db.MetadataKeyRollup = metaRollup
	var _ db.MetadataValueCount = metaCount
	var _ db.Pin = pin
	var _ db.ProposalRunLink = link
	var _ db.ProposalRunLinkFilter = linkFilter
	var _ db.ReapAck = ack
	var _ db.ResultTrailRow = trail
	var _ db.RunBudgetFacts = budget
	var _ db.RunFence = fence
	var _ db.RunIssue = runIssue
	var _ db.RunListOptions = runList
	var _ db.RunNote = note
	var _ db.SchemaListOptions = schemaList
	var _ db.SilentVoteSeatRow = seat
	var _ db.StaleTargetWaiver = waiver
	var _ db.StepRow = stepRow
	var _ db.StepUsageRow = stepUsage
	var _ db.UnitTotal = unitTotal
	var _ db.UsageRow = usage
	var _ db.VerdictCount = verdicts
	var _ db.VoteUsageCoverage = coverage
	var _ db.WorkflowListOptions = wfList
	var _ db.WorkflowVersion = wfVersion
}

// TestAliasedConstantsEqualTheDBConstants compares through interface values,
// so a store constant must match its db counterpart in both type and value.
func TestAliasedConstantsEqualTheDBConstants(t *testing.T) {
	for _, c := range []struct {
		name       string
		store, dbv any
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
		{"ArtifactMaxBytes", ArtifactMaxBytes, db.ArtifactMaxBytes},
		{"AckAlreadyDone", AckAlreadyDone, db.AckAlreadyDone},
		{"AckNoSuchReap", AckNoSuchReap, db.AckNoSuchReap},
		{"AckByGuardSpawn", AckByGuardSpawn, db.AckByGuardSpawn},
		{"AckByDispatchOpen", AckByDispatchOpen, db.AckByDispatchOpen},
		{"AckByDispatchClose", AckByDispatchClose, db.AckByDispatchClose},
		{"DispatchOpen", DispatchOpen, db.DispatchOpen},
		{"DispatchClosed", DispatchClosed, db.DispatchClosed},
		{"DispatchAbandoned", DispatchAbandoned, db.DispatchAbandoned},
		{"CloseReasonTTL", CloseReasonTTL, db.CloseReasonTTL},
		{"CloseReasonReconciled", CloseReasonReconciled, db.CloseReasonReconciled},
		{"CloseReasonAcceptedMissingUsage", CloseReasonAcceptedMissingUsage, db.CloseReasonAcceptedMissingUsage},
		{"CloseReasonOperator", CloseReasonOperator, db.CloseReasonOperator},
		{"ClaimEndFailed", ClaimEndFailed, db.ClaimEndFailed},
		{"ClaimEndReaped", ClaimEndReaped, db.ClaimEndReaped},
		{"ClaimPhasePreGates", ClaimPhasePreGates, db.ClaimPhasePreGates},
		{"ClassMaxAuto", ClassMaxAuto, db.ClassMaxAuto},
		{"DefaultProjectID", DefaultProjectID, db.DefaultProjectID},
		{"UnregisteredProjectID", UnregisteredProjectID, db.UnregisteredProjectID},
		{"EngineAuthor", EngineAuthor, db.EngineAuthor},
		{"IssueResolutionAbandoned", IssueResolutionAbandoned, db.IssueResolutionAbandoned},
		{"KeyAutoRegister", KeyAutoRegister, db.KeyAutoRegister},
		{"KeyBudgetDefault", KeyBudgetDefault, db.KeyBudgetDefault},
		{"KeyBudgetUnit", KeyBudgetUnit, db.KeyBudgetUnit},
		{"KeyClassMaxPrefix", KeyClassMaxPrefix, db.KeyClassMaxPrefix},
		{"KeyContextErrorBytes", KeyContextErrorBytes, db.KeyContextErrorBytes},
		{"KeyContextWarnBytes", KeyContextWarnBytes, db.KeyContextWarnBytes},
		{"KeyEventsRetain", KeyEventsRetain, db.KeyEventsRetain},
		{"KeyLeaseTTLDefault", KeyLeaseTTLDefault, db.KeyLeaseTTLDefault},
		{"KeyLeaseTTLPrefix", KeyLeaseTTLPrefix, db.KeyLeaseTTLPrefix},
		{"KeyUsageBudgetDefault", KeyUsageBudgetDefault, db.KeyUsageBudgetDefault},
		{"KeyUsageBudgetUnit", KeyUsageBudgetUnit, db.KeyUsageBudgetUnit},
		{"KeyVoteHoldCost", KeyVoteHoldCost, db.KeyVoteHoldCost},
		{"KeyVoteHoldRule", KeyVoteHoldRule, db.KeyVoteHoldRule},
		{"KeyVoteHoldVoters", KeyVoteHoldVoters, db.KeyVoteHoldVoters},
		{"KeyVoteRulePrefix", KeyVoteRulePrefix, db.KeyVoteRulePrefix},
		{"PinKindWorkflow", PinKindWorkflow, db.PinKindWorkflow},
		{"PinKindFile", PinKindFile, db.PinKindFile},
		{"PinKindSchema", PinKindSchema, db.PinKindSchema},
		{"ScopeVoteCreate", ScopeVoteCreate, db.ScopeVoteCreate},
		{"UnitNameMaxBytes", UnitNameMaxBytes, db.UnitNameMaxBytes},
		{"UsageSourceReported", UsageSourceReported, db.UsageSourceReported},
		{"UsageUnitsMax", UsageUnitsMax, db.UsageUnitsMax},
		{"VoteMetadataMaxBytes", VoteMetadataMaxBytes, db.VoteMetadataMaxBytes},
	} {
		if c.store != c.dbv {
			t.Errorf("store.%s = %#v, db.%s = %#v", c.name, c.store, c.name, c.dbv)
		}
	}
}

func TestAliasedSentinelsMatchTheDBErrors(t *testing.T) {
	for _, c := range []struct {
		name       string
		store, dbv error
	}{
		{"ErrStepNotFound", ErrStepNotFound, db.ErrStepNotFound},
		{"ErrSagaStageMoved", ErrSagaStageMoved, db.ErrSagaStageMoved},
		{"ErrConflict", ErrConflict, db.ErrConflict},
		{"ErrCycleDetected", ErrCycleDetected, db.ErrCycleDetected},
		{"ErrDispatchAlreadyOpen", ErrDispatchAlreadyOpen, db.ErrDispatchAlreadyOpen},
		{"ErrDuplicateRelation", ErrDuplicateRelation, db.ErrDuplicateRelation},
		{"ErrIssueHasParent", ErrIssueHasParent, db.ErrIssueHasParent},
		{"ErrIssueInRun", ErrIssueInRun, db.ErrIssueInRun},
		{"ErrLabelColorConflict", ErrLabelColorConflict, db.ErrLabelColorConflict},
		{"ErrLeaseExpired", ErrLeaseExpired, db.ErrLeaseExpired},
		{"ErrLeaseHeld", ErrLeaseHeld, db.ErrLeaseHeld},
		{"ErrNoEventLowWatermark", ErrNoEventLowWatermark, db.ErrNoEventLowWatermark},
		{"ErrNoOpenDispatch", ErrNoOpenDispatch, db.ErrNoOpenDispatch},
		{"ErrNotAttached", ErrNotAttached, db.ErrNotAttached},
		{"ErrNotFound", ErrNotFound, db.ErrNotFound},
		{"ErrNotHolder", ErrNotHolder, db.ErrNotHolder},
		{"ErrProjectInUse", ErrProjectInUse, db.ErrProjectInUse},
		{"ErrProjectIsDefault", ErrProjectIsDefault, db.ErrProjectIsDefault},
		{"ErrRunNotFound", ErrRunNotFound, db.ErrRunNotFound},
		{"ErrSchemaAlreadyDeprecated", ErrSchemaAlreadyDeprecated, db.ErrSchemaAlreadyDeprecated},
		{"ErrSchemaBuiltin", ErrSchemaBuiltin, db.ErrSchemaBuiltin},
		{"ErrSchemaConflict", ErrSchemaConflict, db.ErrSchemaConflict},
		{"ErrSchemaNotFound", ErrSchemaNotFound, db.ErrSchemaNotFound},
		{"ErrSelfRelation", ErrSelfRelation, db.ErrSelfRelation},
		{"ErrUnknownConfigKey", ErrUnknownConfigKey, db.ErrUnknownConfigKey},
		{"ErrUsageAlreadyRecorded", ErrUsageAlreadyRecorded, db.ErrUsageAlreadyRecorded},
		{"ErrValidation", ErrValidation, db.ErrValidation},
		{"ErrVersionConflict", ErrVersionConflict, db.ErrVersionConflict},
		{"ErrWorkflowAlreadyDeprecated", ErrWorkflowAlreadyDeprecated, db.ErrWorkflowAlreadyDeprecated},
		{"ErrWorkflowConflict", ErrWorkflowConflict, db.ErrWorkflowConflict},
		{"ErrWorkflowNotFound", ErrWorkflowNotFound, db.ErrWorkflowNotFound},
	} {
		if !errors.Is(c.dbv, c.store) {
			t.Errorf("errors.Is(db.%s, store.%s) = false", c.name, c.name)
		}
		if wrapped := fmt.Errorf("reading step: %w", c.dbv); !errors.Is(wrapped, c.store) {
			t.Errorf("errors.Is(wrapped db.%s, store.%s) = false", c.name, c.name)
		}
	}
}

func TestForwardedHelpersReturnTheDBResults(t *testing.T) {
	errText := func(err error) string {
		if err == nil {
			return "<nil>"
		}
		return err.Error()
	}
	usageRows, usageErr := ParseUsage(`{"tokens": 12, "usd": 0.5}`)
	dbUsageRows, dbUsageErr := db.ParseUsage(`{"tokens": 12, "usd": 0.5}`)
	_, badUsageErr := ParseUsage(`[1]`)
	_, dbBadUsageErr := db.ParseUsage(`[1]`)
	if usageErr != nil || len(usageRows) != 2 || badUsageErr == nil {
		t.Fatalf("ParseUsage samples do not exercise both paths: rows=%v err=%v refusal=%v", usageRows, usageErr, badUsageErr)
	}
	if ValidateUnitName("bad unit\n") == nil {
		t.Fatal("ValidateUnitName sample is not a refused name")
	}

	for _, c := range []struct {
		name       string
		store, dbv any
	}{
		{"KnownConfigKeys", KnownConfigKeys(), db.KnownConfigKeys()},
		{"RenderMetadataValue", RenderMetadataValue(map[string]any{"k": 1.5}), db.RenderMetadataValue(map[string]any{"k": 1.5})},
		{"DefaultProjectIDOr(0)", DefaultProjectIDOr(0), db.DefaultProjectIDOr(0)},
		{"DefaultProjectIDOr(7)", DefaultProjectIDOr(7), db.DefaultProjectIDOr(7)},
		{"DoneTransitionClearsResolution", DoneTransitionClearsResolution(model.StatusDone, model.StatusTodo, false), db.DoneTransitionClearsResolution(model.StatusDone, model.StatusTodo, false)},
		{"ParseUsage rows", usageRows, dbUsageRows},
		{"ParseUsage error", errText(usageErr), errText(dbUsageErr)},
		{"ParseUsage refusal", errText(badUsageErr), errText(dbBadUsageErr)},
		{"RoutingRecord", RoutingRecord("park", "gate failed"), db.RoutingRecord("park", "gate failed")},
		{"SplitNameList", SplitNameList(" a, b ,,c "), db.SplitNameList(" a, b ,,c ")},
		{"StepOffScheduler", StepOffScheduler(db.StepWaitingHuman), db.StepOffScheduler(db.StepWaitingHuman)},
		{"StepTerminal", StepTerminal(db.StepDone), db.StepTerminal(db.StepDone)},
		{"ValidateUnitName valid", errText(ValidateUnitName("tokens")), errText(db.ValidateUnitName("tokens"))},
		{"ValidateUnitName invalid", errText(ValidateUnitName("bad unit\n")), errText(db.ValidateUnitName("bad unit\n"))},
		{"VoteRuleCriticalityKey", VoteRuleCriticalityKey("security"), db.VoteRuleCriticalityKey("security")},
		{"VoteRuleHoldOnDissentKey", VoteRuleHoldOnDissentKey("security"), db.VoteRuleHoldOnDissentKey("security")},
		{"VoteRuleSealedKey", VoteRuleSealedKey("security"), db.VoteRuleSealedKey("security")},
		{"VoteRuleThresholdKey", VoteRuleThresholdKey("security"), db.VoteRuleThresholdKey("security")},
	} {
		if !reflect.DeepEqual(c.store, c.dbv) {
			t.Errorf("store.%s = %#v, db.%s = %#v", c.name, c.store, c.name, c.dbv)
		}
	}
}
