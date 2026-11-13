package engine

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// WorkflowsReferencingSchema is WorkflowsReferencingSchemaStore on store.StoreOn(conn).
func WorkflowsReferencingSchema(
	conn store.LegacyDB, projectID int, name string, version int,
) ([]string, error) {
	return WorkflowsReferencingSchemaStore(store.StoreOn(conn), projectID, name, version)
}

// WorkflowsReferencingSchemaStore lists the workflow versions in one project that
// are still IN SERVICE and whose definition names schema `name@version` as a
// step's `payload`.
//
// It is the question `docket schema deprecate` asks before retiring a version:
// a workflow that still binds and still references the schema would have its
// next registration or activation refused by the retirement (V25a's retired
// branch), so the verb refuses first and names the referencers instead of
// leaving that refusal for the next operator to find.
//
// RETIRED WORKFLOW VERSIONS DO NOT COUNT. A workflow already out of binding
// cannot be newly activated, and a run that pinned it keeps its pins
// regardless; counting it would make a cleanup pass unable to retire a schema
// whose only referencers were themselves already cleaned up.
//
// The definitions are read from the stored canonical form — what activation
// reads — rather than re-parsed from the recorded TOML, so what is checked is
// what would run.
func WorkflowsReferencingSchemaStore(
	storage store.Store, projectID int, name string, version int,
) ([]string, error) {
	conn, err := store.SQLDB(storage)
	if err != nil {
		return nil, err
	}
	workflows, _, err := store.StoreOn(conn).ListWorkflows(store.WorkflowListOptions{
		ProjectID: projectID, ExcludeDeprecated: true,
	})
	if err != nil {
		return nil, fmt.Errorf("listing workflows for project %d: %w", projectID, err)
	}

	ref := fmt.Sprintf("%s@%d", name, version)
	var referencing []string
	for _, wf := range workflows {
		def, err := workflow.FromCanonical([]byte(wf.Parsed))
		if err != nil {
			return nil, fmt.Errorf("reading the stored definition of %s: %w", wf.Ref(), err)
		}
		for _, step := range def.Steps {
			if step.Payload == ref {
				referencing = append(referencing, wf.Ref())
				break
			}
		}
	}
	return referencing, nil
}
