package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `docket policy` — reads of the routing a run's PINNED policy.toml assigns.
//
// Routing rides every step row already (`next --run` fills an executor row's
// model/effort/variant and a vote row's voter_assignments). A CONVERSATIONAL
// gate — ack-reap, activation, budget, fix-batch — has no step row for it to
// ride on, and its panel is seated by a conductor whose contract forbids
// choosing a model, tier or effort. This namespace is that lookup: a read of
// the pinned policy, so no caller has to open policy.toml and re-derive the
// answer with a parser of its own.

var policyCmd = &cobra.Command{
	Use:   "policy",
	Short: "Read the routing a run's pinned policy.toml assigns",
	Long: `Read what a run's PINNED policy.toml routes to.

These are lookups, not choices: the answer is whatever the policy the run
pinned at activation already says, resolved by the engine's own resolver — the
same one that fills a step row's routing.`,
}

var policyResolveCmd = &cobra.Command{
	Use:   "resolve --run RUN-N SEAT [SEAT...]",
	Short: "Resolve each seat's model, effort and variant from the run's pinned policy",
	Long: `Resolve one {voter, model, effort, variant} per named seat, in the order given.

This is the SAME resolution a vote step row's voter_assignments carry: the
per-seat ` + "`[executors].<seat>.never`" + ` list merged with ` + "`[security].never`" + ` on a
sensitive seat, the standing variant clamped to ` + "`[security].ceiling`" + `, and a
still-forbidden model redirected through ` + "`[escalation.fallback]`" + `. A seat is
sensitive when it is named in ` + "`[security].nodes`" + ` or when a --label matches
` + "`[security].labels`" + `.

--label (repeatable) stands in for the issue labels a vote step row snapshots.
A conversational gate carries none of its own, so pass whatever labels govern
the sensitivity check; with none passed the seats resolve as an unlabelled row
does.

EVERY SEAT RESOLVES OR THE CALL FAILS with NOT_FOUND naming the seat. A panel
needs its whole roster, and a partial answer would seat a panel nobody chose.
A run with no pinned policy.toml is NOT_FOUND for the same reason: there is
nothing to look the seats up in.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPolicyResolve(cmd, args, getWriter(cmd))
	},
}

// policyResolveResult is one entry per requested seat, in request order.
//
// The entries are model.VoterAssignment — the SAME wire shape a vote step
// row's voter_assignments carry — so a caller that already reads a panel off a
// step row reads this one with the same code.
type policyResolveResult struct {
	Seats []model.VoterAssignment `json:"seats"`
	Total int                     `json:"total"`
}

func (r policyResolveResult) CollectionItems() any      { return r.Seats }
func (r policyResolveResult) CollectionTotal() int      { return r.Total }
func (r policyResolveResult) CollectionTruncated() bool { return false }

var _ output.Collection = policyResolveResult{}

func runPolicyResolve(cmd *cobra.Command, args []string, w *output.Writer) error {
	ref, _ := cmd.Flags().GetString("run")
	if ref == "" {
		return cmdErr(fmt.Errorf(
			"--run is required: name the run whose pinned policy.toml the seats "+
				"resolve against"), output.ErrValidation)
	}
	runID, err := parseRunFlag(ref)
	if err != nil {
		return err
	}
	labels, _ := cmd.Flags().GetStringSlice("label")

	seats, err := engine.ResolveSeats(getDB(cmd), runID, args, labels)
	if err != nil {
		return runErr(err)
	}

	result := policyResolveResult{Seats: seats, Total: len(seats)}

	var message string
	if !w.JSONMode {
		message = renderPolicySeats(seats)
	}
	w.Success(result, message)
	return nil
}

func renderPolicySeats(seats []model.VoterAssignment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-24s %-12s %-10s %s\n", "SEAT", "MODEL", "EFFORT", "VARIANT")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 60))
	for _, s := range seats {
		fmt.Fprintf(&b, "%-24s %-12s %-10s %s\n", s.Voter, s.Model, s.Effort, s.Variant)
	}
	return b.String()
}

func init() {
	policyResolveCmd.Flags().String(
		"run", "", "The run whose pinned policy.toml the seats resolve against")
	policyResolveCmd.Flags().StringSliceP("label", "l", nil,
		"Label to weigh in the [security] sensitivity check (repeatable)")

	policyCmd.AddCommand(policyResolveCmd)
	rootCmd.AddCommand(policyCmd)
}
