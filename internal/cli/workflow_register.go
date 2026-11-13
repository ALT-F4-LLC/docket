package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

var workflowRegisterCmd = &cobra.Command{
	Use:   "register <file.toml>",
	Short: "Register a workflow definition",
	Long: `Parse, validate, and register a workflow definition.

Pass - as the file to read from stdin, so configuration generated in a pipeline
needs no temporary file.

Registering the same bytes at an existing name@version again is a success and
changes nothing. Registering DIFFERENT bytes at an existing name@version is a
CONFLICT: a registered name@version is frozen, so that a run which pinned it
cannot have the definition swapped underneath it.

A registry is PER PROJECT. By default the definition lands in the project the
working directory resolves to; --project registers it into one other project,
and --all-projects registers it into every project in the store. Both report
each project's own outcome, and each project's idempotency and conflict rules
are decided there — a conflict in one project neither cancels nor hides another
project's registration.

VALIDATION IS PER TARGET, not once for the invocation. A definition's
'vote_rule' and 'payload' references resolve against the registry of the project
being written to, so the SAME bytes can be valid in one project and reference a
schema that does not exist in the next. The project that cannot resolve them is
refused there and reported as invalid; the rest still register. Register the
schemas first — 'docket schema register <ref> <file> --all-projects' — when
sweeping a corpus into a store that has not seen it.`,
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkflowRegister(cmd, args, getWriter(cmd))
	},
}

func runWorkflowRegister(cmd *cobra.Command, args []string, w *output.Writer) error {
	path := args[0]
	project, _ := cmd.Flags().GetString("project")
	all, _ := cmd.Flags().GetBool("all-projects")

	res, err := verb.WorkflowRegister(cmd.Context(), verbEnv(cmd), verb.WorkflowRegisterInput{
		Source:      func() ([]byte, error) { return readWorkflowSource(cmd, path) },
		SourcePath:  path,
		Project:     project,
		AllProjects: all,
	})
	recordProjectRegistration(cmd, res.Registered)
	if err != nil {
		return verbErr(err)
	}

	if res.Targets != nil {
		return finishWorkflowFanout(w, &registryFanoutReport{
			Operation: "workflow register",
			Subject:   fmt.Sprintf("%s@%d", res.Name, res.Version),
			Scope:     fanoutScope(cmd),
		}, res.Targets, outcomeRegistered, outcomeUnchanged)
	}

	message := fmt.Sprintf("Registered %s", res.Workflow.Ref())
	if !res.Created {
		message = fmt.Sprintf("%s is already registered with these bytes", res.Workflow.Ref())
	}
	w.Success(res.Workflow, message)
	return nil
}

// finishWorkflowFanout writes a --project / --all-projects registry write's
// per-project report. Each project's refusal keeps the code its
// single-project write would exit with; a success reads changed when the
// write changed that project's row, else unchanged.
func finishWorkflowFanout(
	w *output.Writer, report *registryFanoutReport,
	targets []verb.WorkflowTargetOutcome, changed, unchanged string,
) error {
	for _, target := range targets {
		if target.Err != nil {
			report.Results = append(report.Results,
				registryFailureResult(target.Project, target.Err, verbErr))
			continue
		}
		outcome := unchanged
		if target.Changed {
			outcome = changed
		}
		report.Results = append(report.Results,
			registrySuccessResult(target.Project, outcome, target.Workflow.Ref()))
	}
	return finishRegistryFanout(w, report)
}

// readWorkflowSource reads the definition bytes from a file or, for "-", from
// stdin.
func readWorkflowSource(cmd *cobra.Command, path string) ([]byte, error) {
	if path == "-" {
		src, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, cmdErr(fmt.Errorf("reading workflow from stdin: %w", err), output.ErrGeneral)
		}
		if len(src) == 0 {
			return nil, cmdErr(
				fmt.Errorf("no workflow definition on stdin"), output.ErrValidation)
		}
		return src, nil
	}

	src, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, cmdErr(fmt.Errorf("workflow file %s not found", path), output.ErrNotFound)
		}
		return nil, cmdErr(
			fmt.Errorf("reading workflow file %s: %w", path, err), output.ErrNotFound)
	}
	return src, nil
}

func init() {
	addRegistryTargetFlags(workflowRegisterCmd, "Register")
	workflowCmd.AddCommand(workflowRegisterCmd)
}
