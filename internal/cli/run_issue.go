package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `docket run issue add|remove` — DKT-53. Before these verbs the issue set was
// fixed at `run start` while `run --help` said the run "gathers" its issues:
// an operator who approved one more issue during planning had to abandon the
// run and retype the start, leaving an audit-noise run row behind.

var runIssueCmd = &cobra.Command{
	Use:   "issue",
	Short: "Add or remove a run's issues before activation binds them",
}

var runIssueAddCmd = &cobra.Command{
	Use:   "add RUN-N DKT-N...",
	Short: "Attach issues to a run",
	Long: `Attach one or more issues to a run.

Legal while the run is in ` + "`planning`" + ` — the set is still being gathered — and
while it is ` + "`active`" + `, where the new issues are bound and snapshotted by the
NEXT ` + "`docket run activate`" + ` (they join as their dependencies allow, exactly as
a later phase does). A parked or terminal run refuses.`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunIssueAdd(cmd, args, getWriter(cmd))
	},
}

var runIssueRemoveCmd = &cobra.Command{
	Use:   "remove RUN-N DKT-N...",
	Short: "Detach issues from a planning run",
	Long: `Detach one or more issues from a run still in ` + "`planning`" + `.

After activation an issue is bound, snapshotted, and possibly scheduled, so
the set is immutable from there: removing would strand steps that already
exist. Abandon the run instead if its shape is wrong.`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunIssueRemove(cmd, args, getWriter(cmd))
	},
}

// resolveRunIssueArgs parses `RUN-N DKT-N...` and loads the run.
func resolveRunIssueArgs(cmd *cobra.Command, args []string) (*model.Run, []int, error) {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(args[0])
	if err != nil {
		return nil, nil, cmdErr(err, output.ErrValidation)
	}
	run, err := db.GetRun(conn, runID)
	if errors.Is(err, db.ErrRunNotFound) {
		return nil, nil, cmdErr(
			fmt.Errorf("run %s not found", model.FormatRunID(runID)), output.ErrNotFound)
	}
	if err != nil {
		return nil, nil, cmdErr(err, output.ErrGeneral)
	}

	issueIDs := make([]int, 0, len(args)-1)
	for _, ref := range args[1:] {
		id, err := model.ParseID(ref)
		if err != nil {
			return nil, nil, cmdErr(
				fmt.Errorf("invalid issue ID %q: %w", ref, err), output.ErrValidation)
		}
		issueIDs = append(issueIDs, id)
	}
	return run, issueIDs, nil
}

// runIssueSet is the wire shape both verbs answer with: the run and its issue
// list AFTER the change, so a caller sees the set it now holds rather than
// re-deriving it from what it asked for.
type runIssueSet struct {
	Run    string   `json:"run"`
	Status string   `json:"status"`
	Issues []string `json:"issues"`
}

func runIssueAnswer(conn *sql.DB, run *model.Run) (runIssueSet, error) {
	attached, err := db.ListRunIssues(conn, run.ID)
	if err != nil {
		return runIssueSet{}, err
	}
	refs := make([]string, 0, len(attached))
	for _, ri := range attached {
		refs = append(refs, model.FormatID(ri.IssueID))
	}
	return runIssueSet{Run: run.Ref(), Status: string(run.Status), Issues: refs}, nil
}

func runRunIssueAdd(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)
	run, issueIDs, err := resolveRunIssueArgs(cmd, args)
	if err != nil {
		return err
	}

	// `planning` gathers; `active` feeds RA3 (issues added since activation are
	// bound and snapshotted by the next activation). A parked run is a person's
	// decision in progress, and a terminal run's issue set is history.
	if run.Status != model.RunPlanning && run.Status != model.RunActive {
		return cmdErr(
			fmt.Errorf("run %s is %s; issues can be added while a run is "+
				"planning or active", run.Ref(), run.Status),
			output.ErrConflict)
	}

	// Existence AND homing are checked for the WHOLE set before anything is
	// written, so a typo'd second id cannot leave a half-applied add behind.
	//
	// The homing half is DKT-21: issue ids are store-wide, so another
	// project's issue parses from any cwd — but activation binds issues
	// against the RUN's project workflow registry and books their snapshots,
	// steps, and gaps there, so a cross-project attach mis-scopes everything
	// downstream. `issue move --project` already refuses while a run holds an
	// issue for exactly this reason; the invariant is enforced at both edges.
	for _, id := range issueIDs {
		issueProject, err := db.IssueProjectID(conn, id)
		if errors.Is(err, db.ErrNotFound) {
			return cmdErr(
				fmt.Errorf("issue %s not found", model.FormatID(id)), output.ErrNotFound)
		}
		if err != nil {
			return cmdErr(fmt.Errorf("checking issue %s: %w", model.FormatID(id), err),
				output.ErrGeneral)
		}
		if issueProject != run.ProjectID {
			return cmdErr(fmt.Errorf(
				"issue %s belongs to %s, run %s to %s — a run's issues live in the "+
					"run's own project; migrate the issue first with `docket issue "+
					"move %s --project <target>`, or start the run from the issue's "+
					"own repository",
				model.FormatID(id), projectLabel(conn, issueProject), run.Ref(),
				projectLabel(conn, run.ProjectID), model.FormatID(id)),
				output.ErrValidation)
		}
	}

	for _, id := range issueIDs {
		if err := db.AddRunIssue(conn, run.ID, id); err != nil {
			return runErr(err)
		}
	}

	answer, err := runIssueAnswer(conn, run)
	if err != nil {
		return cmdErr(err, output.ErrGeneral)
	}

	message := fmt.Sprintf("Added %s to %s", issueRefList(issueIDs), run.Ref())
	if run.Status == model.RunActive {
		message += " (bound and snapshotted at the next `docket run activate`)"
	}
	w.Success(answer, message)
	return nil
}

func runRunIssueRemove(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)
	run, issueIDs, err := resolveRunIssueArgs(cmd, args)
	if err != nil {
		return err
	}

	if run.Status != model.RunPlanning {
		return cmdErr(
			fmt.Errorf("run %s is %s; the issue set is immutable once activation "+
				"has bound it — issues can be removed only while a run is planning",
				run.Ref(), run.Status),
			output.ErrConflict)
	}

	// Membership is checked for the WHOLE set first, so `remove A TYPO` cannot
	// detach A and then refuse.
	attached, err := db.ListRunIssues(conn, run.ID)
	if err != nil {
		return cmdErr(err, output.ErrGeneral)
	}
	member := make(map[int]bool, len(attached))
	for _, ri := range attached {
		member[ri.IssueID] = true
	}
	for _, id := range issueIDs {
		if !member[id] {
			return cmdErr(
				fmt.Errorf("issue %s is not attached to %s", model.FormatID(id), run.Ref()),
				output.ErrNotFound)
		}
	}

	for _, id := range issueIDs {
		if err := db.RemoveRunIssue(conn, run.ID, id); err != nil {
			return runErr(err)
		}
	}

	answer, err := runIssueAnswer(conn, run)
	if err != nil {
		return cmdErr(err, output.ErrGeneral)
	}
	w.Success(answer, fmt.Sprintf("Removed %s from %s",
		issueRefList(issueIDs), run.Ref()))
	return nil
}

func issueRefList(ids []int) string {
	refs := make([]string, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, model.FormatID(id))
	}
	return strings.Join(refs, ", ")
}

func init() {
	runIssueCmd.AddCommand(runIssueAddCmd, runIssueRemoveCmd)
	runCmd.AddCommand(runIssueCmd)
}
