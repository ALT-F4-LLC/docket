package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
	"github.com/spf13/cobra"
)

// `docket schema deprecate` — retire a registered schema version from service.
//
// THE GAP THIS CLOSES. `registry audit` reports orphaned schemas — names no
// file in any instance-config root declares — but a schema had no way to be
// retired: the verb group offered only register, list, and show, and the only
// way to clear an orphan was a direct edit to the store. `workflow deprecate`
// existed; this is its schema half, with the same never-delete contract.
//
// THE ONE RULE THIS VERB ADDS. A schema is REFERENCED, by name@version, from
// the `payload` of registered workflow steps, and a workflow is never
// referenced by anything. So retiring a schema a workflow still names would
// make that workflow's next registration or activation fail on V25a's retired
// branch, after the fact and in someone else's terminal. The verb refuses
// first, naming the referencing workflow versions, and offers no override: the
// operator retires or edits the workflows, then retires the schema. Only
// workflow versions still IN SERVICE count — a retired workflow cannot be newly
// activated, and a run that pinned it keeps its pins regardless.

var schemaDeprecateCmd = &cobra.Command{
	Use:   "deprecate <name>@<version>",
	Short: "Retire a registered schema version from service",
	Long: `Retire one registered schema version from service.

The version keeps its row and stays fully readable: ` + "`schema show`" + ` still
renders it, ` + "`--body`" + ` still emits the exact bytes that were registered, and
a run that already pinned it still validates its payloads against it. Retirement
is a REGISTRATION-TIME FILTER, not a retraction, and it never deletes.

What retirement stops is NEW references. ` + "`workflow register`" + `,
` + "`workflow lint`" + `, and activation's auto-registration refuse a step whose
` + "`payload`" + ` names a retired version, naming the schema and the remedy.
` + "`schema list`" + ` hides retired versions unless --deprecated is passed, and
` + "`schema show NAME`" + ` without @version resolves the highest version still in
service. ` + "`registry audit`" + ` reports an orphaned schema whose every version
is retired as retired: true — the orphan an operator has finished with.

A SCHEMA STILL REFERENCED BY A LIVE WORKFLOW IS REFUSED. If any workflow
version in the target project that is itself still in service names this
schema as a step's payload, the verb exits CONFLICT and lists those workflow
versions; there is no override flag. Retire or re-version those workflows
first. Under --all-projects the refusal is that project's own outcome and the
sweep continues. The builtin schema shipped in the binary cannot be retired.

Use --restore to put a retired version back into service.

A registry is PER PROJECT, and so is retirement. By default this retires the
version in the project the working directory resolves to; --project retires it
in one other project, and --all-projects retires it in every project in the
store. Both report each project's own outcome: a project where the version was
never registered is reported as not-registered, one where it was already
retired as already-deprecated, and one where a live workflow still references
it as in-use, while every project that did retire it still did.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSchemaDeprecate(cmd, args, getWriter(cmd))
	},
}

// errSchemaInUse wraps the in-use refusal so the fan-out can classify it by
// sentinel, the way it classifies already-deprecated.
var errSchemaInUse = errors.New("schema is referenced by a workflow still in service")

func runSchemaDeprecate(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	// The reference grammar is the registry's own PayloadShape, so an explicit
	// version is REQUIRED by construction: a `payload` declaration names an
	// exact version, and retirement applies to exactly one.
	name, version, err := workflow.ParsePayloadRef(args[0])
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	restore, _ := cmd.Flags().GetBool("restore")

	targets, fannedOut, err := resolveRegistryTargets(cmd, conn)
	if err != nil {
		return err
	}
	if fannedOut {
		return deprecateSchemaAcrossProjects(
			cmd, w, conn, args[0], name, version, restore, targets)
	}

	if restore {
		s, err := db.RestoreSchema(conn, getProjectID(cmd), name, version)
		if err != nil {
			return schemaErr(describeSchemaDeprecateFailure(err, args[0]))
		}
		w.Success(s, fmt.Sprintf("%s restored to service.", s.Ref()))
		return nil
	}

	s, err := deprecateSchemaIn(conn, getProjectID(cmd), name, version)
	if err != nil {
		return schemaErr(describeSchemaDeprecateFailure(err, args[0]))
	}

	w.Success(s, fmt.Sprintf(
		"%s retired from service. It stays registered and readable, runs that "+
			"pinned it still validate against it, and new payload references "+
			"to it are refused.", s.Ref()))
	return nil
}

// deprecateSchemaIn is the single-project retirement: the reference check,
// then the write. The check runs OUTSIDE the write's transaction, which is
// acceptable here because a workflow registered between the two is refused by
// its own V25a check the moment the retirement lands — the race resolves to the
// same refusal either way, only attributed to the other command.
func deprecateSchemaIn(conn *sql.DB, projectID int, name string, version int) (*model.Schema, error) {
	referencing, err := engine.WorkflowsReferencingSchema(conn, projectID, name, version)
	if err != nil {
		return nil, err
	}
	if len(referencing) > 0 {
		return nil, fmt.Errorf(
			"%w: %s@%d is named as `payload` by %s. Retire or re-version those "+
				"workflows first; their next registration or activation would "+
				"otherwise be refused",
			errSchemaInUse, name, version, strings.Join(referencing, ", "))
	}
	return db.DeprecateSchema(conn, projectID, name, version, model.NowMS())
}

// describeSchemaDeprecateFailure gives a fanned-out refusal the SAME sentence
// its single-project counterpart writes. The sentinels stay wrapped, since the
// classifier and the report both match on them.
func describeSchemaDeprecateFailure(err error, ref string) error {
	switch {
	case errors.Is(err, db.ErrSchemaAlreadyDeprecated):
		return fmt.Errorf("%s is already deprecated: %w", ref, err)
	case errors.Is(err, db.ErrSchemaBuiltin):
		return fmt.Errorf("%s ships with docket and is visible to every project; "+
			"it cannot be retired: %w", ref, err)
	}
	return describeMissingSchema(err, ref)
}

// deprecateSchemaAcrossProjects is the --project / --all-projects path for
// both directions of the verb, deprecateWorkflowAcrossProjects's shape: restore
// is idempotent (already-in-service is a success), deprecate is not
// (already-deprecated is a CONFLICT), and in-use is a per-project refusal that
// never cancels a neighbor's retirement.
func deprecateSchemaAcrossProjects(
	cmd *cobra.Command, w *output.Writer, conn *sql.DB,
	ref, name string, version int, restore bool, targets []*model.Project,
) error {
	operation := "schema deprecate"
	if restore {
		operation = "schema deprecate --restore"
	}
	report := &registryFanoutReport{
		Operation: operation, Subject: ref, Scope: fanoutScope(cmd),
	}

	for _, target := range targets {
		var (
			s       *model.Schema
			err     error
			outcome string
		)
		if restore {
			before, lookupErr := db.GetSchema(conn, target.ID, name, version)
			if lookupErr != nil {
				report.Results = append(report.Results, registryFailureResult(
					target, describeSchemaDeprecateFailure(lookupErr, ref), schemaErr))
				continue
			}
			outcome = outcomeAlreadyBinding
			if before.Deprecated() {
				outcome = outcomeRestored
			}
			s, err = db.RestoreSchema(conn, target.ID, name, version)
		} else {
			outcome = outcomeDeprecated
			s, err = deprecateSchemaIn(conn, target.ID, name, version)
		}
		if err != nil {
			report.Results = append(report.Results, registryFailureResult(
				target, describeSchemaDeprecateFailure(err, ref), schemaErr))
			continue
		}
		report.Results = append(report.Results,
			registrySuccessResult(target, outcome, s.Ref()))
	}

	return finishRegistryFanout(w, report)
}

func init() {
	schemaDeprecateCmd.Flags().Bool(
		"restore", false, "Return a retired version to service")
	addRegistryTargetFlags(schemaDeprecateCmd, "Retire the version")
	schemaCmd.AddCommand(schemaDeprecateCmd)
}
