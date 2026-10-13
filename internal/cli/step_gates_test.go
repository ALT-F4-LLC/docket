package cli

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// TestStepGatesListsRecordedResults pins the DKT-104 read path end to end:
// rows recorded by the saga's writer come back through the verb's own reader
// and renderer with verdict, exit, duration, reason, and output tail intact.
// Before this verb existed the rows were stored complete but unreachable —
// diagnosing a parked gate meant re-running it out-of-band.
func TestStepGatesListsRecordedResults(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedRunForNext(t, conn)

	exitZero, exitOne := 0, 1
	longOutput := strings.Repeat("early line\n", 20) + "final line"
	rows := []db.GateResultRow{
		{RunID: runID, StepID: 1, Gate: "vet", Ordinal: 0, Argv: []string{"go", "vet"},
			Exit: &exitZero, DurationMS: 1200, Output: "ok", Verdict: db.GateVerdictPass,
			Pre: true, CreatedAtMS: model.NowMS()},
		{RunID: runID, StepID: 1, Gate: "test", Ordinal: 0, Argv: []string{"go", "test"},
			Exit: &exitOne, DurationMS: 8000, Output: longOutput,
			Verdict: db.GateVerdictFail, CreatedAtMS: model.NowMS()},
		{RunID: runID, StepID: 1, Gate: "vuln-scan", Ordinal: 0,
			Verdict: db.GateVerdictUnmatched, Reason: "no trust entry matched the argv",
			CreatedAtMS: model.NowMS()},
	}
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	for _, r := range rows {
		testsupport.Must(t, db.InsertGateResultTx(tx, r), "InsertGateResultTx: %v", err)
	}
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)

	got, err := db.GateResultsForStep(conn, 1)
	testsupport.Must(t, err, "GateResultsForStep: %v", err)
	if len(got) != 3 {
		t.Fatalf("read back %d rows, want 3", len(got))
	}

	rendered := render.RenderStepGates("STEP-1", gateRows(got))

	// The table names every gate with its verdict.
	for _, needle := range []string{"vet", "pass", "test", "fail", "vuln-scan", "unmatched"} {
		if !strings.Contains(rendered, needle) {
			t.Errorf("rendered output does not contain %q:\n%s", needle, rendered)
		}
	}
	// An unmatched gate's reason must surface — it is the field DKT-104 was
	// opened about — and its NULL exit must not render as a code.
	if !strings.Contains(rendered, "no trust entry matched the argv") {
		t.Errorf("the unmatched reason is missing:\n%s", rendered)
	}
	// The failing gate's output tail keeps the end, drops the front, and says
	// where the rest lives.
	if !strings.Contains(rendered, "final line") {
		t.Errorf("the failing gate's output tail is missing:\n%s", rendered)
	}
	if !strings.Contains(rendered, "full output in --json") {
		t.Errorf("the truncation marker is missing:\n%s", rendered)
	}
	// A passing gate does not dump its output.
	if strings.Count(rendered, "ok") > 1 {
		t.Errorf("the passing gate's output leaked into the detail section:\n%s", rendered)
	}
}

// TestStepGatesEmptyAndMissing pins the boundary the artifacts verb draws:
// a step with no recorded gates lists nothing and succeeds, a step that does
// not exist is NOT_FOUND — "no gate results" must never read as a fact about
// a step that is not there.
func TestStepGatesEmptyAndMissing(t *testing.T) {
	conn := newTestDB(t)
	activatedRunForNext(t, conn)

	cmd := cmdWithDB(conn)
	if err := stepGatesCmd.RunE(cmd, []string{"STEP-1"}); err != nil {
		t.Errorf("step gates on a gateless step failed: %v", err)
	}

	err := stepGatesCmd.RunE(cmdWithDB(conn), []string{"STEP-9999"})
	if err == nil {
		t.Fatal("step gates on a missing step succeeded")
	}
	var ce *CmdError
	if !asCmdError(err, &ce) || ce.Code != output.ErrNotFound {
		t.Errorf("error = %v, want NOT_FOUND", err)
	}
}

// TestGateOutputTail pins the tail helper: short output passes through
// untouched, long output keeps the last lines and counts the dropped ones.
func TestGateOutputTail(t *testing.T) {
	if got := render.GateOutputTail("one\ntwo\n"); got != "one\ntwo" {
		t.Errorf("short output = %q, want it verbatim minus the trailing newline", got)
	}
	if got := render.GateOutputTail(""); got != "" {
		t.Errorf("empty output = %q, want empty", got)
	}
	long := render.GateOutputTail(strings.Repeat("x\n", 30) + "last")
	if !strings.HasSuffix(long, "last") {
		t.Errorf("tail %q does not end with the final line", long)
	}
	if !strings.Contains(long, "21 earlier line(s)") {
		t.Errorf("tail %q does not count the dropped lines", long)
	}
}

