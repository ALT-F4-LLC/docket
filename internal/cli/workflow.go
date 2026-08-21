package cli

import (
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
	"github.com/spf13/cobra"
)

var workflowCmd = &cobra.Command{
	Use:     "workflow",
	Short:   "Register and inspect workflow definitions",
	Aliases: []string{"wf"},
}

func init() {
	rootCmd.AddCommand(workflowCmd)
}

// workflowErr maps a register-time failure to its error code, per the §4.5
// taxonomy. No new codes: every situation maps to one that already exists.
func workflowErr(err error) error {
	var we *workflow.Error
	if errors.As(err, &we) {
		return cmdErr(err, output.ErrValidation)
	}
	switch {
	case errors.Is(err, db.ErrWorkflowConflict):
		return cmdErr(err, output.ErrConflict)
	case errors.Is(err, db.ErrWorkflowNotFound):
		return cmdErr(err, output.ErrNotFound)
	default:
		return cmdErr(err, output.ErrGeneral)
	}
}

// loadWorkflowSource parses, validates, and lints source bytes, stamping the
// path into the error so an operator registering several files knows which one
// failed.
func loadWorkflowSource(src []byte, path string) (*workflow.Definition, error) {
	def, err := workflow.Load(src)
	if err != nil {
		if path != "" && path != "-" {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return nil, err
	}
	return def, nil
}
