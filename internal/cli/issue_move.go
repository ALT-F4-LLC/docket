package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var moveCmd = &cobra.Command{
	Use:   "move <id> <status>",
	Short: "Move an issue to a new status, or migrate it to another project",
	Long: `Move an issue to a new status:

  docket issue move DKT-1 review

Or migrate a root issue — and its whole sub-issue tree — to another project
in the shared store:

  docket issue move DKT-1 --project <prefix|name|identity|id>

Migration re-homes work that landed in the wrong project (a gap recorded by
a run whose repository does not own the fix, most commonly). Labels re-map
by name into the target project; comments, relations, and activity ride
along — ids are store-wide, so nothing referencing the issue goes stale.
An issue a run holds cannot migrate, and a sub-issue migrates with its
root, not alone.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := issueArg(args[0])
		if err != nil {
			return err
		}

		if project, _ := cmd.Flags().GetString("project"); project != "" {
			if len(args) != 1 {
				return cmdErr(fmt.Errorf(
					"a status move and a project migration are separate operations; "+
						"pass a status or --project, not both"), output.ErrValidation)
			}
			if cmd.Flags().Changed("if-version") {
				return cmdErr(fmt.Errorf(
					"--if-version applies to status moves only"), output.ErrValidation)
			}
			return moveIssueToProject(cmd, id, project)
		}
		if len(args) != 2 {
			return cmdErr(fmt.Errorf(
				"usage: docket issue move <id> <status>, or docket issue move <id> --project <target>"),
				output.ErrValidation)
		}

		newStatus := model.Status(args[1])
		if err := model.ValidateStatus(newStatus); err != nil {
			return cmdErr(err, output.ErrValidation)
		}

		issue, err := getIssueOrErr(conn, id, fmt.Sprintf("issue %s", args[0]))
		if err != nil {
			return err
		}

		ifVersion, err := ifVersionOf(cmd)
		if err != nil {
			return err
		}

		oldStatus := issue.Status

		if oldStatus == newStatus {
			// A no-op move must still honor --if-version, so a caller cannot
			// read a stale version and conclude its precondition held.
			if ifVersion != nil {
				if err := db.UpdateIssueCAS(conn, id, nil, config.DefaultAuthor(), ifVersion); err != nil {
					if e := casError(err, fmt.Sprintf("issue %s", model.FormatID(id))); e != nil {
						return e
					}
					return cmdErr(fmt.Errorf("updating issue: %w", err), output.ErrGeneral)
				}
			}
			if w.JSONMode {
				w.Success(withIssueVersion(issue), "")
			} else {
				w.Info("Issue %s is already %s", model.FormatID(id), newStatus)
			}
			return nil
		}

		if err := db.UpdateIssueCAS(conn, id, map[string]interface{}{"status": string(newStatus)}, config.DefaultAuthor(), ifVersion); err != nil {
			if e := casError(err, fmt.Sprintf("issue %s", model.FormatID(id))); e != nil {
				return e
			}
			return cmdErr(fmt.Errorf("updating issue: %w", err), output.ErrGeneral)
		}

		issue, err = db.GetIssue(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching updated issue: %w", err), output.ErrGeneral)
		}

		w.Success(withIssueVersion(issue), fmt.Sprintf("Moved %s: %s \u2192 %s", model.FormatID(id), oldStatus, newStatus))

		return nil
	},
}

// moveIssueToProject is the migration half of `issue move` (DKT-27): the
// issue and its sub-issue tree re-home to the named project.
func moveIssueToProject(cmd *cobra.Command, id int, target string) error {
	w := getWriter(cmd)
	conn := getDB(cmd)

	project, err := resolveProjectRef(conn, target)
	if err != nil {
		return err
	}

	ids, err := db.MoveIssueProject(conn, id, project.ID, config.DefaultAuthor(), time.Now().UnixMilli())
	switch {
	case errors.Is(err, db.ErrNotFound):
		return cmdErr(fmt.Errorf("issue %s not found", model.FormatID(id)), output.ErrNotFound)
	case errors.Is(err, db.ErrIssueHasParent):
		return cmdErr(fmt.Errorf(
			"issue %s has a parent; a sub-issue migrates with its root — migrate "+
				"the root, or reparent with `issue edit --parent none` first",
			model.FormatID(id)), output.ErrValidation)
	case errors.Is(err, db.ErrIssueInRun):
		return cmdErr(fmt.Errorf(
			"issue %s (or one of its sub-issues) belongs to a run; a run's records "+
				"are project-scoped and would be stranded", model.FormatID(id)),
			output.ErrConflict)
	case err != nil:
		return cmdErr(fmt.Errorf("migrating issue: %w", err), output.ErrGeneral)
	}

	migrated := make([]string, 0, len(ids))
	for _, m := range ids {
		migrated = append(migrated, model.FormatID(m))
	}
	w.Success(map[string]any{
		"id": model.FormatID(id),
		"project": map[string]any{
			"id": project.ID, "identity": project.Identity,
			"name": project.Name, "prefix": project.Prefix,
		},
		"migrated": migrated,
	}, fmt.Sprintf("Moved %s to project %s (%d issue(s))",
		model.FormatID(id), projectDisplayName(project), len(ids)))
	return nil
}

func init() {
	addIfVersionFlag(moveCmd)
	moveCmd.Flags().String("project", "",
		"Migrate the issue (and its sub-issue tree) to this project instead of moving status; prefix, name, identity path, or row id")
	issueCmd.AddCommand(moveCmd)
}
