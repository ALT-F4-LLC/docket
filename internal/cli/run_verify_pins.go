package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var runVerifyPinsCmd = &cobra.Command{
	Use:   "verify-pins RUN-N",
	Short: "Check a run's whole pin set against what those refs resolve to now",
	Long: `Answer "is this run's pin state sound" for EVERY pin the run holds.

READ-ONLY, AND IT WRITES NOTHING — not even a re-pin. A pin is the run's
agreement about bytes; re-pinning on drift would silently rewrite the agreement
instead of reporting that it broke. Moving the agreement is a separate,
explicit, event-recorded act: ` + "`docket run repin`" + ` (DKT-408).

Each pin gets one verdict:

  ok        the ref still resolves to the bytes the run pinned
  changed   it resolves, to DIFFERENT bytes — every verb that reads this ref
            refuses (exit 4), so the run is already blocked on it
  missing   the run depends on the ref and it no longer resolves at all

THEN THE PIN SET IS CHECKED FOR CLOSURE (DKT-821). Every pin matching disk does
not make a pin set healthy: the pinned bytes themselves reference files, and a
reference the run never pinned makes every step whose packet resolves it
unclaimable. Those are reported separately, as ` + "`references`" + `:

  unpinned-reference  a pinned file's packet_includes (or a pending step's
                      packet entry) names a ref this run does not pin — the
                      claim refuses (exit 3) whether or not the file is there

Exit 4 if any pin changed, exit 2 if any is missing and none changed, exit 3 if
the pins are all sound but the pin set is not closed, 0 when both halves pass.

WHY THIS VERB EXISTS. The verbs that check pins each check only the pins THEY
read: step render verifies its template and its own step's packet files, and a
payload validates against the one schema its step declares. That scoping is
correct for them, and it means none of them answers the whole-run question — a
step render can return a clean packet and exit 0 while a contract another step
depends on has already drifted, which is a run that is structurally blocked and
does not say so yet.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunVerifyPins(cmd, args[0], getWriter(cmd))
	},
}

func runRunVerifyPins(cmd *cobra.Command, ref string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(ref)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	report, err := engine.VerifyPins(conn, runID)
	if err != nil {
		return runErr(err)
	}

	// UNSOUND IS AN ERROR, not a report with a sad line in it. The question
	// this verb answers is asked by scripts and by conductors deciding whether
	// to proceed, and an exit code is the only part of the answer they cannot
	// miss. The code matches what the blocked verbs themselves return, so a
	// caller that handles one handles both.
	if !report.Sound() {
		code := output.ErrConflict
		remedy := fmt.Sprintf("; restore the file(s), or `docket run repin %s "+
			"--reason ...` adopts current disk bytes for the remaining steps "+
			"(DKT-408)", report.Run)
		switch {
		case report.Changed > 0:
			// Drift wins the disposition: it is the half a repin can fix, and
			// its remedy is the one to try first.
		case report.Missing > 0:
			code = output.ErrNotFound
			// A missing ref has no current bytes to adopt; repin would refuse.
			remedy = "; restore the file(s) or abandon the run"
		default:
			// ONLY THE CLOSURE IS OPEN. Nothing drifted, so there is nothing on
			// disk to restore and repin has no changed bytes to adopt — it
			// would report a clean no-op on exactly this run. The pin set froze
			// without the ref, and only a new activation pins one. The code is
			// the one the blocked claims themselves return, which is this
			// verb's rule: a caller that handles the refusal handles the
			// prediction of it.
			code = output.ErrValidation
			remedy = "; the pin set froze without the ref(s), so the step(s) " +
				"reading them refuse at claim (VALIDATION_ERROR) — start a new " +
				"run to pin them, or take the reference back out of the " +
				"referencing file"
		}
		return cmdErr(fmt.Errorf("%s: %s%s",
			report.Run, engine.PinReportReason(report), remedy), code)
	}

	var message string
	if !w.JSONMode {
		message = renderPinReport(report)
	}
	w.Success(report, message)
	return nil
}

func renderPinReport(r *engine.PinReport) string {
	var b strings.Builder
	// BOTH HALVES ARE STATED on success, because a reader who was told only
	// "all sound" cannot tell whether the closure was checked at all — which is
	// precisely the false confidence DKT-821 was.
	fmt.Fprintf(&b, "%s: %d pin(s), all sound; every ref they reference is pinned",
		r.Run, len(r.Pins))
	return b.String()
}

func init() {
	runCmd.AddCommand(runVerifyPinsCmd)
}
