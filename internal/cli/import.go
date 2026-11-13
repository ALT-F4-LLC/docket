package cli

import (
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

type importResult struct {
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
	// Remapped counts rows whose source id was already taken in this store
	// and which were assigned a fresh id, with every reference rewritten.
	// Before v12 those rows were silently skipped — the consolidation trap.
	Remapped int `json:"remapped"`
}

// newImportCmd builds a fresh import command with its own flag set, real
// registration included. Production wires exactly one instance (importCmd,
// below); tests use this constructor directly so the command under test
// carries the same --merge/--replace/--yes flags a real invocation parses,
// rather than a hand-rolled stand-in that can drift from them.
func newImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "import <file>",
		Short:       "Import issues from a JSON export file",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"verbOwnsProject": ""},
		RunE:        runImport,
	}
	cmd.Flags().Bool("merge", false,
		"Import into a non-empty project; this project's own rows are skipped, ids colliding with another project's rows are remapped, nothing is dropped")
	cmd.Flags().Bool("replace", false,
		"Replace THIS PROJECT's data with the import file (destructive)")
	cmd.Flags().Bool("yes", false,
		"Confirm --replace's destructive data replacement (required in every output mode)")
	return cmd
}

var importCmd = newImportCmd()

func runImport(cmd *cobra.Command, args []string) error {
	w := getWriter(cmd)

	merge, _ := cmd.Flags().GetBool("merge")
	replace, _ := cmd.Flags().GetBool("replace")
	yes, _ := cmd.Flags().GetBool("yes")

	data, err := os.ReadFile(args[0])
	if err != nil {
		return cmdErr(fmt.Errorf("reading file: %w", err), output.ErrGeneral)
	}

	res, err := verb.Import(cmd.Context(), verbEnv(cmd), verb.ImportInput{
		Document: string(data), Merge: merge, Replace: replace, Yes: yes,
	})
	recordProjectRegistration(cmd, res.Registered)
	if err != nil {
		return verbErr(err)
	}

	result := importResult{Imported: res.Imported, Skipped: res.Skipped, Remapped: res.Remapped}
	var message string
	if !w.JSONMode {
		message = fmt.Sprintf("Imported %d entities", result.Imported)
		if result.Remapped > 0 {
			message += fmt.Sprintf(", %d assigned fresh ids (source ids were taken in this store)", result.Remapped)
		}
		if result.Skipped > 0 {
			message += fmt.Sprintf(", skipped %d duplicates", result.Skipped)
		}
	}
	w.Success(&result, message)
	return nil
}

func init() {
	rootCmd.AddCommand(importCmd)
}
