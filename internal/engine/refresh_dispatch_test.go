package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-4267: the refresh verbs no longer refuse run-wide while a dispatch is
// open. An issue with nothing in flight refreshes, and the dispatch still
// verifies.

// runEventData is the data of the run's newest event of kind.
func runEventData(t *testing.T, conn *sql.DB, runID int, kind string) string {
	t.Helper()
	var data string
	testsupport.Must(t, conn.QueryRow(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		runID, kind).Scan(&data), "reading the %s event", kind)
	return data
}

// TestRefreshScopeUnderAnOpenDispatchVerifies is the acceptance check: with a
// dispatch open and the issue's step unstarted, a scope refresh succeeds, names
// the dispatch and the re-rendered row, and the open dispatch still verifies.
func TestRefreshScopeUnderAnOpenDispatchVerifies(t *testing.T) {
	conn := mustDB(t)
	runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)
	manifest := openDispatch(t, conn, runID, 0, nowMS)
	widen(t, conn, issue, `["internal/a/**","internal/b/**"]`)

	refreshed, err := refresh(conn, runID, issue)
	testsupport.Must(t, err, "refresh under an open dispatch: %v", err)
	if len(refreshed.To) != 2 {
		t.Errorf("refreshed scope = %v; want the widened pair", refreshed.To)
	}
	assertFrozenScope(t, conn, runID, issue, "internal/a/**", "internal/b/**")

	data := runEventData(t, conn, runID, EventIssueScopeRefreshed)
	for _, want := range []string{manifest.Dispatch, "rerendered_rows", "flaky@0"} {
		if !strings.Contains(data, want) {
			t.Errorf("refresh event %s lacks %q", data, want)
		}
	}

	verify, mismatch, err := NewEngine().VerifyDispatch(conn, runID, nowMS)
	testsupport.Must(t, err, "VerifyDispatch: %v", err)
	if mismatch != nil || !verify.Verified {
		t.Errorf("the open dispatch no longer verifies after the refresh: %+v %+v", verify, mismatch)
	}
}

// TestRefreshUnderAnOpenDispatchStillRefusesAnInFlightStep: the per-issue rule
// holds with a dispatch open.
func TestRefreshUnderAnOpenDispatchStillRefusesAnInFlightStep(t *testing.T) {
	for _, status := range []string{db.StepClaimed, db.StepRunning, db.StepGated} {
		t.Run(status, func(t *testing.T) {
			conn := mustDB(t)
			runID, issue, stepID := scopedIssueInRun(t, conn, `["internal/a/**"]`)
			openDispatch(t, conn, runID, 0, nowMS)
			widen(t, conn, issue, `["internal/a/**","internal/b/**"]`)
			mustExec(t, conn, `UPDATE steps SET status = ? WHERE id = ?`, status, stepID)

			_, err := refresh(conn, runID, issue)
			if code, ok := CodeOf(err); !ok || code != CodeConflict {
				t.Fatalf("refresh with a %s step = %v; want CONFLICT", status, err)
			}
			assertFrozenScope(t, conn, runID, issue, "internal/a/**")
			assertNoRefreshEvent(t, conn, runID)
		})
	}
}

// TestRefreshBodyUnderAnOpenDispatch: the body verb takes the same path, and a
// refresh with no dispatch records no dispatch keys.
func TestRefreshBodyUnderAnOpenDispatch(t *testing.T) {
	conn := mustDB(t)
	runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)
	openDispatch(t, conn, runID, 0, nowMS)
	amend(t, conn, issue, "amended under an open manifest")

	_, err := refreshBody(conn, runID, issue)
	testsupport.Must(t, err, "refresh-body under an open dispatch: %v", err)
	if data := runEventData(t, conn, runID, EventIssueBodyRefreshed); !strings.Contains(data, "rerendered_rows") {
		t.Errorf("body refresh event %s does not name the re-rendered rows", data)
	}

	conn2 := mustDB(t)
	runID2, issue2, _ := scopedIssueInRun(t, conn2, `["internal/a/**"]`)
	amend(t, conn2, issue2, "amended with no dispatch")
	_, err = refreshBody(conn2, runID2, issue2)
	testsupport.Must(t, err, "refresh-body: %v", err)
	if data := runEventData(t, conn2, runID2, EventIssueBodyRefreshed); strings.Contains(data, "dispatch") {
		t.Errorf("a refresh with no open dispatch recorded dispatch keys: %s", data)
	}
}
