package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type deleteResult struct {
	ID string `json:"id"`
}

var deleteCmd = &cobra.Command{
	Use:         "delete <id>",
	Short:       "Delete an issue",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		force, _ := cmd.Flags().GetBool("force")
		orphan, _ := cmd.Flags().GetBool("orphan")

		// `--yes` is `--force` under the name a scripted caller reaches for
		// first (DKT-72). It is an ALIAS rather than a third behavior on
		// purpose: the prompt it answers is a three-way choice — cascade,
		// orphan, or cancel — and a flag that meant "yes" without saying to
		// WHAT would have to pick one of them silently. `--force` already
		// names the choice, so `--yes` simply spells it the way the muscle
		// memory does.
		if yes, _ := cmd.Flags().GetBool("yes"); yes {
			force = true
		}

		in := verb.IssueDeleteInput{Issue: args[0], Cascade: force, Orphan: orphan}
		res, err := deleteIssue(cmd, w, in)
		// JSON mode requires explicit flag when sub-issues exist.
		if !errors.Is(err, verb.ErrIssueHasSubIssues) || w.JSONMode {
			return verbErr(err)
		}

		// Interactive prompt.
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return cmdErr(fmt.Errorf("non-interactive environment detected; %w", err), output.ErrValidation)
		}
		var choice string
		form := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title(fmt.Sprintf("Issue %s has %d sub-issue(s). How do you want to proceed?", res.Issue, res.SubIssues)).
					Options(
						huh.NewOption("Delete issue and all sub-issues", "cascade"),
						huh.NewOption("Make sub-issues root issues", "orphan"),
						huh.NewOption("Cancel", "cancel"),
					).
					Value(&choice),
			),
		)

		if err := form.Run(); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				w.Info("Cancelled.")
				return nil
			}
			return cmdErr(fmt.Errorf("interactive form failed: %w", err), output.ErrGeneral)
		}

		switch choice {
		case "cascade":
			in.Cascade = true
		case "orphan":
			in.Orphan = true
		case "cancel":
			w.Info("Cancelled.")
			return nil
		default:
			return nil
		}
		_, err = deleteIssue(cmd, w, in)
		return verbErr(err)
	},
}

// deleteIssue runs verb.IssueDelete and reports a delete it made. Its error
// is the verb's own, so a caller can match verb.ErrIssueHasSubIssues.
func deleteIssue(cmd *cobra.Command, w *output.Writer, in verb.IssueDeleteInput) (verb.IssueDeleteResult, error) {
	res, err := verb.IssueDelete(cmd.Context(), verbEnv(cmd), in)
	recordProjectRegistration(cmd, res.Registered)
	if err != nil {
		return res, err
	}

	message := fmt.Sprintf("Deleted %s: %s", res.Issue, res.Title)
	switch {
	case res.SubIssues == 0:
	case in.Cascade:
		message += fmt.Sprintf(" (and %d sub-issue(s))", res.SubIssues)
	case in.Orphan:
		message += fmt.Sprintf(" (orphaned %d sub-issue(s))", res.SubIssues)
	}
	w.Success(deleteResult{ID: res.Issue}, message)
	return res, nil
}

func init() {
	deleteCmd.Flags().BoolP("force", "f", false, "Skip confirmation and cascade-delete all sub-issues")
	deleteCmd.Flags().BoolP("yes", "y", false,
		"Alias for --force: answer the sub-issue confirmation non-interactively")
	deleteCmd.Flags().Bool("orphan", false, "Remove parent reference from sub-issues (make them root issues)")
	issueCmd.AddCommand(deleteCmd)
}
