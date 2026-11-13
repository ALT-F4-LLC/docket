package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
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
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSchemaDeprecate(cmd, args, getWriter(cmd))
	},
}

// errSchemaInUse forwards to verb.ErrSchemaInUse, which the fan-out
// classifies by sentinel the way it classifies already-deprecated.
var errSchemaInUse = verb.ErrSchemaInUse

func runSchemaDeprecate(cmd *cobra.Command, args []string, w *output.Writer) error {
	restore, _ := cmd.Flags().GetBool("restore")
	project, _ := cmd.Flags().GetString("project")
	all, _ := cmd.Flags().GetBool("all-projects")

	res, err := verb.SchemaDeprecate(cmd.Context(), schemaVerbEnv(cmd), verb.SchemaDeprecateInput{
		Ref:         args[0],
		Restore:     restore,
		Project:     project,
		AllProjects: all,
	})
	recordProjectRegistration(cmd, res.Registered)
	if err != nil {
		return verbErr(err)
	}

	if res.Projects != nil {
		return renderSchemaDeprecateAcrossProjects(cmd, w, args[0], restore, res.Projects)
	}

	s := res.Schema
	if restore {
		w.Success(s, fmt.Sprintf("%s restored to service.", s.Ref()))
		return nil
	}
	w.Success(s, fmt.Sprintf(
		"%s retired from service. It stays registered and readable, runs that "+
			"pinned it still validate against it, and new payload references "+
			"to it are refused.", s.Ref()))
	return nil
}

// renderSchemaDeprecateAcrossProjects writes the --project / --all-projects
// report for both directions of the verb, deprecateWorkflowAcrossProjects's
// shape: restore is idempotent (already-in-service is a success), deprecate is
// not (already-deprecated is a CONFLICT), and in-use is a per-project refusal
// that never cancels a neighbor's retirement.
func renderSchemaDeprecateAcrossProjects(
	cmd *cobra.Command, w *output.Writer, ref string, restore bool,
	results []verb.SchemaProjectResult,
) error {
	operation := "schema deprecate"
	if restore {
		operation = "schema deprecate --restore"
	}
	report := &registryFanoutReport{
		Operation: operation, Subject: ref, Scope: fanoutScope(cmd),
	}
	for _, r := range results {
		if r.Err != nil {
			report.Results = append(report.Results,
				registryFailureResult(r.Project, r.Err, verbErr))
			continue
		}
		outcome := outcomeDeprecated
		if restore {
			outcome = outcomeAlreadyBinding
			if r.Changed {
				outcome = outcomeRestored
			}
		}
		report.Results = append(report.Results,
			registrySuccessResult(r.Project, outcome, r.Schema.Ref()))
	}
	return finishRegistryFanout(w, report)
}

func init() {
	schemaDeprecateCmd.Flags().Bool(
		"restore", false, "Return a retired version to service")
	addRegistryTargetFlags(schemaDeprecateCmd, "Retire the version")
	schemaCmd.AddCommand(schemaDeprecateCmd)
}
