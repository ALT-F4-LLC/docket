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
    current bytes to adopt), UNLESS you retire it with --drop/--drop-unresolvable
    and no non-terminal step reads it — see below

DELETED REFS: --drop and --drop-unresolvable. A corpus commit that DELETES a
contract leaves a pin with no bytes to adopt, and refusing the whole set for it
wedges a run that may have no step left which would ever open that file.

  --drop REF              retire this one ref (repeatable)
  --drop-unresolvable     retire every currently drifted file pin that no
                          longer resolves and that no pending step reads

Retiring means: the pin row is removed, so verify-pins stops reporting it and
the remaining steps proceed; a ` + "`run-repinned`" + ` event is recorded for it with a
NULL new_sha256 and ` + "`dropped: true`" + `, so the old sha — the agreement the
completed steps worked under — stays in the trail exactly as an ordinary repin
leaves it. Completed steps' rows, artifacts, and events are untouched here too.

It is opt-in, never automatic: a NOT_FOUND ref that a NON-TERMINAL step's packet
closure still reaches refuses either way, naming the steps that read it, because
dropping it would only move the wedge to render time. --drop-unresolvable does
not touch refs that resolve to DIFFERENT bytes — those are the ordinary repin —
and neither flag applies to workflow or schema pins, which name registered
objects no packet closure can call unread.

NEWLY-REQUIRED REFS: the adopted bytes bring their own closure. A corpus edit
can make an adopted contract include a fragment the run never snapshotted;
adopting the contract without the fragment would report success while every
step reading it becomes unrenderable (its packet refuses on the unpinned ref).
So a repin that adopts changed bytes also walks the packet closure those bytes
reach and PINS every file ref the run does not already hold, at its current
disk bytes, in the same transaction — each addition recorded as its own
` + "`run-repinned`" + ` event with a null old_sha256 and ` + "`added: true`" + `, naming what
requires it. A newly-required ref with no bytes on disk refuses the whole
repin up front, naming the ref and its readers: restore the file, or abandon
the run and re-plan.

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

	drop, _ := cmd.Flags().GetStringArray("drop")
	dropUnresolvable, _ := cmd.Flags().GetBool("drop-unresolvable")

	outcome, err := engine.RepinRunWith(conn, runID, engine.RepinOptions{
		Reason: reason, Drop: drop, DropUnresolvable: dropUnresolvable,
	}, model.NowMS())
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
	if len(o.Repinned) == 0 && len(o.Dropped) == 0 && len(o.Added) == 0 {
		return fmt.Sprintf("%s: every pin already matches disk; nothing to repin", o.Run)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Repinned %d pin(s) for %s (%d added, %d dropped, %d already matched):\n",
		len(o.Repinned), o.Run, len(o.Added), len(o.Dropped), o.Unchanged)
	for _, c := range o.Repinned {
		fmt.Fprintf(&b, "  %-9s %-40s %s -> %s\n",
			c.Kind, c.Ref, shortSHA(c.OldSHA256), shortSHA(c.NewSHA256))
	}
	// An added ref renders with the arrow pointing FROM nothing, the mirror of
	// the drop below: the run held no bytes for it before, and the adopted
	// bytes are why it holds these now.
	for _, c := range o.Added {
		fmt.Fprintf(&b, "  %-9s %-40s (unpinned) -> %s (added: newly required "+
			"by the adopted bytes)\n", c.Kind, c.Ref, shortSHA(c.NewSHA256))
	}
	// A dropped ref renders with the arrow pointing at nothing on purpose: the
	// operator asked what happened to a pin, and "gone" is the answer, in the
	// same column the new hash would have been.
	for _, c := range o.Dropped {
		fmt.Fprintf(&b, "  %-9s %-40s %s -> (dropped: no longer resolves, unread "+
			"by pending steps)\n", c.Kind, c.Ref, shortSHA(c.OldSHA256))
	}
	b.WriteString(
		"Completed steps keep their original pins as history (see the run-repinned events).")
	return strings.TrimRight(b.String(), "\n")
}

func init() {
	runRepinCmd.Flags().String("reason", "",
		"Why the recorded agreement is moving (required)")
	runRepinCmd.Flags().StringArray("drop", nil,
		"Retire this pinned ref, which no longer resolves and no pending step "+
			"reads (repeatable)")
	runRepinCmd.Flags().Bool("drop-unresolvable", false,
		"Retire every drifted file pin that no longer resolves and no pending "+
			"step reads")
	runCmd.AddCommand(runRepinCmd)
}
