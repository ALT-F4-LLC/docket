package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var runRefreshScopeCmd = &cobra.Command{
	Use:   "refresh-scope RUN-N --issue DKT-M --reason R",
	Short: "Make an authorized scope widen reach one issue's remaining steps",
	Long: `Re-snapshot ONE issue's scope in a live run, from what the issue declares now (DKT-869).

Activation freezes an issue's title, kind, labels and scope into the run, and
every packet renders from that snapshot — so ` + "`issue edit --scope`" + ` reaches the
scheduler's mutual-exclusion check and NOTHING ELSE. That freeze is correct and
stays. What it cost on RUN-52 was the case where the widen was authorized: the
panel rejected the work as out of scope, the operator agreed and widened it,
and the already-minted ` + "`fix@2`" + ` step still rendered the old scope, so the honest
remedy was unexecutable and the issue was abandoned mid-loop.

This verb is that case and only that case. It copies ` + "`issues.scope_globs`" + ` —
the column ` + "`issue create|edit --scope`" + ` is the sole writer of — into the run's
snapshot for one issue, and rewrites nothing else in it: the title, kind,
labels, linked pins and description snapshot activation froze are re-encoded
byte for byte, so a mid-run relabel still cannot reroute a step and a mid-run
description edit still cannot reach a packet.

IT CARRIES NO SCOPE OF ITS OWN, deliberately. There is no --scope here. The
only way to change what this verb will copy is to declare it on the issue,
through the one gate scope widening has always had — so a refresh can never
make real a scope nobody authorized, and a refresh with no widen behind it is
refused with nothing to copy.

WHAT IT NEVER CHANGES: terminal steps. Their artifacts and their recorded diffs
keep the scope they ran under, and the ` + "`issue-scope-refreshed`" + ` event carries
the old scope, the new one, and the instances the change reaches — so two steps
of one run declaring two different scopes is a dated, attributable fact in the
ledger rather than drift a reader has to infer.

It REFUSES rather than proceeding when the change could be straddled:

  - while any of the issue's steps is claimed, running, or gated (an executor
    holds a packet rendered under the frozen scope, or a diff-shaped gate is
    mid-saga over the artifact's paths)
  - while a dispatch is open (its manifest was offered under the frozen scope)
  - on a run that is planning, done, or abandoned — a planning run snapshots
    the current scope at its next activation by itself, and a terminal run's
    snapshot is history
  - when every step of the issue in this run is terminal — nothing will render
    again, so the widened scope reaches the issue's NEXT run
  - when the live scope already matches the frozen one: widen it first

--reason is required. A live run's packets changing what they declare is
something somebody will ask about later, and a trail that shows the scope
moving without saying why is a record that rewrote itself.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunRefreshScope(cmd, args[0], getWriter(cmd))
	},
}

func runRunRefreshScope(cmd *cobra.Command, ref string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(ref)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	issueRef, _ := cmd.Flags().GetString("issue")
	if issueRef == "" {
		return cmdErr(
			fmt.Errorf("--issue is required: the snapshot is frozen per issue, "+
				"so a refresh names the one issue whose remaining steps should "+
				"render the widened scope"),
			output.ErrValidation)
	}
	issueID, err := issueArg(issueRef)
	if err != nil {
		return err
	}

	reason, _ := cmd.Flags().GetString("reason")
	if reason == "" {
		return cmdErr(
			fmt.Errorf("--reason is required to refresh a snapshotted scope; the "+
				"event trail must say why a live run's packets changed what "+
				"they declare"),
			output.ErrValidation)
	}

	outcome, err := engine.RefreshIssueScopeInRun(
		conn, runID, issueID, reason, model.NowMS())
	if err != nil {
		return runErr(err)
	}

	var message string
	if !w.JSONMode {
		message = renderRefreshScopeOutcome(outcome)
	}
	w.Success(outcome, message)
	return nil
}

func renderRefreshScopeOutcome(o *engine.RefreshedScope) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Refreshed %s's scope in %s:\n  %s\n  ->\n  %s\n",
		o.Issue, o.Run, renderScopeList(o.From), renderScopeList(o.To))
	fmt.Fprintf(&b, "%d step(s) will render it and record their diffs over it: %s\n",
		len(o.Steps), strings.Join(o.Steps, ", "))
	// The half the operator must not have to infer: what did NOT move. It is
	// the same division `run repin` prints, and for the same reason — an
	// operator reading only the first half could reasonably think the run's
	// completed work had been re-scoped underneath them.
	b.WriteString(
		"Steps that already recorded keep the scope they ran under (see the " +
			"issue-scope-refreshed event).")
	return b.String()
}

// renderScopeList names a scope for the terminal, keeping the undeclared case
// distinct from the declared-empty one exactly as the engine's advisory does.
func renderScopeList(globs []string) string {
	if len(globs) == 0 {
		return "(no declared scope)"
	}
	return "[" + strings.Join(globs, ", ") + "]"
}

func init() {
	runRefreshScopeCmd.Flags().String("issue", "",
		"The issue whose snapshotted scope is refreshed (required)")
	runRefreshScopeCmd.Flags().String("reason", "",
		"Why the run's frozen scope is moving (required)")
	runCmd.AddCommand(runRefreshScopeCmd)
}