// TestStubEntryRoundTripsAndRendersAsHollow is DKT-265's storage and read half:
// the marker survives the write, comes back off the row, and reaches the FLAGS
// column a reviewer actually reads.
//
// The interesting row is the PASS. A hollow fail is already a fail; a hollow
// pass is the row that reads as assurance and is not, which is the whole of
// RUN-17 and RUN-19.
func TestStubEntryRoundTripsAndRendersAsHollow(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedRunForNext(t, conn)

	exitZero := 0
	rows := []db.GateResultRow{
		{RunID: runID, StepID: 1, Gate: "secret-scan", Ordinal: 0,
			Argv: []string{"/usr/bin/true"}, Exit: &exitZero, DurationMS: 3,
			Verdict: db.GateVerdictPass, StubEntry: true, CreatedAtMS: model.NowMS()},
		{RunID: runID, StepID: 1, Gate: "vet", Ordinal: 0,
			Argv: []string{"go", "vet"}, Exit: &exitZero, DurationMS: 1200,
			Verdict: db.GateVerdictPass, CreatedAtMS: model.NowMS()},
	}
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	for _, r := range rows {
		testsupport.Must(t, db.InsertGateResultTx(tx, r), "InsertGateResultTx: %v", err)
	}
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)

	got, err := db.GateResultsForStep(conn, 1)
	testsupport.Must(t, err, "GateResultsForStep: %v", err)
	if len(got) != 2 {
		t.Fatalf("read back %d rows, want 2", len(got))
	}

	byGate := map[string]db.GateResultRow{}
	for _, r := range got {
		byGate[r.Gate] = r
	}
	if !byGate["secret-scan"].StubEntry {
		t.Error("the stub marker did not survive the round trip; the column is " +
			"what every downstream reader consults")
	}
	if byGate["vet"].StubEntry {
		t.Error("a gate whose entry declared no stub came back marked; the " +
			"marker would then say nothing about any row")
	}

	// The FLAGS column is where a reviewer scanning a step's gates sees it.
	// Both rows say `pass`, and the whole point is that they must not read
	// alike.
	rendered := render.RenderStepGates("STEP-1", gateRows(got))
	var scanLine, vetLine string
	for _, line := range strings.Split(rendered, "\n") {
		switch {
		case strings.HasPrefix(line, "secret-scan"):
			scanLine = line
		case strings.HasPrefix(line, "vet"):
			vetLine = line
		}
	}
	if !strings.Contains(scanLine, "stub") {
		t.Errorf("the hollow gate's row does not carry the stub flag:\n%s", scanLine)
	}
	if strings.Contains(vetLine, "stub") {
		t.Errorf("the real gate's row carries a stub flag:\n%s", vetLine)
	}
}

// TestGateRollupCountsStubsBesideThePasses is the run-report half.
//
// `secret-scan: pass 1` is the sentence a reviewer converts into "a secret scan
// happened", so the interception has to be beside that number rather than a
// column away. The rollup's `Stub` OVERLAPS pass/fail/unmatched rather than
// partitioning with them — it counts rows, so `pass 2, stub 2` means both
// passes were hollow.
func TestGateRollupCountsStubsBesideThePasses(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedRunForNext(t, conn)

	exitZero := 0
	rows := []db.GateResultRow{
		{RunID: runID, StepID: 1, Gate: "secret-scan", Ordinal: 0, Exit: &exitZero,
			Verdict: db.GateVerdictPass, StubEntry: true, CreatedAtMS: model.NowMS()},
		{RunID: runID, StepID: 1, Gate: "secret-scan", Ordinal: 1, Exit: &exitZero,
			Verdict: db.GateVerdictPass, StubEntry: true, CreatedAtMS: model.NowMS()},
		{RunID: runID, StepID: 1, Gate: "vet", Ordinal: 0, Exit: &exitZero,
			Verdict: db.GateVerdictPass, CreatedAtMS: model.NowMS()},
	}
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	for _, r := range rows {
		testsupport.Must(t, db.InsertGateResultTx(tx, r), "InsertGateResultTx: %v", err)
	}
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)

	counts, err := db.GateRollup(conn, runID)
	testsupport.Must(t, err, "GateRollup: %v", err)

	byName := map[string]db.VerdictCount{}
	for _, c := range counts {
		byName[c.Name] = c
	}
	if got := byName["secret-scan"]; got.Pass != 2 || got.Stub != 2 {
		t.Errorf("secret-scan rolled up as pass %d stub %d, want pass 2 stub 2 — "+
			"a gate whose every pass was hollow must say so where the pass count is",
			got.Pass, got.Stub)
	}
	if got := byName["vet"]; got.Pass != 1 || got.Stub != 0 {
		t.Errorf("vet rolled up as pass %d stub %d, want pass 1 stub 0", got.Pass, got.Stub)
	}

	// Actions have no stub declaration to carry, and the shared rollup body
	// must return an honest zero for them rather than failing on a column that
	// table does not have.
	if _, err := db.ActionRollup(conn, runID); err != nil {
		t.Errorf("ActionRollup failed after the gate rollup gained a stub "+
			"column: %v", err)
	}
}

