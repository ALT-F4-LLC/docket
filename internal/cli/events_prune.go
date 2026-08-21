package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `docket events prune` — engine-spec §3's retention clause as a verb
// (docs/tdd/events-follow.md §5).
//
// THE FIRST DESTRUCTIVE VERB IN THE ENGINE SURFACE. The refusals live in
// internal/engine (events_prune.go); what lives here is the surface that makes
// the destruction deliberate: no default target, and no deletion without
// `--yes`.
//
// THE FLAG SET IS WIDER THAN §1'S SPELLING, which writes `docket events prune
// --before …` and stops. `--before-run`, `--run`, `--dry-run`, and `--yes` are
// additions, and each is RECORDED as an AMENDMENT rather than made silently —
// the same class as `events list`, and resolved the same way: the shape
// §1 and §3 specify is satisfied, and the spelling is recorded. Every addition
// is in the direction of refusing more.

var eventsPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Delete old events",
	Long: `Delete events, permanently.

Docket deletes nothing on its own. This is the verb an operator runs to bound
the log's growth, and it refuses more than it accepts:

  - Events of runs that have not reached ` + "`done`" + ` or ` + "`abandoned`" + ` are never
    deleted. A live run's events are what the engine computes from — its budget
    floor is summed from its claim events, and its saga resumes from its gate
    events — so pruning them would change the run rather than only its record.
  - Events younger than ` + "`docket config events.retain`" + ` are never deleted. That
    window defaults to 0, which retains EVERYTHING: prune deletes nothing at all
    until a retention policy is set.

Pruning breaks the audit trail that ` + "`docket run report`" + ` reports over. Prune
whole runs that have finished rather than the oldest events across all of them,
and the report stays honest for every run it still covers.

A consumer whose cursor falls below what survives is told so — ` + "`GONE`" + `
(exit 9), naming the seq to resume from — rather than handed a short answer it
would read as having caught up.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runEventsPrune(cmd, getWriter(cmd))
	},
}

// eventsPruneResult is the verb's answer (P7).
//
// It is NOT a Collection: a prune returns one fact about what happened, not a
// page of items, and wrapping a scalar answer in `{items, total, truncated}`
// would make a consumer unwrap an envelope to find a number.
type eventsPruneResult struct {
	Pruned          int    `json:"pruned"`
	RetainedMinimum int64  `json:"retained_minimum"`
	HeldByRetention int    `json:"held_by_retention,omitempty"`
	DryRun          bool   `json:"dry_run,omitempty"`
	Before          int64  `json:"before,omitempty"`
	BeforeRun       string `json:"before_run,omitempty"`
}

func runEventsPrune(cmd *cobra.Command, w *output.Writer) error {
	conn := getDB(cmd)

	before, _ := cmd.Flags().GetInt64("before")
	beforeRunRef, _ := cmd.Flags().GetString("before-run")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	yes, _ := cmd.Flags().GetBool("yes")

	var beforeRun int
	if beforeRunRef != "" {
		id, err := model.ParseRunID(beforeRunRef)
		if err != nil {
			return cmdErr(err, output.ErrValidation)
		}
		beforeRun = id
	}

	runRef, _ := cmd.Flags().GetString("run")
	runID, err := engine.ResolveRunFilter(conn, runRef)
	if err != nil {
		return runErr(err)
	}

	// P6: A DELETION REQUIRES `--yes`.
	//
	// It is required in BOTH modes rather than only under `--json`. The
	// alternative — prompting a human and requiring the flag only from a script
	// — would make the verb's behavior depend on whether a terminal happened to
	// be attached, which is the property that turns "I piped it to less" into a
	// deleted log. `--dry-run` needs no confirmation, because it deletes
	// nothing.
	if !dryRun && !yes {
		return cmdErr(
			fmt.Errorf("events prune deletes events permanently; pass --yes to "+
				"confirm, or --dry-run to see what it would delete"),
			output.ErrValidation)
	}

	result, err := engine.PruneEvents(conn, engine.PruneQuery{
		Before:    before,
		BeforeRun: beforeRun,
		RunID:     runID,
		DryRun:    dryRun,
		NowMS:     model.NowMS(),
	})
	if err != nil {
		return runErr(err)
	}

	payload := eventsPruneResult{
		Pruned:          result.Pruned,
		RetainedMinimum: result.RetainedMinimum,
		HeldByRetention: result.HeldByRetention,
		DryRun:          result.DryRun,
		Before:          before,
	}
	if beforeRun != 0 {
		payload.BeforeRun = model.FormatRunID(beforeRun)
	}

	var message string
	if !w.JSONMode {
		message = renderPruneResult(payload)
	}
	w.Success(payload, message)
	return nil
}

// renderPruneResult is the human line.
//
// It ALWAYS names the retained minimum, including when nothing was deleted,
// because that number is what a consumer sets its cursor to and an operator
// running this verb is usually about to tell one.
//
// It names what the retention window held back whenever it held anything back
// (P14). An operator who asked to prune below seq 900, pruned 200 events, and
// was told only "200" would conclude the verb is broken; told "700 held by
// events.retain", they know it is policy.
func renderPruneResult(r eventsPruneResult) string {
	verb := "Pruned"
	if r.DryRun {
		verb = "Would prune"
	}

	line := fmt.Sprintf("%s %d event(s).", verb, r.Pruned)
	if r.HeldByRetention > 0 {
		line += fmt.Sprintf(" %d held by events.retain.", r.HeldByRetention)
	}
	if r.RetainedMinimum > 0 {
		line += fmt.Sprintf(" Retained minimum is now seq %d.", r.RetainedMinimum)
	} else {
		line += " No events remain."
	}
	return line
}

func init() {
	eventsPruneCmd.Flags().Int64(
		"before", 0, "Delete events with seq strictly less than this")
	eventsPruneCmd.Flags().String(
		"before-run", "", "Delete every event of this run (RUN-N)")
	eventsPruneCmd.Flags().String(
		"run", "", "Narrow --before to one run (RUN-N)")
	eventsPruneCmd.Flags().Bool(
		"dry-run", false, "Report what would be deleted and delete nothing")
	eventsPruneCmd.Flags().Bool(
		"yes", false, "Confirm the deletion (required unless --dry-run)")

	eventsCmd.AddCommand(eventsPruneCmd)
}
