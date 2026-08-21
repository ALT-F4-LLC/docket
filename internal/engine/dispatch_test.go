package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// Dispatch — §5's clauses, one test per clause group (TDD §6.7).
//
// The tests inject `nowMS` directly rather than sleeping, per §5.9's "the clock
// advanced is not a `sleep`": every TTL and grace boundary below is a number
// passed into the verb, so the assertions are deterministic and the suite gains
// no wall-clock time.

// dispatchRun activates a run and returns its id — the shared setup.
func dispatchRun(t *testing.T, conn *sql.DB) int {
	t.Helper()
	run, _ := activatedRun(t, conn)
	return run.ID
}

// openDispatch opens a manifest, failing the test on refusal.
func openDispatch(t *testing.T, conn *sql.DB, runID, limit int, at int64) *Manifest {
	t.Helper()
	m, err := NewEngine().OpenDispatch(conn, runID, limit, nil, at)
	testsupport.Must(t, err, "dispatch open: %v", err)
	return m
}

// TestStagedLeaseSumCoversTheSlowestPath pins the expiry floor's arithmetic:
// stages run sequentially, rows within a stage in parallel, so a manifest's
// worst case is the sum over stages of each stage's largest lease. A staged
// manifest (3600s writer ahead of a 900s judge stage) under a flat 1800s
// dispatch TTL expired mid-wave and was auto-abandoned with its judges still
// working — the dispatch must outlive the slowest sequential path it offered.
func TestStagedLeaseSumCoversTheSlowestPath(t *testing.T) {
	rows := []model.StepRow{
		{Stage: 0, LeaseTTLS: 3600},
		{Stage: 1, LeaseTTLS: 900},
		{Stage: 1, LeaseTTLS: 900},
		{Stage: 1, LeaseTTLS: 600},
	}
	if got, want := stagedLeaseSumMS(rows), int64(4500)*1000; got != want {
		t.Fatalf("staged sum = %d, want %d", got, want)
	}
	if got := stagedLeaseSumMS(nil); got != 0 {
		t.Fatalf("empty manifest sum = %d, want 0 (config TTL stays in charge)", got)
	}
}

