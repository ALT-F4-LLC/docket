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
// attempted, unsettled earlier stage of its own issue is chain-deferred.
func TestChainDeferredRowsSeparatesChainFromBudget(t *testing.T) {
	steps := []*db.Step{
		{ID: 1, Status: db.StepFailedRouted}, // stage 1, issue A: attempted, did not settle
		{ID: 2, Status: db.StepPending},      // stage 2, issue A: chain-deferred
		{ID: 3, Status: db.StepPending},      // stage 1, issue B: never attempted (budget hold)
		{ID: 4, Status: db.StepPending},      // stage 2, issue B: not chain
		{ID: 5, Status: db.StepPending},      // stage 0: unstaged, no ordering
		{ID: 6, Status: db.StepDone},         // stage 2, issue A: already ran
		{ID: 7, Status: db.StepDone},         // stage 1, issue C: settled
		{ID: 8, Status: db.StepPending},      // stage 2, issue C: not chain
	}
	rows := []db.DispatchRow{
		manifestRow(1, 1, "ISSUE-A"), manifestRow(2, 2, "ISSUE-A"),
		manifestRow(3, 1, "ISSUE-B"), manifestRow(4, 2, "ISSUE-B"),
		manifestRow(5, 0, "ISSUE-A"), manifestRow(6, 2, "ISSUE-A"),
		manifestRow(7, 1, "ISSUE-C"), manifestRow(8, 2, "ISSUE-C"),
	}
	attempted := map[int]bool{1: true, 7: true}
	got, err := chainDeferredRows(rows, steps, attempted)
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

// failFirstStage claims and fails the lowest-staged ready row of the manifest,
// the shape of a wave whose first stage fails, and returns how many staged
// rows the manifest holds behind it.
func failFirstStage(t *testing.T, conn *sql.DB, m *Manifest) (stepID, staged int) {
	t.Helper()
	for _, r := range m.Rows {
		if r.Status == db.StepStaged {
			staged++
		} else if stepID == 0 {
			id, err := model.ParseStepID(r.Step)
			testsupport.Must(t, err, "parsing %q: %v", r.Step, err)
			stepID = id
		}
	}
	if stepID == 0 || staged == 0 {
		t.Skip("fixture offers no ready row with staged rows behind it")
	}
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	testsupport.Must(t, NewEngine().FailStep(conn, stepID, claim.Token, "failed", "", nowMS),
		"fail: %v", err)
	return stepID, staged
}

// TestCloseRecordsNoChainFactWhenNothingLaunched: a manifest closed with no
// row attempted is a budget or park hold, so the engine records no chain fact.
func TestCloseRecordsNoChainFactWhenNothingLaunched(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	_, err := NewEngine().CloseDispatch(conn, runID, false, IntegrationSkip{}, nowMS)
	testsupport.Must(t, err, "close: %v", err)
	if n := chainFacts(t, conn, runID); n != 0 {
		t.Errorf("%d chain facts after a close that launched nothing, want 0", n)
	}
}

// TestCloseRecordsChainDeferralsOnce: closing a dispatch whose first stage
// failed records one engine chain fact per staged row behind it, with no `run
// fact add`, and recording again for the same dispatch adds none.
func TestCloseRecordsChainDeferralsOnce(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	m := openDispatch(t, conn, runID, 0, nowMS)
	_, staged := failFirstStage(t, conn, m)

	_, err := NewEngine().CloseDispatch(conn, runID, false, IntegrationSkip{}, nowMS)
	testsupport.Must(t, err, "close: %v", err)
	first := chainFacts(t, conn, runID)
	if first == 0 || first > staged {
		t.Fatalf("%d chain facts after close, want between 1 and %d", first, staged)
	}

	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	defer tx.Rollback()
	closed, err := db.GetDispatchTx(tx, dispatchIDOf(t, m.Dispatch))
	testsupport.Must(t, err, "dispatch: %v", err)
	steps, err := db.ListRunStepsTx(tx, runID)
	testsupport.Must(t, err, "steps: %v", err)
	testsupport.Must(t, recordChainDeferralsTx(tx, runID, closed, steps, nowMS), "replay: %v", err)
	testsupport.Must(t, tx.Commit(), "commit: %v", err)
	if again := chainFacts(t, conn, runID); again != first {
		t.Errorf("replaying the recording left %d chain facts, want %d", again, first)
	}
}

func chainFactStep(t *testing.T, conn *sql.DB, runID int) int {
	t.Helper()
	var stepID int
	err := conn.QueryRow(
		`SELECT step_id FROM events WHERE run_id = ? AND kind = ? LIMIT 1`,
		runID, EventStepDeferred).Scan(&stepID)
	testsupport.Must(t, err, "finding a chain fact: %v", err)
	return stepID
}

// TestRunFactChainReturnsTheEngineRecord: filing a chain deferral the latest
// closed dispatch already recorded returns that record and writes nothing.
func TestRunFactChainReturnsTheEngineRecord(t *testing.T) {
	conn := mustDB(t)
	runID, _, token := runFactRun(t, conn)
	m := openDispatch(t, conn, runID, 0, nowMS)
	failFirstStage(t, conn, m)
	_, err := NewEngine().CloseDispatch(conn, runID, false, IntegrationSkip{}, nowMS)
	testsupport.Must(t, err, "close: %v", err)
	stepID := chainFactStep(t, conn, runID)

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

// TestRunFactChainWritesBehindAPredecessorNeverClaimed: a stage-1 row skipped
// before its claim leaves no event, so the engine records no chain fact behind
// it and the conductor's filing must be written.
func TestRunFactChainWritesBehindAPredecessorNeverClaimed(t *testing.T) {
	conn := mustDB(t)
	runID, _, token := runFactRun(t, conn)
	m := openDispatch(t, conn, runID, 0, nowMS)
	var stagedID int
	for _, r := range m.Rows {
		if r.Status == db.StepStaged {
			id, err := model.ParseStepID(r.Step)
			testsupport.Must(t, err, "parsing %q: %v", r.Step, err)
			stagedID = id
			break
		}
	}
	if stagedID == 0 {
		t.Skip("fixture offers no staged row")
	}
	_, err := NewEngine().CloseDispatch(conn, runID, false, IntegrationSkip{}, nowMS)
	testsupport.Must(t, err, "close: %v", err)
	if n := chainFacts(t, conn, runID); n != 0 {
		t.Fatalf("%d engine chain facts with no predecessor attempted, want 0", n)
	}

	_, err = RecordRunFact(conn, RunFactOptions{
		RunID: runID, Kind: RunFactStepDeferred, StepID: stagedID, Cause: DeferralChain,
		Reason: "skipped-chain-dead", By: factBy, Token: token, NowMS: nowMS + 1,
	})
	testsupport.Must(t, err, "run fact add --cause chain: %v", err)
	if n := eventKindCount(t, conn, runID, EventStepDeferred); n != 1 {
		t.Errorf("%d step-deferred events, want the conductor's one", n)
	}
}

// TestRunFactChainWritesWhenALaterDispatchHasNoEngineRecord: an engine fact
// from an earlier dispatch does not cover a conductor filing for a later one.
func TestRunFactChainWritesWhenALaterDispatchHasNoEngineRecord(t *testing.T) {
	conn := mustDB(t)
	runID, _, token := runFactRun(t, conn)
	m := openDispatch(t, conn, runID, 0, nowMS)
	failFirstStage(t, conn, m)
	_, err := NewEngine().CloseDispatch(conn, runID, false, IntegrationSkip{}, nowMS)
	testsupport.Must(t, err, "first close: %v", err)
	stepID := chainFactStep(t, conn, runID)

	// A second dispatch that launches nothing: the engine records no fact for it.
	openDispatch(t, conn, runID, 0, nowMS+1)
	_, err = NewEngine().CloseDispatch(conn, runID, false, IntegrationSkip{}, nowMS+1)
	testsupport.Must(t, err, "second close: %v", err)

	before := eventKindCount(t, conn, runID, EventStepDeferred)
	_, err = RecordRunFact(conn, RunFactOptions{
		RunID: runID, Kind: RunFactStepDeferred, StepID: stepID, Cause: DeferralChain,
		Reason: "skipped-chain-dead", By: factBy, Token: token, NowMS: nowMS + 2,
	})
	testsupport.Must(t, err, "run fact add --cause chain: %v", err)
	if after := eventKindCount(t, conn, runID, EventStepDeferred); after != before+1 {
		t.Errorf("step-deferred events %d -> %d; the later dispatch's filing must be written",
			before, after)
	}
}