// TestSkippedIsAVisibleRollupColumn is DKT-254's third defect.
//
// verdictRollup counted pass, fail, and unmatched only, so a `skipped` row
// landed in NO column and disappeared from the run report's gates section
// entirely. gate_exec.go is explicit that skipped means nothing ran — and a
// verdict meaning "no evidence was collected" rendering as an absence reads as
// green, which is the exact inversion of what it means.
func TestSkippedIsAVisibleRollupColumn(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedRunForNext(t, conn)

	exitZero := 0
	rows := []db.GateResultRow{
		{RunID: runID, StepID: 1, Gate: "ac-commands", Ordinal: 0,
			Verdict:     db.GateVerdictSkipped,
			Reason:      "the tree under review could not be bound",
			CreatedAtMS: model.NowMS()},
		{RunID: runID, StepID: 1, Gate: "tests", Ordinal: 0, Exit: &exitZero,
			Verdict: db.GateVerdictPass, CreatedAtMS: model.NowMS()},
	}
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	for _, r := range rows {
		testsupport.Must(t, db.InsertGateResultTx(tx, r), "InsertGateResultTx: %v", err)
	}
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)

	counts, err := db.GateRollup(conn, runID)
	testsupport.Must(t, err, "GateRollup: %v", err)

	byName := map[string]db.VerdictCount{}
	for _, c := range counts {
		byName[c.Name] = c
	}

	ac := byName["ac-commands"]
	if ac.Skipped != 1 {
		t.Errorf("ac-commands rolled up as skipped %d, want 1 — before this "+
			"column the row landed nowhere and vanished from the report", ac.Skipped)
	}
	// The four verdicts PARTITION: a skipped row must not also be counted as a
	// pass or a fail, or the section's numbers stop adding up.
	if ac.Pass != 0 || ac.Fail != 0 || ac.Unmatched != 0 {
		t.Errorf("ac-commands rolled up as pass %d fail %d unmatched %d beside "+
			"its skip; the four verdicts partition", ac.Pass, ac.Fail, ac.Unmatched)
	}
	if got := byName["tests"]; got.Pass != 1 || got.Skipped != 0 {
		t.Errorf("tests rolled up as pass %d skipped %d, want pass 1 skipped 0",
			got.Pass, got.Skipped)
	}
}

// TestGateRollupCountsPreGateRows is DKT-862's rollup half.
//
// `run report`'s Gates tally rendered `ac-commands: pass 0, fail 1` whether the
// failure BLOCKED the step or was advisory. A `pre = true` gate never routes,
// so the two are different facts and the section had no way to tell them apart:
// the rollup counted four verdicts and nothing about WHEN the row was produced.
//
// `Pre` counts ROWS and OVERLAPS the verdict columns, exactly as `Stub` does —
// it describes the phase that produced the row, not what the row decided.
func TestGateRollupCountsPreGateRows(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedRunForNext(t, conn)

	exitZero, exitTwo := 0, 2
	rows := []db.GateResultRow{
		// RUN-61's shape: a pre-gate that failed and routed nothing.
		{RunID: runID, StepID: 1, Gate: "ac-commands", Ordinal: 0, Exit: &exitTwo,
			Verdict: db.GateVerdictFail, Pre: true, CreatedAtMS: model.NowMS()},
		// A blocking gate that failed identically.
		{RunID: runID, StepID: 1, Gate: "build", Ordinal: 0, Exit: &exitTwo,
			Verdict: db.GateVerdictFail, CreatedAtMS: model.NowMS()},
		// One NAME, both declarations — the case the tally cannot mark whole.
		{RunID: runID, StepID: 1, Gate: "render-verify", Ordinal: 0, Exit: &exitZero,
			Verdict: db.GateVerdictPass, Pre: true, CreatedAtMS: model.NowMS()},
		{RunID: runID, StepID: 1, Gate: "render-verify", Ordinal: 1, Exit: &exitTwo,
			Verdict: db.GateVerdictFail, CreatedAtMS: model.NowMS()},
	}
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	for _, r := range rows {
		testsupport.Must(t, db.InsertGateResultTx(tx, r), "InsertGateResultTx: %v", err)
	}
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)

	counts, err := db.GateRollup(conn, runID)
	testsupport.Must(t, err, "GateRollup: %v", err)

	byName := map[string]db.VerdictCount{}
	for _, c := range counts {
		byName[c.Name] = c
	}
	if got := byName["ac-commands"]; got.Pre != 1 || got.Fail != 1 {
		t.Errorf("ac-commands rolled up as pre %d fail %d, want pre 1 fail 1 — "+
			"an advisory failure must be countable as one", got.Pre, got.Fail)
	}
	// The marker must not spread: a blocking gate that failed the same way is
	// the row the report has to keep distinguishable.
	if got := byName["build"]; got.Pre != 0 {
		t.Errorf("build rolled up as pre %d, want 0", got.Pre)
	}
	if got := byName["render-verify"]; got.Pre != 1 || got.Pass+got.Fail != 2 {
		t.Errorf("render-verify rolled up as pre %d over %d rows, want pre 1 "+
			"of 2 — the tally must be able to say which half was advisory",
			got.Pre, got.Pass+got.Fail)
	}

	// Actions have no `pre` column, and the shared rollup body must return an
	// honest zero for them rather than failing on a column that table lacks —
	// the same asymmetry `stub_entry` already has.
	actions, err := db.ActionRollup(conn, runID)
	if err != nil {
		t.Fatalf("ActionRollup failed after the gate rollup gained a pre "+
			"column: %v", err)
	}
	for _, a := range actions {
		if a.Pre != 0 {
			t.Errorf("action %q rolled up as pre %d, want 0", a.Name, a.Pre)
		}
	}
}

