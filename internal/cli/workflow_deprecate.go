package cli

import (
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
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

THE OLDER ESCAPE HATCH, for context. Before this verb, the only way to stop a
name from binding was to re-register that SAME name at a HIGHER version whose
[match] admits nothing — for example labels_all and unless_labels naming the
same label, which is contradictory for every issue. That still works and is
still what an older repo will show. It is worth knowing because it explains
the mechanism: binding reduces to the highest version per name BEFORE [match]
runs, so a non-matching highest version takes the whole name out of contention.
This verb records the same intent as a fact rather than as a puzzle.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkflowDeprecate(cmd, args, getWriter(cmd))
	},
}

func runWorkflowDeprecate(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	name, version, err := model.ParseWorkflowRef(args[0])
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}
	// An explicit version is REQUIRED. Retirement is version-scoped, and
	// letting a bare name mean "the highest" would make the destructive-
	// sounding verb operate on whichever version happened to be on top —
	// a different row tomorrow than today.
	if version == 0 {
		return cmdErr(fmt.Errorf(
			"deprecate needs an explicit version, as %s@<version>: "+
				"retirement applies to one registered version, and a bare name "+
				"would silently mean whichever is highest today", name),
			output.ErrValidation)
	}

	restore, _ := cmd.Flags().GetBool("restore")
	if restore {
		wf, err := db.RestoreWorkflow(conn, getProjectID(cmd), name, version)
		if err != nil {
			return workflowErr(describeMissingWorkflow(err, args[0]))
		}
		w.Success(wf, fmt.Sprintf("%s restored to binding.", wf.Ref()))
		return nil
	}

	wf, err := db.DeprecateWorkflow(conn, getProjectID(cmd), name, version, model.NowMS())
	if err != nil {
		if errors.Is(err, db.ErrWorkflowAlreadyDeprecated) {
			return cmdErr(fmt.Errorf("%s is already deprecated", args[0]),
				output.ErrConflict)
		}
		return workflowErr(describeMissingWorkflow(err, args[0]))
	}

	w.Success(wf, fmt.Sprintf(
		"%s retired from binding. It stays registered and readable, and runs "+
			"that pinned it still resolve it.", wf.Ref()))
	return nil
}

func init() {
	workflowDeprecateCmd.Flags().Bool(
		"restore", false, "Return a retired version to binding")
	workflowCmd.AddCommand(workflowDeprecateCmd)
}
