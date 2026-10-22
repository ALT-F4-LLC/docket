package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"

	"github.com/ALT-F4-LLC/docket/internal/db"
)

// REFRESHING AN ISSUE UNDER AN OPEN DISPATCH (DKT-4267).
//
// `run refresh-scope`, `refresh-files` and `refresh-body` each refused run-wide
// while any dispatch was open. A ruled amendment for one parked issue waited
// ninety-three minutes behind rolling relaunches that kept some other issue's
// dispatch open.
//
// The open dispatch is no reason to hold the refresh when the issue it targets
// has nothing in flight. refreshableSteps already refuses while a step of THE
// ISSUE is claimed, running, or gated, so what remains in the manifest for it is
// rows with nothing in flight (pending, waiting-human, or already finished):
// those still pending will render, at claim, the amended packet, which is the
// point of the ruling. Verify compares scheduling fields, never packet content,
// and ignores the scope the offer read, so a body or files refresh still
// verifies. A widened scope does not always: it feeds the scope-conflict
// readiness rule, so a holder outside the manifest that overlaps it makes the
// row a verify mismatch until the holder records. The refresh event names the dispatch and the rows it re-rendered, so
// the record shows which offered rows now claim an amended packet.

// refreshUnderDispatchTx describes the open dispatch's rows of one issue for a
// refresh event: the dispatch id and the instances re-rendered. It returns nil
// when no dispatch is open, or the issue has no row in it.
func refreshUnderDispatchTx(tx *sql.Tx, runID, issueID int) (map[string]any, error) {
	open, err := db.OpenDispatchTx(tx, runID)
	if errors.Is(err, db.ErrNoOpenDispatch) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(
		`SELECT dr.instance
		   FROM dispatch_rows dr JOIN steps s ON s.id = dr.step_id
		  WHERE dr.dispatch_id = ? AND s.issue_id = ?
		  ORDER BY dr.position`, open.ID, issueID)
	if err != nil {
		return nil, fmt.Errorf("reading %s's rows for the issue: %w", FormatDispatchID(open.ID), err)
	}
	instances, err := scanTxRows(rows, func(r *sql.Rows) (string, error) {
		var s string
		return s, r.Scan(&s)
	})
	if err != nil {
		return nil, err
	}
	if len(instances) == 0 {
		return nil, nil
	}
	return map[string]any{
		"dispatch": FormatDispatchID(open.ID), "rerendered_rows": instances,
	}, nil
}

// withDispatchRows adds the refresh-under-dispatch keys to an event's data.
func withDispatchRows(data, under map[string]any) map[string]any {
	maps.Copy(data, under)
	return data
}
