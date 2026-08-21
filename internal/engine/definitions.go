package engine

import (
	"database/sql"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// StepDefinitions loads the PARSED definition of every workflow a run's steps
// were expanded from, keyed by workflow id.
//
// It restores from `workflows.parsed`, never by re-parsing `body` — the parsed
// JSON is the PINNED INTERPRETATION (TDD §4.1). Re-parsing the TOML here would
// mean a parser change silently re-interpreting a definition a run already
// pinned, which is the thing version pinning exists to prevent. That rule
// applies with more force at S3 than at activation: activation happens once,
// but every readiness computation, every context assembly, and every routing
// decision for the life of the run comes through here.
//
// It is keyed by workflow id rather than by name because a run may legally bind
// two issues to two different versions of the same pipeline, and the step row
// records which one it was expanded from.
func StepDefinitions(conn *sql.DB, runID int) (map[int]*workflow.Definition, error) {
	rows, err := conn.Query(
		`SELECT DISTINCT w.id, w.parsed
		   FROM steps s JOIN workflows w ON w.id = s.workflow_id
		  WHERE s.run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading step definitions: %w", err)
	}
	defer rows.Close()

	out, err := scanStepDefinitions(rows)
	if err != nil {
		return nil, err
	}

	// A run whose issues are BOUND but not yet expanded has no step rows and
	// therefore no definitions above. Fall back to the bindings so readiness
	// over a freshly-activated run whose later phases have not expanded still
	// resolves `after` edges rather than treating every step as unknown.
	if err := addBoundDefinitions(conn, runID, out); err != nil {
		return nil, err
	}
	return out, nil
}

// addBoundDefinitions fills in the definitions of workflows bound to the run's
// issues but not represented among its expanded steps.
func addBoundDefinitions(conn *sql.DB, runID int, out map[int]*workflow.Definition) error {
	rows, err := conn.Query(
		`SELECT DISTINCT w.id, w.parsed
		   FROM run_issues ri JOIN workflows w ON w.id = ri.workflow_id
		  WHERE ri.run_id = ? AND ri.workflow_id IS NOT NULL`, runID)
	if err != nil {
		return fmt.Errorf("reading bound definitions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id     int
			parsed string
		)
		if err := rows.Scan(&id, &parsed); err != nil {
			return fmt.Errorf("reading bound definition: %w", err)
		}
		if _, ok := out[id]; ok {
			continue
		}
		def, err := workflow.FromCanonical([]byte(parsed))
		if err != nil {
			return fmt.Errorf("reading stored definition %d: %w", id, err)
		}
		out[id] = def
	}
	return rows.Err()
}

// stepSpec returns the §11.1 definition of one step, from its run's pinned
// workflow. A step whose definition is missing is a corrupted run rather than a
// recoverable state — the workflow row is referenced by a foreign key — so the
// caller treats a nil result as an error rather than a default.
//
// `tally` reaches the synthesized spec of a materialized held step; see
// materializedSpec. Its zero value is the default, and a caller holding a
// DECLARED step is unaffected by it.
func stepSpec(
	defs map[int]*workflow.Definition, step *db.Step, tally holdTally,
) *workflow.Step {
	def := defs[step.WorkflowID]
	if def == nil {
		return nil
	}
	return materializedSpec(def, step, tally)
}

// scanStepDefinitions decodes the (id, parsed) rows both readers produce.
func scanStepDefinitions(rows *sql.Rows) (map[int]*workflow.Definition, error) {
	out := make(map[int]*workflow.Definition)
	for rows.Next() {
		var (
			id     int
			parsed string
		)
		if err := rows.Scan(&id, &parsed); err != nil {
			return nil, fmt.Errorf("reading step definition: %w", err)
		}
		def, err := workflow.FromCanonical([]byte(parsed))
		if err != nil {
			return nil, fmt.Errorf("reading stored definition %d: %w", id, err)
		}
		out[id] = def
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading step definitions: %w", err)
	}
	return out, nil
}

// StepDefinitionsTx is StepDefinitions inside a transaction, for readers that
// must see rows the open transaction wrote — the dry run's, whose steps exist
// only there and are about to be discarded.
func StepDefinitionsTx(tx *sql.Tx, runID int) (map[int]*workflow.Definition, error) {
	rows, err := tx.Query(
		`SELECT DISTINCT w.id, w.parsed
		   FROM steps s JOIN workflows w ON w.id = s.workflow_id
		  WHERE s.run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading step definitions: %w", err)
	}
	defer rows.Close()
	return scanStepDefinitions(rows)
}
