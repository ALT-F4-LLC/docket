package engine

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// `dispatch extend` — DKT-2071's append (docs/tdd/runs-dispatch.md §5.11).
//
// A fix round is minted at the RECORD TIME of the step whose routing chose
// `fix-loop`, never at `next`, so before this verb every loop round started in
// a later dispatch by construction. The tests below drive exactly that path: a
// real `fix-loop` routing recorded under an OPEN manifest, then the append.

// extendDispatch extends a manifest, failing the test on refusal.
func extendDispatch(t *testing.T, conn *sql.DB, runID int, at int64) *Extension {
	t.Helper()
	x, err := NewEngine().ExtendDispatch(conn, runID, at)
	testsupport.Must(t, err, "dispatch extend: %v", err)
	return x
}

// openManifestRows reads the OPEN manifest's rows as stored, in position order.
func openManifestRows(t *testing.T, conn *sql.DB, runID int) []db.DispatchRow {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	open, err := db.OpenDispatchTx(tx, runID)
	testsupport.Must(t, err, "OpenDispatchTx: %v", err)
	rows, err := db.ListDispatchRowsTx(tx, open.ID)
	testsupport.Must(t, err, "ListDispatchRowsTx: %v", err)
	return rows
}

// openDispatchExpiry reads the open manifest's current expiry.
func openDispatchExpiry(t *testing.T, conn *sql.DB, runID int) int64 {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	open, err := db.OpenDispatchTx(tx, runID)
	testsupport.Must(t, err, "OpenDispatchTx: %v", err)
	return open.ExpiresMS
}

// extendEventData reads the newest event of one kind for a run.
func extendEventData(t *testing.T, conn *sql.DB, runID int, kind string) map[string]any {
	t.Helper()
	var raw string
	err := conn.QueryRow(
		`SELECT data FROM events WHERE run_id = ? AND kind = ?
		  ORDER BY seq DESC LIMIT 1`, runID, kind).Scan(&raw)
	testsupport.Must(t, err, "reading the newest %s event: %v", kind, err)
	var data map[string]any
	testsupport.Must(t, json.Unmarshal([]byte(raw), &data),
		"parsing the %s event data %q: %v", kind, raw, err)
	return data
}

// instancesOf renders a row set's instances, for failure messages.
func instancesOf(rows []model.StepRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Instance)
	}
	return out
}

