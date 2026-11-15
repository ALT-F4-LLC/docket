package cli

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/verb"
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

		res, err := verb.IssueFileAdd(cmd.Context(), verbEnv(cmd), verb.IssueFileAddInput{
			Issue: args[0],
			Paths: args[1:],
		})
		recordProjectRegistration(cmd, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		w.Success(res.Files, fmt.Sprintf("Added file(s) to %s: %s", model.FormatID(res.Issue.ID), res.Issue.Title))
		return nil
	},
}

var fileRemoveCmd = &cobra.Command{
	Use:   "remove <id> <file-path>...",
	Short: "Remove files from an issue",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		res, err := verb.IssueFileRemove(cmd.Context(), verbEnv(cmd), verb.IssueFileRemoveInput{
			Issue: args[0],
			Paths: args[1:],
		})
		recordProjectRegistration(cmd, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		w.Success(res.Files, fmt.Sprintf("Removed file(s) from %s: %s", model.FormatID(res.Issue.ID), res.Issue.Title))
		return nil
	},
}

var fileListCmd = &cobra.Command{
	Use:   "list <id>",
	Short: "List files attached to an issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		listed, err := verb.IssueFileList(cmd.Context(), verbEnv(cmd), verb.IssueFileListInput{Issue: args[0]})
		if err != nil {
			return verbErr(err)
		}
		id, files := listed.IssueID, listed.Files

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
