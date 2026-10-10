package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var runRefreshBodyCmd = &cobra.Command{
	Use:   "refresh-body RUN-N --issue DKT-M --reason R",
	Short: "Make an authorized description amendment reach one issue's remaining steps",
	Long: `Re-snapshot ONE issue's description in a live run, from what the issue declares now.

Activation freezes an issue's description into the run, and every packet's
REQUEST and issue.body input render from that snapshot — so ` + "`issue edit --description`" + `
reaches no live run. This verb is the explicit exception for an amendment the
operator authorized, the body twin of ` + "`run refresh-scope`" + `.

It copies the issue's live description verbatim into that one run-issue's
snapshot. There is no --body here: ` + "`issue create|edit --description`" + ` stays the
sole writer of the text it copies, so a refresh cannot make real anything that
was not declared through that gate. The title, kind, labels and scope snapshot
are not touched.

It refuses when the live description already equals the snapshot: amend the
issue first, then refresh.

It refuses while any of the issue's steps is claimed, running, or gated, or
while a dispatch is open — a holder of a packet rendered under the frozen
description must not record under the amended one. It also refuses on a run
that is not active or waiting-human, and on an issue activation has not bound.

It rewrites no already-recorded step context: terminal steps keep the request
their claim recorded, and only the remaining steps render the new text. One
` + "`issue-body-refreshed`" + ` event carries both digests, the superseded text, the
steps reached, and the reason.

--reason is required: the event trail must say why a live run's packets
changed what they ask for.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunRefreshBody(cmd, args[0], getWriter(cmd))
	},
}

func runRunRefreshBody(cmd *cobra.Command, ref string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(ref)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	issueRef, _ := cmd.Flags().GetString("issue")
	if issueRef == "" {
		return cmdErr(
			fmt.Errorf("--issue is required: the body snapshot is frozen per "+
				"issue, so a refresh names the one issue whose remaining steps "+
				"should render the amended description"),
			output.ErrValidation)
	}
	issueID, err := issueArg(issueRef)
	if err != nil {
		return err
	}

	reason, _ := cmd.Flags().GetString("reason")
	if reason == "" {
		return cmdErr(
			fmt.Errorf("--reason is required to refresh a snapshotted body; the "+
				"event trail must say why a live run's packets changed what "+
				"they ask for"),
			output.ErrValidation)
	}

	outcome, err := engine.RefreshIssueBodyInRun(conn, runID, issueID, reason, model.NowMS())
	if err != nil {
		return runErr(err)
	}

	var message string
	if !w.JSONMode {
		message = renderRefreshBodyOutcome(outcome)
	}
	w.Success(outcome, message)
	return nil
}

func renderRefreshBodyOutcome(o *engine.RefreshedBody) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Refreshed %s's description in %s:\n  sha256 %s\n  ->\n  sha256 %s\n",
		o.Issue, o.Run, o.FromSHA256, o.ToSHA256)
	fmt.Fprintf(&b, "%d step(s) will render it: %s\n",
		len(o.Steps), strings.Join(o.Steps, ", "))
	b.WriteString(
		"Steps that already recorded keep the request they were given (see the " +
			"issue-body-refreshed event).")
	return b.String()
}

func init() {
	runRefreshBodyCmd.Flags().String("issue", "",
		"The issue whose snapshotted description is refreshed (required)")
	runRefreshBodyCmd.Flags().String("reason", "",
		"Why the run's frozen description is moving (required)")
	runCmd.AddCommand(runRefreshBodyCmd)
}