// TestDispatchExtendAppendsAMintedFixRound is AC1 and AC2 together: the fix
// round a `fix-loop` routing minted UNDER THE OPEN MANIFEST is appended to that
// same manifest, at position max+1, with the canonical bytes and sha256 the
// open would have stored — and the manifest's expiry, `extended_seq` and event
// all record the append.
func TestDispatchExtendAppendsAMintedFixRound(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	// The whole round up to `verify@0`, then the manifest that freezes it.
	driveToVerify(t, conn, e, 0)
	opened := openDispatch(t, conn, run.ID, 0, nowMS)
	beforeRows := openManifestRows(t, conn, run.ID)
	beforeExpiry := openDispatchExpiry(t, conn, run.ID)

	if len(beforeRows) == 0 {
		t.Fatal("the opened manifest is empty; the test's premise is broken")
	}
	for _, r := range beforeRows {
		if strings.HasPrefix(r.Instance, "fix@") {
			t.Fatalf("the opened manifest already carries %s — the fix round "+
				"must be minted AFTER the open for this test to mean anything",
				r.Instance)
		}
	}

	// The routing that mints fix@1, recorded while the dispatch is open. This
	// is the hop that used to cost a whole dispatch boundary.
	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)
	if got := stepRouting(t, conn, "verify@0"); got != workflow.OnFailFixLoop {
		t.Fatalf("verify@0 routed %q, want %q", got, workflow.OnFailFixLoop)
	}

	x := extendDispatch(t, conn, run.ID, nowMS)

	// AC1: the minted round is on the manifest.
	var appendedFix bool
	for _, r := range x.Rows {
		if r.Instance == "fix@1" {
			appendedFix = true
		}
	}
	if !appendedFix {
		t.Fatalf("extend appended %v, want fix@1 — the round the `fix-loop` "+
			"routing minted under the open manifest", instancesOf(x.Rows))
	}
	if x.Dispatch != opened.Dispatch {
		t.Errorf("extend reports %s, want the SAME open manifest %s — an "+
			"extend appends, it does not open a second dispatch",
			x.Dispatch, opened.Dispatch)
	}

	// AC1: stored at max+1, with the open's own canonical bytes and hash.
	after := openManifestRows(t, conn, run.ID)
	if len(after) != len(beforeRows)+len(x.Rows) {
		t.Fatalf("the manifest stores %d row(s), want %d (%d opened + %d appended)",
			len(after), len(beforeRows)+len(x.Rows), len(beforeRows), len(x.Rows))
	}
	for i, r := range after {
		if r.Position != i {
			t.Errorf("stored row %d is at position %d; appended rows take max+1 "+
				"and the stored list stays densely ordered", i, r.Position)
		}
		if workflow.SHA256([]byte(r.RowJSON)) != r.RowSHA256 {
			t.Errorf("stored row %d (%s): row_json does not hash to row_sha256 — "+
				"an appended row must be byte-hashed exactly as an opened one is",
				i, r.Instance)
		}
	}
	for i, want := range x.Rows {
		raw, sum, err := canonicalRowBytes(want)
		testsupport.Must(t, err, "canonicalRowBytes: %v", err)
		got := after[len(beforeRows)+i]
		if got.RowJSON != raw || got.RowSHA256 != sum {
			t.Errorf("appended row %s stored as %s/%s, want %s/%s — the bytes "+
				"must come from the SAME canonicalizer `dispatch open` uses",
				want.Instance, got.RowJSON, got.RowSHA256, raw, sum)
		}
	}

	// AC2: the expiry grew by the APPENDED rows' staged lease sum, the event
	// carries the run, the dispatch, the count and `extended_seq`, and the
	// response reports the same seq.
	wantExpiry := beforeExpiry + stagedLeaseSumMS(x.Rows)
	if got := openDispatchExpiry(t, conn, run.ID); got != wantExpiry {
		t.Errorf("expires_ms = %d after the extend, want %d (%d + the appended "+
			"rows' staged lease sum) — the manifest must outlive the work it "+
			"now carries", got, wantExpiry, beforeExpiry)
	}
	if x.ExpiresMS != wantExpiry {
		t.Errorf("the extend reports expires_ms %d, want %d", x.ExpiresMS, wantExpiry)
	}
	if n := eventKindCount(t, conn, run.ID, EventDispatchExtended); n != 1 {
		t.Fatalf("%s event count = %d, want 1", EventDispatchExtended, n)
	}
	data := extendEventData(t, conn, run.ID, EventDispatchExtended)
	if got := data["dispatch"]; got != opened.Dispatch {
		t.Errorf("the %s event names dispatch %v, want %s",
			EventDispatchExtended, got, opened.Dispatch)
	}
	if got, want := data["rows"], float64(len(x.Rows)); got != want {
		t.Errorf("the %s event reports %v appended row(s), want %v",
			EventDispatchExtended, got, want)
	}
	if got, want := data["extended_seq"], float64(x.ExtendedSeq); got != want {
		t.Errorf("the %s event records extended_seq %v, want %v — the log "+
			"position the appended rows were computed at",
			EventDispatchExtended, got, want)
	}
	if x.ExtendedSeq <= 0 {
		t.Errorf("extended_seq = %d, want the event cursor beside opened_seq (%d)",
			x.ExtendedSeq, opened.OpenedSeq)
	}
}

// TestDispatchExtendWithNothingNewAppendsNothing is AC1's dedupe: a second
// extend drops every step id already stored and succeeds with zero rows. Its
// mutant — skipping the dedupe — stores the same step twice and fails here.
func TestDispatchExtendWithNothingNewAppendsNothing(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	openDispatch(t, conn, run.ID, 0, nowMS)
	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)

	first := extendDispatch(t, conn, run.ID, nowMS)
	if len(first.Rows) == 0 {
		t.Fatal("the first extend appended nothing; the test's premise is broken")
	}
	storedAfterFirst := openManifestRows(t, conn, run.ID)
	expiryAfterFirst := openDispatchExpiry(t, conn, run.ID)

	second := extendDispatch(t, conn, run.ID, nowMS)
	if len(second.Rows) != 0 {
		t.Errorf("the second extend appended %v, want none — every ready step "+
			"is already stored on the manifest", instancesOf(second.Rows))
	}
	if got := len(openManifestRows(t, conn, run.ID)); got != len(storedAfterFirst) {
		t.Errorf("the manifest stores %d row(s) after a second extend, want %d — "+
			"a step id already on the manifest must be dropped",
			got, len(storedAfterFirst))
	}
	if got := openDispatchExpiry(t, conn, run.ID); got != expiryAfterFirst {
		t.Errorf("expires_ms = %d after an empty extend, want %d unchanged — "+
			"appending nothing buys no extra wall clock", got, expiryAfterFirst)
	}
}