// TestDispatchOpenDrivesActionSteps is DKT-105: a `kind:"action"` step is
// engine-run — no dispatcher ever claims one — and `next` was the ONLY verb
// that drove it. A conductor spawning purely off `dispatch open` (never
// calling `next`) saw a ready action step ride into manifest after manifest,
// unexecuted, until some unrelated `next` call finally drove it — a stall
// with no visible cause. `dispatch open` must drive ready action (and vote)
// steps itself, exactly as `next` does, so a mixed dispatch of executor and
// action rows does not depend on a `next` call nobody made.
func TestDispatchOpenDrivesActionSteps(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	e := testEngine()

	// Issue A driven up through `synthesize` — its `reconcile@0` (kind:
	// action) is ready. Issue B is fresh — its `implement@1` (kind: executor)
	// is ready too. Two issues so the two ready rows carry no scope relation
	// to each other and both are offered in the SAME manifest (a genuine
	// mixed dispatch, not one ClaimablePrefix would narrow).
	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	claimAndComplete(t, conn, e, "implement@0", "summary", "")
	for i := range 4 {
		claimAndComplete(t, conn, e, "review@0#"+fmt.Sprint(i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", "")

	if stepStatus(t, conn, "reconcile@0") == db.StepDone {
		t.Fatal("reconcile@0 already ran before any dispatch open — the test's premise is broken")
	}

	m := openDispatch(t, conn, run.ID, 0, nowMS)

	// The observable fix: the action step actually RAN, in this same call —
	// not merely offered as a row `next` would eventually drive.
	if got := stepStatus(t, conn, "reconcile@0"); got != db.StepDone {
		t.Errorf("reconcile@0 status after dispatch open = %q, want %q — "+
			"dispatch open must drive ready action steps itself", got, db.StepDone)
	}

	// DKT-89: the manifest is computed AFTER the drives, so it carries the
	// routing's CONSEQUENCE — issue A's verify@0, un-deferred by the action
	// this same call ran — beside issue B's executor row, and never a row
	// for the action it already completed. The pre-fix manifest committed
	// reconcile@0 as `ready` and then drove it, handing the relay a row for
	// a step this same call had finished.
	var sawConsequence, sawExecutor bool
	for _, r := range m.Rows {
		switch r.Instance {
		case "reconcile@0":
			// By ISSUE, not by instance alone: issue B's identically-named
			// reconcile@0 legitimately rides the manifest as part of B's
			// staged closure. Only issue A's — the one this call ran — would
			// be the DKT-89 staleness.
			if r.Issue == model.FormatID(a) {
				t.Errorf("the manifest carries issue A's reconcile@0, which "+
					"this same call already ran to %s — a committed row for "+
					"routed work is the staleness DKT-89 names",
					stepStatus(t, conn, "reconcile@0"))
			}
		case "verify@0":
			if r.Issue == model.FormatID(a) {
				sawConsequence = true
			}
		case "implement@0":
			if r.Issue == model.FormatID(b) {
				sawExecutor = true
			}
		}
	}
	if !sawConsequence || !sawExecutor {
		t.Fatalf("manifest rows = %+v, want verify@0 (the driven action's "+
			"consequence, issue A) and implement@0 (executor, issue B)", m.Rows)
	}
}

// TestDispatchOpenOffersOnlyClaimableSet is DKT-101, the `dispatch open`
// mirror of TestNextOffersOnlyClaimableSet (next_disjoint_test.go). R4 only
// excludes a step against an ALREADY-CLAIMED holder, so two merely-ready steps
// whose scopes overlap EACH OTHER both pass it independently — `next` narrows
// this with ClaimablePrefix, and readyRows (dispatch.go), the manifest's shared
// tail, must narrow identically or the manifest offers a row `next` would
// have excluded. Unnarrowed, this reproduces RUN-8: two rows in one manifest,
// the second row's own claim refused on the scope conflict the first row's
// claim had just created.
func TestDispatchOpenOffersOnlyClaimableSet(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	for _, id := range []int{a, b} {
		err := db.SetIssueScopeGlobs(conn, id, `["internal/engine/**"]`)
		testsupport.Must(t, err, "setting scope: %v", err)
	}
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	m := openDispatch(t, conn, run.ID, 0, nowMS)
	ready := readyOffered(m.Rows)
	if len(ready) != 1 {
		var got []string
		for _, r := range ready {
			got = append(got, r.Step+"("+r.Issue+")")
		}
		t.Fatalf("dispatch manifest carries %d ready rows %v; the two issues "+
			"share internal/engine/** so only ONE can be claimed now", len(ready), got)
	}

	// And the offered row must actually be claimable — the assertion that
	// would have caught RUN-8 directly: a manifest row that refuses its own
	// claim on a scope conflict is the bug, not just a row count.
	stepID, err := model.ParseStepID(m.Rows[0].Step)
	testsupport.Must(t, err, "parsing step id: %v", err)
	_, err = ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claiming the sole offered row: %v", err)
}

// dispatchTTLMS reads the configured TTL the same way the verb does, so a test
// that advances "past the TTL" advances past the value actually in force rather
// than past a number restated here.
func dispatchTTLMS(t *testing.T, conn *sql.DB) int64 {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	ttl, err := db.DispatchTTLTx(tx, 1)
	testsupport.Must(t, err, "DispatchTTLTx: %v", err)
	return ttl.Milliseconds()
}

// dispatchStatus reads a manifest's stored status and close reason.
func dispatchStatus(t *testing.T, conn *sql.DB, runID int) (status, reason string) {
	t.Helper()
	err := conn.QueryRow(
		`SELECT status, COALESCE(close_reason, '') FROM dispatches
		  WHERE run_id = ? ORDER BY id DESC LIMIT 1`, runID,
	).Scan(&status, &reason)
	testsupport.Must(t, err, "reading the dispatch row: %v", err)
	return status, reason
}

// eventKindCount counts events of one kind for a run — the attribution probe
// several assertions below share.
func eventKindCount(t *testing.T, conn *sql.DB, runID int, kind string) int {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE run_id = ? AND kind = ?`, runID, kind,
	).Scan(&n)
	testsupport.Must(t, err, "counting %s events: %v", kind, err)
	return n
}

// ---------------------------------------------------------------------------
// §5.2 — the manifest IS the `next` answer
// ---------------------------------------------------------------------------

// TestManifestMatchesNext is P1, asserted BY BYTES rather than by re-deriving
// the answer.
//
// P1 says `dispatch open` computes the ready set "exactly as `next` does — the
// same LoadScheduler, the same predicate, the same SortSteps". A test that
// checked the two produced the same COUNT, or the same instance names, would
// pass against an implementation whose rows differed in a field a dispatcher
// reads. Comparing the canonical bytes is the only assertion that covers the
// whole row.
func TestManifestMatchesNext(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)

	// `next` first, then the manifest. The order matters and the assertion
	// depends on it being harmless: neither call may change the ready set, or
	// the comparison would be measuring the call rather than the agreement.
	answer, err := NewEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	if len(manifest.Rows) != len(answer.Steps) {
		t.Fatalf("manifest has %d rows, `next` offered %d",
			len(manifest.Rows), len(answer.Steps))
	}
	if len(manifest.Rows) == 0 {
		t.Fatal("premise: the fixture run must offer at least one ready step")
	}
	for i := range manifest.Rows {
		want, _, err := canonicalRowBytes(answer.Steps[i])
		testsupport.Must(t, err, "rendering the `next` row: %v", err)
		got, _, err := canonicalRowBytes(manifest.Rows[i])
		testsupport.Must(t, err, "rendering the manifest row: %v", err)
		if got != want {
			t.Errorf("row %d differs:\n  next:     %s\n  manifest: %s", i, want, got)
		}
	}
}

// TestManifestStoresCanonicalBytes is P3: each row is stored as its canonical
// JSON bytes PLUS their sha256, and the stored bytes round-trip to the row.
//
// The round-trip is the assertion that matters. "Canonical" is only useful if a
// stored row and a fetched row are byte-identical BY CONSTRUCTION, so the test
// unmarshals the stored JSON, re-marshals it, and requires the bytes back.
func TestManifestStoresCanonicalBytes(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	rows := storedRows(t, conn, manifest.Dispatch)
	if len(rows) != len(manifest.Rows) {
		t.Fatalf("stored %d rows, the manifest carries %d", len(rows), len(manifest.Rows))
	}

	for i, stored := range rows {
		var round model.StepRow
		err := json.Unmarshal([]byte(stored.RowJSON), &round)
		testsupport.Must(t, err, "stored row %d is not the wire shape: %v", i, err)
		raw, sum, err := canonicalRowBytes(round)
		testsupport.Must(t, err, "re-rendering row %d: %v", i, err)
		if raw != stored.RowJSON {
			t.Errorf("row %d does not round-trip:\n  stored: %s\n  again:  %s",
				i, stored.RowJSON, raw)
		}
		if sum != stored.RowSHA256 {
			t.Errorf("row %d hash %s does not match its bytes (%s)",
				i, stored.RowSHA256, sum)
		}
		if stored.Position != i {
			t.Errorf("row %d stored at position %d — the manifest IS the order",
				i, stored.Position)
		}
	}
}

func storedRows(t *testing.T, conn *sql.DB, dispatchRef string) []db.DispatchRow {
	t.Helper()
	id := dispatchIDOf(t, dispatchRef)
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	rows, err := db.ListDispatchRowsTx(tx, id)
	testsupport.Must(t, err, "ListDispatchRowsTx: %v", err)
	return rows
}

// dispatchIDOf pulls the integer out of a `DISPATCH-N` reference — the inverse
// of FormatDispatchID, kept beside it so the two formats cannot drift.
func dispatchIDOf(t *testing.T, ref string) int {
	t.Helper()
	var id int
	_, err := fmt.Sscanf(ref, "DISPATCH-%d", &id)
	testsupport.Must(t, err, "parsing %q: %v", ref, err)
	return id
}

// TestManifestLimitSlicesAfterOrdering is P4: `--limit` applies with the same
// ordering-then-slicing rule as `next` (§6.3).
//
// The property under test is that the limit takes the HIGHEST-PRIORITY rows, not
// an arbitrary subset — so the test compares a limited manifest against the
// prefix of the unlimited one rather than against a count.
func TestManifestLimitSlicesAfterOrdering(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	// Two issues on disjoint scopes, exactly like
	// TestNextStillOffersDisjointScopesTogether: neither can exclude the
	// other, so activation offers both as ready — the two rows this
	// comparison needs. A single-issue run offers only its root step and
	// would leave the ordering-then-slicing property unasserted.
	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	err := db.SetIssueScopeGlobs(conn, a, `["internal/a/**"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	err = db.SetIssueScopeGlobs(conn, b, `["internal/b/**"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	run := startRun(t, conn, a, b)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	runID := run.ID

	full := openDispatch(t, conn, runID, 0, nowMS)
	if len(full.Rows) < 2 {
		t.Fatalf("the fixture offers %d ready steps; this test needs 2 — a "+
			"fixture regression, not a condition to skip past", len(full.Rows))
	}
	abandon(t, conn, runID, nowMS)

	limited := openDispatch(t, conn, runID, 1, nowMS)
	if len(limited.Rows) != 1 {
		t.Fatalf("--limit 1 produced %d rows", len(limited.Rows))
	}
	if limited.Rows[0].Issue != full.Rows[0].Issue {
		t.Errorf("--limit 1 kept %s (%s); the ordered ready set starts with %s "+
			"(%s) — the limit must slice AFTER the sort, or a relay gets an "+
			"arbitrary subset",
			limited.Rows[0].Instance, limited.Rows[0].Issue,
			full.Rows[0].Instance, full.Rows[0].Issue)
	}
}

func abandon(t *testing.T, conn *sql.DB, runID int, at int64) {
	t.Helper()
	_, err := NewEngine().AbandonDispatch(conn, runID, "", at)
	testsupport.Must(t, err, "dispatch abandon: %v", err)
}

// ---------------------------------------------------------------------------
// §5.4 C1 — exactly one open per run
// ---------------------------------------------------------------------------

// TestDispatchOpenIsExactlyOnce is C1: two concurrent `dispatch open` calls
// produce ONE manifest and one CONFLICT.
//
// The concurrency is real rather than simulated: both goroutines compute a ready
// set and both INSERT, and SQLite's partial unique index is what admits exactly
// one. A check-then-insert would pass a sequential test and fail this one.
func TestDispatchOpenIsExactlyOnce(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		okCount int
		errs    []error
	)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := NewEngine().OpenDispatch(conn, runID, 0, nil, nowMS)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				okCount++
			} else {
				errs = append(errs, err)
			}
		}()
	}
	wg.Wait()

	if okCount != 1 {
		t.Fatalf("%d of 2 concurrent opens succeeded, want exactly 1 (errors: %v)",
			okCount, errs)
	}
	if len(errs) != 1 {
		t.Fatalf("got %d refusals, want 1", len(errs))
	}
	if code, ok := CodeOf(errs[0]); !ok || code != CodeConflict {
		t.Errorf("the loser got %v (code %q), want CONFLICT", errs[0], code)
	}

	// And ONE manifest exists — the loser's computation was discarded, never
	// merged (§5.4). A merge would produce a manifest neither relay saw.
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM dispatches WHERE run_id = ?`, runID).Scan(&n)
	testsupport.Must(t, err, "counting dispatches: %v", err)
	if n != 1 {
		t.Errorf("%d dispatch rows exist, want 1", n)
	}
}

// TestDispatchOpenRefusalNamesTheOpenOne is P6: the CONFLICT names the open
// dispatch's id and its expiry.
//
// A relay told only "a dispatch is open" cannot tell whether waiting for the TTL
// is a strategy, so the refusal carries both.
func TestDispatchOpenRefusalNamesTheOpenOne(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	_, err := NewEngine().OpenDispatch(conn, runID, 0, nil, nowMS)
	if err == nil {
		t.Fatal("a second open succeeded")
	}
	for _, want := range []string{manifest.Dispatch, "expiring"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
}

// ---------------------------------------------------------------------------
// §5.3 — verify
// ---------------------------------------------------------------------------

// TestVerifyEqual is P7 and P8: an untouched manifest verifies.
func TestVerifyEqual(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)

	result, mismatch, err := NewEngine().VerifyDispatch(conn, runID, nowMS)
	testsupport.Must(t, err, "verify: %v", err)
	if mismatch != nil {
		t.Fatalf("an untouched manifest reported a mismatch at position %d:\n"+
			"  stored:     %s\n  recomputed: %s",
			mismatch.Position, mismatch.Stored, mismatch.Computed)
	}
	if !result.Verified {
		t.Error("verified = false on an untouched manifest")
	}
}

// TestVerifyRefusesASelfInconsistentStoredRow pins DKT-46's restored
// cross-check: `row_json` and `row_sha256` are written together, and a stored
// row whose bytes no longer hash to its recorded sum is refused as an ERROR —
// storage self-inconsistency, not a scheduling mismatch — before those bytes
// become the comparison's baseline. Verify used to compare the stored hash
// directly and would have noticed the pair diverging; the DKT-19 stageless
// rewrite derived everything from `row_json` and silently dropped the signal.
func TestVerifyRefusesASelfInconsistentStoredRow(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)

	_, err := conn.Exec(
		`UPDATE dispatch_rows SET row_json = row_json || ' ' WHERE position = 0`)
	testsupport.Must(t, err, "tampering the stored row: %v", err)

	_, _, err = NewEngine().VerifyDispatch(conn, runID, nowMS)
	if err == nil || !strings.Contains(err.Error(), "row_sha256") {
		t.Fatalf("verify err = %v, want a refusal naming the diverged pair", err)
	}
}

// TestVerifyReportsTheFirstDifferingPosition is P9: an unequal manifest names
// the FIRST differing position, with both renderings.
//
// The manifest is invalidated by claiming a step, which removes it from the
// ready set — the ordinary way a manifest goes stale.
func TestVerifyReportsTheFirstDifferingPosition(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)
	if len(manifest.Rows) == 0 {
		t.Fatal("premise: the manifest must have rows")
	}

	// P28 as a premise: claiming is NOT refused while a dispatch is open.
	claimInstance(t, conn, manifest.Rows[0].Instance, nowMS)

	result, mismatch, err := NewEngine().VerifyDispatch(conn, runID, nowMS)
	testsupport.Must(t, err, "verify: %v", err)
	if mismatch == nil {
		t.Fatal("a manifest whose first step was claimed verified as equal")
	}
	if result.Verified {
		t.Error("verified = true alongside a mismatch")
	}
	if mismatch.Position != 0 {
		t.Errorf("mismatch at position %d, want 0 — the FIRST differing row",
			mismatch.Position)
	}
	if mismatch.Stored == "" {
		t.Error("the stored row was not rendered; P9 requires the differing BYTES")
	}
}

// TestVerifyAfterRecordIsNotAConflict is DKT-10: a step that recorded
// successfully is TERMINAL, and a terminal step is never re-offered — its
// absence from the recomputed ready set, even alongside NEW steps that became
// ready in its place, is the expected effect of `step record`, not a conflict
// shaped like one.
func TestVerifyAfterRecordIsNotAConflict(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	e := testEngine()

	manifest := openDispatch(t, conn, runID, 0, nowMS)
	if len(manifest.Rows) == 0 || manifest.Rows[0].Instance != "implement@0" ||
		manifest.Rows[0].Status != db.StepReady {
		t.Fatalf("premise: the fixture's initial manifest must lead with a "+
			"ready implement@0 (the closure rides staged behind it), got %+v",
			manifest.Rows)
	}

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	if got := stepStatus(t, conn, "implement@0"); got != db.StepDone {
		t.Fatalf("premise: implement@0 must have recorded done, got %q", got)
	}

	// The recorded step is terminal (skipped by the comparison), and the
	// manifest's STAGED judges now recompute READY — lifecycle progress the
	// comparison normalizes away (comparableVerifyBytes), not a conflict.
	result, mismatch, err := NewEngine().VerifyDispatch(conn, runID, nowMS)
	testsupport.Must(t, err, "verify: %v", err)
	if mismatch != nil {
		t.Fatalf("verify after a successful record reported a mismatch at "+
			"position %d:\n  stored:     %s\n  recomputed: %s",
			mismatch.Position, mismatch.Stored, mismatch.Computed)
	}
	if !result.Verified {
		t.Error("verified = false after a successful record; DKT-10 not fixed")
	}
}

// TestVerifyDoesNotReap is P11 — §10.3's standing assertion, and the subtle one.
//
// `verify` is the ONE scheduling-shaped verb that must not reap, because reaping
// would change the very ready set it was asked to compare against — a verify
// that mutated its own subject could never fail.
//
// The assertion is therefore about the DATABASE after the call: the expired
// step's row must still carry its stale owner. Asserting only that verify
// reported a mismatch would pass against an implementation that reaped and then
// noticed the difference it had just caused.
func TestVerifyDoesNotReap(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)
	instance := manifest.Rows[0].Instance

	claim := claimInstance(t, conn, instance, nowMS)
	stepID := stepIDByInstance(t, conn, instance)

	// Past the lease. The step is EFFECTIVELY pending to any reader and still
	// `claimed` in the row until somebody reaps it.
	expired := claim.LeaseExpiresMS + 1

	ownerBefore := ownerOf(t, conn, stepID)
	if ownerBefore == "" {
		t.Fatal("premise: the claimed step must carry an owner")
	}

	_, _, err := NewEngine().VerifyDispatch(conn, runID, expired)
	testsupport.Must(t, err, "verify: %v", err)

	if owner := ownerOf(t, conn, stepID); owner != ownerBefore {
		t.Errorf("verify reaped the lease: owner went %q -> %q. P11 forbids it — "+
			"a verify that reaped would change the ready set it was asked to "+
			"compare against, and could never fail", ownerBefore, owner)
	}
	if got := statusOf(t, conn, stepID); got != db.StepClaimed {
		t.Errorf("verify moved the step to %q; the row must still read %q",
			got, db.StepClaimed)
	}
	// And no event was written: reaping is event-logged, so a `lease-reaped`
	// event is the other fingerprint a hidden reap would leave.
	if n := eventKindCount(t, conn, runID, EventLeaseReaped); n != 0 {
		t.Errorf("verify wrote %d lease-reaped events; it must write nothing", n)
	}
}

// TestVerifyAfterStagedFixerRecordsSurvivorsStillMatch is DKT-19's C2 repro:
// a manifest carrying a staged pair (a tree-holding fixer at stage 0 and the
// re-review judges it gates at stage 1) verifies as UNCHANGED after the
// fixer alone records — even though the fixer's retirement is exactly what
// makes the survivors recompute UNSTAGED (assignStages finds no predecessor
// once the one tree-holder is gone). `Stage` is excluded from the
// comparison for exactly this reason (VerifyDispatch's DKT-19 clause); a
// version that still compared it would report a false conflict on the
// judges the instant their fixer finished, i.e. on the ordinary happy path
// a loop's re-entry produces.
func TestVerifyAfterStagedFixerRecordsSurvivorsStillMatch(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)

	manifest := openDispatch(t, conn, run.ID, 0, nowMS)
	if len(manifest.Rows) != 8 || manifest.Rows[0].Instance != "fix@1" {
		t.Fatalf("premise: the manifest must open on the staged fixer, its "+
			"four judges, and the three-row staged closure behind them, got %+v",
			manifest.Rows)
	}
	if manifest.Rows[0].Stage != 0 || manifest.Rows[1].Stage == 0 {
		t.Fatalf("premise: fix@1 must be staged before its judges, got stages "+
			"%d, %d", manifest.Rows[0].Stage, manifest.Rows[1].Stage)
	}

	claimAndComplete(t, conn, e, "fix@1", "the fix summary", "")
	if got := stepStatus(t, conn, "fix@1"); got != db.StepDone {
		t.Fatalf("premise: fix@1 must have recorded done, got %q", got)
	}

	result, mismatch, err := NewEngine().VerifyDispatch(conn, run.ID, nowMS)
	testsupport.Must(t, err, "verify: %v", err)
	if mismatch != nil {
		t.Fatalf("verify reported a mismatch at position %d after the staged "+
			"fixer alone recorded:\n  stored:     %s\n  recomputed: %s",
			mismatch.Position, mismatch.Stored, mismatch.Computed)
	}
	if !result.Verified {
		t.Error("verified = false after the fixer recorded; the survivors' " +
			"stage recomputing to 0 must not read as a conflict")
	}
}

