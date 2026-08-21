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

Exit 4 if any pin changed, exit 2 if any is missing and none changed, 0 when
every pin is sound.

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
		if report.Changed == 0 {
			code = output.ErrNotFound
			// A missing ref has no current bytes to adopt; repin would refuse.
			remedy = "; restore the file(s) or abandon the run"
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
	fmt.Fprintf(&b, "%s: %d pin(s), all sound", r.Run, len(r.Pins))
	return b.String()
}

func init() {
	runCmd.AddCommand(runVerifyPinsCmd)
}
