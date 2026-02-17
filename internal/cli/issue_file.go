package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

var fileCmd = &cobra.Command{
	Use:   "file",
	Short: "Manage issue file attachments",
}

var fileAddCmd = &cobra.Command{
	Use:   "add <id> <file-path>...",
	Short: "Add files to an issue",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		issue, err := db.GetIssue(conn, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
		}

		filePaths := args[1:]
		if err := db.AttachFiles(conn, id, filePaths, config.DefaultAuthor()); err != nil {
			return cmdErr(fmt.Errorf("attaching files: %w", err), output.ErrGeneral)
		}

		files, err := db.GetIssueFiles(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching files: %w", err), output.ErrGeneral)
		}

		w.Success(files, fmt.Sprintf("Added file(s) to %s: %s", model.FormatID(id), issue.Title))
		return nil
	},
}

var fileRemoveCmd = &cobra.Command{
	Use:   "remove <id> <file-path>...",
	Short: "Remove files from an issue",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		issue, err := db.GetIssue(conn, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
		}

		filePaths := args[1:]
		if err := db.DetachFiles(conn, id, filePaths, config.DefaultAuthor()); err != nil {
			return cmdErr(fmt.Errorf("removing files: %w", err), output.ErrGeneral)
		}

		files, err := db.GetIssueFiles(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching files: %w", err), output.ErrGeneral)
		}

		w.Success(files, fmt.Sprintf("Removed file(s) from %s: %s", model.FormatID(id), issue.Title))
		return nil
	},
}

var fileListCmd = &cobra.Command{
	Use:   "list <id>",
	Short: "List files attached to an issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		if _, err := db.GetIssue(conn, id); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
		}

		files, err := db.GetIssueFiles(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching files: %w", err), output.ErrGeneral)
		}

		if len(files) == 0 {
			quiet, _ := cmd.Flags().GetBool("quiet")
			msg := render.EmptyState(
				fmt.Sprintf("No files attached to %s", model.FormatID(id)),
				fmt.Sprintf("Add one with: docket issue file add %s <path>", model.FormatID(id)),
				quiet,
			)
			w.Success([]string{}, msg)
			return nil
		}

		if w.JSONMode {
			w.Success(files, "")
			return nil
		}

		var sb strings.Builder
		if render.ColorsEnabled() {
			sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
			dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
			fmt.Fprintf(&sb, "%s\n", sectionStyle.Render(fmt.Sprintf("Files for %s", model.FormatID(id))))
			for _, f := range files {
				fmt.Fprintf(&sb, "  %s\n", dimStyle.Render("\u25b8 "+f))
			}
		} else {
			fmt.Fprintf(&sb, "Files for %s:\n", model.FormatID(id))
			for _, f := range files {
				fmt.Fprintf(&sb, "  %s\n", f)
			}
		}

		w.Success(files, sb.String())
		return nil
	},
}

func init() {
	fileCmd.AddCommand(fileAddCmd)
	fileCmd.AddCommand(fileRemoveCmd)
	fileCmd.AddCommand(fileListCmd)
	issueCmd.AddCommand(fileCmd)
}