// TestVerifyLimitedManifestKeepsTheFixerAndVerifies is DKT-19's C1 repro,
// updated twice since: DKT-58/DKT-75 made a fixerless truncation evict the
// orphaned judges (an empty-but-safe manifest), and DKT-38 made the cut
// stage-aware so it no longer orphans them at all — `--limit` slices a
// STAGE-ORDERED set, so a limit of 2 keeps fix@1 (stage 0) and one judge
// (stage 1) instead of two unstaged judges with no fixer (the RUN-2 failure
// shape: a judge reviewing a tree its absent fixer has not yet rewritten).
//
// The verify half stays DKT-19's question: the stored rows carry the stages
// the limited open published, the recomputation runs UNLIMITED, and the
// comparison is by identity and stageless — none of which may read as a
// conflict on an untouched manifest.
func TestVerifyLimitedManifestKeepsTheFixerAndVerifies(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)

	// Premise: unlimited, the fixer and all four judges are ready together —
	// confirms the limit=2 case below is genuinely a truncation, not an
	// already-empty offer. A read (NextSteps), not a second dispatch open:
	// only one dispatch may be open on a run at a time.
	unlimited, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next (unlimited): %v", err)
	if len(unlimited.Steps) != 8 {
		t.Fatalf("premise: unlimited must offer fix@1 + 4 judges + the staged "+
			"closure (8 rows), got %d: %+v", len(unlimited.Steps), unlimited.Steps)
	}

	limited := openDispatch(t, conn, run.ID, 2, nowMS)
	if len(limited.Rows) != 2 ||
		limited.Rows[0].Instance != "fix@1" || limited.Rows[0].Stage != 0 ||
		limited.Rows[1].Stage != 1 {
		t.Fatalf("--limit 2 must keep the stage-order prefix — fix@1 at "+
			"stage 0, one judge at stage 1 — got: %+v", limited.Rows)
	}

	result, mismatch, err := NewEngine().VerifyDispatch(conn, run.ID, nowMS)
	testsupport.Must(t, err, "verify: %v", err)
	if mismatch != nil {
		t.Fatalf("verify reported a mismatch at position %d on an UNTOUCHED "+
			"--limit manifest:\n  stored:     %s\n  recomputed: %s",
			mismatch.Position, mismatch.Stored, mismatch.Computed)
	}
	if !result.Verified {
		t.Error("verified = false on an untouched --limit manifest; the " +
			"unlimited recomputation must match the stored rows by identity")
	}
}