// TestDispatchExtendRefusesWithNoOpenDispatch is AC1's third case: the verb
// appends to a manifest and there is none.
func TestDispatchExtendRefusesWithNoOpenDispatch(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	_, err := NewEngine().ExtendDispatch(conn, run.ID, nowMS)
	if err == nil {
		t.Fatal("extend succeeded with no dispatch open, want a refusal")
	}
	if !strings.Contains(err.Error(), "no dispatch is open") {
		t.Errorf("extend refused with %q, want the no-open-dispatch error", err)
	}
}

// TestDispatchExtendKeepsVerifyPassing is AC3's first half: `dispatch verify`
// iterates the GROWN stored list by step id and still passes. An appended row
// is an ordinary manifest row to every consumer of the manifest.
func TestDispatchExtendKeepsVerifyPassing(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	openDispatch(t, conn, run.ID, 0, nowMS)
	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)
	x := extendDispatch(t, conn, run.ID, nowMS)
	if len(x.Rows) == 0 {
		t.Fatal("the extend appended nothing; the test's premise is broken")
	}

	result, mismatch, err := e.VerifyDispatch(conn, run.ID, nowMS)
	testsupport.Must(t, err, "dispatch verify: %v", err)
	if mismatch != nil {
		t.Fatalf("verify reported a mismatch at position %d after an extend:\n"+
			"  stored:   %s\n  computed: %s",
			mismatch.Position, mismatch.Stored, mismatch.Computed)
	}
	if !result.Verified {
		t.Errorf("verify.Verified = false after an extend, want true")
	}
	if len(result.Rows) != len(openManifestRows(t, conn, run.ID)) {
		t.Errorf("verify reported %d row verdict(s), want one per stored row "+
			"(%d) — it must iterate the GROWN list",
			len(result.Rows), len(openManifestRows(t, conn, run.ID)))
	}
}

// TestDispatchExtendLeavesUnlaunchedRowsPending is AC4: an appended row nobody
// claimed stays `pending` and the close does not call it a discrepancy. A
// manifest is not a lock and an APPENDED row is no more of one than an opened
// row; a close that counted them would make the verb unusable, since the whole
// point is offering rows a relay may or may not get to.
func TestDispatchExtendLeavesUnlaunchedRowsPending(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	openDispatch(t, conn, run.ID, 0, nowMS)
	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)

	x := extendDispatch(t, conn, run.ID, nowMS)
	if len(x.Rows) == 0 {
		t.Fatal("the extend appended nothing; the test's premise is broken")
	}
	// Nothing is claimed: every appended row is left exactly as offered.
	for _, r := range x.Rows {
		if got := stepStatus(t, conn, r.Instance); got != db.StepPending {
			t.Errorf("appended row %s is %q, want %q — an appended row is an "+
				"OFFER, not a claim", r.Instance, got, db.StepPending)
		}
	}

	// The close REFUSES CONFLICT on any discrepancy, so succeeding is the
	// assertion: a pending appended row is the dispatch working, not drift.
	outcome, err := e.CloseDispatch(conn, run.ID, false, "", nowMS)
	if err != nil {
		t.Fatalf("close after an extend refused: %v — the never-launched "+
			"appended rows %v are still pending, which is not a discrepancy",
			err, instancesOf(x.Rows))
	}
	if outcome.Status != db.DispatchClosed {
		t.Errorf("close left the manifest %q, want %q", outcome.Status, db.DispatchClosed)
	}
	for _, r := range x.Rows {
		if got := stepStatus(t, conn, r.Instance); got != db.StepPending {
			t.Errorf("appended row %s is %q after the close, want %q — the close "+
				"reconciles claims and usage, never the offer", r.Instance, got, db.StepPending)
		}
	}
}
