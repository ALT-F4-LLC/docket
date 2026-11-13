package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

// `docket workflow deprecate` — retire a registered version from binding.
//
// THE DEFECT THIS EXISTS TO FIX. A registered workflow NAME binds forever at
// its highest version. Deleting its TOML does not unregister it — nothing in
// the tree deletes a `workflows` row — so a name registered once keeps
// competing for every issue it matches, and a second name covering the same
// work makes activation refuse with "matches 2 workflows" that no edit to
// either `[match]` can resolve.
//
// RETIREMENT IS NOT DELETION, and there is no delete verb here on purpose: the
// operator was offered one and rejected it. A retired version keeps its row,
// stays readable by explicit `@version`, and still resolves for runs that
// pinned it. Only its candidacy for NEW bindings stops.

var workflowDeprecateCmd = &cobra.Command{
	Use:   "deprecate <name>@<version>",
	Short: "Retire a registered workflow version from binding",
	Long: `Retire one registered workflow version from binding.

The version keeps its row and stays fully readable: ` + "`workflow show`" + ` still
renders it, ` + "`--source`" + ` still emits the exact bytes that were registered, and
a run that already pinned it still resolves it and still completes. Retirement
is a BINDING-TIME FILTER, not a retraction, and it never deletes.

Binding picks the highest version of each name that is NOT retired, so
retiring the top version falls back to the one beneath it. Retiring EVERY
version of a name removes that name from routing altogether — which is the
point when a name was registered by mistake, or when its TOML was deleted and
nothing else can stop it from binding.

Use --restore to put a retired version back into binding.

A registry is PER PROJECT, and so is retirement. By default this retires the
version in the project the working directory resolves to; --project retires it
in one other project, and --all-projects retires it in every project in the
store. Both report each project's own outcome: a project where the version was
never registered is reported as not-registered and a project where it was
already retired as already-deprecated, while every project that did retire it
still did. This is the answer to "this orphaned name has to go everywhere",
which previously meant one invocation per checkout.

THE OLDER ESCAPE HATCH, for context. Before this verb, the only way to stop a
name from binding was to re-register that SAME name at a HIGHER version whose
[match] admits nothing — for example labels_all and unless_labels naming the
same label, which is contradictory for every issue. That still works and is
still what an older repo will show. It is worth knowing because it explains
the mechanism: binding reduces to the highest version per name BEFORE [match]
runs, so a non-matching highest version takes the whole name out of contention.
This verb records the same intent as a fact rather than as a puzzle.`,
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkflowDeprecate(cmd, args, getWriter(cmd))
	},
}

func runWorkflowDeprecate(cmd *cobra.Command, args []string, w *output.Writer) error {
	restore, _ := cmd.Flags().GetBool("restore")
	project, _ := cmd.Flags().GetString("project")
	all, _ := cmd.Flags().GetBool("all-projects")

	res, err := verb.WorkflowDeprecate(cmd.Context(), verbEnv(cmd), verb.WorkflowDeprecateInput{
		Ref:         args[0],
		Restore:     restore,
		Project:     project,
		AllProjects: all,
	})
	recordProjectRegistration(cmd, res.Registered)
	if err != nil {
		return verbErr(err)
	}

	if res.Targets != nil {
		report := &registryFanoutReport{
			Operation: "workflow deprecate", Subject: args[0], Scope: fanoutScope(cmd),
		}
		if restore {
			report.Operation = "workflow deprecate --restore"
			return finishWorkflowFanout(w, report, res.Targets,
				outcomeRestored, outcomeAlreadyBinding)
		}
		return finishWorkflowFanout(w, report, res.Targets,
			outcomeDeprecated, outcomeDeprecated)
	}

	if restore {
		w.Success(res.Workflow, fmt.Sprintf("%s restored to binding.", res.Workflow.Ref()))
		return nil
	}
	w.Success(res.Workflow, fmt.Sprintf(
		"%s retired from binding. It stays registered and readable, and runs "+
			"that pinned it still resolve it.", res.Workflow.Ref()))
	return nil
}

func init() {
	workflowDeprecateCmd.Flags().Bool(
		"restore", false, "Return a retired version to binding")
	addRegistryTargetFlags(workflowDeprecateCmd, "Retire the version")
	workflowCmd.AddCommand(workflowDeprecateCmd)
}