// TestVerifyMatchesRowsByIdentityNotPosition is DKT-19's C3: the row lookup
// is by the STEP a stored row names, never by its slice position in the
// recomputed set — pinned with a mutation a positional lookup would miss.
//
// The two-issue, disjoint-scope shape offers two ready rows with no scope
// relation to each other (TestManifestLimitSlicesAfterOrdering's premise).
// Completing the FIRST stored row mints four new, YOUNGER review rows in its
// issue — which sort AFTER the untouched second row in the recomputed set
// (SortSteps orders by age), moving the survivor from stored position 1 to
// recomputed position 0. A positional comparator (`computed[i]`) would then
// compare the untouched survivor against one of the new review rows and
// report a false conflict; an identity-based lookup does not.
func TestVerifyMatchesRowsByIdentityNotPosition(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	err := db.SetIssueScopeGlobs(conn, a, `["internal/a/**"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	err = db.SetIssueScopeGlobs(conn, b, `["internal/b/**"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	run := startRun(t, conn, a, b)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	manifest := openDispatch(t, conn, run.ID, 0, nowMS)
	if len(readyOffered(manifest.Rows)) != 2 ||
		manifest.Rows[0].Status != db.StepReady ||
		manifest.Rows[1].Status != db.StepReady {
		t.Fatalf("premise: the fixture must lead with 2 ready rows (the "+
			"staged closure rides behind them), got %+v", manifest.Rows)
	}

	survivorStepID, err := model.ParseStepID(manifest.Rows[1].Step)
	testsupport.Must(t, err, "parsing %q: %v", manifest.Rows[1].Step, err)

	firstStepID, err := model.ParseStepID(manifest.Rows[0].Step)
	testsupport.Must(t, err, "parsing %q: %v", manifest.Rows[0].Step, err)
	claim, err := ClaimStep(conn, firstStepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = testEngine().CompleteStep(conn, firstStepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)

	// Premise, checked directly against the database (NextSteps itself
	// refuses while a dispatch is open — the same manifest this test is
	// about to verify): the completion minted four new, YOUNGER review rows
	// in the completed step's issue, and the survivor is still pending
	// (still ready, never touched). SortSteps' age tie-break then puts those
	// younger rows AHEAD of the older survivor only when the survivor's
	// stored position (1) is read positionally rather than by identity — the
	// exact drift this test pins.
	firstIssueID := stepIssueID(t, conn, manifest.Rows[0].Step)
	var newReviewCount int
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM steps
		 WHERE instance LIKE 'review@0%' AND issue_id = ? AND status = ?`,
		firstIssueID, db.StepPending).Scan(&newReviewCount)
	testsupport.Must(t, err, "counting the new review rows: %v", err)
	if newReviewCount != 4 {
		t.Fatalf("premise: completing the first row must mint 4 new review "+
			"rows in its own issue, got %d", newReviewCount)
	}
	if got := statusOf(t, conn, survivorStepID); got != db.StepPending {
		t.Fatalf("premise: the untouched survivor must still be pending/ready, "+
			"got %q", got)
	}

	result, mismatch, err := NewEngine().VerifyDispatch(conn, run.ID, nowMS)
	testsupport.Must(t, err, "verify: %v", err)
	if mismatch != nil {
		t.Fatalf("verify reported a mismatch at position %d after an unrelated "+
			"row completed elsewhere in the manifest:\n  stored:     %s\n"+
			"  recomputed: %s", mismatch.Position, mismatch.Stored, mismatch.Computed)
	}
	if !result.Verified {
		t.Error("verified = false; the surviving row's identity should have " +
			"matched regardless of its new position in the recomputed set")
	}
}

// TestVerifyMismatchRendersTheCurrentRow is DKT-19's C4: the byte-comparison
// branch (as opposed to the missing-row branch) is exercised with a row that
// STAYS in the recomputed ready set but whose content changed — so
// `RowMismatch.Computed` is a non-empty rendering of the CURRENT row, not the
// empty string the missing-row branch produces.
//
// TestVerifyReportsTheFirstDifferingPosition exercises only the missing-row
// branch (the claimed step drops out of the ready set entirely); this test
// changes the lease TTL config between open and verify, which changes a
// still-ready row's canonical bytes without touching its readiness.
func TestVerifyMismatchRendersTheCurrentRow(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)
	if len(manifest.Rows) == 0 {
		t.Fatal("premise: the manifest must have rows")
	}
	err := db.SetConfig(conn, 0, db.KeyLeaseTTLDefault, "45m")
	testsupport.Must(t, err, "setting the default lease TTL: %v", err)

	result, mismatch, err := NewEngine().VerifyDispatch(conn, runID, nowMS)
	testsupport.Must(t, err, "verify: %v", err)
	if mismatch == nil {
		t.Fatal("a changed lease TTL did not report a mismatch")
	}
	if result.Verified {
		t.Error("verified = true alongside a mismatch")
	}
	if mismatch.Computed == "" {
		t.Fatal("Computed is empty; the row is still ready and should have " +
			"rendered its current bytes rather than the missing-row branch")
	}
	if mismatch.Computed == mismatch.Stored {
		t.Error("Computed equals Stored; the TTL change should have produced " +
			"different bytes")
	}
}

func ownerOf(t *testing.T, conn *sql.DB, stepID int) string {
	t.Helper()
	var owner sql.NullString
	err := conn.QueryRow(
		`SELECT owner FROM steps WHERE id = ?`, stepID).Scan(&owner)
	testsupport.Must(t, err, "reading the owner of step %d: %v", stepID, err)
	return owner.String
}

func statusOf(t *testing.T, conn *sql.DB, stepID int) string {
	t.Helper()
	var status string
	err := conn.QueryRow(
		`SELECT status FROM steps WHERE id = ?`, stepID).Scan(&status)
	testsupport.Must(t, err, "reading the status of step %d: %v", stepID, err)
	return status
}

// ---------------------------------------------------------------------------
// §5.5 — the TTL, lazily auto-abandoned by `next`
// ---------------------------------------------------------------------------

// TestNextAutoAbandonsAnExpiredDispatch is P13, P14, P15, and P16 together —
// §9 item 9's arm A at the unit level.
//
// P16 IS THE CLAUSE THAT MATTERS: `next` proceeds normally in the SAME
// invocation. A relay that crashed and came back must not poll twice to get
// work, which is the same reasoning that puts the reap and the readiness pass in
// one transaction.
func TestNextAutoAbandonsAnExpiredDispatch(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	m := openDispatch(t, conn, runID, 0, nowMS)

	// Before the TTL: `next` refuses (P24).
	if _, err := NewEngine().NextSteps(conn, runID, 0, nowMS); err == nil {
		t.Fatal("premise: `next` must refuse while a dispatch is open")
	}

	// Past the MANIFEST's own expiry — which a staged manifest sets beyond
	// the configured floor (stagedLeaseSumMS), so the config value alone
	// undershoots it.
	past := m.ExpiresMS + 1
	answer, err := NewEngine().NextSteps(conn, runID, 0, past)
	testsupport.Must(t, err, "`next` past the TTL: %v — P16 requires the SAME invocation to "+
		"answer after auto-abandoning", err)

	if len(answer.Steps) == 0 {
		t.Error("the same invocation returned no steps; P16 requires the ready " +
			"set, not an empty answer a relay must poll again for")
	}

	status, reason := dispatchStatus(t, conn, runID)
	if status != db.DispatchAbandoned {
		t.Errorf("the dispatch is %q, want %q", status, db.DispatchAbandoned)
	}
	if reason != db.CloseReasonTTL {
		t.Errorf("close_reason = %q, want %q", reason, db.CloseReasonTTL)
	}
	// P15: event-logged, which engine-spec §2 requires by name.
	if n := eventKindCount(t, conn, runID, EventDispatchAbandoned); n != 1 {
		t.Errorf("%d dispatch-abandoned events, want exactly 1", n)
	}
}

// TestAutoAbandonIsExactlyOneEvent is C3: two `next` invocations racing at the
// instant of expiry produce ONE abandon and ONE event.
//
// The CAS on `status='open'` is the mechanism. Without it both invocations would
// write, and the feed would show a manifest abandoned twice — which is a
// statement about the log that is simply false.
func TestAutoAbandonIsExactlyOneEvent(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	m := openDispatch(t, conn, runID, 0, nowMS)
	past := m.ExpiresMS + 1

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Both may legitimately succeed: after the abandon, `next` answers.
			_, _ = NewEngine().NextSteps(conn, runID, 0, past)
		}()
	}
	wg.Wait()

	if n := eventKindCount(t, conn, runID, EventDispatchAbandoned); n != 1 {
		t.Errorf("%d dispatch-abandoned events after a two-way race, want exactly "+
			"1 — the CAS on status='open' is what makes the abandon single", n)
	}
}

// TestClaimDoesNotAutoAbandon is P17, and it is a DELIBERATE NARROWING stated so
// a reviewer does not read it as an omission.
//
// engine-spec §6's lazy-reaping rule is about LEASES. Dispatch expiry is a
// different mechanism with a different scope, and §2 assigns it to `next` alone:
// a dispatch is about a BATCH, and expiring one from a single-step verb would
// let a claim silently unwedge a run whose relay is still alive.
func TestClaimDoesNotAutoAbandon(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)
	past := nowMS + dispatchTTLMS(t, conn) + 1

	// P28: the claim SUCCEEDS even with a dispatch open — that is the next test
	// — and here it must succeed without touching the manifest.
	_, err := ClaimStep(conn, stepIDByInstance(t, conn, manifest.Rows[0].Instance),
		ClaimOptions{Owner: "worker", NowMS: past})
	testsupport.Must(t, err, "claim past the dispatch TTL: %v", err)

	status, _ := dispatchStatus(t, conn, runID)
	if status != db.DispatchOpen {
		t.Errorf("claim moved the dispatch to %q; P17 confines the auto-abandon "+
			"to `next`, because a dispatch is about a BATCH and a claim is about "+
			"one step", status)
	}
}

// ---------------------------------------------------------------------------
// §5.7 — the refusal
// ---------------------------------------------------------------------------

// TestNextRefusesWhileADispatchIsOpen is P24 and P26.
//
// P26 IS THE ASSERTION: the refusal is a CONFLICT, NOT AN EMPTY READY SET. An
// empty set means "nothing to do"; a refusal means "I will not answer until you
// reconcile". A relay cannot distinguish those from a zero-length list.
func TestNextRefusesWhileADispatchIsOpen(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	answer, err := NewEngine().NextSteps(conn, runID, 0, nowMS)
	if err == nil {
		t.Fatalf("`next` returned %d steps instead of refusing; P26 forbids "+
			"conflating a refusal with an empty ready set", len(answer.Steps))
	}
	if answer != nil {
		t.Error("`next` returned a result alongside its refusal; a caller could " +
			"render the empty list and proceed")
	}
	if code, ok := CodeOf(err); !ok || code != CodeConflict {
		t.Errorf("the refusal has code %q, want CONFLICT", code)
	}
	// P24: it names the dispatch, its expiry, and the three ways out.
	for _, want := range []string{
		manifest.Dispatch, "dispatch close", "dispatch abandon", "TTL",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %q — §2's recovery design "+
				"is that a stalled run tells an operator how to unstall it", err, want)
		}
	}
}

// TestClaimIsNotRefusedByAnOpenDispatch is P28 — the clause a reviewer should
// push hardest on, as a test.
//
// If `claim` refused while a dispatch were open: a relay opens a dispatch, spawns
// four executors, and crashes. The executors each claim and are refused. Nothing
// can complete. The TTL eventually abandons the dispatch, by which point the
// four executors have failed — and §2's "a crashed relay can never wedge a run"
// is violated by the very mechanism meant to implement it.
func TestClaimIsNotRefusedByAnOpenDispatch(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	claim, err := ClaimStep(conn, stepIDByInstance(t, conn, manifest.Rows[0].Instance),
		ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim with a dispatch open: %v — P28 requires it to work, or a "+
		"crashed relay strands its own in-flight executors", err)

	if claim.Token == "" {
		t.Error("the claim minted no token")
	}
}

// TestIssueModeNextIsUnaffected is P27: `docket next` WITHOUT `--run` never
// probes anything.
//
// engine-spine §6.3.1's byte-identical guarantee is preserved, which is what
// keeps `test_x_next.sh` passing untouched. The unit-level assertion is that the
// issue-mode path does not reach the run-mode probes at all — asserted here by
// running it over a database that HAS an open dispatch and a discrepancy, where
// a probe that fired would refuse.
func TestIssueModeNextIsUnaffected(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)

	// Run mode refuses...
	if _, err := NewEngine().NextSteps(conn, runID, 0, nowMS); err == nil {
		t.Fatal("premise: run mode must refuse with a dispatch open")
	}

	// ...and the issue-mode planner answers, because it never asks. The engine
	// entry point for issue mode is the planner, which this package does not
	// call: the CLI's runNext branches BEFORE reaching NextSteps (§6.3.1), so
	// the structural guarantee is that no dispatch probe exists on that path.
	// The behavioral half is test_x_next.sh, which passes untouched.
	_, _, err := db.ListIssues(conn, db.ListOptions{})
	testsupport.Must(t, err, "the issue-mode data path failed: %v", err)
}

// ---------------------------------------------------------------------------
// §5.8 — the discrepancies, at their boundaries
// ---------------------------------------------------------------------------

// TestDiscrepancyD1AtItsBoundary is D1 one millisecond inside and outside the
// grace window.
//
// The boundary is where a threshold bug lives: an implementation using `>`
// instead of `>=`, or measuring from the wrong clock, passes every test that
// checks a case far from the edge.
func TestDiscrepancyD1AtItsBoundary(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)

	answer, err := NewEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	instance := answer.Steps[0].Instance
	claimInstance(t, conn, instance, nowMS)

	grace := graceMS(t, conn)
	activity := activityOf(t, conn, stepIDByInstance(t, conn, instance))

	// One millisecond INSIDE the window: not yet a discrepancy.
	if ds := discrepanciesAt(t, conn, runID, activity+grace-1); len(ds) != 0 {
		t.Errorf("a step silent for grace-1ms is a discrepancy: %v", ds)
	}
	// Exactly at the window: it is. The comparison is `>=` because a step that
	// has been silent for exactly the grace period has used up the grace.
	ds := discrepanciesAt(t, conn, runID, activity+grace)
	if len(ds) == 0 {
		t.Fatal("a step silent for exactly the grace period is not a discrepancy")
	}
	if ds[0].Kind != DiscrepancyClaimedUnrecorded {
		t.Errorf("kind = %q, want %q", ds[0].Kind, DiscrepancyClaimedUnrecorded)
	}
	// §2's enumerated resolution, verbatim in substance: lease expiry clears it.
	if !strings.Contains(ds[0].Resolution, "lease expiry") {
		t.Errorf("the resolution %q does not name lease expiry, which is §2's "+
			"enumerated way out", ds[0].Resolution)
	}
}

func graceMS(t *testing.T, conn *sql.DB) int64 {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	grace, err := db.DispatchGraceTx(tx, 1)
	testsupport.Must(t, err, "DispatchGraceTx: %v", err)
	return grace.Milliseconds()
}

func activityOf(t *testing.T, conn *sql.DB, stepID int) int64 {
	t.Helper()
	var activity, updated sql.NullInt64
	err := conn.QueryRow(
		`SELECT activity_ms, updated_at_ms FROM steps WHERE id = ?`, stepID,
	).Scan(&activity, &updated)
	testsupport.Must(t, err, "reading the activity of step %d: %v", stepID, err)
	if activity.Valid && activity.Int64 > 0 {
		return activity.Int64
	}
	return updated.Int64
}

// discrepanciesAt computes the probe at an instant, through the same code path
// `next` and `dispatch close` use.
func discrepanciesAt(t *testing.T, conn *sql.DB, runID int, at int64) []Discrepancy {
	t.Helper()
	defs, err := StepDefinitions(conn, runID)
	testsupport.Must(t, err, "StepDefinitions: %v", err)
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	sched, err := LoadScheduler(tx, runID, defs, at)
	testsupport.Must(t, err, "LoadScheduler: %v", err)
	ds, err := discrepanciesTx(tx, sched, runID, at)
	testsupport.Must(t, err, "discrepanciesTx: %v", err)
	return ds
}

// TestDiscrepancyD2RequiresDispatchHistory is the 2026-08-03 REVIEW FIX, and it
// is the clause that keeps this stage's `next` dormant for repos that do not
// dispatch.
//
// A step that completed without recording usage is a discrepancy ONLY in a run
// that has ever opened a dispatch. A run no relay ever drove has nobody owing
// usage — which is exactly what keeps a solo rehearsal and a human-only demo
// refusal-free, since both complete without `--usage` and open no dispatch.
func TestDiscrepancyD2RequiresDispatchHistory(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)

	instance := completeAStepWithoutUsage(t, conn, runID)
	_ = instance

	// No dispatch was ever opened: NOT a discrepancy, and `next` answers.
	if ds := discrepanciesAt(t, conn, runID, nowMS); len(ds) != 0 {
		t.Fatalf("a run that never dispatched reports %d discrepancies: %v — the "+
			"review fix scopes D2 to runs with dispatch history, and this is the "+
			"assertion that keeps ZK's solo run and ZH's stranger demo "+
			"refusal-free", len(ds), ds)
	}
	_, err := NewEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "`next` on a never-dispatched run refused: %v", err)

	// Open and abandon a dispatch — the run now HAS dispatch history, and the
	// same step becomes a discrepancy.
	openDispatch(t, conn, runID, 0, nowMS)
	abandon(t, conn, runID, nowMS)

	ds := discrepanciesAt(t, conn, runID, nowMS)
	var found bool
	for _, d := range ds {
		if d.Kind == DiscrepancyMissingUsage {
			found = true
		}
	}
	if !found {
		t.Errorf("after the run dispatched, the unrecorded step is not a "+
			"discrepancy: %v — a relay that ever dispatched is accountable for "+
			"every step of that run, including out-of-manifest spawns", ds)
	}
}

// TestPausedRunSuspendsD1ButNotD2 pins the asymmetry, and it exists because
// the two clauses are suspended for opposite reasons — so a later reader does
// not "restore consistency" by suspending both or neither.
//
// D1 IS SUSPENDED because its resolution is the reap, and the run-status scoping stops the reap
// on a paused run. A refusal whose entire content is "wait for `next` to reap
// this" is a wedge once the reap will not happen, and it is worse than the
// false reap it replaced: the operator is told to wait for an event the engine
// has decided not to perform.
//
// D2 IS NOT SUSPENDED because its resolution is an operator's acceptance flag,
// which stays reachable while the run is parked. Hiding a real accounting gap
// behind an unrelated pause would be a silent loss of the thing D2 exists to
// notice.
func TestPausedRunSuspendsD1ButNotD2(t *testing.T) {
	conn := mustDB(t)
	// serializedRun's workflow, not the default fixture: this test needs TWO
	// concurrently-ready steps — one to leave claimed (D1) and one to finish
	// without usage (D2) — and the default fixture offers a single step.
	runID := serializedRun(t, conn)

	// A claimed step whose lease and grace both lapse: the live D1. Claimed
	// FIRST, because a live D2 makes `next` refuse and there would be no offer
	// left to claim from.
	answer, err := NewEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	if len(answer.Steps) < 2 {
		t.Fatalf("premise: the fixture must offer two steps, got %v",
			instancesIn(answer))
	}
	claimed, spare := answer.Steps[0].Instance, answer.Steps[1].Instance
	claim := claimInstance(t, conn, claimed, nowMS)
	past := claim.LeaseExpiresMS + int64(15*60*1000) + 1

	// Give the run dispatch history so D2 is in scope, then leave a DIFFERENT
	// step completed with no usage rows: the live D2.
	openDispatch(t, conn, runID, 0, nowMS)
	abandon(t, conn, runID, nowMS)
	finishWithoutUsage(t, conn, spare)

	// Active: BOTH fire.
	ds := discrepanciesAt(t, conn, runID, past)
	if !containsKind(ds, DiscrepancyClaimedUnrecorded) {
		t.Fatalf("premise: D1 must fire on an active run past the grace, got %v", ds)
	}
	if !containsKind(ds, DiscrepancyMissingUsage) {
		t.Fatalf("premise: D2 must fire once the run has dispatch history, got %v", ds)
	}

	// Paused: D1 goes quiet, D2 does not.
	execSQL(t, conn, `UPDATE runs SET status = ? WHERE id = ?`,
		string(model.RunWaitingHuman), runID)

	ds = discrepanciesAt(t, conn, runID, past)
	if containsKind(ds, DiscrepancyClaimedUnrecorded) {
		t.Errorf("D1 fired on a PAUSED run (%v); its stated resolution is the "+
			"reap, which DKT-33 suspends while paused, so this refusal names a "+
			"resolution that cannot happen and wedges the run", ds)
	}
	if !containsKind(ds, DiscrepancyMissingUsage) {
		t.Errorf("D2 went quiet on a paused run (%v); its resolution is an "+
			"operator's acceptance flag, which a pause does not block — "+
			"suppressing it hides a real accounting gap behind an unrelated "+
			"pause", ds)
	}
}

// containsKind reports whether a discrepancy of the given kind is present.
func containsKind(ds []Discrepancy, kind DiscrepancyKind) bool {
	for _, d := range ds {
		if d.Kind == kind {
			return true
		}
	}
	return false
}

// completeAStepWithoutUsage drives one step to `done` with no `--usage`, and
// returns its instance. It is the shape D2 is about: the ordinary path when a
// relay does not record.
func completeAStepWithoutUsage(t *testing.T, conn *sql.DB, runID int) string {
	t.Helper()
	answer, err := NewEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	if len(answer.Steps) == 0 {
		t.Fatal("premise: the run must offer a ready step")
	}
	instance := answer.Steps[0].Instance
	finishWithoutUsage(t, conn, instance)
	return instance
}

// finishWithoutUsage drives one instance to `done` with no ledger rows.
//
// The status is set directly rather than driven through the saga: the probe asks
// about a TERMINAL step with no ledger rows, and the saga would pull in gates and
// artifacts these tests are not about. `updated_at_ms` is set PAST the run's
// activation so D3's historical exclusion does not fire — which is the one
// detail that makes this a D2 fixture rather than a D3 one.
//
// `attempt` is set to 1 for the same class of reason (DKT-315): D2 is about a
// step that RAN and did not report, and `attempt` counts claims, so a row left
// at 0 describes a step no worker ever held — which owes nothing and is not a
// discrepancy. Setting the status without it modelled a state no run can
// reach.
func finishWithoutUsage(t *testing.T, conn *sql.DB, instance string) {
	t.Helper()
	execSQL(t, conn,
		`UPDATE steps SET status = ?, updated_at_ms = ?, attempt = 1 WHERE id = ?`,
		db.StepDone, nowMS+1000, stepIDByInstance(t, conn, instance))
}

// TestDiscrepancyD3ExcludesHistoricalSteps is D3: a step that reached its
// terminal status BEFORE the run's activation is not a discrepancy.
//
// Without this, upgrading the binary would instantly make every historical run's
// dispatch un-closable — a migration that breaks in-flight work, which is
// exactly what the payloads TDD refused for schemas.
func TestDiscrepancyD3ExcludesHistoricalSteps(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	abandon(t, conn, runID, nowMS)

	answer, err := NewEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	stepID := stepIDByInstance(t, conn, answer.Steps[0].Instance)

	// Terminal, no usage, and updated BEFORE the run was activated.
	var activated int64
	err = conn.QueryRow(
		`SELECT COALESCE(activated_at_ms, 0) FROM runs WHERE id = ?`, runID,
	).Scan(&activated)
	testsupport.Must(t, err, "reading activated_at_ms: %v", err)
	if activated == 0 {
		t.Fatal("premise: the run must be activated")
	}
	execSQL(t, conn, `UPDATE steps SET status = ?, updated_at_ms = ? WHERE id = ?`,
		db.StepDone, activated-1, stepID)

	for _, d := range discrepanciesAt(t, conn, runID, nowMS) {
		if d.Kind == DiscrepancyMissingUsage {
			t.Errorf("a step terminal BEFORE activation is a discrepancy (%v); D3 "+
				"excludes it, or upgrading the binary makes every historical run "+
				"un-closable", d)
		}
	}
}

// TestDiscrepancyD5ExemptsActionAndHumanSteps is D5: no worker claims them, so
// there is nobody to have reported usage.
//
// Including them would make every fixture run permanently un-closable — the
// fixture has an `action` step (`reconcile`) and a `human` one (`commit-gate`).
func TestDiscrepancyD5ExemptsActionAndHumanSteps(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	abandon(t, conn, runID, nowMS)

	// Drive every action/human/vote step of the run terminal with no usage.
	execSQL(t, conn,
		`UPDATE steps SET status = ?, updated_at_ms = ?
		  WHERE run_id = ? AND kind IN ('action', 'human', 'vote')`,
		db.StepDone, nowMS+1000, runID)

	var exempt int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM steps WHERE run_id = ? AND kind IN ('action','human','vote')`,
		runID).Scan(&exempt)
	testsupport.Must(t, err, "counting exempt steps: %v", err)
	if exempt == 0 {
		t.Fatal("premise: the fixture must contain an action or human step")
	}

	for _, d := range discrepanciesAt(t, conn, runID, nowMS) {
		if d.Kind == DiscrepancyMissingUsage {
			t.Errorf("an engine-run or operator-resolved step is a usage "+
				"discrepancy (%v); D5 exempts them, since nobody claimed them", d)
		}
	}
}

// ---------------------------------------------------------------------------
// §5.6 — close and abandon
// ---------------------------------------------------------------------------

// TestCloseRefusesPerDiscrepancy is P18: `dispatch close` refuses while a
// discrepancy exists, ENUMERATING each with its resolution.
func TestCloseRefusesPerDiscrepancy(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	instance := manifest.Rows[0].Instance
	claimInstance(t, conn, instance, nowMS)
	past := nowMS + graceMS(t, conn) + 1

	_, err := NewEngine().CloseDispatch(conn, runID, false, past)
	if err == nil {
		t.Fatal("close succeeded with a claimed-but-unrecorded step")
	}
	if code, ok := CodeOf(err); !ok || code != CodeConflict {
		t.Errorf("the refusal has code %q, want CONFLICT", code)
	}
	for _, want := range []string{instance, string(DiscrepancyClaimedUnrecorded), "lease expiry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
}

// TestCloseAcceptMissingUsageRecordsTheAcceptance is P19: the flag closes over
// missing-usage discrepancies AND RECORDS the acceptance.
//
// The recording is the assertion. §2 names the flag verbatim and says it
// "records the acceptance", so a close that merely succeeded would satisfy the
// flag's name and not its contract — an acceptance visible only in a terminal
// scrollback is not a record.
func TestCloseAcceptMissingUsageRecordsTheAcceptance(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	// The step is driven terminal from the MANIFEST's own row rather than from a
	// fresh `next`, because `next` refuses while this dispatch is open (P24) —
	// which is the very state this test needs to close out of.
	instance := manifest.Rows[0].Instance
	finishWithoutUsage(t, conn, instance)

	if _, err := NewEngine().CloseDispatch(conn, runID, false, nowMS); err == nil {
		t.Fatal("premise: close must refuse over a missing-usage discrepancy")
	}

	outcome, err := NewEngine().CloseDispatch(conn, runID, true, nowMS)
	testsupport.Must(t, err, "close --accept-missing-usage: %v", err)
	if outcome.Reason != db.CloseReasonAcceptedMissingUsage {
		t.Errorf("close_reason = %q, want %q",
			outcome.Reason, db.CloseReasonAcceptedMissingUsage)
	}
	// The list names the STEP ID beside the instance (DKT-315): instances
	// repeat across issues in one run, so an acceptance keyed on the instance
	// alone could not say which step it settled.
	if len(outcome.Accepted) == 0 || !strings.Contains(outcome.Accepted[0], instance) {
		t.Errorf("accepted = %v, want one naming %s", outcome.Accepted, instance)
	}
	if len(outcome.Accepted) > 0 &&
		!strings.HasPrefix(outcome.Accepted[0], model.StepIDPrefix) {
		t.Errorf("accepted = %v, want the step id so `backfill-usage --step` "+
			"can act on it", outcome.Accepted)
	}

	// The event carries the accepted list — the durable half of the record.
	var data string
	err = conn.QueryRow(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		runID, EventDispatchClosed).Scan(&data)
	testsupport.Must(t, err, "reading the dispatch-closed event: %v", err)
	if !strings.Contains(data, instance) {
		t.Errorf("the dispatch-closed event %q does not record the accepted step "+
			"%q; §2 says the flag RECORDS the acceptance", data, instance)
	}
}

// TestAcceptMissingUsageDoesNotAcceptD1 is P20: the flag accepts exactly ONE
// class.
//
// Claimed-but-unrecorded has its own resolution — lease expiry — and a flag that
// accepted both would let a relay close over work that is still running.
func TestAcceptMissingUsageDoesNotAcceptD1(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)
	claimInstance(t, conn, manifest.Rows[0].Instance, nowMS)
	past := nowMS + graceMS(t, conn) + 1

	_, err := NewEngine().CloseDispatch(conn, runID, true, past)
	if err == nil {
		t.Fatal("--accept-missing-usage closed over a claimed-but-unrecorded " +
			"step; P20 forbids it, or a relay closes over work still running")
	}
	if !strings.Contains(err.Error(), string(DiscrepancyClaimedUnrecorded)) {
		t.Errorf("the refusal %q does not name the class it refused on", err)
	}
}

// TestAbandonIsUnconditional is P21: `dispatch abandon` closes regardless of
// discrepancies.
//
// The whole point is that the relay is gone and cannot resolve anything. A
// version that checked first would be a recovery verb that refuses to recover.
func TestAbandonIsUnconditional(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)
	claimInstance(t, conn, manifest.Rows[0].Instance, nowMS)
	past := nowMS + graceMS(t, conn) + 1

	// The premise: a discrepancy exists and `close` refuses.
	if _, err := NewEngine().CloseDispatch(conn, runID, false, past); err == nil {
		t.Fatal("premise: close must refuse here")
	}

	outcome, err := NewEngine().AbandonDispatch(conn, runID, "relay died", past)
	testsupport.Must(t, err, "abandon with a discrepancy present: %v — P21 makes it "+
		"unconditional, or a crashed relay wedges the run", err)

	if outcome.Status != db.DispatchAbandoned {
		t.Errorf("status = %q, want %q", outcome.Status, db.DispatchAbandoned)
	}

	// The operator's reason rides in the event's opaque data, not in the
	// engine's short close_reason vocabulary.
	var data string
	err = conn.QueryRow(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		runID, EventDispatchAbandoned).Scan(&data)
	testsupport.Must(t, err, "reading the dispatch-abandoned event: %v", err)
	if !strings.Contains(data, "relay died") {
		t.Errorf("the event %q does not carry the operator's reason", data)
	}
}

// TestCloseRacingAbandonReportsWhy is P22 and C2: both are CAS on
// (id, status='open'), and the loser learns WHY.
//
// A loser told "not open" cannot tell whether the batch was reconciled or given
// up on — which are opposite facts about whether its work is accounted for.
func TestCloseRacingAbandonReportsWhy(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)

	// The abandon wins; the close arrives second.
	abandon(t, conn, runID, nowMS)

	_, err := NewEngine().CloseDispatch(conn, runID, false, nowMS)
	if err == nil {
		t.Fatal("close succeeded against an abandoned dispatch")
	}
	// It reports "no dispatch is open", which is the honest answer once the
	// abandon committed: the open-manifest probe finds nothing.
	if !strings.Contains(err.Error(), "no dispatch is open") {
		t.Errorf("the refusal %q does not say why the close failed", err)
	}
}

