package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
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

Each declared gate whose relatively named script the run pins is also checked
where a gate run from this checkout would execute it, as ` + "`gates`" + `:

  executed-copy-changed  this checkout's copy of the script differs from the
                         run's pin; restore the gate script in the invoking
                         checkout, or run the gate from the run's checkout

Exit 4 if any pin changed or any gate is executed-copy-changed, exit 2 if any
pin is missing and none changed, exit 3 if the pins are all sound but the pin
set is not closed, 0 when every check passes.

WHY THIS VERB EXISTS. The verbs that check pins each check only the pins THEY
read: step render verifies its template and its own step's packet files, and a
payload validates against the one schema its step declares. That scoping is
correct for them, and it means none of them answers the whole-run question — a
step render can return a clean packet and exit 0 while a contract another step
depends on has already drifted, which is a run that is structurally blocked and
does not say so yet.`,
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunVerifyPins(cmd, args[0], getWriter(cmd))
	},
}

func runRunVerifyPins(cmd *cobra.Command, ref string, w *output.Writer) error {
	// UNSOUND IS AN ERROR, not a report with a sad line in it. The question
	// this verb answers is asked by scripts and by conductors deciding whether
	// to proceed, and an exit code is the only part of the answer they cannot
	// miss, so the CLI renders the refusal alone.
	res, err := verb.RunVerifyPins(cmd.Context(), verbEnv(cmd), verb.RunVerifyPinsInput{Run: ref})
	if err != nil {
		return verbErr(err)
	}

	var message string
	if !w.JSONMode {
		message = renderPinReport(res.Report)
	}
	w.Success(res.Report, message)
	return nil
}

func renderPinReport(r *engine.PinReport) string {
	var b strings.Builder
	// BOTH HALVES ARE STATED on success, because a reader who was told only
	// "all sound" cannot tell whether the closure was checked at all — which is
	// precisely the false confidence DKT-821 was.
	fmt.Fprintf(&b, "%s: %d pin(s), all sound; every ref they reference is pinned",
		r.Run, len(r.Pins))
	// Unpinned gates are a report, never a refusal: a `make <target>` trust
	// entry names no script, so the run holds nothing to compare.
	for _, g := range r.Gates {
		fmt.Fprintf(&b, "\ngate %q: %s (%s)", g.Gate, g.Status, g.Reason)
	}
	return b.String()
}

func init() {
	runCmd.AddCommand(runVerifyPinsCmd)
}
