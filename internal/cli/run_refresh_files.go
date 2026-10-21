package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var runRefreshFilesCmd = &cobra.Command{
	Use:   "refresh-files RUN-N --issue DKT-M --reason R",
	Short: "Make an attach or detach reach one issue's remaining steps",
	Long: `Re-snapshot ONE issue's attached file list in a live run, from what the issue declares now.

Activation freezes an issue's attached paths into the run, and every packet's
issue.files input renders from that snapshot — so ` + "`issue file add|remove`" + `
reaches no live run. A detached path keeps rendering, and deleting its file
makes the render refuse. This verb is the explicit exception, the attachment
twin of ` + "`run refresh-scope`" + ` and ` + "`run refresh-body`" + `.

It copies the issue's live attachment list verbatim into that one run-issue's
snapshot. There is no --file here: ` + "`issue file add|remove`" + ` stays the sole
writer of the list it copies. Every other snapshot field is re-encoded
unchanged.

It refuses when the live list already equals the frozen one: attach or detach
first, then refresh.

It refuses while any of the issue's steps is claimed, running, or gated,
when every step of the issue is terminal, on a run
that is not active or waiting-human, and on an issue activation has not bound
or whose snapshot predates the frozen list.

An open dispatch does not refuse it: the issue's unstarted manifest rows render the
amended packet at claim, the event names the dispatch and those rows, and
` + "`dispatch verify`" + ` still passes.


It rewrites no already-recorded step context: terminal steps keep what they
recorded, and only the remaining steps render the new list. One
` + "`issue-files-refreshed`" + ` event carries both lists, the steps reached, and the
reason.

--reason is required: the event trail must say why a live run's packets
changed which files they carry.

Requires the run's conductor capability (see ` + "`docket run conduct --help`" + `)
via DOCKET_TOKEN or stdin; a run activated before the capability existed asks
for none.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunRefreshFiles(cmd, args[0], getWriter(cmd))
	},
}

func runRunRefreshFiles(cmd *cobra.Command, ref string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(ref)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	issueRef, _ := cmd.Flags().GetString("issue")
	if issueRef == "" {
		return cmdErr(
			fmt.Errorf("--issue is required: the attachment list is frozen per "+
				"issue, so a refresh names the one issue whose remaining steps "+
				"should render the live list"),
			output.ErrValidation)
	}
	issueID, err := issueArg(issueRef)
	if err != nil {
		return err
	}

	reason, _ := cmd.Flags().GetString("reason")
	if reason == "" {
		return cmdErr(
			fmt.Errorf("--reason is required to refresh a snapshotted attachment "+
				"list; the event trail must say why a live run's packets changed "+
				"which files they carry"),
			output.ErrValidation)
	}

	by, err := rulingBy()
	if err != nil {
		return err
	}
	outcome, err := engine.RefreshIssueFilesInRun(
		conn, runID, issueID, reason, by, conductorToken(conn, runID, os.Stdin),
		model.NowMS())
	if err != nil {
		return runErr(err)
	}

	var message string
	if !w.JSONMode {
		message = renderRefreshFilesOutcome(outcome)
	}
	w.Success(outcome, message)
	return nil
}

func renderRefreshFilesOutcome(o *engine.RefreshedFiles) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Refreshed %s's attached files in %s:\n  [%s]\n  ->\n  [%s]\n",
		o.Issue, o.Run, strings.Join(o.From, ", "), strings.Join(o.To, ", "))
	fmt.Fprintf(&b, "%d step(s) will render it: %s\n",
		len(o.Steps), strings.Join(o.Steps, ", "))
	b.WriteString(
		"Steps that already recorded keep the files they ran under (see the " +
			"issue-files-refreshed event).")
	return b.String()
}

func init() {
	runRefreshFilesCmd.Flags().String("issue", "",
		"The issue whose snapshotted attachment list is refreshed (required)")
	runRefreshFilesCmd.Flags().String("reason", "",
		"Why the run's frozen attachment list is moving (required)")
	runCmd.AddCommand(runRefreshFilesCmd)
}
