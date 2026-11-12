package cli

import (
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

var exportCmd = &cobra.Command{
	Use:         "export",
	Short:       "Export issues to JSON, CSV, or Markdown",
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		format, _ := cmd.Flags().GetString("format")
		filePath, _ := cmd.Flags().GetString("file")
		statuses, _ := cmd.Flags().GetStringSlice("status")
		labels, _ := cmd.Flags().GetStringSlice("label")

		// A command built outside the root hook carries no config, and its
		// empty identity resolves to the default project.
		var project verb.ProjectInput
		if cfg := getCfg(cmd); cfg != nil {
			project = verb.ProjectInput{Identity: cfg.Identity, Anchored: cfg.Anchored}
		}
		cwd, _ := os.Getwd()
		env := verb.Env{
			Store:   store.StoreOn(getDB(cmd)),
			Author:  config.DefaultAuthor(),
			Cwd:     cwd,
			Project: project,
		}
		result, err := verb.Export(cmd.Context(), env, verb.ExportInput{
			Format:   format,
			Statuses: statuses,
			Labels:   labels,
		})
		if err != nil {
			return verbErr(err)
		}

		if filePath != "" {
			if err := os.WriteFile(filePath, []byte(result.Document), 0o644); err != nil {
				return cmdErr(fmt.Errorf("writing file: %w", err), output.ErrGeneral)
			}
			fmt.Fprintf(os.Stderr, "Exported to %s\n", filePath)
			return nil
		}

		fmt.Fprint(os.Stdout, result.Document)
		return nil
	},
}

func init() {
	exportCmd.Flags().StringP("format", "o", "json", "Export format: json, csv, markdown")
	exportCmd.Flags().StringP("file", "f", "", "Output file path (default: stdout)")
	exportCmd.Flags().StringSliceP("status", "s", nil, "Filter by status (repeatable)")
	exportCmd.Flags().StringSliceP("label", "l", nil, "Filter by label (OR, repeatable)")
	rootCmd.AddCommand(exportCmd)
}
