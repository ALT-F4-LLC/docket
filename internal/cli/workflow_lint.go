package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

// `docket workflow lint` — DKT-38. Before this verb the ONLY way to run the
// validator was to REGISTER, a persistent write: checking a draft inserted a
// row and minted a frozen name@version. An author iterating on a definition
// either accumulated registered versions they did not want or did not check
// at all.

var workflowLintCmd = &cobra.Command{
	Use:   "lint <file.toml>",
	Short: "Validate a workflow definition without registering it",
	Long: `Run the exact validation ` + "`docket workflow register`" + ` runs — grammar, step
rules, vote rules, and schema cross-checks — and WRITE NOTHING.

Pass - as the file to read from stdin.

The registry is consulted, never written: schema and vote-rule references
resolve against what is registered, and the verdict reports whether a real
register would be NEW, UNCHANGED (identical bytes already registered), or a
CONFLICT (different bytes at a frozen name@version — bump [pipeline].version
to adopt the edit). The conflict case fails the lint: a definition that can
never register as-is has not passed.

A registry is PER PROJECT, and so is the verdict. By default the references
resolve and the registration probe runs against the project the working
directory resolves to; --project runs them against one other project's
registry instead, so the verdict is the one 'docket workflow register
--project <ref>' would reach there. It takes the same refs the writing verbs
take (PREFIX, NAME, IDENTITY, or row id), so a project whose checkout is
missing from this machine can still be linted against from any other.

--all-projects lints against every project in the store and writes the
per-project report 'workflow register --all-projects' writes, with each
project's verdict as its outcome: new, unchanged, conflict, or invalid (a
vote_rule or payload reference that does not resolve there). The grammar is
checked once, before any project, since a definition that does not parse is
wrong everywhere. The command exits non-zero when any project would refuse.`,
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkflowLint(cmd, args, getWriter(cmd))
	},
}

// lintResult is the verb's wire shape.
type lintResult struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
	// Registration is what a real register would do: `new`, `unchanged`.
	// (The conflict case refuses rather than reporting, so it never appears.)
	Registration string `json:"registration"`
}

func runWorkflowLint(cmd *cobra.Command, args []string, w *output.Writer) error {
	path := args[0]
	src, err := readWorkflowSource(cmd, path)
	if err != nil {
		return err
	}
	project, _ := cmd.Flags().GetString("project")
	all, _ := cmd.Flags().GetBool("all-projects")

	linted, err := verb.WorkflowLint(cmd.Context(), verbEnv(cmd), verb.WorkflowLintInput{
		Body:        src,
		SourcePath:  path,
		Project:     project,
		AllProjects: all,
	})
	if err != nil {
		return verbErr(err)
	}

	if all {
		return renderLintAcrossProjects(w, linted)
	}
	result := lintResult{
		Name:         linted.Name,
		Version:      linted.Version,
		SHA256:       linted.SHA256,
		Registration: linted.Registration,
	}
	w.Success(result, fmt.Sprintf("%s@%d is valid (%s; nothing written)",
		result.Name, result.Version, result.Registration))
	return nil
}

// renderLintAcrossProjects writes --all-projects' verdicts in the per-project
// report `workflow register --all-projects` writes, so a store-wide sweep can
// be planned from the same shape it will produce. The process exits non-zero
// when any project would refuse.
func renderLintAcrossProjects(w *output.Writer, linted verb.WorkflowLintResult) error {
	report := &registryFanoutReport{
		Operation: "workflow lint",
		Subject:   fmt.Sprintf("%s@%d", linted.Name, linted.Version),
		Scope:     scopeAllProjects,
	}
	for _, v := range linted.Projects {
		if v.Err != nil {
			report.Results = append(report.Results,
				registryFailureResult(v.Project, v.Err, verbErr))
			continue
		}
		report.Results = append(report.Results,
			registrySuccessResult(v.Project, v.Registration, report.Subject))
	}
	return finishRegistryFanout(w, report)
}

func init() {
	addProjectReadFlag(workflowLintCmd, "Lint against the registry")
	workflowLintCmd.Flags().Bool("all-projects", false,
		"Lint against EVERY project's registry, reporting each project's own verdict")
	workflowLintCmd.MarkFlagsMutuallyExclusive("project", "all-projects")
	workflowCmd.AddCommand(workflowLintCmd)
}
