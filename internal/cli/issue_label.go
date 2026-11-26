package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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
		color, _ := cmd.Flags().GetString("color")

		res, err := verb.IssueLabelAdd(cmd.Context(), verbEnv(cmd), verb.IssueLabelAddInput{
			Issue:  args[0],
			Labels: args[1:],
			Color:  color,
		})
		recordProjectRegistration(cmd, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		w.Success(res.Labels, fmt.Sprintf("Added label(s) to %s: %s", model.FormatID(res.Issue.ID), res.Issue.Title))
		return nil
	},
}

var labelRmCmd = &cobra.Command{
	Use:   "rm <id> <label>...",
	Short: "Remove labels from an issue",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		res, err := verb.IssueLabelRm(cmd.Context(), verbEnv(cmd), verb.IssueLabelRmInput{
			Issue:  args[0],
			Labels: args[1:],
		})
		recordProjectRegistration(cmd, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		w.Success(res.Labels, fmt.Sprintf("Removed label(s) from %s: %s", model.FormatID(res.Issue.ID), res.Issue.Title))
		return nil
	},
}

var labelListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all labels",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		listed, err := verb.IssueLabelList(cmd.Context(), verbEnv(cmd), verb.IssueLabelListInput{})
		if err != nil {
			return verbErr(err)
		}
		labels := listed.Labels

		if len(labels) == 0 {
			quiet, _ := cmd.Flags().GetBool("quiet")
			msg := render.EmptyState(
				"No labels found.",
				"Add one with: docket issue label add <id> <label>",
				quiet,
			)
			w.Success([]string{}, msg)
			return nil
		}

		if w.JSONMode {
			w.Success(labels, "")
			return nil
		}

		if render.ColorsEnabled() {
			rows := make([][]string, 0, len(labels))
			for _, l := range labels {
				color := l.Color
				if color == "" {
					color = "-"
				}
				var swatch string
				if l.Color != "" {
					swatch = lipgloss.NewStyle().Foreground(lipgloss.Color(l.Color)).Render("\u25a0")
				} else {
					swatch = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render("\u25a0")
				}
				rows = append(rows, []string{swatch + " " + l.Name, color, fmt.Sprintf("%d", l.IssueCount)})
			}

			t := table.New().
				Border(lipgloss.NormalBorder()).
				BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("8"))).
				Headers("NAME", "COLOR", "ISSUES").
				Rows(rows...).
				StyleFunc(func(row, col int) lipgloss.Style {
					s := lipgloss.NewStyle().PaddingLeft(1).PaddingRight(1)
					if row == table.HeaderRow {
						return s.Bold(true).Foreground(lipgloss.Color("15"))
					}
					return s
				})

			w.Success(labels, t.Render())
		} else {
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
		}
		return nil
	},
}

var labelDeleteCmd = &cobra.Command{
	Use:   "delete <label>",
	Short: "Delete a label",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		name := args[0]
		force, _ := cmd.Flags().GetBool("force")

		in := verb.IssueLabelDeleteInput{Label: name, Force: force}
		env := verbEnv(cmd)
		res, err := verb.IssueLabelDelete(cmd.Context(), env, in)
		recordProjectRegistration(cmd, res.Registered)

		// Without --force the verb deletes nothing from a label still in use
		// and returns the label with its refusal; only a real terminal may
		// then confirm.
		if errors.Is(err, verb.ErrLabelInUse) {
			label := res.Label
			if w.JSONMode {
				return cmdErr(fmt.Errorf("cannot delete label %q without --force in JSON mode (attached to %d issue(s))", name, label.IssueCount), output.ErrValidation)
			}

			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return cmdErr(fmt.Errorf("non-interactive environment detected; label %q is attached to %d issue(s): use --force to skip confirmation", name, label.IssueCount), output.ErrValidation)
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

			in.Force = true
			res, err = verb.IssueLabelDelete(cmd.Context(), env, in)
			recordProjectRegistration(cmd, res.Registered)
		}
		if err != nil {
			return verbErr(err)
		}

		msg := fmt.Sprintf("Deleted label %q", name)
		if res.Detached > 0 {
			msg = fmt.Sprintf("Deleted label %q (removed from %d issue(s))", name, res.Detached)
		}

		w.Success(labelDeleteResult{Name: name}, msg)
		return nil
	},
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
