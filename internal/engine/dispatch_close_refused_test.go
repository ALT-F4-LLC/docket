package engine

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// closeRefusedEvents decodes a run's dispatch-close-refused events.
func closeRefusedEvents(t *testing.T, conn *sql.DB, runID int) []map[string]any {
	t.Helper()
	page, err := ListEvents(conn, EventQuery{RunID: runID, Kind: EventDispatchCloseRefused})
	testsupport.Must(t, err, "ListEvents: %v", err)
	var out []map[string]any
	for _, e := range page.Events {
		var data map[string]any
		testsupport.Must(t, json.Unmarshal(e.Data, &data), "decoding: %v", nil)
		out = append(out, data)
	}
	return out
}

// TestDispatchCloseRefusalsWriteAnEvent is DKT-2758: each of the five CONFLICT
// returns from CloseDispatch writes exactly one dispatch-close-refused event
// carrying the refusal reason and, when one was open, the dispatch.
func TestDispatchCloseRefusalsWriteAnEvent(t *testing.T) {
	cases := []struct {
		name string
		// setup returns the engine, store, run, whether a dispatch is open,
		// and the close's own arguments.
		setup func(t *testing.T) (e *Engine, conn *sql.DB, runID int, open bool, accept bool, at int64)
	}{
		{"unintegrated commits", func(t *testing.T) (*Engine, *sql.DB, int, bool, bool, int64) {
			conn, e, runID := integrationFixture(t, "deadbeef00", "/worktrees/wf-implement")
			e.IsAncestorFn = func(_, _ string) (bool, bool) { return false, true }
			e.PatchContainedFn = func(_, _ string) (bool, bool) { return false, true }
			openDispatch(t, conn, runID, 0, nowMS)
			return e, conn, runID, true, true, nowMS
		}},
		{"no open dispatch", func(t *testing.T) (*Engine, *sql.DB, int, bool, bool, int64) {
			conn := mustDB(t)
			return NewEngine(), conn, dispatchRun(t, conn), false, false, nowMS
		}},
		{"unreconciled discrepancies", func(t *testing.T) (*Engine, *sql.DB, int, bool, bool, int64) {
			conn := mustDB(t)
			runID := dispatchRun(t, conn)
			manifest := openDispatch(t, conn, runID, 0, nowMS)
			past := claimPastGraceWithLiveLease(t, conn, manifest.Rows[0].Instance)
			return NewEngine(), conn, runID, true, false, past
		}},
		{"nothing to accept", func(t *testing.T) (*Engine, *sql.DB, int, bool, bool, int64) {
			conn := mustDB(t)
			return NewEngine(), conn, dispatchRun(t, conn), false, true, nowMS
		}},
		{"lost close race", func(t *testing.T) (*Engine, *sql.DB, int, bool, bool, int64) {
			conn := mustDB(t)
			runID := dispatchRun(t, conn)
			openDispatch(t, conn, runID, 0, nowMS)
			e := NewEngine()
			e.beforeCloseCAS = func(tx *sql.Tx, id int) {
				_, err := tx.Exec(`UPDATE dispatches SET status = 'abandoned', close_reason = 'expired' WHERE id = ?`, id)
				testsupport.Must(t, err, "moving the dispatch first: %v", err)
			}
			return e, conn, runID, true, false, nowMS
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, conn, runID, open, accept, at := c.setup(t)
			before := len(closeRefusedEvents(t, conn, runID))

			_, err := e.CloseDispatch(conn, runID, accept, IntegrationSkip{}, at)
			if code, ok := CodeOf(err); !ok || code != CodeConflict {
				t.Fatalf("CloseDispatch err = %v, want CONFLICT", err)
			}

			events := closeRefusedEvents(t, conn, runID)
			if len(events)-before != 1 {
				t.Fatalf("%d dispatch-close-refused events written, want exactly 1", len(events)-before)
			}
			got := events[len(events)-1]
			if got["reason"] != err.Error() {
				t.Errorf("event reason = %v, want the refusal %q", got["reason"], err.Error())
			}
			if _, named := got["dispatch"]; named != open {
				t.Errorf("event names a dispatch = %v, want %v (data %v)", named, open, got)
			}
		})
	}
}
