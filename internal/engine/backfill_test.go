package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The usage back-fill verb, and the wedge it retires.
//
// docs/tdd/usage-backfill-wedge.md. E-3 (no back-fill verb), E-4 (acceptance
// cannot clear D2), and E-12 (the D2 refusal starves action steps) are one
// defect: usage that was measured had no way into the ledger, so D2 correctly
// and permanently reported a gap the engine gave nobody a way to close.

// completeWithoutUsage claims a step and completes it with NO `--usage`, which
// is what an executor that cannot know its own spend does.
func completeWithoutUsage(t *testing.T, conn *sql.DB, e *Engine, stepID int) {
	t.Helper()
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim %d: %v", stepID, err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("done"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete %d: %v", stepID, err)
}

// usageRowsFor reads the ledger for one step.
func usageRowsFor(t *testing.T, conn *sql.DB, stepID int) []db.UsageRow {
	t.Helper()
	rows, err := conn.Query(
		`SELECT run_id, step_id, attempt, unit, quantity, source
		   FROM usage_ledger WHERE step_id = ? ORDER BY unit`, stepID)
	testsupport.Must(t, err, "reading usage_ledger: %v", err)
	defer rows.Close()
	var out []db.UsageRow
	for rows.Next() {
		var r db.UsageRow
		err := rows.Scan(&r.RunID, &r.StepID, &r.Attempt, &r.Unit,
			&r.Quantity, &r.Source)
		testsupport.Must(t, err, "scanning usage row: %v", err)
		out = append(out, r)
	}
	return out
}

// TestBackfillRetiresTheD2Wedge is §4.1 — RUN-3's exact shape, end to end, and
// the regression that would have caught the run-ending wedge.
//
// An executor completes without `--usage`; `next` refuses forever because D2's
// probe reads `steps.usage_recorded`; the back-fill records what the relay
// measured; `next` answers again. On today's HEAD it fails at the back-fill,
// because the verb does not exist.
func TestBackfillRetiresTheD2Wedge(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)

	// A dispatch must have been opened for D2 to apply at all — the probe is
	// skipped entirely on a run that never dispatched.
	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)

	// Retire the manifest so the open-dispatch refusal is not what we measure:
	// D2 is the subject, and it must be the ONLY thing refusing.
	//
	// ABANDON, not `close --accept-missing-usage`. This line used to close
	// with the acceptance flag and then assert that D2 still stood — E-4, the
	// finding that acceptance cleared the manifest and not the block. DKT-315
	// fixed that: the acceptance now settles the accepted steps, so closing
	// that way would retire the very discrepancy the back-fill is here to
	// retire. `abandon` retires the manifest and accepts nothing, which is
	// exactly the state this test needs.
	_, err := e.AbandonDispatch(conn, runID, "clearing the manifest", nowMS)
	testsupport.Must(t, err, "dispatch abandon: %v", err)

	// D2 stands: the step ran and reported nothing, and nobody has accepted it.
	_, err = e.NextSteps(conn, runID, 0, nowMS)
	if err == nil {
		t.Fatal("`next` answered despite a missing-usage discrepancy; this " +
			"test's premise is that it refuses")
	}
	if !strings.Contains(err.Error(), string(DiscrepancyMissingUsage)) {
		t.Fatalf("the refusal is %q, want one naming %q",
			err, DiscrepancyMissingUsage)
	}

	// E-3: the back-fill carries the relay's measured usage into the ledger.
	_, err = e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "tokens", Quantity: 48211},
	}, "", "", nowMS)
	testsupport.Must(t, err, "backfill-usage: %v", err)

	// E-12: `next` answers, and the action step the refusal was starving is
	// driven in its existing slot — no reordering (§1.5).
	answer, err := e.NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "`next` still refuses after the back-fill: %v — the ledger "+
		"now holds the usage D2 was asking for", err)

	if answer == nil {
		t.Fatal("`next` returned no answer and no error")
	}
}

