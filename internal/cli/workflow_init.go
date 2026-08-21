package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
	"github.com/spf13/cobra"
)

var workflowInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Write a starter workflow definition",
	Long: `Write a shipped workflow template into .docket/config/workflows/.

The written file is an ordinary workflow definition — byte-identical to one a
person could have typed — so read it and edit it if you like. You do NOT register
it: activation auto-registers everything under every instance-config root, so the
next step is simply to start a run.

The template is written into THIS CHECKOUT's .docket/config/workflows/, not the
shared ~/.docket/config/ the store may also carry — a definition typed here
belongs to this repository. Pass --dir to write it somewhere else.

    docket workflow init --template standard-dev
    docket issue create --title "..." --type task
    docket run start --issue DKT-1
    docket run activate RUN-1        # registers standard-dev@1, then binds

Editing a definition after it has been registered needs a version bump: a
registered name@version is frozen so a run that pinned it can reproduce, and
activation refuses changed bytes at an unchanged version, naming the edit to
make.

Existing files are never overwritten without --force.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkflowInit(cmd, args, getWriter(cmd))
	},
	// Writing a template needs no database, so `workflow init` works before
	// `docket init` does — a person can read a template before committing to
	// a tracker.
	Annotations: map[string]string{"skipDB": "true"},
}

func runWorkflowInit(cmd *cobra.Command, args []string, w *output.Writer) error {

	name, _ := cmd.Flags().GetString("template")
	dir, _ := cmd.Flags().GetString("dir")
	force, _ := cmd.Flags().GetBool("force")

	src, err := workflow.Template(name)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	targetDir := dir
	if targetDir == "" {
		cfg := getCfg(cmd)
		if cfg == nil {
			return cmdErr(fmt.Errorf("resolving the docket directory"), output.ErrGeneral)
		}
		// The instance-config tree, not the store: under the global store the
		// two differ, and a workflow definition is repo-shippable content that
		// belongs with the repository.
		configDir := cfg.InstanceConfigDir()
		if configDir == "" {
			return cmdErr(fmt.Errorf("resolving the instance config directory"), output.ErrGeneral)
		}
		targetDir = filepath.Join(configDir, "workflows")
	}

	path := filepath.Join(targetDir, workflow.TemplateFileName(name))

	if _, err := os.Stat(path); err == nil && !force {
		return cmdErr(
			fmt.Errorf("%s already exists; pass --force to overwrite it", path),
			output.ErrConflict,
		)
	} else if err != nil && !os.IsNotExist(err) {
		return cmdErr(fmt.Errorf("checking %s: %w", path, err), output.ErrGeneral)
	}

	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return cmdErr(fmt.Errorf("creating %s: %w", targetDir, err), output.ErrGeneral)
	}
	if err := os.WriteFile(path, src, 0o644); err != nil {
		return cmdErr(fmt.Errorf("writing %s: %w", path, err), output.ErrGeneral)
	}

	result := map[string]any{
		"template": name,
		"path":     path,
	}
	// The message names ACTIVATION, not a register verb. §9 item 11's zero-touch
	// criterion is that `workflow register` is never invoked on the path from a
	// template to a completed run, and a success message telling an operator to
	// run it would be the one place the product itself asked for the manual step
	// the acceptance criterion forbids. QA section ZK greps the whole rehearsal's
	// command trace for exactly that verb.
	w.Success(result, fmt.Sprintf(
		"Wrote %s\nIt registers itself when you activate a run — no register step.", path))
	return nil
}

func init() {
	workflowInitCmd.Flags().String("template", "standard-dev",
		fmt.Sprintf("Template to write (%s)", strings.Join(workflow.TemplateNames(), ", ")))
	workflowInitCmd.Flags().String("dir", "",
		"Directory to write into (default .docket/config/workflows)")
	workflowInitCmd.Flags().Bool("force", false, "Overwrite an existing file")
	workflowCmd.AddCommand(workflowInitCmd)
}
