package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// The three status-moving verbs of engine-core §1.1's machine:
//
//	planning -> active <-> waiting-human -> done | abandoned
//
// `pause` and `resume` are the `active <-> waiting-human` edge; `abandon` is
// terminal. They share one implementation because they differ only in the
// status they move to, which status they will accept moving FROM, and whether
// a reason is required.

var runPauseCmd = &cobra.Command{
	Use:   "pause RUN-N",
	Short: "Park an active run",
	Long: `Move an active run to waiting-human.

A paused run blocks new claims and honors in-flight completes: work already
under way finishes rather than being abandoned mid-step.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return moveRun(cmd, args[0], runMove{
			to:   model.RunWaitingHuman,
			from: []model.RunStatus{model.RunActive},
			verb: "Paused",
		}, getWriter(cmd))
	},
}

var runResumeCmd = &cobra.Command{
	Use:   "resume RUN-N",
	Short: "Return a parked run to active",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return moveRun(cmd, args[0], runMove{
			to:   model.RunActive,
			from: []model.RunStatus{model.RunWaitingHuman},
			verb: "Resumed",
		}, getWriter(cmd))
	},
}

var runAbandonCmd = &cobra.Command{
	Use:   "abandon RUN-N --reason R",
	Short: "End a run without completing it, or stop one issue's work in it",
	Long: `Abandon a run. Terminal: an abandoned run refuses re-activation.

With --issue, the disposition narrows to ONE issue of an active (or parked)
run: every remaining step of that issue moves to failed-routed with the
reason recorded, and the run and its other issues continue. The issue's own
status is not forced terminal — triage stays yours. Use it for a mis-routed
or unimplementable issue that should not take the whole run down with it.

--reason is required either way. Work that ended without completing is
something somebody will ask about later, and "abandoned" alone does not
answer the question.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if issueRef, _ := cmd.Flags().GetString("issue"); issueRef != "" {
			return abandonIssueInRun(cmd, args[0], issueRef)
		}
		return moveRun(cmd, args[0], runMove{
			to:             model.RunAbandoned,
			from:           []model.RunStatus{model.RunPlanning, model.RunActive, model.RunWaitingHuman},
			verb:           "Abandoned",
			requiresReason: true,
		}, getWriter(cmd))
	},
}

// abandonIssueInRun is `run abandon --issue` (DKT-28): the per-issue
// disposition, leaving the run and its other issues intact.
func abandonIssueInRun(cmd *cobra.Command, runRef, issueRef string) error {
	w := getWriter(cmd)
	conn := getDB(cmd)

	runID, err := model.ParseRunID(runRef)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}
	issueID, err := issueArg(issueRef)
	if err != nil {
		return err
	}
	reason, _ := cmd.Flags().GetString("reason")
	if reason == "" {
		return cmdErr(
			fmt.Errorf("--reason is required to abandon an issue's work"),
			output.ErrValidation)
	}

	outcome, err := engine.AbandonIssueInRun(conn, runID, issueID, reason, model.NowMS())
	if err != nil {
		return runErr(err)
	}
	message := fmt.Sprintf("Abandoned %s in %s (%d step(s)); the run is %s: %s",
		outcome.Issue, outcome.Run, len(outcome.Steps), outcome.RunStatus, reason)
	message += worktreeNotice(outcome.Worktrees, "the issue's steps")
	w.Success(abandonIssuePayload{outcome: outcome}, message)
	return nil
}

// worktreeNotice is the abandon's worktree tail — the DKT-116 list, STATTED
// before it is printed (DKT-405).
//
// The engine names what the run's steps RECORDED and deliberately walks no
// filesystem (recordedWorktreesTx). That record is a superset of what is
// actually still there: a relay that swept its own checkouts at close time
// leaves the `steps.work_root` rows behind, so the recorded list survives the
// directories. Printed unqualified it read as an inventory of live debris —
// abandoning RUN-14 flagged 20 "outstanding" worktrees that `git worktree
// list` showed were already gone, and the conductor spent a verification pass
// discovering the warning was about nothing.
//
// A stat is not a discovery pass: it asks one question about paths docket was
// already told about, and it turns "here are 20 paths" into "here is what is
// still on disk". Only the present ones are listed, because only they are
// actionable; the absent ones are counted, because their number is what tells
// the reader the list is short for a reason rather than truncated.
//
// Returns "" for a run that recorded nothing — the pre-DKT-116 silence, for
// the case where there is genuinely nothing to say.
func worktreeNotice(worktrees []string, recordedBy string) string {
	if len(worktrees) == 0 {
		return ""
	}
	present, removed := splitRecordedWorktrees(worktrees)

	var notice string
	if len(present) > 0 {
		notice += fmt.Sprintf(
			"\nOutstanding worktrees (recorded by %s, still on disk; no close will sweep them):\n  %s",
			recordedBy, strings.Join(present, "\n  "))
	}
	switch {
	case len(removed) == 0:
	case len(present) == 0:
		notice += fmt.Sprintf(
			"\nWorktrees: all %d recorded by %s are already gone from disk; nothing to sweep.",
			len(removed), recordedBy)
	default:
		notice += fmt.Sprintf(
			"\n(%d further recorded worktree(s) are already gone from disk.)",
			len(removed))
	}
	return notice
}

