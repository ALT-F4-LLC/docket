package cli

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
)

type deleteResult struct {
	ID string `json:"id"`
}

var deleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete an issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		force, _ := cmd.Flags().GetBool("force")
		orphan, _ := cmd.Flags().GetBool("orphan")

		if force && orphan {
			return cmdErr(fmt.Errorf("--force and --orphan are mutually exclusive"), output.ErrValidation)
		}

		id, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(err, output.ErrValidation)
		}

		issue, err := db.GetIssue(conn, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("issue %s not found", model.FormatID(id)), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
		}

		subIssues, err := db.GetSubIssues(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("checking sub-issues: %w", err), output.ErrGeneral)
		}

		// No sub-issues: simple delete.
		if len(subIssues) == 0 {
			if err := db.DeleteIssue(conn, id); err != nil {
				return cmdErr(fmt.Errorf("deleting issue: %w", err), output.ErrGeneral)
			}
			w.Success(deleteResult{ID: model.FormatID(id)}, fmt.Sprintf("Deleted %s: %s", model.FormatID(id), issue.Title))
			return nil
		}

		// Sub-issues exist: handle based on flags.
		if force {
			return doCascadeDelete(w, conn, id, issue.Title, len(subIssues))
		}

		if orphan {
			return doOrphanDelete(w, conn, id, issue.Title, len(subIssues))
		}

		// JSON mode requires explicit flag when sub-issues exist.
		if w.JSONMode {
			return cmdErr(fmt.Errorf("issue %s has %d sub-issue(s): use --force to cascade-delete or --orphan to make them root issues", model.FormatID(id), len(subIssues)), output.ErrValidation)
		}

		// Interactive prompt.
		var choice string
		form := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title(fmt.Sprintf("Issue %s has %d sub-issue(s). How do you want to proceed?", model.FormatID(id), len(subIssues))).
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
			return doCascadeDelete(w, conn, id, issue.Title, len(subIssues))
		case "orphan":
			return doOrphanDelete(w, conn, id, issue.Title, len(subIssues))
		case "cancel":
			w.Info("Cancelled.")
		}

		return nil
	},
}

func doCascadeDelete(w *output.Writer, conn *sql.DB, id int, title string, subCount int) error {
	if err := db.CascadeDeleteIssue(conn, id); err != nil {
		return cmdErr(fmt.Errorf("cascade deleting issue: %w", err), output.ErrGeneral)
	}
	w.Success(deleteResult{ID: model.FormatID(id)}, fmt.Sprintf("Deleted %s: %s (and %d sub-issue(s))", model.FormatID(id), title, subCount))
	return nil
}

func doOrphanDelete(w *output.Writer, conn *sql.DB, id int, title string, subCount int) error {
	if err := db.OrphanSubIssues(conn, id, config.DefaultAuthor()); err != nil {
		return cmdErr(fmt.Errorf("orphaning sub-issues: %w", err), output.ErrGeneral)
	}
	if err := db.DeleteIssue(conn, id); err != nil {
		return cmdErr(fmt.Errorf("deleting issue: %w", err), output.ErrGeneral)
	}
	w.Success(deleteResult{ID: model.FormatID(id)}, fmt.Sprintf("Deleted %s: %s (orphaned %d sub-issue(s))", model.FormatID(id), title, subCount))
	return nil
}

func init() {
	deleteCmd.Flags().BoolP("force", "f", false, "Skip confirmation and cascade-delete all sub-issues")
	deleteCmd.Flags().Bool("orphan", false, "Remove parent reference from sub-issues (make them root issues)")
	issueCmd.AddCommand(deleteCmd)
}
