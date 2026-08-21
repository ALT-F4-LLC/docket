package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var runRepinCmd = &cobra.Command{
	Use:   "repin RUN-N --reason R",
	Short: "Re-pin a run's drifted pins to current disk bytes, for its remaining steps",
	Long: `Adopt what each drifted ref resolves to NOW as the run's pinned bytes (DKT-408).

This is the recovery half of the pin story. A corpus install that replaces
files under an instance-config root while a run is active or parked leaves the
run's frozen pins mismatching disk permanently: verify-pins reports it, every
step reading a drifted ref refuses (CONFLICT), and re-activation deliberately
inherits the original pin set. Before this verb the only disposition was
abandon + full re-plan.

WHAT IT CHANGES, AND WHAT IT NEVER CHANGES. It updates the run's CURRENT
agreement — the pin rows future claims and renders verify against — and
records one ` + "`run-repinned`" + ` event per changed ref carrying the old sha, the new
sha, and your --reason, in the same transaction. Completed steps' provenance
is never rewritten: their rows, artifacts, and events are untouched, and the
agreement they worked under stays recoverable from the trail (steps recorded
before the repin event worked under its old_sha256).

It REFUSES rather than proceeding when the transition could be straddled:

  - while any step is claimed (an executor mid-flight holds a packet rendered
    under the old agreement)
  - while a dispatch is open (its manifest was offered under the current pins)
  - on a run that is done, abandoned, or planning, or whose steps are all
    terminal — nothing remains for the new agreement to govern, so a repin
    could only rewrite completed steps' history
  - when a pinned ref no longer resolves at all (NOT_FOUND: there are no
    current bytes to adopt; restore the file instead)

--reason is required. A repin moves the agreement every packet is verified
against, and a trail that shows the hashes changing without saying why is a
record that rewrote itself.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunRepin(cmd, args[0], getWriter(cmd))
	},
}

func runRunRepin(cmd *cobra.Command, ref string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(ref)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}
	reason, _ := cmd.Flags().GetString("reason")
	if reason == "" {
		return cmdErr(
			fmt.Errorf("--reason is required to repin a run; the event trail must "+
				"say why the recorded agreement moved"),
			output.ErrValidation)
	}

	outcome, err := engine.RepinRun(conn, runID, reason, model.NowMS())
	if err != nil {
		return runErr(err)
	}

	var message string
	if !w.JSONMode {
		message = renderRepinOutcome(outcome)
	}
	w.Success(outcome, message)
	return nil
}

func renderRepinOutcome(o *engine.RepinOutcome) string {
	if len(o.Repinned) == 0 {
		return fmt.Sprintf("%s: every pin already matches disk; nothing to repin", o.Run)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Repinned %d pin(s) for %s (%d already matched):\n",
		len(o.Repinned), o.Run, o.Unchanged)
	for _, c := range o.Repinned {
		fmt.Fprintf(&b, "  %-9s %-40s %s -> %s\n",
			c.Kind, c.Ref, shortSHA(c.OldSHA256), shortSHA(c.NewSHA256))
	}
	b.WriteString(
		"Completed steps keep their original pins as history (see the run-repinned events).")
	return strings.TrimRight(b.String(), "\n")
}

func init() {
	runRepinCmd.Flags().String("reason", "",
		"Why the recorded agreement is moving (required)")
	runCmd.AddCommand(runRepinCmd)
}