// TestVoteUsageCoverageMakesSilenceVisible is DKT-257.
//
// The vote_usage ledger has existed since v14 and held ZERO rows for an entire
// store epoch while 21+ seat-votes did real verification work. Everything was
// wired — the table, the writer, the rollup — and nothing supplied a number, so
// `vote_usage` was `omitempty` and the section simply did not appear.
//
// An absent section reads as "no panels ran", which is a different and much
// more comfortable claim than "panels ran and none of them said what they
// cost". In-wave panels ARE counted, because their seats run as steps; a
// conductor-side panel of identical shape recorded 287/0. Roughly 40k output
// tokens per panel the run's budget never saw, and the only difference was
// WHERE the ballot executed.
func TestVoteUsageCoverageMakesSilenceVisible(t *testing.T) {
	conn := newTestDB(t)

	// Two seats cast on one vote-step proposal; ONE reports its spend.
	proposalID, err := db.CreateProposalIdempotent(conn, &model.Proposal{
		Description: "verify the change", Status: model.ProposalStatusOpen,
		Criticality: model.CriticalityMedium, RequiredVoters: 2, Threshold: 0.67,
	}, "vote-step:1:1:verify-tribunal@0")
	testsupport.Must(t, err, "creating the proposal: %v", err)

	quiet := castSeat(t, conn, proposalID, "tribunal-architecture")
	loud := castSeat(t, conn, proposalID, "tribunal-security")

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	testsupport.Must(t, db.InsertVoteUsageTx(tx, loud, "output_tokens", 39500, "", model.NowMS()),
		"InsertVoteUsageTx: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)
	_ = quiet

	got, err := db.VoteUsageCoverageFor(conn, db.ScopeVoteCreate, "vote-step:1:")
	testsupport.Must(t, err, "VoteUsageCoverageFor: %v", err)

	if got.Casts != 2 {
		t.Errorf("casts = %d, want 2", got.Casts)
	}
	if got.Reported != 1 {
		t.Errorf("reported = %d, want 1", got.Reported)
	}
	if got.Silent() != 1 {
		t.Errorf("silent = %d, want 1 — a seat that deliberated and reported "+
			"nothing is a GAP, not a zero", got.Silent())
	}
}

// TestVoteUsageCoverageIsZeroWithNoCasts: a run with no panels reports no
// coverage, and that is a different fact from a run whose panels went silent.
//
// This is the distinction the whole change exists to make, so it is asserted
// from both sides.
func TestVoteUsageCoverageIsZeroWithNoCasts(t *testing.T) {
	conn := newTestDB(t)
	got, err := db.VoteUsageCoverageFor(conn, db.ScopeVoteCreate, "vote-step:1:")
	testsupport.Must(t, err, "VoteUsageCoverageFor: %v", err)
	if got.Casts != 0 || got.Reported != 0 {
		t.Errorf("coverage = %+v on a run with no casts, want zeroes", got)
	}
}

// TestSilentVoteSeatsAreNamed is DKT-733: the coverage count said 12 of 57
// seats reported nothing and nothing anywhere said WHICH twelve, so the
// backfill verb built to close the gap could not be aimed. The enumeration
// goes through the SAME membership builder as the count it explains.
func TestSilentVoteSeatsAreNamed(t *testing.T) {
	conn := newTestDB(t)

	proposalID, err := db.CreateProposalIdempotent(conn, &model.Proposal{
		Description: "verify the change", Status: model.ProposalStatusOpen,
		Criticality: model.CriticalityMedium, RequiredVoters: 2, Threshold: 0.67,
	}, "vote-step:1:1:verify-tribunal@0")
	testsupport.Must(t, err, "creating the proposal: %v", err)

	castSeat(t, conn, proposalID, "tribunal-architecture") // stays silent
	loud := castSeat(t, conn, proposalID, "tribunal-security")

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	testsupport.Must(t, db.InsertVoteUsageTx(tx, loud, "output_tokens", 39500, "", model.NowMS()),
		"InsertVoteUsageTx: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)

	got, err := db.SilentVoteSeatsFor(conn, db.ScopeVoteCreate, "vote-step:1:")
	testsupport.Must(t, err, "SilentVoteSeatsFor: %v", err)

	if len(got) != 1 {
		t.Fatalf("silent seats = %+v, want exactly the one quiet cast", got)
	}
	if got[0].ProposalID != proposalID || got[0].Voter != "tribunal-architecture" {
		t.Errorf("silent seat = %+v, want tribunal-architecture on proposal %d",
			got[0], proposalID)
	}
	if got[0].Role != "judge" {
		t.Errorf("silent seat role = %q, want the cast's recorded role", got[0].Role)
	}
}

// --- DKT-425: what `step gates --json` returns by default -------------------

// hygieneLog stands in for the row DKT-425 was opened about: a PASSING gate
// that stored 174,498 bytes and rode along in every --json read of the step.
var hygieneLog = strings.Repeat("hygiene: checked one more file\n", 6000)

// insertGateRows records rows the way the saga's writer does, so these tests
// read what a real run stores rather than a hand-built payload.
func insertGateRows(t *testing.T, conn *sql.DB, rows []db.GateResultRow) {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	for _, r := range rows {
		testsupport.Must(t, db.InsertGateResultTx(tx, r), "InsertGateResultTx: %v", err)
	}
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)
}

// gatesCmdWithDB is cmdWithDB plus the two flags `step gates` registers, so a
// test drives the same flag reads the real command does.
func gatesCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().Bool("full", false, "")
	cmd.Flags().StringSlice("gate", nil, "")
	return cmd
}

