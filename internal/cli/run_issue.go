package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
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
	Args:        cobra.MinimumNArgs(2),
	Annotations: map[string]string{"verbOwnsProject": ""},
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
	Args:        cobra.MinimumNArgs(2),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunIssueRemove(cmd, args, getWriter(cmd))
	},
}

// runIssueSet is the wire shape both verbs answer with: the run and its issue
// list AFTER the change, so a caller sees the set it now holds rather than
// re-deriving it from what it asked for.
type runIssueSet struct {
	Run    string   `json:"run"`
	Status string   `json:"status"`
	Issues []string `json:"issues"`
}

func runIssueAnswer(run *model.Run, issueIDs []int) runIssueSet {
	refs := make([]string, 0, len(issueIDs))
	for _, id := range issueIDs {
		refs = append(refs, model.FormatID(id))
	}
	return runIssueSet{Run: run.Ref(), Status: string(run.Status), Issues: refs}
}

func runRunIssueAdd(cmd *cobra.Command, args []string, w *output.Writer) error {
	res, err := verb.RunIssueAdd(cmd.Context(), verbEnv(cmd), verb.RunIssueAddInput{
		Run: args[0], Issues: args[1:],
	})
	recordProjectRegistration(cmd, res.Registered)
	if err != nil {
		return verbErr(err)
	}

	message := fmt.Sprintf("Added %s to %s", issueRefList(res.Added), res.Run.Ref())
	if res.Run.Status == model.RunActive {
		message += " (bound and snapshotted at the next `docket run activate`)"
	}
	w.Success(runIssueAnswer(res.Run, res.Issues), message)
	return nil
}

func runRunIssueRemove(cmd *cobra.Command, args []string, w *output.Writer) error {
	res, err := verb.RunIssueRemove(cmd.Context(), verbEnv(cmd), verb.RunIssueRemoveInput{
		Run: args[0], Issues: args[1:],
	})
	recordProjectRegistration(cmd, res.Registered)
	if err != nil {
		return verbErr(err)
	}
	w.Success(runIssueAnswer(res.Run, res.Issues), fmt.Sprintf("Removed %s from %s",
		issueRefList(res.Removed), res.Run.Ref()))
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