// TestBackfillUsageSurvivesDispatchAbandon is DKT-98: RUN-6/DISPATCH-58
// abandoned a wave after executors had already burned measured usage, and the
// worry on record was that back-fill "targets a dispatch being CLOSED" — so an
// abandoned dispatch's usage could never reach the ledger.
//
// It does not reproduce. BackfillUsage is keyed on (run, step) alone — it
// never reads dispatch state at all — so usage measured before an abandon
// back-fills exactly as it would after a close. This test pins that so a
// future change to BackfillUsage's scoping cannot silently reintroduce the
// gap retro's cost reports would otherwise under-count again.
func TestBackfillUsageSurvivesDispatchAbandon(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)

	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)

	// The wave is operator-stopped: abandon, not close.
	_, err := e.AbandonDispatch(conn, runID, "operator stopped the wave", nowMS)
	testsupport.Must(t, err, "dispatch abandon: %v", err)

	// The usage the relay measured before the abandon still lands.
	_, err = e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "tokens", Quantity: 141000},
	}, "", "", nowMS)
	testsupport.Must(t, err, "backfill-usage after abandon: %v", err)

	rows := usageRowsFor(t, conn, implID)
	if len(rows) != 1 || rows[0].Quantity != 141000 {
		t.Fatalf("usage_ledger rows for step %d = %+v, want one row of 141000 tokens",
			implID, rows)
	}
	if rows[0].Source != UsageSourceBackfilled {
		t.Errorf("source = %q, want %q", rows[0].Source, UsageSourceBackfilled)
	}
}

// TestBackfillWritesTheLedgerRow pins the row the verb writes, including the
// source that keeps a relay's reconstruction distinguishable from a claimant's
// self-report (§1.3.2).
func TestBackfillWritesTheLedgerRow(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)

	_, err := e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "tokens", Quantity: 1200},
		{Step: implID, Unit: "seconds", Quantity: 42},
	}, "", "", nowMS)
	testsupport.Must(t, err, "backfill-usage: %v", err)

	rows := usageRowsFor(t, conn, implID)
	if len(rows) != 2 {
		t.Fatalf("wrote %d ledger rows, want 2 (one per unit)", len(rows))
	}

	// The step's RECORDED attempt (§1.3.1) — one claim happened, so 1.
	for _, r := range rows {
		if r.Attempt != 1 {
			t.Errorf("unit %q landed at attempt %d, want 1 — the back-fill "+
				"targets the step's recorded attempt", r.Unit, r.Attempt)
		}
		// §1.3.2: NEVER the empty string, which InsertUsageRowTx would default
		// to "reported" — labelling a relay's reconstruction as a claimant's
		// own report and destroying the distinction `source` exists for.
		if r.Source == "" || r.Source == db.UsageSourceReported {
			t.Errorf("unit %q recorded source %q, want the back-fill default "+
				"— an empty source falls through to %q",
				r.Unit, r.Source, db.UsageSourceReported)
		}
	}

	// And the fast path the probe reads is flipped.
	step, err := db.GetStep(conn, implID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if !step.UsageRecorded {
		t.Error("usage_recorded is still false after a back-fill; D2's probe " +
			"reads that column and would keep refusing")
	}
}

// TestBackfillSourceOverride pins that --source overrides the default and is
// carried verbatim — core enumerates no sources (§1.4).
func TestBackfillSourceOverride(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)

	_, err := e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "pages", Quantity: 7},
	}, "ci-runner-timing-log", "", nowMS)
	testsupport.Must(t, err, "backfill-usage: %v", err)

	rows := usageRowsFor(t, conn, implID)
	if len(rows) != 1 || rows[0].Source != "ci-runner-timing-log" {
		t.Fatalf("rows = %+v, want one row with the given source verbatim", rows)
	}
}

