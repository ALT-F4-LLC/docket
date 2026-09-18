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

// `dispatch extend` — DKT-2071's append (docs/tdd/runs-dispatch.md §5.10).
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

// openDispatchSeqs reads the open manifest's two log cursors as STORED, which
// is AC2's subject: `extended_seq` beside `opened_seq` on one durable row.
func openDispatchSeqs(t *testing.T, conn *sql.DB, runID int) (opened, extended int64) {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	open, err := db.OpenDispatchTx(tx, runID)
	testsupport.Must(t, err, "OpenDispatchTx: %v", err)
	return open.OpenedSeq, open.ExtendedSeq
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
	// AC2's third clause: `extended_seq` is RECORDED BESIDE `opened_seq` — on
	// the same durable `dispatches` row, not only on the wire and in the log.
	storedOpened, storedExtended := openDispatchSeqs(t, conn, run.ID)
	if storedExtended != x.ExtendedSeq {
		t.Errorf("the dispatches row stores extended_seq %d, want %d — the seq "+
			"must live beside opened_seq, so a reader holding the manifest row "+
			"can tell an extended manifest from an untouched one without "+
			"joining the event log", storedExtended, x.ExtendedSeq)
	}
	if storedOpened != opened.OpenedSeq {
		t.Errorf("opened_seq moved to %d, want %d unchanged — an extend appends "+
			"to a manifest, it does not re-open one", storedOpened, opened.OpenedSeq)
	}

	// The oracle is the OPEN's seq, not the extend's own computation: an
	// implementation that wrote back whatever it had just read would satisfy an
	// equality against itself. The append happens strictly after the open, and
	// the events between them (the record that minted fix@1) guarantee the
	// inequality is strict.
	if x.ExtendedSeq <= opened.OpenedSeq {
		t.Errorf("extended_seq = %d, want strictly greater than opened_seq %d — "+
			"the append is a later position in the same log", x.ExtendedSeq,
			opened.OpenedSeq)
	}
}

// TestDispatchExtendRecordsTheLatestSeqOnEachExtend pins `extended_seq`'s
// contract as the LATEST append rather than the first: the column is overwritten
// on every extend, including one that appends nothing. An empty extend still
// happened, and a reader holding the manifest row must be able to see it.
func TestDispatchExtendRecordsTheLatestSeqOnEachExtend(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	opened := openDispatch(t, conn, run.ID, 0, nowMS)

	// Before any extend, the column is the never-extended zero.
	if _, extended := openDispatchSeqs(t, conn, run.ID); extended != 0 {
		t.Errorf("extended_seq = %d on a freshly opened manifest, want 0 — an "+
			"event seq is 1-based, so zero is the only value that can mean "+
			"never extended", extended)
	}

	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)
	first := extendDispatch(t, conn, run.ID, nowMS)
	if len(first.Rows) == 0 {
		t.Fatal("the first extend appended nothing; the test's premise is broken")
	}
	_, afterFirst := openDispatchSeqs(t, conn, run.ID)
	if afterFirst != first.ExtendedSeq || afterFirst <= opened.OpenedSeq {
		t.Fatalf("extended_seq = %d after the first extend, want %d and greater "+
			"than opened_seq %d", afterFirst, first.ExtendedSeq, opened.OpenedSeq)
	}

	second := extendDispatch(t, conn, run.ID, nowMS)
	if len(second.Rows) != 0 {
		t.Fatalf("the second extend appended %v, want none — the premise is an "+
			"EMPTY extend", instancesOf(second.Rows))
	}
	_, afterSecond := openDispatchSeqs(t, conn, run.ID)
	if afterSecond != second.ExtendedSeq {
		t.Errorf("the dispatches row stores extended_seq %d after the second "+
			"extend, want %d — every extend overwrites the cursor",
			afterSecond, second.ExtendedSeq)
	}
	if afterSecond <= afterFirst {
		t.Errorf("extended_seq = %d after a second extend, want greater than the "+
			"first extend's %d — the first extend's own event moved the log on, "+
			"so a column holding the FIRST append would fail here",
			afterSecond, afterFirst)
	}
}

