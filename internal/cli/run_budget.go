package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `docket run budget RUN-N [--set N]` — engine-spec §1's budget line
// (docs/tdd/events-follow.md §7).
//
// The verb that was missing. A run that breached its cap parked at
// `waiting-human`; `run resume` returned it to `active` and the very next claim
// breached again, because the cap had not moved. Until this stage the only
// answers were "abandon the run and start a new one" or an `sqlite3` UPDATE
// performed at the operator's own risk (operations.md §4).

var runBudgetCmd = &cobra.Command{
	Use:   "budget RUN-N",
	Short: "Read or change a run's budget cap",
	Long: `Read a run's effective cap, or change it with --set.

Without --set this reports the cap, where it came from, and the two quantities
it is compared against: the FLOOR (the sum of the declared costs of every step
this run has claimed — a fact the engine produced itself, so the cap holds even
with reporting absent) and the REPORTED usage in the unit ` + "`budget.unit`" + ` names.
What is enforced is the larger of the two.

A SECOND, INDEPENDENT CAP counts MEASURED usage — what the ledger actually
recorded — rather than declared step costs (DKT-238). It is armed by
` + "`--usage-budget`" + ` (or ` + "`budget.usage.default`" + `) together with
` + "`budget.usage.unit`" + `, and both are needed: a cap with no unit counts
nothing. The two dimensions are enforced independently and are NOT combined,
because they are not commensurable — 280 declared units and 4.8M output tokens
are answers to different questions, and one number cannot carry both. They are
also checked differently: a step's declared cost is known before it runs, so
the declared cap RESERVES; a step's token spend is not, so the measured cap
STOPS once recorded usage passes it.

--set raises or lowers the cap. A run that breached its cap is parked at
waiting-human: raising the cap does NOT restart it, because deciding when work
resumes is what waiting-human hands to a person. Raise the cap, then
` + "`docket run resume RUN-N`" + `, and the next claim proceeds.

Lowering below what the run has already spent takes effect the same way: the
next claim refuses. Raising a cap cannot un-spend what was spent — the floor is
computed from what the run actually claimed.

The change is recorded as an event, so a run whose cap moved has a trail that
says so. The cap is a bare number: what it counts is the workflow author's
business, and core attaches no denomination to it.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRunBudget(cmd, args[0], getWriter(cmd))
	},
}

func runRunBudget(cmd *cobra.Command, ref string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(ref)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	// The READ form. Bare `run budget RUN-N` answers the question an operator
	// asks before deciding what to set it to, so they do not have to run a
	// report to find out what the run has spent.
	if !cmd.Flags().Changed("set") {
		result, err := engine.GetRunBudget(conn, runID)
		if err != nil {
			return runErr(err)
		}
		return emitRunBudget(w, result, "")
	}

	budget, err := cmd.Flags().GetFloat64("set")
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}
	reason, _ := cmd.Flags().GetString("reason")

	ifVersion, err := ifVersionOf(cmd)
	if err != nil {
		return err
	}

	result, err := engine.SetRunBudget(
		conn, runID, budget, reason, ifVersion, model.NowMS())
	if err != nil {
		// A CAS mismatch is CONFLICT and a missing row is NOT_FOUND, mapped by
		// the same helper every other `--if-version` verb uses — so the code a
		// script tests for is the same one it already handles elsewhere.
		if mapped := casError(err, model.FormatRunID(runID)); mapped != nil {
			return mapped
		}
		return runErr(err)
	}

	message := fmt.Sprintf("%s budget set to %s", result.Run, formatCap(result.Budget))
	if reason != "" {
		message += ": " + reason
	}
	return emitRunBudget(w, result, message)
}

func emitRunBudget(w *output.Writer, b *engine.RunBudget, message string) error {
	if !w.JSONMode && message == "" {
		message = renderRunBudget(b)
	}
	w.Success(b, message)
	return nil
}

// renderRunBudget is the read form's human output.
//
// It prints the SPEND and the two numbers it was taken over, because "why did
// my run stop?" is answered by the relationship between them and not by any one
// of them alone.
func renderRunBudget(b *engine.RunBudget) string {
	// One label column wide enough for every label, including the measured
	// dimension's, so the numbers line up as a column rather than as two
	// blocks that happen to be adjacent.
	const label = "  %-12s"
	var out strings.Builder
	fmt.Fprintf(&out, "%s budget\n", b.Run)
	fmt.Fprintf(&out, label+"%s (%s)\n", "cap", formatCap(b.Budget), b.Source)
	fmt.Fprintf(&out, label+"%g\n", "floor", b.Floor)
	if b.Unit != "" {
		fmt.Fprintf(&out, label+"%g (%s)\n", "reported", b.Reported, b.Unit)
	}
	fmt.Fprintf(&out, label+"%g", "spend", b.Spend)
	// The MEASURED dimension, when it is armed (DKT-238). Rendered as its own
	// block rather than folded into the numbers above, because it is a
	// separate cap over a different quantity and raising one does nothing for
	// the other.
	if b.UsageBudget > 0 {
		fmt.Fprintf(&out, "\n"+label+"%s", "usage cap", formatCap(b.UsageBudget))
		if b.UsageUnit == "" {
			out.WriteString(" (DORMANT: budget.usage.unit is unset, " +
				"so this cap counts nothing)")
		} else {
			fmt.Fprintf(&out, " (%s)\n"+label+"%g",
				b.UsageUnit, "usage spend", b.UsageSpend)
		}
	}
	return out.String()
}

// formatCap renders a cap, spelling 0 as the word it means.
//
// `%g` would print "0", and a report reading "cap 0" invites exactly the wrong
// conclusion — that the run may spend nothing — when it means the opposite.
// This is the same reason the config key's doc line spells it out.
func formatCap(cap float64) string {
	if cap <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%g", cap)
}

func init() {
	runBudgetCmd.Flags().Float64(
		"set", 0, "Set the cap to this number; 0 means unlimited")
	runBudgetCmd.Flags().String(
		"reason", "", "Why the cap is changing (recorded in the event)")
	addIfVersionFlag(runBudgetCmd)

	runCmd.AddCommand(runBudgetCmd)
}
