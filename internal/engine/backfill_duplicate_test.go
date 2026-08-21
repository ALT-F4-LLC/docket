package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestBackfillSkipsDuplicatesAndReportsThem is DKT-241(a).
//
// The ledger is append-only and a duplicate refusal is right — a merge would
// hide a double-count of real spend. What was wrong was the GRANULARITY: the
// WHOLE batch aborted on the first already-recorded row. Cross-wave duplicates
// are structural (a gate probed in wave N and seated in wave N+1 emits usage
// in both journals), so conductors hand-filtered rows seven times across three
// sessions to get a batch through.
func TestBackfillSkipsDuplicatesAndReportsThem(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)

	// One unit is already recorded — the structural cross-wave duplicate.
	_, err := e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "input_tokens", Quantity: 100},
	}, "", "", nowMS)
	testsupport.Must(t, err, "seeding the duplicate: %v", err)

	batch := []BackfillRow{
		{Step: implID, Unit: "input_tokens", Quantity: 100},
		{Step: implID, Unit: "output_tokens", Quantity: 250},
		{Step: implID, Unit: "cache_read", Quantity: 9000},
	}

	// The default still refuses the whole batch: a duplicate usually means
	// real spend is about to be double-counted.
	if _, err := e.BackfillUsage(conn, runID, batch, "", "", nowMS); err == nil {
		t.Fatal("the default accepted a batch containing a recorded row; " +
			"refusing is still the right default")
	} else if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("refusal code = %v, want CONFLICT", code)
	}

	// And the refusal now points at a verb that can actually answer it.
	_, err = e.BackfillUsage(conn, runID, batch, "", "", nowMS)
	if !strings.Contains(err.Error(), "run report") ||
		!strings.Contains(err.Error(), "step_usage") {
		t.Errorf("the refusal does not name where the recorded rows are "+
			"listed: %v", err)
	}
	if !strings.Contains(err.Error(), "--on-duplicate=skip") {
		t.Errorf("the refusal does not name the way past it: %v", err)
	}

	// Nothing landed from the refused batch: it is still one transaction.
	if got := len(usageRowsFor(t, conn, implID)); got != 1 {
		t.Fatalf("the refused batch left %d ledger rows, want the 1 that "+
			"predated it — the batch is atomic", got)
	}

	// --on-duplicate=skip records the rest and NAMES what it passed over.
	outcome, err := e.BackfillUsage(
		conn, runID, batch, "", OnDuplicateSkip, nowMS)
	testsupport.Must(t, err, "skip mode refused: %v", err)

	if outcome.Written != 2 {
		t.Errorf("wrote %d rows, want the 2 that were not already recorded",
			outcome.Written)
	}
	if len(outcome.Skipped) != 1 {
		t.Fatalf("reported %d skips, want 1", len(outcome.Skipped))
	}
	skip := outcome.Skipped[0]
	if skip.Unit != "input_tokens" || skip.Instance != "implement@0" {
		t.Errorf("the skip names %+v, want the recorded input_tokens row on "+
			"implement@0 — a bare count cannot tell a structural cross-wave "+
			"duplicate from a real double-count", skip)
	}

	// The ledger holds exactly one row per unit: the skip added nothing and
	// overwrote nothing.
	rows := usageRowsFor(t, conn, implID)
	if len(rows) != 3 {
		t.Fatalf("ledger holds %d rows, want 3 (one per unit)", len(rows))
	}
	for _, r := range rows {
		if r.Unit == "input_tokens" && r.Quantity != 100 {
			t.Errorf("the skipped unit's quantity moved to %g; a skip must "+
				"neither add nor overwrite", r.Quantity)
		}
	}

	// A second skip run is a no-op that says so.
	again, err := e.BackfillUsage(conn, runID, batch, "", OnDuplicateSkip, nowMS)
	testsupport.Must(t, err, "second skip run refused: %v", err)
	if again.Written != 0 || len(again.Skipped) != 3 {
		t.Errorf("re-running skip wrote %d and skipped %d, want 0 and 3",
			again.Written, len(again.Skipped))
	}
}

// TestBackfillRejectsAnUnknownOnDuplicate keeps the setting a closed set: a
// typo must not silently fall back to a mode the caller did not ask for.
func TestBackfillRejectsAnUnknownOnDuplicate(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	implID := stepIDByInstance(t, conn, "implement@0")

	_, err := backfillWith(conn, runID, implID, "ignore")
	if err == nil {
		t.Fatal("an unknown --on-duplicate was accepted")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("code = %v, want VALIDATION_ERROR", code)
	}
}

func backfillWith(
	conn *sql.DB, runID, stepID int, onDuplicate string,
) (*BackfillOutcome, error) {
	return NewEngine().BackfillUsage(conn, runID, []BackfillRow{
		{Step: stepID, Unit: "tokens", Quantity: 1},
	}, "", onDuplicate, nowMS)
}

// TestUsageByStepListsTheLedgerRowByRow is DKT-241(b): the refusal's advice
// was impossible to follow, because `run report` exposed per-UNIT totals only
// and no read verb answered "which steps already have usage".
func TestUsageByStepListsTheLedgerRowByRow(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)

	_, err := e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "output_tokens", Quantity: 250},
		{Step: implID, Unit: "input_tokens", Quantity: 100},
	}, "wave-journal", "", nowMS)
	testsupport.Must(t, err, "backfill: %v", err)

	report, err := LoadRunReport(conn, runID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	if len(report.StepUsage) != 2 {
		t.Fatalf("report carries %d per-step usage rows, want 2 — without "+
			"them the back-fill refusal points at a verb that cannot answer "+
			"it (DKT-241)", len(report.StepUsage))
	}
	// Ordered by a total key, so two reports of the same rows agree byte for
	// byte (R9).
	if report.StepUsage[0].Unit != "input_tokens" {
		t.Errorf("rows are not in (step, attempt, unit) order: %+v", report.StepUsage)
	}
	for _, row := range report.StepUsage {
		if row.Instance != "implement@0" {
			t.Errorf("row %+v names no instance; the step id alone is not "+
				"what an operator filtering a batch reads", row)
		}
		if row.Source != "wave-journal" {
			t.Errorf("row %+v lost its source; who measured it is the "+
				"distinction the column exists for", row)
		}
	}

	// The per-unit rollup still answers its own question, unchanged.
	if len(report.Budget.Reported) == 0 {
		t.Error("the per-unit rollup went missing; the detail is beside the " +
			"headline, not instead of it")
	}
}
