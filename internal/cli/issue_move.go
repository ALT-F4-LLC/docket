package cli

import (
	"fmt"
	"strconv"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
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

--note records the reason for the move as an issue comment, in the SAME
transaction as the move itself:

  docket issue move DKT-1 done --note "closed on RUN-40's outcome"

It is the two-verb ` + "`issue comment add` + `issue move`" + ` form in one verb,
and atomic: a refused move records no comment, and a recorded comment never
narrates a move that did not happen. Applies to status moves only.

Migration re-homes work that landed in the wrong project (a gap recorded by
a run whose repository does not own the fix, most commonly). Labels re-map
by name into the target project; comments, relations, and activity ride
along — ids are store-wide, so nothing referencing the issue goes stale.
An issue a run holds cannot migrate, and a sub-issue migrates with its
root, not alone.`,
	Args:        cobra.RangeArgs(1, 2),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		in := verb.IssueMoveInput{Issue: args[0], IfVersion: ifVersionFlagValue(cmd)}
		if len(args) == 2 {
			in.Status = args[1]
		}
		in.Project, _ = cmd.Flags().GetString("project")
		in.Note, _ = cmd.Flags().GetString("note")
		return runIssueMove(cmd, in)
	},
}

// runIssueMove runs the move in and renders its outcome.
func runIssueMove(cmd *cobra.Command, in verb.IssueMoveInput) error {
	w := getWriter(cmd)

	res, err := verb.IssueMove(cmd.Context(), verbEnv(cmd), in)
	recordProjectRegistration(cmd, res.Registered)
	if err != nil {
		return verbErr(err)
	}

	if res.Migration != nil {
		renderIssueMigration(w, res.Migration)
		return nil
	}

	issue := res.Issue
	if !res.Changed {
		if w.JSONMode {
			w.Success(withIssueVersion(issue), "")
		} else {
			w.Info("Issue %s is already %s", model.FormatID(issue.ID), issue.Status)
		}
		return nil
	}

	w.Success(withIssueVersion(issue), fmt.Sprintf("Moved %s: %s → %s", model.FormatID(issue.ID), res.From, issue.Status))

	return nil
}

// moveIssueToProject is the migration half of `issue move` (DKT-27): the
// issue and its sub-issue tree re-home to the named project.
func moveIssueToProject(cmd *cobra.Command, id int, target string) error {
	return runIssueMove(cmd, verb.IssueMoveInput{Issue: strconv.Itoa(id), Project: target})
}

// renderIssueMigration reports a migrated subtree and the project it now
// lives in.
func renderIssueMigration(w *output.Writer, m *verb.IssueMigration) {
	migrated := make([]string, 0, len(m.Migrated))
	for _, id := range m.Migrated {
		migrated = append(migrated, model.FormatID(id))
	}
	project := m.Project
	w.Success(map[string]any{
		"id": model.FormatID(m.Root),
		"project": map[string]any{
			"id": project.ID, "identity": project.Identity,
			"name": project.Name, "prefix": project.Prefix,
		},
		"migrated": migrated,
	}, fmt.Sprintf("Moved %s to project %s (%d issue(s))",
		model.FormatID(m.Root), projectDisplayName(project), len(m.Migrated)))
}

func init() {
	addIfVersionFlag(moveCmd)
	moveCmd.Flags().String("project", "",
		"Migrate the issue (and its sub-issue tree) to this project instead of moving status; prefix, name, identity path, or row id")
	moveCmd.Flags().String("note", "",
		"Why the issue moved; recorded as an issue comment in the same transaction as the move (status moves only)")
	issueCmd.AddCommand(moveCmd)
}
