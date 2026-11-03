package engine

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// StepDefinitions loads the PARSED definition of every workflow a run's steps
// were expanded from, keyed by workflow id.
//
// It restores from `workflows.parsed`, never by re-parsing `body` — the parsed
// JSON is the PINNED INTERPRETATION. Re-parsing the TOML here would
// mean a parser change silently re-interpreting a definition a run already
// pinned, which is the thing version pinning exists to prevent. That rule
// applies with more force here than at activation: activation happens once,
// but every readiness computation, every context assembly, and every routing
// decision for the life of the run comes through here.
//
// It is keyed by workflow id rather than by name because a run may legally bind
// two issues to two different versions of the same pipeline, and the step row
// records which one it was expanded from.
func StepDefinitions(conn store.LegacyDB, runID int) (map[int]*workflow.Definition, error) {
	st := store.StoreOn(conn)
	stepWorkflows, err := st.RunStepWorkflows(runID)
	if err != nil {
		return nil, err
	}
	out, err := decodeStepDefinitions(stepWorkflows)
	if err != nil {
		return nil, err
	}

	// A run whose issues are BOUND but not yet expanded has no step rows and
	// therefore no definitions above. Fall back to the bindings so readiness
	// over a freshly-activated run whose later phases have not expanded still
	// resolves `after` edges rather than treating every step as unknown.
	bound, err := st.RunBoundWorkflows(runID)
	if err != nil {
		return nil, err
	}
	for _, w := range bound {
		if _, ok := out[w.ID]; ok {
			continue
		}
		def, err := workflow.FromCanonical([]byte(w.Parsed))
		if err != nil {
			return nil, fmt.Errorf("reading stored definition %d: %w", w.ID, err)
		}
		out[w.ID] = def
	}
	return out, nil
}

// stepSpec returns the definition of one step, from its run's pinned
// workflow. A step whose definition is missing is a corrupted run rather than a
// recoverable state — the workflow row is referenced by a foreign key — so the
// caller treats a nil result as an error rather than a default.
//
// `tally` reaches the synthesized spec of a materialized held step; see
// materializedSpec. Its zero value is the default, and a caller holding a
// DECLARED step is unaffected by it.
func stepSpec(
	defs map[int]*workflow.Definition, step *store.Step, tally holdTally,
) *workflow.Step {
	def := defs[step.WorkflowID]
	if def == nil {
		return nil
	}
	return materializedSpec(def, step, tally)
}

// decodeStepDefinitions decodes the stored parsed form of each workflow both
// readers return, keyed by workflow id.
func decodeStepDefinitions(workflows []store.ParsedWorkflow) (map[int]*workflow.Definition, error) {
	out := make(map[int]*workflow.Definition, len(workflows))
	for _, w := range workflows {
		def, err := workflow.FromCanonical([]byte(w.Parsed))
		if err != nil {
			return nil, fmt.Errorf("reading stored definition %d: %w", w.ID, err)
		}
		out[w.ID] = def
	}
	return out, nil
}

// StepDefinitionsTx is StepDefinitions inside a transaction, for readers that
// must see rows the open transaction wrote — the dry run's, whose steps exist
// only there and are about to be discarded.
func StepDefinitionsTx(tx store.LegacyReadTx, runID int) (map[int]*workflow.Definition, error) {
	stepWorkflows, err := store.TxOn(tx).RunStepWorkflowsTx(runID)
	if err != nil {
		return nil, err
	}
	return decodeStepDefinitions(stepWorkflows)
}
