package engine

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func manifestRow(step, stage int, issue string) db.DispatchRow {
	b, _ := json.Marshal(model.StepRow{Issue: issue, Stage: stage})
	return db.DispatchRow{StepID: step, RowJSON: string(b)}
}

// TestChainDeferredRowsSeparatesChainFromBudget: only a pending row behind an
// unsettled earlier stage of its own issue is chain-deferred.
func TestChainDeferredRowsSeparatesChainFromBudget(t *testing.T) {
	steps := []*db.Step{
		{ID: 1, Status: db.StepFailedRouted}, // stage 1, issue A: did not settle
		{ID: 2, Status: db.StepPending},      // stage 2, issue A: chain-deferred
		{ID: 3, Status: db.StepDone},         // stage 1, issue B: settled
		{ID: 4, Status: db.StepPending},      // stage 2, issue B: budget hold, not chain
		{ID: 5, Status: db.StepPending},      // stage 0: unstaged, no ordering
		{ID: 6, Status: db.StepDone},         // stage 2, issue A: already ran
	}
	rows := []db.DispatchRow{
		manifestRow(1, 1, "ISSUE-A"), manifestRow(2, 2, "ISSUE-A"),
		manifestRow(3, 1, "ISSUE-B"), manifestRow(4, 2, "ISSUE-B"),
		manifestRow(5, 0, "ISSUE-A"), manifestRow(6, 2, "ISSUE-A"),
	}
	got, err := chainDeferredRows(rows, steps)
	testsupport.Must(t, err, "chainDeferredRows: %v", err)
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("chain-deferred = %v, want only step 2", got)
	}
}

func chainFacts(t *testing.T, conn *sql.DB, runID int) int {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE run_id = ? AND kind = ?
		    AND json_extract(data, '$.cause') = 'chain'
		    AND json_extract(data, '$.source') = ?`,
		runID, EventStepDeferred, FactSourceEngine).Scan(&n)
	testsupport.Must(t, err, "counting chain facts: %v", err)
	return n
}

// TestCloseRecordsChainDeferralsOnce: closing a dispatch whose staged rows
// never launched records one engine chain fact per such row, with no `run fact
// add`, and recording again for the same dispatch adds none.
func TestCloseRecordsChainDeferralsOnce(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	m := openDispatch(t, conn, runID, 0, nowMS)

	staged := 0
	for _, r := range m.Rows {
		if r.Status == db.StepStaged {
			staged++
		}
	}
	if staged == 0 {
		t.Skip("fixture offers no staged row")
	}

	_, err := NewEngine().CloseDispatch(conn, runID, false, IntegrationSkip{}, nowMS)
	testsupport.Must(t, err, "close: %v", err)
	first := chainFacts(t, conn, runID)
	if first != staged {
		t.Fatalf("%d chain facts after close, want %d (one per staged row)", first, staged)
	}

	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	defer tx.Rollback()
	steps, err := db.ListRunStepsTx(tx, runID)
	testsupport.Must(t, err, "steps: %v", err)
	testsupport.Must(t, recordChainDeferralsTx(tx, runID, dispatchIDOf(t, m.Dispatch), steps, nowMS),
		"replay: %v", err)
	testsupport.Must(t, tx.Commit(), "commit: %v", err)
	if again := chainFacts(t, conn, runID); again != first {
		t.Errorf("replaying the recording left %d chain facts, want %d", again, first)
	}
}

// TestRunFactChainReturnsTheEngineRecord: filing a chain deferral the engine
// already recorded returns that record and writes nothing; budget is unchanged.
func TestRunFactChainReturnsTheEngineRecord(t *testing.T) {
	conn := mustDB(t)
	runID, _, token := runFactRun(t, conn)
	m := openDispatch(t, conn, runID, 0, nowMS)
	_, err := NewEngine().CloseDispatch(conn, runID, false, IntegrationSkip{}, nowMS)
	testsupport.Must(t, err, "close: %v", err)

	var stepID int
	err = conn.QueryRow(
		`SELECT step_id FROM events WHERE run_id = ? AND kind = ? LIMIT 1`,
		runID, EventStepDeferred).Scan(&stepID)
	if err == sql.ErrNoRows {
		t.Skip("fixture offers no staged row: " + m.Dispatch)
	}
	testsupport.Must(t, err, "finding a chain fact: %v", err)

	before := eventKindCount(t, conn, runID, EventStepDeferred)
	fact, err := RecordRunFact(conn, RunFactOptions{
		RunID: runID, Kind: RunFactStepDeferred, StepID: stepID, Cause: DeferralChain,
		Reason: "skipped-chain-dead", By: factBy, Token: token, NowMS: nowMS,
	})
	testsupport.Must(t, err, "run fact add --cause chain: %v", err)
	if fact.Cause != DeferralChain || fact.Seq == 0 {
		t.Errorf("fact = %+v, want the engine's chain record", fact)
	}
	if after := eventKindCount(t, conn, runID, EventStepDeferred); after != before {
		t.Errorf("step-deferred events %d -> %d; the engine record should stand alone", before, after)
	}
}
