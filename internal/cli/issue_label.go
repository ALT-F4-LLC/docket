package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
)

type labelDeleteResult struct {
	Name string `json:"name"`
}

var labelCmd = &cobra.Command{
	Use:   "label",
	Short: "Manage labels",
}

var labelAddCmd = &cobra.Command{
	Use:   "add <id> <label>...",
	Short: "Add labels to an issue",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		color, _ := cmd.Flags().GetString("color")
		author := config.DefaultAuthor()

		labelNames := args[1:]
		for _, label := range labelNames {
			if err := validateLabelName(label); err != nil {
				return cmdErr(err, output.ErrValidation)
			}
		}

		if err := db.AddLabelsToIssue(conn, id, labelNames, color, author); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
			}
			if errors.Is(err, db.ErrLabelColorConflict) {
				return cmdErr(fmt.Errorf("label already exists with a different color"), output.ErrValidation)
			}
			return cmdErr(fmt.Errorf("adding labels: %w", err), output.ErrGeneral)
		}

		labels, err := db.GetIssueLabelObjects(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching labels: %w", err), output.ErrGeneral)
		}

		w.Success(labels, fmt.Sprintf("Added label(s) to %s", model.FormatID(id)))
		return nil
	},
}

var labelRmCmd = &cobra.Command{
	Use:   "rm <id> <label>...",
	Short: "Remove labels from an issue",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		author := config.DefaultAuthor()

		labelNames := args[1:]
		for _, label := range labelNames {
			if err := validateLabelName(label); err != nil {
				return cmdErr(err, output.ErrValidation)
			}
		}

		if err := db.RemoveLabelsFromIssue(conn, id, labelNames, author); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("issue or label not found"), output.ErrNotFound)
			}
			if errors.Is(err, db.ErrNotAttached) {
				return cmdErr(fmt.Errorf("label is not attached to %s", model.FormatID(id)), output.ErrValidation)
			}
			return cmdErr(fmt.Errorf("removing labels: %w", err), output.ErrGeneral)
		}

		labels, err := db.GetIssueLabelObjects(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching labels: %w", err), output.ErrGeneral)
		}

		w.Success(labels, fmt.Sprintf("Removed label(s) from %s", model.FormatID(id)))
		return nil
	},
}

var labelListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all labels",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		labels, err := db.ListAllLabels(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("listing labels: %w", err), output.ErrGeneral)
		}

		if len(labels) == 0 {
			w.Info("No labels found")
			return nil
		}

		if w.JSONMode {
			w.Success(labels, "")
			return nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%-20s %-12s %s\n", "NAME", "COLOR", "ISSUES")
		fmt.Fprintf(&sb, "%-20s %-12s %s\n", "----", "-----", "------")
		for _, l := range labels {
			color := l.Color
			if color == "" {
				color = "-"
			}
			fmt.Fprintf(&sb, "%-20s %-12s %d\n", l.Name, color, l.IssueCount)
		}

		w.Success(labels, sb.String())
		return nil
	},
}

var labelDeleteCmd = &cobra.Command{
	Use:   "delete <label>",
	Short: "Delete a label",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		name := args[0]
		force, _ := cmd.Flags().GetBool("force")

		label, err := db.GetLabelByName(conn, name)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("label %q not found", name), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("fetching label: %w", err), output.ErrGeneral)
		}

		if !force && label.IssueCount > 0 {
			if w.JSONMode {
				return cmdErr(fmt.Errorf("cannot delete label %q without --force in JSON mode (attached to %d issue(s))", name, label.IssueCount), output.ErrValidation)
			}

			var confirm bool
			prompt := fmt.Sprintf("Delete label %q? It is attached to %d issue(s)", name, label.IssueCount)

			form := huh.NewForm(
				huh.NewGroup(
					huh.NewConfirm().
						Title(prompt).
						Value(&confirm),
				),
			)

			if err := form.Run(); err != nil {
				if errors.Is(err, huh.ErrUserAborted) {
					w.Info("Cancelled.")
					return nil
				}
				return cmdErr(fmt.Errorf("interactive form failed: %w", err), output.ErrGeneral)
			}

			if !confirm {
				w.Info("Cancelled.")
				return nil
			}
		}

		author := config.DefaultAuthor()
		affectedIDs, err := db.DeleteLabel(conn, label.ID, name, author)
		if err != nil {
			return cmdErr(fmt.Errorf("deleting label: %w", err), output.ErrGeneral)
		}

		msg := fmt.Sprintf("Deleted label %q", name)
		if len(affectedIDs) > 0 {
			msg = fmt.Sprintf("Deleted label %q (removed from %d issue(s))", name, len(affectedIDs))
		}

		w.Success(labelDeleteResult{Name: name}, msg)
		return nil
	},
}

func validateLabelName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("label name cannot be empty")
	}
	return nil
}

func init() {
	labelAddCmd.Flags().String("color", "", "Label color (hex)")
	labelDeleteCmd.Flags().BoolP("force", "f", false, "Skip confirmation")

	labelCmd.AddCommand(labelAddCmd)
	labelCmd.AddCommand(labelRmCmd)
	labelCmd.AddCommand(labelListCmd)
	labelCmd.AddCommand(labelDeleteCmd)
	issueCmd.AddCommand(labelCmd)
}
