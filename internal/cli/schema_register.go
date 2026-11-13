package cli

import (
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

var schemaRegisterCmd = &cobra.Command{
	Use:   "register <name@version> <file.json>",
	Short: "Register a payload schema",
	Long: `Read, validate, and register a payload schema document.

The document is compiled as JSON Schema here, at registration, rather than at
first use: a schema that does not compile should be refused while an author is
looking at it, not hours into a run on a step whose work is already done.

` + "`ordered_enum: true`" + ` on a property that also declares ` + "`enum`" + `
declares that the enum array's order, as written, runs from first to last. There
is no second list to disagree with it.

Registering the same bytes at an existing name@version again is a success and
changes nothing. Registering DIFFERENT bytes is a CONFLICT: a schema decides
whether a worker's payload is accepted, so a mutable one would change a run's
acceptance criteria mid-flight. Bump the version.

A registry is PER PROJECT. By default the schema lands in the project the
working directory resolves to; --project registers it into one other project,
and --all-projects registers it into every project in the store. Both report
each project's own outcome, and the frozen-bytes rule is decided per project —
a conflict in one project neither cancels nor hides another project's
registration. Registering schemas store-wide is the step that comes BEFORE
sweeping a workflow corpus, since a workflow's 'payload' references resolve
against the registry of the project it is being registered into.`,
	Args:        cobra.ExactArgs(2),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSchemaRegister(cmd, args, getWriter(cmd))
	},
}

func runSchemaRegister(cmd *cobra.Command, args []string, w *output.Writer) error {
	ref, path := args[0], args[1]
	project, _ := cmd.Flags().GetString("project")
	all, _ := cmd.Flags().GetBool("all-projects")

	res, err := verb.SchemaRegister(cmd.Context(), schemaVerbEnv(cmd), verb.SchemaRegisterInput{
		Ref:         ref,
		Document:    func() ([]byte, error) { return readSchemaFile(path) },
		SourcePath:  path,
		Project:     project,
		AllProjects: all,
	})
	recordProjectRegistration(cmd, res.Registered)
	if err != nil {
		return verbErr(err)
	}

	if res.Projects != nil {
		return renderSchemaRegisterAcrossProjects(cmd, w, res)
	}

	stored := res.Schema
	message := fmt.Sprintf("Registered %s", stored.Ref())
	if fields := stored.OrderedFields(); len(fields) > 0 {
		message += fmt.Sprintf(" (ordered: %s)", joinFields(fields))
	}
	if !res.Created {
		message = fmt.Sprintf("%s is already registered with these bytes", stored.Ref())
	}
	w.Success(stored, message)
	return nil
}

// readSchemaFile reads the schema document at path, refusing a missing or
// unreadable file as NOT_FOUND.
func readSchemaFile(path string) ([]byte, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, cmdErr(fmt.Errorf("schema file %s not found", path), output.ErrNotFound)
		}
		return nil, cmdErr(
			fmt.Errorf("reading schema file %s: %w", path, err), output.ErrNotFound)
	}
	return src, nil
}

// renderSchemaRegisterAcrossProjects writes the --project / --all-projects
// report: each project's own outcome, a conflict in one never hiding
// another's registration.
func renderSchemaRegisterAcrossProjects(
	cmd *cobra.Command, w *output.Writer, res verb.SchemaRegisterResult,
) error {
	report := &registryFanoutReport{
		Operation: "schema register",
		Subject:   fmt.Sprintf("%s@%d", res.Name, res.Version),
		Scope:     fanoutScope(cmd),
	}
	for _, r := range res.Projects {
		if r.Err != nil {
			report.Results = append(report.Results,
				registryFailureResult(r.Project, r.Err, verbErr))
			continue
		}
		outcome := outcomeUnchanged
		if r.Changed {
			outcome = outcomeRegistered
		}
		report.Results = append(report.Results,
			registrySuccessResult(r.Project, outcome, r.Schema.Ref()))
	}
	return finishRegistryFanout(w, report)
}

func init() {
	addRegistryTargetFlags(schemaRegisterCmd, "Register")
	schemaCmd.AddCommand(schemaRegisterCmd)
}