// TestDispatchExtendRefusesAnExpiredManifest is §5.10's expiry invariant:
// extending a lapsed manifest would push its expiry back out and resurrect a
// batch the TTL had already given up on, which is the wedge `dispatch abandon`
// exists to avoid. Its mutant — dropping the refusal — extends one instead.
func TestDispatchExtendRefusesAnExpiredManifest(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	opened := openDispatch(t, conn, run.ID, 0, nowMS)
	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)

	// One millisecond past the manifest's own TTL: Expired is `now >= expires`.
	past := opened.ExpiresMS + 1
	_, err := NewEngine().ExtendDispatch(conn, run.ID, past)
	if err == nil {
		t.Fatal("extend succeeded on an EXPIRED manifest, want a refusal — " +
			"extending one pushes a lapsed expiry back out and resurrects a " +
			"batch the TTL gave up on")
	}
	if !strings.Contains(err.Error(), "cannot be extended") {
		t.Errorf("extend refused with %q, want the expired-manifest refusal", err)
	}

	// The refusal is total: nothing was appended and the expiry did not move.
	if got := openDispatchExpiry(t, conn, run.ID); got != opened.ExpiresMS {
		t.Errorf("expires_ms = %d after a refused extend, want %d unchanged",
			got, opened.ExpiresMS)
	}
	if _, extended := openDispatchSeqs(t, conn, run.ID); extended != 0 {
		t.Errorf("extended_seq = %d after a refused extend, want 0 — a refusal "+
			"rolls back, it does not half-record an append", extended)
	}
	if n := eventKindCount(t, conn, run.ID, EventDispatchExtended); n != 0 {
		t.Errorf("%d %s events after a refused extend, want 0",
			n, EventDispatchExtended)
	}
}

// TestDispatchExtendReapsLapsedLeasesAndReportsTheHold is the extend half of
// P5's lazy reap, and A11's hold on top of it. `ExtendDispatch` runs the SAME
// shared reap every scheduling verb runs, so a lapsed lease is freed by the
// extend itself — and on a BOUNDED class that reap leaves an unacknowledged
// `reap_acks` row which denies the next `guard spawn --active`.
//
// The fixture is the bounded-class workflow for exactly that reason: a relay
// that extends and then spawns meets the denial, and the verb that created the
// hold is the one that must name it. Without ReapHold on the response the
// engine's own advice points at `dispatch open --ack-reap`, a verb this relay
// cannot reach without the dispatch boundary the extend exists to avoid.
func TestDispatchExtendReapsLapsedLeasesAndReportsTheHold(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	// The manifest was budgeted at the DEFAULT TTL. Shortening the lease before
	// the claim puts the lapse well inside that expiry, which is the state the
	// reap must be reached in: core ships `lease.ttl.default` equal to
	// `dispatch.grace`, so a claim at the default lapses exactly AT the
	// one-stage manifest's expiry and the expired-manifest refusal would fire
	// before the reap ran.
	err := db.SetConfig(conn, 0, db.KeyLeaseTTLDefault, "1m")
	testsupport.Must(t, err, "setting the default lease TTL: %v", err)

	instance := manifest.Rows[0].Instance
	claim := claimInstance(t, conn, instance, nowMS)

	lapsed := claim.LeaseExpiresMS + graceMS(t, conn) + 1
	if lapsed >= manifest.ExpiresMS {
		t.Fatalf("the lapse at %d is not inside the manifest expiring at %d; "+
			"the test's premise is broken", lapsed, manifest.ExpiresMS)
	}

	x := extendDispatch(t, conn, runID, lapsed)

	var reaped bool
	for _, got := range x.Reaped {
		if got == instance {
			reaped = true
		}
	}
	if !reaped {
		t.Errorf("extend reported reaped %v, want %s — the shared reap is not "+
			"optional for an appending verb either: offering a row a reap would "+
			"have freed makes the manifest wrong as it is written",
			x.Reaped, instance)
	}
	if n := eventKindCount(t, conn, runID, EventLeaseReaped); n != 1 {
		t.Errorf("%d lease-reaped events, want 1 — the extend's reap must be "+
			"logged the way `next` and `dispatch open` log theirs", n)
	}

	stepID := stepIDByInstance(t, conn, instance)
	var status, owner string
	err = conn.QueryRow(
		`SELECT status, COALESCE(owner, '') FROM steps WHERE id = ?`,
		stepID).Scan(&status, &owner)
	testsupport.Must(t, err, "reading the reaped step: %v", err)
	if status != string(db.StepPending) || owner != "" {
		t.Errorf("the reaped step is %q owned by %q, want %q and unowned",
			status, owner, db.StepPending)
	}

	// The hold the reap left, and the response that names it.
	acks := openReapsOf(t, conn, runID)
	if len(acks) != 1 {
		t.Fatalf("%d unacknowledged reaps after an extend reaped a bounded-class "+
			"step, want 1 — this is the hold the response must report", len(acks))
	}
	if x.ReapHold == "" {
		t.Fatal("the extend left an unacknowledged bounded-class reap and " +
			"reported no reap_hold; the next `guard spawn --active` denies on " +
			"it, and the verb that made the hold is the one that must name it")
	}
	if !strings.Contains(x.ReapHold, instance) {
		t.Errorf("reap_hold %q does not name the held instance %s",
			x.ReapHold, instance)
	}
	if !strings.Contains(x.ReapHold, "--ack-reap") {
		t.Errorf("reap_hold %q does not name --ack-reap, the way out of it",
			x.ReapHold)
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
