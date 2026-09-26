package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestRetryOnDoneRun pins that `step resolve --as retry` never strands a step
// under a terminal run.
//
// RUN-93's verify-tribunal was skipped when its worktree vanished before the
// panel seated, the run rolled up to `done` around it, and the operator asked
// for the gate to re-run. R11 offers a resolution on a vote step at any
// status, so the retry was accepted: the step went `pending`, `step show`
// reported blocked_reason "run is not active", and `run resume` refused
// because resume applies to `waiting-human`. No verb reopens a `done` run
// (RA5), so the honest answer is a refusal up front that names the run.
//
// The last assertion is the acceptance criterion itself, written to fire on
// either outcome: whatever the resolve returned, the step must not sit
// `pending` blocked on the run's status.
func TestRetryOnDoneRun(t *testing.T) {
	for _, runStatus := range []model.RunStatus{model.RunDone, model.RunAbandoned} {
		t.Run(string(runStatus), func(t *testing.T) {
			conn := mustDB(t)
			registerVoteRule(t, conn, "majority", "0.6", "medium")
			e := testEngine()

			step, _ := seedVoteStep(t, conn)
			execSQL(t, conn, `UPDATE steps SET status = ? WHERE id = ?`,
				db.StepSkipped, step.ID)
			execSQL(t, conn, `UPDATE runs SET status = ? WHERE id = ?`,
				string(runStatus), step.RunID)
			runRef := model.FormatRunID(step.RunID)

			err := e.ResolveStep(conn, step.ID, ResolveRetry, "re-run the gate", nowMS)
			if err == nil {
				t.Errorf("retry was accepted on a step whose run is %s", runStatus)
			} else {
				if code, _ := CodeOf(err); code != CodeConflict {
					t.Errorf("error code = %q, want %q", code, CodeConflict)
				}
				// The refusal names the run and its status: the conflict is
				// the run's, and an operator told only "cannot retry" would
				// keep looking for a step-level remedy.
				for _, want := range []string{runRef, string(runStatus)} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal does not name %q: %v", want, err)
					}
				}
			}

			after, err := db.GetStep(conn, step.ID)
			testsupport.Must(t, err, "re-reading the step: %v", err)
			if got := runStatusOf(t, conn, step.RunID); got != string(runStatus) &&
				got != string(model.RunActive) {
				t.Errorf("run status = %q after the resolve, want %q untouched "+
					"or %q reopened", got, runStatus, model.RunActive)
			}

			// The acceptance criterion: in no case does the step land in
			// `pending` with blocked_reason "run is not active".
			if after.Status != db.StepPending {
				return
			}
			view, err := LoadStepView(conn, step.ID, nowMS)
			testsupport.Must(t, err, "LoadStepView: %v", err)
			if view.Row.BlockedReason == string(CondRunActive) {
				t.Fatalf("step %s is %q with blocked_reason %q: the retry was "+
					"accepted under a %s run and nothing can offer the step",
					after.Instance, after.Status, view.Row.BlockedReason, runStatus)
			}
		})
	}
}