// TestBackfillRefusesADoubleBackfill asserts the ledger's own unique key rather
// than working around it: a second back-fill of the same (step, attempt, unit)
// fails loudly, so a re-run after a partial failure cannot silently merge.
func TestBackfillRefusesADoubleBackfill(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)

	rows := []BackfillRow{{Step: implID, Unit: "tokens", Quantity: 10}}
	_, err := e.BackfillUsage(conn, runID, rows, "", "", nowMS)
	testsupport.Must(t, err, "first backfill: %v", err)
	_, err = e.BackfillUsage(conn, runID, rows, "", "", nowMS)
	if err == nil {
		t.Fatal("a second back-fill of the same (step, attempt, unit) " +
			"succeeded; the unique key must refuse it rather than merge")
	}
	// The refusal is PHRASED, not raw SQLite text — §2's rule that a refusal
	// names its way out. A relay re-running after a partial failure reads this.
	if code, ok := CodeOf(err); !ok || code != CodeConflict {
		t.Errorf("the refusal has code %q, want CONFLICT", code)
	}
	for _, want := range []string{"already has", "tokens", "run report"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "UNIQUE constraint") {
		t.Errorf("the refusal leaks raw SQLite text: %q", err)
	}
}

// TestBackfillIsAllOrNothing pins §1.3's one-transaction rule: a batch with one
// bad row writes NOTHING, so a half-applied back-fill cannot leave a dispatch
// that is neither closable nor honestly re-runnable.
func TestBackfillIsAllOrNothing(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)

	// The second row names a step that is not in this run.
	_, err := e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "tokens", Quantity: 99},
		{Step: 99999, Unit: "tokens", Quantity: 1},
	}, "", "", nowMS)
	if err == nil {
		t.Fatal("a batch naming a step outside the run succeeded; it must refuse")
	}
	if rows := usageRowsFor(t, conn, implID); len(rows) != 0 {
		t.Errorf("the good row survived a refused batch: %+v — the whole "+
			"back-fill is one transaction", rows)
	}
	step, err := db.GetStep(conn, implID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.UsageRecorded {
		t.Error("usage_recorded was flipped by a refused batch")
	}
}