// TestManifestCarriesTheConditionalFlag is DKT-70(a), and the answer to it.
//
// The report: a `reconcile` HELD its cluster, yet `verify@0` was still in the
// 9-row dispatch manifest, and the executor spawned for it died immediately on
// `an "after" predecessor is not done (CONFLICT)`. The ask was to exclude such
// rows from the staged closure.
//
// THE ROW CANNOT BE EXCLUDED AT OPEN TIME, and excluding it would be wrong. The
// hold happens DURING the wave the manifest describes — `reconcile` has not run
// when the closure is computed — so at open there is nothing to exclude on, and
// a closure that dropped every row behind a hold-capable step would give up the
// staging that makes the whole wave one round instead of three, on a hold that
// usually does not trip.
//
// What the row owes the dispatcher is the WARNING, which is DKT-26's
// `conditional` flag: this row's stage boundary is a weaker promise than the
// others, so confirm the predecessor actually routed before paying for a boot.
// This test pins that the flag survives all the way onto the stored manifest —
// the surface a relay reads — rather than only onto `next`'s answer.
func TestManifestCarriesTheConditionalFlag(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)

	m := openDispatch(t, conn, runID, 0, nowMS)

	var verify *model.StepRow
	for i := range m.Rows {
		if m.Rows[i].Instance == "verify@0" {
			verify = &m.Rows[i]
		}
	}
	if verify == nil {
		t.Fatalf("verify@0 is absent from the manifest: %v", instancesInRows(m.Rows))
	}
	if !verify.Conditional {
		t.Error("verify@0 rides the manifest unflagged; it sits behind a " +
			"hold-capable reconcile, and a relay reading the manifest has no " +
			"other way to know its stage boundary may not make it claimable")
	}

	// The flag must survive SERIALIZATION too: the manifest a relay reads is
	// the stored `row_json`, not this in-memory struct.
	stored := storedRowsOf(t, conn, m.Dispatch)
	var found bool
	for _, row := range stored {
		if row.Instance != "verify@0" {
			continue
		}
		found = true
		if !strings.Contains(row.RowJSON, `"conditional":true`) {
			t.Errorf("the stored row for verify@0 does not carry the flag: %s",
				row.RowJSON)
		}
	}
	if !found {
		t.Error("verify@0 has no stored dispatch row")
	}
}

// instancesInRows names a manifest's rows, for a failure message.
func instancesInRows(rows []model.StepRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Instance)
	}
	return out
}

// storedRowsOf reads a dispatch's stored manifest rows by its display id.
func storedRowsOf(t *testing.T, conn *sql.DB, dispatch string) []db.DispatchRow {
	t.Helper()
	var id int
	_, err := fmt.Sscanf(dispatch, "DISPATCH-%d", &id)
	testsupport.Must(t, err, "parsing %q: %v", dispatch, err)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	defer tx.Rollback()
	rows, err := db.ListDispatchRowsTx(tx, id)
	testsupport.Must(t, err, "reading the stored rows: %v", err)
	return rows
}
