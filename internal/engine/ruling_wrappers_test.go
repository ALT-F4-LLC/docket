package engine

import "database/sql"

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

func (e *Engine) DecideStep(conn *sql.DB, stepID int, approve bool, note string, nowMS int64) error {
	return e.DecideStepValue(conn, stepID, approve, note, "", nowMS)
}

func (e *Engine) DecideStepValue(
	conn *sql.DB, stepID int, approve bool, note, value string, nowMS int64,
) error {
	return e.DecideStepWith(conn, stepID, DecideOptions{
		Approve: approve, Note: note, Value: value, By: testBy, NowMS: nowMS,
	})
}

func (e *Engine) ResolveStep(
	conn *sql.DB, stepID int, as, note string, nowMS int64,
) error {
	_, err := e.ResolveStepWith(conn, stepID, ResolveOptions{
		As: as, Note: note, By: testBy, NowMS: nowMS,
	})
	return err
}

func (e *Engine) ResolveStepBatch(
	conn *sql.DB, stepID int, as, note string, nowMS int64,
) error {
	_, err := e.ResolveStepWith(conn, stepID, ResolveOptions{
		As: as, Note: note, Batch: true, By: testBy, NowMS: nowMS,
	})
	return err
}

func (e *Engine) ResolveStepDropInterposed(
	conn *sql.DB, stepID int, as, note string, batch bool, nowMS int64,
) error {
	_, err := e.ResolveStepWith(conn, stepID, ResolveOptions{
		As: as, Note: note, Batch: batch, DropInterposed: true, By: testBy, NowMS: nowMS,
	})
	return err
}

func ForceReapStep(conn *sql.DB, stepID int, reason string, nowMS int64) error {
	return ForceReapStepWith(conn, stepID, ForceReapOptions{
		Reason: reason, By: testBy, NowMS: nowMS,
	})
}