// splitRecordedWorktrees stats each recorded work root, in the order it was
// recorded, and splits it into what is still on disk and what is not.
//
// A stat error that is NOT "does not exist" — a permission denial, a dead
// mount — counts as PRESENT. The claim being made about the absent list is
// "there is nothing here to clean up", and only ErrNotExist supports it;
// anything else is docket failing to look, and reporting a failure to look as
// an absence is how the original defect read in reverse.
func splitRecordedWorktrees(worktrees []string) (present, removed []string) {
	for _, path := range worktrees {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			removed = append(removed, path)
			continue
		}
		present = append(present, path)
	}
	return present, removed
}

// runMove describes one status transition.
type runMove struct {
	to   model.RunStatus
	from []model.RunStatus
	verb string
	// requiresReason makes --reason mandatory rather than optional.
	requiresReason bool
}

func moveRun(cmd *cobra.Command, ref string, move runMove, w *output.Writer) error {
	conn := getDB(cmd)

	runID, err := model.ParseRunID(ref)
	if err != nil {
		return cmdErr(err, output.ErrValidation)
	}

	reason, _ := cmd.Flags().GetString("reason")
	if move.requiresReason && reason == "" {
		return cmdErr(
			fmt.Errorf("--reason is required to %s a run", cmd.Name()),
			output.ErrValidation)
	}

	// The transition, its refusal rules, and its EVENT live engine-side
	// (DKT-86): the status write and the `run-paused` / `run-resumed` /
	// `run-abandoned` event commit in one transaction, so the feed is a
	// complete transition trail rather than one that goes silent exactly at
	// the terminal step.
	updated, worktrees, err := engine.MoveRun(
		conn, runID, cmd.Name(), move.to, move.from, reason, model.NowMS())
	if err != nil {
		return runErr(err)
	}

	message := fmt.Sprintf("%s %s", move.verb, updated.Ref())
	if reason != "" {
		message = fmt.Sprintf("%s: %s", message, reason)
	}
	// An abandon NAMES the run's recorded worktrees (DKT-116): no close will
	// ever sweep them now, and a silent abandon is how debris stood in five
	// repos with nothing reporting it.
	message += worktreeNotice(worktrees, "this run's steps")
	if move.to == model.RunAbandoned {
		w.Success(runAbandonPayload{run: updated, worktrees: worktrees}, message)
		return nil
	}
	// A RESUME states pin drift unprompted (DKT-408): resuming is exactly the
	// moment a conduct session attaches to a parked run and starts dispatching,
	// and a corpus install during the park is invisible until then. RUN-35 was
	// paused, `just activate` replaced two pinned files, and the drift
	// surfaced only because the resuming conductor's skill prose said to check
	// by hand — the engine owning the warning removes that dependence. The
	// resume itself still SUCCEEDS: the operator may be resuming precisely in
	// order to repin, and the per-step CONFLICT remains the enforcement.
	// Advisory posture throughout — the transition committed above, so a
	// failed drift check must not turn a completed resume into an error.
	if move.to == model.RunActive {
		if drift, driftErr := engine.PinDrift(conn, runID); driftErr == nil && len(drift) > 0 {
			if notice := engine.PinDriftNotice(drift, updated.Ref()); notice != "" {
				message += "\n" + notice
			}
			w.Success(runResumePayload{run: updated, pinDrift: drift}, message)
			return nil
		}
	}
	w.Success(withRunVersion(updated), message)
	return nil
}

func init() {
	runPauseCmd.Flags().String("reason", "", "Why the run is being paused")
	runResumeCmd.Flags().String("reason", "", "Why the run is being resumed")
	runAbandonCmd.Flags().String("reason", "", "Why the run is being abandoned (required)")
	runAbandonCmd.Flags().String("issue", "",
		"Abandon only this issue's remaining steps; the run and its other issues continue")

	runCmd.AddCommand(runPauseCmd, runResumeCmd, runAbandonCmd)
}