// gateRowsFromJSON pulls the `gates` array out of a --json envelope as MAPS.
//
// The maps matter: an elided body is an ABSENT KEY, and a struct with a string
// field cannot tell that apart from a gate that printed nothing.
func gateRowsFromJSON(t *testing.T, raw []byte) []map[string]json.RawMessage {
	t.Helper()
	var envelope struct {
		Data struct {
			Gates []map[string]json.RawMessage `json:"gates"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, string(raw))
	}
	return envelope.Data.Gates
}

// gateRowByName returns the single row recorded for a gate.
func gateRowByName(t *testing.T, rows []map[string]json.RawMessage, gate string) map[string]json.RawMessage {
	t.Helper()
	for _, row := range rows {
		var name string
		testsupport.Must(t, json.Unmarshal(row["gate"], &name), "unmarshal gate: %v", nil)
		if name == gate {
			return row
		}
	}
	t.Fatalf("no row for gate %q", gate)
	return nil
}

// jsonInt reads an integer field off a decoded row.
func jsonInt(t *testing.T, row map[string]json.RawMessage, key string) int {
	t.Helper()
	raw, ok := row[key]
	if !ok {
		t.Fatalf("row has no %q key: %v", key, row)
	}
	var n int
	testsupport.Must(t, json.Unmarshal(raw, &n), "unmarshal %s: %v", key, nil)
	return n
}

// jsonString reads a string field off a decoded row.
func jsonString(t *testing.T, row map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, ok := row[key]
	if !ok {
		t.Fatalf("row has no %q key: %v", key, row)
	}
	var s string
	testsupport.Must(t, json.Unmarshal(raw, &s), "unmarshal %s: %v", key, nil)
	return s
}

// gatesStepWithABigPass records the DKT-425 shape: one enormous PASSING gate,
// one failing gate worth triaging, and one unmatched row that never ran.
func gatesStepWithABigPass(t *testing.T, conn *sql.DB) {
	t.Helper()
	runID := activatedRunForNext(t, conn)
	exitZero, exitOne := 0, 1
	insertGateRows(t, conn, []db.GateResultRow{
		{RunID: runID, StepID: 1, Gate: "self-hygiene", Ordinal: 0,
			Argv: []string{"scripts/qa/self-hygiene.sh"}, Exit: &exitZero,
			DurationMS: 4000, Output: hygieneLog, Verdict: db.GateVerdictPass,
			CreatedAtMS: model.NowMS()},
		{RunID: runID, StepID: 1, Gate: "vuln-scan", Ordinal: 0,
			Argv: []string{"scripts/qa/vuln-scan.sh"}, Exit: &exitOne,
			DurationMS: 9000,
			Output:     strings.Repeat("vulnerability report line\n", 800) + "FAILED: 3 findings",
			Verdict:    db.GateVerdictFail, CreatedAtMS: model.NowMS()},
		{RunID: runID, StepID: 1, Gate: "secret-scan", Ordinal: 0,
			Argv: []string{"scripts/qa/secret-scan.sh"}, Exit: &exitZero,
			DurationMS: 120, Output: "no findings\n", Verdict: db.GateVerdictPass,
			CreatedAtMS: model.NowMS()},
	})
}

// TestStepGatesJSONIsHumanScaleByDefault is DKT-425's whole complaint.
//
// One `step gates STEP-824 --json` returned 216,076 bytes — 174,498 of them a
// PASSING self-hygiene log — which is too large to read in a session at all.
// The size assertion is the test: a payload that names a 180KB capture and
// weighs a few KB is the fix, and one that weighs 180KB is the bug back.
func TestStepGatesJSONIsHumanScaleByDefault(t *testing.T) {
	conn := newTestDB(t)
	gatesStepWithABigPass(t, conn)

	w, buf := bufWriter(true)
	err := runStepGates(gatesCmdWithDB(conn), []string{"STEP-1"}, w)
	testsupport.Must(t, err, "runStepGates: %v", err)

	const humanScale = 8 * 1024
	if buf.Len() > humanScale {
		t.Errorf("default --json response = %d bytes, want under %d — a read "+
			"whose size is set by a passing gate's log is the DKT-425 defect",
			buf.Len(), humanScale)
	}

	rows := gateRowsFromJSON(t, buf.Bytes())
	if len(rows) != 3 {
		t.Fatalf("gates = %d, want 3", len(rows))
	}

	// The bytes are ACCOUNTED FOR, not merely gone: the row still says how
	// large the capture is, so nothing about the summary is a tautology.
	pass := gateRowByName(t, rows, "self-hygiene")
	if got := jsonInt(t, pass, "output_bytes"); got != len(hygieneLog) {
		t.Errorf("self-hygiene output_bytes = %d, want %d", got, len(hygieneLog))
	}
	if _, ok := pass["output"]; ok {
		t.Error("a passing gate's full body rode along in the default payload; " +
			"that log is 80% of what made the read unreadable")
	}
	if _, ok := pass["output_tail"]; ok {
		t.Error("a passing gate carried an output tail; verdict, exit and size " +
			"are what a pass has to say")
	}

	// A FAILING row is what the reader is here for, so it triages in place.
	fail := gateRowByName(t, rows, "vuln-scan")
	if _, ok := fail["output"]; ok {
		t.Error("a failing gate's full body rode along by default; the tail is " +
			"the summary's share and --full is how a reader asks for the rest")
	}
	tail := jsonString(t, fail, "output_tail")
	if !strings.HasSuffix(tail, "FAILED: 3 findings") {
		t.Errorf("the failing gate's tail does not end at the failure:\n%s", tail)
	}
	if len(tail) > gateSummaryTailBytes {
		t.Errorf("tail = %d bytes, want at most %d", len(tail), gateSummaryTailBytes)
	}
	if got := jsonInt(t, fail, "output_bytes"); got <= len(tail) {
		t.Errorf("output_bytes = %d with a %d-byte tail; the row must say how "+
			"much was left behind", got, len(tail))
	}
}

// TestStepGatesJSONFullRestoresEveryBody pins the compatibility half: --full is
// exactly the pre-DKT-425 default, so a consumer that depended on every row's
// `output` loses nothing by adding the flag.
func TestStepGatesJSONFullRestoresEveryBody(t *testing.T) {
	conn := newTestDB(t)
	gatesStepWithABigPass(t, conn)

	cmd := gatesCmdWithDB(conn)
	testsupport.Must(t, cmd.Flags().Set("full", "true"), "setting --full: %v", nil)

	w, buf := bufWriter(true)
	err := runStepGates(cmd, []string{"STEP-1"}, w)
	testsupport.Must(t, err, "runStepGates: %v", err)

	rows := gateRowsFromJSON(t, buf.Bytes())
	if len(rows) != 3 {
		t.Fatalf("gates = %d, want 3", len(rows))
	}
	for _, row := range rows {
		name := jsonString(t, row, "gate")
		if _, ok := row["output"]; !ok {
			t.Fatalf("--full dropped %s's output key; the flag exists to be the "+
				"old behavior exactly", name)
		}
		if got, want := len(jsonString(t, row, "output")), jsonInt(t, row, "output_bytes"); got != want {
			t.Errorf("%s carried %d bytes of output under --full, want %d",
				name, got, want)
		}
	}
	if body := jsonString(t, gateRowByName(t, rows, "self-hygiene"), "output"); body != hygieneLog {
		t.Errorf("the passing gate's body under --full is %d bytes, want the "+
			"stored %d", len(body), len(hygieneLog))
	}

	// An EMPTY capture keeps its key under --full, as `""`. The pre-DKT-425
	// shape carried `output` on every row, and a consumer keying on it must
	// not meet a missing field for a gate that never printed anything.
	empty := buildStepGatesResult("STEP-1", []db.GateResultRow{{
		Gate: "vuln-scan", Verdict: db.GateVerdictUnmatched,
		Reason: "no trust entry matched the argv",
	}}, gateOutputScope{full: true})
	raw, err := json.Marshal(empty)
	testsupport.Must(t, err, "marshal: %v", err)
	if !strings.Contains(string(raw), `"output":""`) {
		t.Errorf("--full dropped the output key on an empty capture:\n%s", raw)
	}
}

// TestStepGatesJSONGateWidensOneGateOnly: --gate is the retrieval half of the
// acceptance criteria — full output for a NAMED gate stays reachable, and the
// rest of the step stays summarized so asking for one log does not re-inflate
// the read.
func TestStepGatesJSONGateWidensOneGateOnly(t *testing.T) {
	conn := newTestDB(t)
	gatesStepWithABigPass(t, conn)

	cmd := gatesCmdWithDB(conn)
	testsupport.Must(t, cmd.Flags().Set("gate", "self-hygiene"), "setting --gate: %v", nil)

	w, buf := bufWriter(true)
	err := runStepGates(cmd, []string{"STEP-1"}, w)
	testsupport.Must(t, err, "runStepGates: %v", err)

	rows := gateRowsFromJSON(t, buf.Bytes())
	if body := jsonString(t, gateRowByName(t, rows, "self-hygiene"), "output"); body != hygieneLog {
		t.Errorf("--gate self-hygiene returned %d bytes, want the stored %d — a "+
			"named gate's full output has to remain retrievable",
			len(body), len(hygieneLog))
	}
	for _, gate := range []string{"vuln-scan", "secret-scan"} {
		if _, ok := gateRowByName(t, rows, gate)["output"]; ok {
			t.Errorf("--gate self-hygiene also widened %s; the other rows stay "+
				"summarized", gate)
		}
	}

	// A name that matches nothing widens nothing, and still lists every gate —
	// the byte counts are how a typo shows itself.
	typo := gatesCmdWithDB(conn)
	testsupport.Must(t, typo.Flags().Set("gate", "self-hygeine"), "setting --gate: %v", nil)
	w2, buf2 := bufWriter(true)
	testsupport.Must(t, runStepGates(typo, []string{"STEP-1"}, w2), "runStepGates: %v", nil)
	for _, row := range gateRowsFromJSON(t, buf2.Bytes()) {
		if _, ok := row["output"]; ok {
			t.Errorf("a --gate matching no row widened %s", jsonString(t, row, "gate"))
		}
	}
}

// TestStepGatesJSONKeepsRetriedAttemptsDistinct is the third acceptance
// criterion. A flaky-declared re-run records its own row, and the summary must
// keep each attempt's ordinal, verdict, exit and size rather than collapsing
// them — "it failed twice then passed" is the fact a conductor reads.
//
// The identical repeat additionally points back at the attempt that already
// carried the tail, so three copies of one 281-byte log travel once.
func TestStepGatesJSONKeepsRetriedAttemptsDistinct(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedRunForNext(t, conn)

	exitZero, exitTwo := 0, 2
	const buildLog = "cmd/docket/main.go:12: undefined: Foo\nbuild failed"
	insertGateRows(t, conn, []db.GateResultRow{
		{RunID: runID, StepID: 1, Gate: "build", Ordinal: 0, Exit: &exitTwo,
			DurationMS: 900, Output: buildLog, Verdict: db.GateVerdictFail,
			CreatedAtMS: model.NowMS()},
		{RunID: runID, StepID: 1, Gate: "build", Ordinal: 1, Exit: &exitTwo,
			DurationMS: 850, Output: buildLog, Verdict: db.GateVerdictFail,
			CreatedAtMS: model.NowMS()},
		{RunID: runID, StepID: 1, Gate: "build", Ordinal: 2, Exit: &exitZero,
			DurationMS: 1100, Output: "ok", Verdict: db.GateVerdictPass,
			CreatedAtMS: model.NowMS()},
	})

	w, buf := bufWriter(true)
	err := runStepGates(gatesCmdWithDB(conn), []string{"STEP-1"}, w)
	testsupport.Must(t, err, "runStepGates: %v", err)

	rows := gateRowsFromJSON(t, buf.Bytes())
	if len(rows) != 3 {
		t.Fatalf("gates = %d, want 3 — the attempts must not collapse into one "+
			"row", len(rows))
	}
	for i, row := range rows {
		if got := jsonInt(t, row, "ordinal"); got != i {
			t.Errorf("row %d has ordinal %d; each attempt keeps its own", i, got)
		}
	}
	if got := jsonString(t, rows[2], "verdict"); got != db.GateVerdictPass {
		t.Errorf("the last attempt's verdict = %q, want pass — a summary that "+
			"lost it would report a step as failing that succeeded", got)
	}

	// Attempt 0 shows the failure; attempt 1 says it is the same bytes again.
	if !strings.Contains(jsonString(t, rows[0], "output_tail"), "undefined: Foo") {
		t.Error("the first failing attempt carries no tail to triage")
	}
	if _, ok := rows[0]["same_output_as_ordinal"]; ok {
		t.Error("the first attempt was marked a repeat of something earlier")
	}
	if got := jsonInt(t, rows[1], "same_output_as_ordinal"); got != 0 {
		t.Errorf("the repeated attempt points at ordinal %d, want 0", got)
	}
	if _, ok := rows[1]["output_tail"]; ok {
		t.Error("the repeated attempt printed the identical tail a second time")
	}
	// The repeat is still fully addressable: --full carries every attempt's
	// body, dedupe marker or not.
	full := buildStepGatesResult("STEP-1", mustGateRows(t, conn), gateOutputScope{full: true})
	if full.Gates[1].Output == nil || *full.Gates[1].Output != buildLog {
		t.Error("--full elided the repeated attempt's body; the marker abbreviates " +
			"the summary, not the archive")
	}
}

// mustGateRows reads a step's recorded rows for a test that needs them raw.
func mustGateRows(t *testing.T, conn *sql.DB) []db.GateResultRow {
	t.Helper()
	rows, err := db.GateResultsForStep(conn, 1)
	testsupport.Must(t, err, "GateResultsForStep: %v", err)
	return rows
}

// TestGateSummaryTailIsBoundedInBytesToo: ten lines is not a size when a gate
// emits one 200KB line of minified JSON, which is exactly the shape a scanner
// prints. The byte cap is what makes the summary bounded rather than usually
// small, and cutting mid-rune must not leave invalid UTF-8 in the payload.
func TestGateSummaryTailIsBoundedInBytesToo(t *testing.T) {
	oneHugeLine := strings.Repeat("é", 100_000)
	tail := gateSummaryTail(oneHugeLine)
	if len(tail) > gateSummaryTailBytes {
		t.Errorf("tail = %d bytes, want at most %d", len(tail), gateSummaryTailBytes)
	}
	if !utf8.ValidString(tail) {
		t.Error("the byte cap severed a rune and left invalid UTF-8 in the payload")
	}
	if !strings.HasSuffix(oneHugeLine, tail) {
		t.Error("the tail is not the END of the capture")
	}

	if got := gateSummaryTail(""); got != "" {
		t.Errorf("empty capture tailed to %q", got)
	}
	if got := gateSummaryTail("one\ntwo\n"); got != "one\ntwo" {
		t.Errorf("short capture = %q, want it verbatim minus the trailing newline", got)
	}
	// No marker prose inside a JSON string: the row's output_bytes says what
	// was left behind, and a sentence pointing at --json from inside --json
	// would be a lie.
	long := gateSummaryTail(strings.Repeat("x\n", 30) + "last")
	if strings.Contains(long, "--json") {
		t.Errorf("the JSON tail carries terminal prose:\n%s", long)
	}
	if !strings.HasSuffix(long, "last") {
		t.Errorf("tail %q does not end with the final line", long)
	}
}

// TestStepGatesHumanModeIgnoresBodyFlagsButSaysSo: --full and --gate shape a
// wire format the table does not use. A flag that silently does nothing is the
// friction this hint exists to prevent.
func TestStepGatesHumanModeIgnoresBodyFlagsButSaysSo(t *testing.T) {
	conn := newTestDB(t)
	gatesStepWithABigPass(t, conn)

	plain := gatesCmdWithDB(conn)
	w, buf := bufWriter(false)
	testsupport.Must(t, runStepGates(plain, []string{"STEP-1"}, w), "runStepGates: %v", nil)
	if strings.Contains(buf.String(), "--full and --gate") {
		t.Error("the human table hints at flags nobody typed")
	}
	if strings.Contains(buf.String(), hygieneLog) {
		t.Error("the passing gate's log reached the human table")
	}

	withFlag := gatesCmdWithDB(conn)
	testsupport.Must(t, withFlag.Flags().Set("full", "true"), "setting --full: %v", nil)
	w2, buf2 := bufWriter(false)
	testsupport.Must(t, runStepGates(withFlag, []string{"STEP-1"}, w2), "runStepGates: %v", nil)
	if !strings.Contains(buf2.String(), "add --json to read a body in full") {
		t.Errorf("human mode swallowed --full without a word:\n%s", buf2.String())
	}
}

// castSeat records one seat's vote and returns its row id.
func castSeat(t *testing.T, conn *sql.DB, proposalID int, voter string) int64 {
	t.Helper()
	res, err := db.CastVote(conn, &model.Vote{
		ProposalID: proposalID, VoterName: voter, VoterRole: "judge",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9,
	})
	testsupport.Must(t, err, "CastVote(%s): %v", voter, err)
	return int64(res.Vote.ID)
}
