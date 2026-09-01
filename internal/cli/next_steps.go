package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

// `next --run` — step mode (TDD §6.3.1, §6.4).

// nextStepsResult is the step-mode payload. It implements output.Collection so
// the envelope is uniform with every other list verb: `next --run` is a NEW
// verb surface with no v1 legacy to preserve, but a dispatcher parsing it
// should not have to learn a second envelope shape.
type nextStepsResult struct {
	Steps []model.StepRow `json:"steps"`
	Total int             `json:"total"`

	readyTotal int
	limit      int
}

func (r nextStepsResult) CollectionItems() any { return r.Steps }
func (r nextStepsResult) CollectionTotal() int { return r.readyTotal }
func (r nextStepsResult) CollectionTruncated() bool {
	return output.IsTruncated(r.limit, r.readyTotal, len(r.Steps))
}

var _ output.Collection = nextStepsResult{}

// issueModeFlags are the filters that mean something in ISSUE mode and nothing
// in step mode.
//
// They are REFUSED rather than silently ignored (§6.3.1). An issue-mode filter
// that quietly does nothing in step mode is a filter a dispatcher will trust
// and be wrong about — it would ask for `--priority critical` steps, get every
// step, and dispatch work it meant to defer.
var issueModeFlags = []string{"status", "priority", "label", "type"}

func runNextSteps(cmd *cobra.Command, runRef string, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(runRef)
	if err != nil {
		return cmdErr(fmt.Errorf("invalid run ID: %w", err), output.ErrValidation)
	}

	var conflicting []string
	for _, name := range issueModeFlags {
		if cmd.Flags().Changed(name) {
			conflicting = append(conflicting, "--"+name)
		}
	}
	if len(conflicting) > 0 {
		subject := "is an issue filter"
		if len(conflicting) > 1 {
			subject = "are issue filters"
		}
		return cmdErr(
			fmt.Errorf(
				"%s %s and cannot be combined with --run, which lists a run's "+
					"ready STEPS; inspect the run with `docket run status` instead",
				strings.Join(conflicting, ", "), subject,
			),
			output.ErrValidation,
		)
	}

	limit, _ := cmd.Flags().GetInt("limit")
	if err := validateLimit(cmd, limit); err != nil {
		return err
	}
	// DKT-564: the issue-mode DEFAULT limit does not apply in step mode. Here
	// the answer IS a dispatch manifest — the conduct pipeline dispatches it
	// verbatim — so a cut the caller never asked for strands the remaining
	// steps un-dispatched, and a caller reading v1 JSON cannot even tell it
	// happened. A run's ready set is bounded by the run, so returning all of
	// it is the safe contract. An EXPLICIT --limit is still honoured: a caller
	// who typed a cut asked for one, and the v2 envelope reports it as
	// truncated.
	if !cmd.Flags().Changed("limit") {
		limit = 0 // 0 means no limit (readyRows)
	}

	ready, err := engine.NewEngine().NextSteps(conn, runID, limit, model.NowMS())
	if err != nil {
		return runErr(err)
	}

	// Reaping is the one write this read-shaped verb performs (§6.3). Saying so
	// on stderr keeps it auditable without touching the payload — a JSON
	// consumer's shape is unchanged, and a human sees why an `attempt` moved.
	for _, instance := range ready.Reaped {
		w.Info("reaped an expired lease on %s; it is ready again", instance)
	}

	// §6.3: a headroom denial with nothing running is baffling, so the hold is
	// named on stderr — where the reap already reports itself, and for the same
	// reason. The PAYLOAD is untouched: a JSON consumer's shape does not change
	// because a run happens to be holding write headroom.
	if ready.HeldReason != "" {
		w.Info("%s", ready.HeldReason)
	}

	// The loop-body hold (DKT-61), on stderr for the same reason: an offer
	// missing its re-review judges is otherwise indistinguishable from any
	// other narrowing, and a fixer parked at waiting-human holds it
	// open-endedly. The payload is untouched.
	if ready.LoopHeldReason != "" {
		w.Info("%s", ready.LoopHeldReason)
	}

	// The budget hold (DKT-242), on stderr for the same reason and with the
	// same payload guarantee. Without it, `next` answering {"steps":[]} against
	// a run reporting nine pending reads as "the graph has run dry" — the one
	// conclusion that makes a dispatcher stop asking — when the truth is that
	// spend has caught up with the cap and the rows return the moment it moves.
	if ready.BudgetHeldReason != "" {
		w.Info("%s", ready.BudgetHeldReason)
	}

	// The unrouted-interposition hold (DKT-470), on stderr for the same
	// reason and with the same payload guarantee: an empty `{"steps":[]}`
	// against a run whose named steps will NEVER become ready as-is reads
	// identically to a run that is simply between predecessors, and only one
	// of those two resolves itself if the caller just waits and polls again.
	if ready.UnroutedReason != "" {
		w.Info("%s", ready.UnroutedReason)
	}

	result := nextStepsResult{
		Steps: ready.Steps, Total: len(ready.Steps),
		readyTotal: ready.Total, limit: limit,
	}

	var message string
	if !w.JSONMode {
		message = render.RenderStepRows(ready.Steps)
	}
	w.Success(result, message)
	return nil
}
