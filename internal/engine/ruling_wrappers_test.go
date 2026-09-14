package engine

import (
	"database/sql"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// The positional ruling verbs, kept so the suite's call sites read as they
// always have.
//
// Production has exactly ONE entry point per ruling — DecideStepWith,
// ResolveStepWith, ForceReapStepWith — and each REQUIRES an attribution
// (DKT-2450), refusing to write an unattributed event the way RecordTrustEvent
// does. These wrappers supply the suite's fixed one. A test asserting
// attribution passes its own through the `With` form instead.

// testBy is the identity every wrapped test ruling carries.
var testBy = Attribution{Actor: "tester", Cwd: "/repo"}

// testConductorToken is the capability every wrapped ruling presents
// (DKT-2465). A run is bound at its first activation, so a fixture's run
// refuses an unauthenticated ruling exactly as a real one does; the wrappers
// seat the suite's fixed conductor on the run — the run's hash is re-keyed to
// this token — and present it, the way testBy stands in for a real identity.
// A test about the capability itself calls the `With` form with its own token
// and never goes through a wrapper.
const testConductorToken = "the-suite-holds-every-run"

// seatTestConductor binds run `runID` to testConductorToken, replacing any
// standing capability. The write is direct: this is a fixture, not a verb,
// and it must leave no `conductor-seated` event for a test to trip over.
func seatTestConductor(conn *sql.DB, runID int) error {
	_, err := conn.Exec(`UPDATE runs SET conductor_token_hash = ? WHERE id = ?`,
		model.HashToken(testConductorToken), runID)
	return err
}

// seatTestConductorForStep is seatTestConductor addressed by the step whose
// run is being ruled on.
func seatTestConductorForStep(conn *sql.DB, stepID int) error {
	step, err := db.GetStep(conn, stepID)
	if err != nil {
		// The ruling itself owns the NOT_FOUND; let it say so.
		return nil
	}
	return seatTestConductor(conn, step.RunID)
}

func (e *Engine) DecideStep(conn *sql.DB, stepID int, approve bool, note string, nowMS int64) error {
	return e.DecideStepValue(conn, stepID, approve, note, "", nowMS)
}

func (e *Engine) DecideStepValue(
	conn *sql.DB, stepID int, approve bool, note, value string, nowMS int64,
) error {
	if err := seatTestConductorForStep(conn, stepID); err != nil {
		return err
	}
	return e.DecideStepWith(conn, stepID, DecideOptions{
		Approve: approve, Note: note, Value: value, By: testBy,
		Token: testConductorToken, NowMS: nowMS,
	})
}

func (e *Engine) ResolveStep(
	conn *sql.DB, stepID int, as, note string, nowMS int64,
) error {
	if err := seatTestConductorForStep(conn, stepID); err != nil {
		return err
	}
	_, err := e.ResolveStepWith(conn, stepID, ResolveOptions{
		As: as, Note: note, By: testBy, Token: testConductorToken, NowMS: nowMS,
	})
	return err
}

func (e *Engine) ResolveStepBatch(
	conn *sql.DB, stepID int, as, note string, nowMS int64,
) error {
	if err := seatTestConductorForStep(conn, stepID); err != nil {
		return err
	}
	_, err := e.ResolveStepWith(conn, stepID, ResolveOptions{
		As: as, Note: note, Batch: true, By: testBy, Token: testConductorToken, NowMS: nowMS,
	})
	return err
}

func (e *Engine) ResolveStepDropInterposed(
	conn *sql.DB, stepID int, as, note string, batch bool, nowMS int64,
) error {
	if err := seatTestConductorForStep(conn, stepID); err != nil {
		return err
	}
	_, err := e.ResolveStepWith(conn, stepID, ResolveOptions{
		As: as, Note: note, Batch: batch, DropInterposed: true, By: testBy,
		Token: testConductorToken, NowMS: nowMS,
	})
	return err
}

func ForceReapStep(conn *sql.DB, stepID int, reason string, nowMS int64) error {
	if err := seatTestConductorForStep(conn, stepID); err != nil {
		return err
	}
	return ForceReapStepWith(conn, stepID, ForceReapOptions{
		Reason: reason, By: testBy, Token: testConductorToken, NowMS: nowMS,
	})
}

// The positional lifecycle verbs, kept for the same reason: production has one
// options entry per verb (MoveRunWith, AbandonIssueInRunWith), and the suite's
// fixed conductor holds the run.

func MoveRun(
	conn *sql.DB, runID int, verb string, to model.RunStatus,
	from []model.RunStatus, reason string, nowMS int64,
) (*model.Run, []string, error) {
	if err := seatTestConductor(conn, runID); err != nil {
		return nil, nil, err
	}
	return MoveRunWith(conn, MoveRunOptions{
		RunID: runID, Verb: verb, To: to, From: from, Reason: reason,
		Token: testConductorToken, NowMS: nowMS,
	})
}

func AbandonIssueInRun(
	conn *sql.DB, runID, issueID int, reason string, nowMS int64,
) (*AbandonIssueOutcome, error) {
	if err := seatTestConductor(conn, runID); err != nil {
		return nil, err
	}
	return AbandonIssueInRunWith(conn, AbandonIssueOptions{
		RunID: runID, IssueID: issueID, Reason: reason,
		Token: testConductorToken, NowMS: nowMS,
	})
}