// TestBackfillRefusesAStepOfAnotherRun keeps the verb's authority scoped to the
// run named on the command line.
func TestBackfillRefusesAStepOfAnotherRun(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()

	runA := dispatchRun(t, conn)
	openDispatch(t, conn, runA, 0, nowMS)

	// A second run, with its own steps.
	issueB := createIssue(t, conn, "issue B", "body B", "task", nil)
	runB := startRun(t, conn, issueB)
	_, err := activate(conn, runB.ID)
	testsupport.Must(t, err, "activate run B: %v", err)
	var stepOfB int
	err = conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? ORDER BY id LIMIT 1`, runB.ID,
	).Scan(&stepOfB)
	testsupport.Must(t, err, "finding a step of run B: %v", err)

	if _, err := e.BackfillUsage(conn, runA, []BackfillRow{
		{Step: stepOfB, Unit: "tokens", Quantity: 5},
	}, "", "", nowMS); err == nil {
		t.Error("back-filling run A over a step of run B succeeded; the verb " +
			"must refuse a step the named run does not own")
	}
}

// ---- DKT-993: the never-claimed target -------------------------------------
//
// Harness RUN-66's conductor back-filled a wave journal whose join heuristic had
// misattributed probe-agent tokens. The rows landed on STEP-3146 (`design-qa@1`,
// `pending`, never claimed) and on a superseded step, and the engine took them
// in silence — so `run report` showed spend on steps that never ran, with
// nothing in the store saying where it came from.
//
// THE CONTRACT IS WARN-AND-RECORD. The row still lands (core cannot know that a
// step which never claimed cost nothing, and enumerating which steps may have
// cost would be core holding an opinion about how a relay measures its own
// work); what ends is the silence.

// TestBackfillNamesANeverClaimedTarget is the acceptance case verbatim: a row
// against a `pending`, never-claimed step is REPORTED, naming the step and its
// status — and is still recorded.
func TestBackfillNamesANeverClaimedTarget(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)

	// `verify@0` is downstream of everything and has never been offered, let
	// alone claimed — RUN-66's `design-qa@1` shape. The premise is ASSERTED
	// rather than assumed: if the fixture ever pre-claims this step, the test
	// must fail here and not silently stop testing anything.
	verifyID := stepIDByInstance(t, conn, "verify@0")
	step, err := db.GetStep(conn, verifyID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepPending || step.Attempt != 0 {
		t.Fatalf("premise: verify@0 is %q at attempt %d, want %q at attempt 0",
			step.Status, step.Attempt, db.StepPending)
	}

	out, err := e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: verifyID, Unit: "cache_creation", Quantity: 8724},
	}, "wave-journal:wf_68b7e3e1-abe", "", nowMS)
	testsupport.Must(t, err, "backfill-usage: %v", err)

	// Reported, by name and by status.
	if len(out.Unclaimed) != 1 {
		t.Fatalf("Unclaimed = %+v, want exactly one entry — the row landed on a "+
			"step no worker ever claimed and the engine must say so", out.Unclaimed)
	}
	got := out.Unclaimed[0]
	if got.Step != model.FormatStepID(verifyID) {
		t.Errorf("the advisory names step %q, want %q — an instance repeats "+
			"across issues and cannot be acted on alone",
			got.Step, model.FormatStepID(verifyID))
	}
	if got.Instance != "verify@0" {
		t.Errorf("the advisory names instance %q, want %q", got.Instance, "verify@0")
	}
	if got.Status != db.StepPending {
		t.Errorf("the advisory reports status %q, want %q — the status is the "+
			"whole diagnostic: it says the step has not run YET",
			got.Status, db.StepPending)
	}

	// And the row IS recorded: warn-and-record, not reject.
	if out.Written != 1 {
		t.Errorf("Written = %d, want 1 — the advisory does not withhold the row",
			out.Written)
	}
	rows := usageRowsFor(t, conn, verifyID)
	if len(rows) != 1 || rows[0].Quantity != 8724 {
		t.Fatalf("usage_ledger rows = %+v, want the one 8724 row; the contract "+
			"is warn-and-record", rows)
	}
	if rows[0].Source != "wave-journal:wf_68b7e3e1-abe" {
		t.Errorf("source = %q, want the given source verbatim", rows[0].Source)
	}
}

// TestBackfillIsQuietForAClaimedTarget is the "existing valid back-fills
// unchanged" half. The verb exists for a step that RAN and could not report its
// own spend; that step must never draw the advisory.
func TestBackfillIsQuietForAClaimedTarget(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)

	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)

	out, err := e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "tokens", Quantity: 48211},
	}, "", "", nowMS)
	testsupport.Must(t, err, "backfill-usage: %v", err)

	if len(out.Unclaimed) != 0 {
		t.Fatalf("Unclaimed = %+v on a claimed, completed step, want none — a "+
			"warning on the verb's own reason for existing is noise that "+
			"teaches conductors to ignore it", out.Unclaimed)
	}
}

// TestBackfillNamesASupersededSweptTarget covers the evidence's second step
// (STEP-3158, `verify-tribunal@1`, superseded).
//
// The loop's supersede sweep takes `pending` instances ONLY, so a swept step is
// a never-claimed step: it warns by the same clause, and its `superseded` status
// rides the advisory so an operator can tell it from a step merely waiting.
func TestBackfillNamesASupersededSweptTarget(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	// A real fix-loop entry, through the engine: `commit@0` is downstream of
	// `after_loop` and unclaimed, so the sweep supersedes it.
	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)

	commitID := stepIDByInstance(t, conn, "commit@0")
	step, err := db.GetStep(conn, commitID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepSuperseded || step.Attempt != 0 {
		t.Fatalf("premise: commit@0 is %q at attempt %d, want %q at attempt 0",
			step.Status, step.Attempt, db.StepSuperseded)
	}

	out, err := e.BackfillUsage(conn, run.ID, []BackfillRow{
		{Step: commitID, Unit: "cache_read", Quantity: 8336},
	}, "", "", nowMS)
	testsupport.Must(t, err, "backfill-usage: %v", err)

	if len(out.Unclaimed) != 1 || out.Unclaimed[0].Status != db.StepSuperseded {
		t.Fatalf("Unclaimed = %+v, want one entry reporting %q",
			out.Unclaimed, db.StepSuperseded)
	}
	if rows := usageRowsFor(t, conn, commitID); len(rows) != 1 {
		t.Errorf("usage_ledger rows = %+v, want the one row; the contract is "+
			"warn-and-record", rows)
	}
}

// TestBackfillIsQuietForASupersededStepThatRan is the discrimination the status
// alone could not make, and the reason the predicate is the CLAIM.
//
// `step resolve --as fix-round` supersedes the step it authorizes a round for —
// a step that claimed, ran, and spent. Its usage is real, and back-filling it is
// D2's own documented resolution, so warning on `superseded` as a status would
// have fired on the legitimate path.
func TestBackfillIsQuietForASupersededStepThatRan(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unverifiablePayload)

	verifyID := stepIDByInstance(t, conn, "verify@0")
	testsupport.Must(t, e.ResolveStep(conn, verifyID, ResolveFixRound,
		"one more round", nowMS), "resolve --as fix-round: %v", nil)

	step, err := db.GetStep(conn, verifyID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepSuperseded || step.Attempt == 0 {
		t.Fatalf("premise: verify@0 is %q at attempt %d, want %q at a nonzero "+
			"attempt", step.Status, step.Attempt, db.StepSuperseded)
	}

	out, err := e.BackfillUsage(conn, run.ID, []BackfillRow{
		{Step: verifyID, Unit: "tokens", Quantity: 1200},
	}, "", "", nowMS)
	testsupport.Must(t, err, "backfill-usage: %v", err)

	if len(out.Unclaimed) != 0 {
		t.Fatalf("Unclaimed = %+v for a superseded step that CLAIMED and ran, "+
			"want none — its spend is real and this is D2's own way out",
			out.Unclaimed)
	}
}

// TestBackfillNamesEachUnclaimedTargetOnce pins the advisory's shape on a batch:
// one entry per STEP (not per row), in the order the batch named them, so two
// identical batches produce identical output.
func TestBackfillNamesEachUnclaimedTargetOnce(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)

	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)
	verifyID := stepIDByInstance(t, conn, "verify@0")

	out, err := e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: verifyID, Unit: "input", Quantity: 18},
		{Step: implID, Unit: "tokens", Quantity: 4000},
		{Step: verifyID, Unit: "output", Quantity: 7},
	}, "", "", nowMS)
	testsupport.Must(t, err, "backfill-usage: %v", err)

	if len(out.Unclaimed) != 1 || out.Unclaimed[0].Instance != "verify@0" {
		t.Fatalf("Unclaimed = %+v, want exactly one entry for verify@0 — two "+
			"rows against one step are one problem", out.Unclaimed)
	}
	if out.Written != 3 {
		t.Errorf("Written = %d, want 3", out.Written)
	}
}

// TestBackfillLeavesOtherStepsRefusing is the other half of §4.2's last bullet:
// D2 goes quiet for exactly the back-filled steps, not for the run at large.
func TestBackfillLeavesOtherStepsRefusing(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)

	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)
	_, err := e.CloseDispatch(conn, runID, true, "", nowMS)
	testsupport.Must(t, err, "close: %v", err)

	// Back-fill implement, then complete ANOTHER step without usage. The run
	// must refuse again, naming the new step and not the back-filled one.
	_, err = e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "tokens", Quantity: 1},
	}, "", "", nowMS)
	testsupport.Must(t, err, "backfill: %v", err)
	_, err = e.NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "`next` refuses after the only gap was back-filled: %v", err)
}
