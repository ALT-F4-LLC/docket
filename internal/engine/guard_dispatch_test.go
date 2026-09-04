package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `guard record | spawn` — §7's exact predicates.

// ---------------------------------------------------------------------------
// G1-G4 — `guard record`
// ---------------------------------------------------------------------------

// TestGuardRecordAllowsAReconciledRun is G1's positive case: with no open
// dispatch and no discrepancy, the guard allows.
//
// It matters that this is the ORDINARY state. A guard that denied by default
// would be one every harness disables on its second day.
func TestGuardRecordAllowsAReconciledRun(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)

	verdict, err := GuardRecord(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "GuardRecord: %v", err)
	if !verdict.Allowed {
		t.Errorf("a reconciled run was denied: %s", verdict.Reason)
	}
}

// TestGuardRecordDeniesAnOpenDispatch is G1 and G3: an open manifest is
// unreconciled, and the denial names it in `next`'s own words.
func TestGuardRecordDeniesAnOpenDispatch(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	verdict, err := GuardRecord(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "GuardRecord: %v", err)
	if verdict.Allowed {
		t.Fatal("G1: a run with an open dispatch was allowed to record")
	}
	if !strings.Contains(verdict.Reason, manifest.Dispatch) {
		t.Errorf("the denial %q does not name the open dispatch %s",
			verdict.Reason, manifest.Dispatch)
	}

	// G2/G3: THE SAME WORDS `next` USES. The guard and the scheduler compute
	// the state with one function, so they cannot describe it differently — and
	// this asserts the sharing rather than trusting it.
	_, nextErr := NewEngine().NextSteps(conn, runID, 0, nowMS)
	if nextErr == nil {
		t.Fatal("premise: `next` must refuse while a dispatch is open")
	}
	if verdict.Reason != nextErr.Error() {
		t.Errorf("the guard and `next` describe one state differently:\n"+
			"  guard: %s\n  next:  %s", verdict.Reason, nextErr.Error())
	}
}

// TestGuardRecordDeniesADiscrepancy is G2's other half: a discrepancy is
// unreconciled even with NO dispatch open.
//
// D6 is why: discrepancies are a property of the RUN, not of the manifest — a
// relay that never opened a dispatch can still leave a claimed step unrecorded.
func TestGuardRecordDeniesADiscrepancy(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)

	claim := claimInstance(t, conn, "implement@0", nowMS)
	_ = claim

	// Past `dispatch.grace` with no record: D1, claimed-but-unrecorded. The
	// clock is ADVANCED rather than slept, following the repo's TTL discipline —
	// the refusal under test is "the clock says this lapsed", which needs no
	// actual waiting and would flake on a loaded runner if it did.
	at := nowMS + dispatchGraceMS(t, conn) + 1

	verdict, err := GuardRecord(conn, runID, 0, at)
	testsupport.Must(t, err, "GuardRecord: %v", err)
	if verdict.Allowed {
		t.Fatal("G2: a run with a claimed-but-unrecorded step was allowed to record")
	}
	// G3: the denial carries the RESOLUTION, because §2 enumerates the
	// resolutions and a refusal without the way out makes reconciliation a
	// documentation lookup rather than an operator's act.
	if !strings.Contains(verdict.Reason, "lease expiry") {
		t.Errorf("the denial %q does not name D1's resolution", verdict.Reason)
	}
}

// TestGuardRecordWithoutRunAnswersOverEveryRun is G4: `--run` is OPTIONAL, and
// without it the guard denies if ANY non-terminal run is unreconciled.
//
// That shape matches `guard stop`'s, so a hook wired once keeps working as runs
// come and go — which is the whole reason the flag is optional.
func TestGuardRecordWithoutRunAnswersOverEveryRun(t *testing.T) {
	conn := mustDB(t)
	first := dispatchRun(t, conn)

	// A second run over the same definition, so "any" has something to range
	// over. Only the SECOND opens a dispatch.
	issue := createIssue(t, conn, "second", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate the second run: %v", err)

	// Both reconciled: the all-runs guard allows.
	verdict, err := GuardRecord(conn, 0, 0, nowMS)
	testsupport.Must(t, err, "GuardRecord over every run: %v", err)
	if !verdict.Allowed {
		t.Fatalf("two reconciled runs were denied: %s", verdict.Reason)
	}

	openDispatch(t, conn, run.ID, 0, nowMS)

	// One unreconciled: the all-runs guard denies, and names THAT run.
	verdict, err = GuardRecord(conn, 0, 0, nowMS)
	testsupport.Must(t, err, "GuardRecord over every run: %v", err)
	if verdict.Allowed {
		t.Fatal("G4: one unreconciled run did not deny the all-runs guard")
	}
	if !strings.Contains(verdict.Reason, model.FormatRunID(run.ID)) {
		t.Errorf("the denial %q does not name the unreconciled run %s",
			verdict.Reason, model.FormatRunID(run.ID))
	}

	// And the RECONCILED run, asked about specifically, is still allowed: the
	// all-runs answer must not make a healthy run un-recordable.
	verdict, err = GuardRecord(conn, first, 0, nowMS)
	testsupport.Must(t, err, "GuardRecord on the reconciled run: %v", err)
	if !verdict.Allowed {
		t.Errorf("the reconciled run was denied because another run was not: %s",
			verdict.Reason)
	}
}

// TestGuardRecordRefusesAnUnknownRun: a named run that does not exist is a
// REFUSAL, not a vacuous allow.
//
// A guard asked about a run that is not there has established nothing, and exit
// 0 would let a typo in a hook read as permission.
func TestGuardRecordRefusesAnUnknownRun(t *testing.T) {
	conn := mustDB(t)
	dispatchRun(t, conn)

	if _, err := GuardRecord(conn, 999, 0, nowMS); err == nil {
		t.Fatal("a guard on a nonexistent run allowed; a typo must not read as permission")
	} else if code, _ := CodeOf(err); code != CodeNotFound {
		t.Errorf("the refusal is %q, want NOT_FOUND", code)
	}
}

// ---------------------------------------------------------------------------
// G5-G10 — `guard spawn`
// ---------------------------------------------------------------------------

// TestGuardSpawnVacuousWithoutManifest is G7: with NO open dispatch and NO
// `--rows`, the row half is vacuously satisfied and the reap half still answers.
//
// This is the case that makes the reap-ack mechanism available to a relay that
// batches its own way. Requiring a manifest would have been core deciding how a
// harness must batch, which is exactly the instance policy §2 keeps out.
func TestGuardSpawnVacuousWithoutManifest(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)

	verdict, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{NowMS: nowMS})
	testsupport.Must(t, err, "GuardSpawn: %v", err)
	if !verdict.Allowed {
		t.Errorf("G7: a relay with no manifest and no reaps was denied: %s", verdict.Reason)
	}
}

// TestGuardSpawnMatchesTheManifest is G5(a) and G6: the proposed rows byte-match
// the open dispatch, position by position.
func TestGuardSpawnMatchesTheManifest(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)

	rows, err := json.Marshal(manifest.Rows)
	testsupport.Must(t, err, "marshaling the manifest rows: %v", err)

	verdict, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{
		Rows: rows, NowMS: nowMS,
	})
	testsupport.Must(t, err, "GuardSpawn: %v", err)
	if !verdict.Allowed {
		t.Errorf("G6: rows taken verbatim from the manifest did not match: %s",
			verdict.Reason)
	}
}

// TestGuardSpawnDeniesAlteredRows is G5(a)'s negative: a row the relay changed
// is a denial, and the denial shows BOTH SIDES' BYTES.
//
// The evidence rather than a summary, for `dispatch verify` P9's reason: a
// computed description would be the engine's opinion about which field moved.
func TestGuardSpawnDeniesAlteredRows(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)
	if len(manifest.Rows) == 0 {
		t.Skip("the fixture offered no ready steps; this needs one")
	}

	altered := make([]model.StepRow, len(manifest.Rows))
	copy(altered, manifest.Rows)
	altered[0].Instance = "not-what-was-offered@0"
	rows, err := json.Marshal(altered)
	testsupport.Must(t, err, "marshaling: %v", err)

	verdict, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{
		Rows: rows, NowMS: nowMS,
	})
	testsupport.Must(t, err, "GuardSpawn: %v", err)
	if verdict.Allowed {
		t.Fatal("G5(a): an altered row was allowed to spawn")
	}
	for _, want := range []string{"not-what-was-offered@0", manifest.Rows[0].Instance} {
		if !strings.Contains(verdict.Reason, want) {
			t.Errorf("the denial %q does not show %q; P9 wants the differing BYTES, "+
				"both sides", verdict.Reason, want)
		}
	}
}

// TestGuardSpawnDeniesRowsWithNoManifest is G8: `--rows` with no open dispatch
// is a DENIAL, not a vacuous pass.
//
// The relay believes it is spawning a batch the engine never issued, and that
// belief is exactly the drift this verb exists to catch. Passing it because
// there was nothing to compare against would be the silent proceeding §2 forbids.
func TestGuardSpawnDeniesRowsWithNoManifest(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)

	answer, err := NewEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	rows, err := json.Marshal(answer.Steps)
	testsupport.Must(t, err, "marshaling: %v", err)

	verdict, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{
		Rows: rows, NowMS: nowMS,
	})
	testsupport.Must(t, err, "GuardSpawn: %v", err)
	if verdict.Allowed {
		t.Fatal("G8: rows proposed against no manifest were allowed; the relay " +
			"believes it is spawning a batch the engine never issued")
	}
	if !strings.Contains(verdict.Reason, "no open dispatch") {
		t.Errorf("the denial %q does not say the manifest is missing", verdict.Reason)
	}
}

// TestGuardSpawnW4AndW6 completes §6.6's W-table: the two rows group 2 deferred
// because `guard spawn` is group 3's verb.
//
// W4: `guard spawn` with an unacknowledged reap is DENIED, naming the seq and
// the flag. W6: with `--ack-reap SEQ` it is ALLOWED, the row is acknowledged,
// and `acked_by` records the VERB.
func TestGuardSpawnW4AndW6(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)
	at, seq := reapOneWriter(t, conn, runID)

	// W4: denied, exit 2, reason naming the seq and `--ack-reap`. A11 is what
	// makes the mechanism DISCOVERABLE rather than documented — an operator
	// copies the next command out of the refusal.
	verdict, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{NowMS: at})
	testsupport.Must(t, err, "GuardSpawn: %v", err)
	if verdict.Allowed {
		t.Fatal("W4: `guard spawn` allowed with an unacknowledged write reap")
	}
	for _, want := range []string{"--ack-reap", "one@0"} {
		if !strings.Contains(verdict.Reason, want) {
			t.Errorf("W4: the denial %q does not name %q", verdict.Reason, want)
		}
	}

	// W6: acknowledged and allowed, in ONE invocation (G10) — which is what lets
	// a relay's hook be a single command.
	verdict, err = NewEngine().GuardSpawn(conn, runID, SpawnOptions{
		AckSeqs: []int64{seq}, NowMS: at,
	})
	testsupport.Must(t, err, "GuardSpawn --ack-reap: %v", err)
	if !verdict.Allowed {
		t.Fatalf("W6: the acknowledged run was still denied: %s", verdict.Reason)
	}
	if acks := openReapsOf(t, conn, runID); len(acks) != 0 {
		t.Errorf("W6: %d reaps remain unacknowledged after the ack", len(acks))
	}

	// A8: `acked_by` records the VERB and the entry point, never a user
	// identity — core has no identity model.
	var ackedBy string
	err = conn.QueryRow(
		`SELECT acked_by FROM reap_acks WHERE reaped_seq = ?`, seq).Scan(&ackedBy)
	testsupport.Must(t, err, "reading acked_by: %v", err)
	if ackedBy != db.AckByGuardSpawn {
		t.Errorf("acked_by is %q, want %q — A8 records the verb", ackedBy, db.AckByGuardSpawn)
	}

	// W7: write-class steps flow again.
	answer, err := NewEngine().NextSteps(conn, runID, 0, at)
	testsupport.Must(t, err, "next after the ack: %v", err)
	if !contains(instancesIn(answer), "one@0") {
		t.Errorf("W7: write-class work did not resume after the ack (%v)",
			instancesIn(answer))
	}
}

// TestGuardSpawnActiveDeniesTheOlderRunWithAHold is DKT-1287 AC1: with two
// active runs where only the older would deny, `guard spawn --active` denies,
// naming the older run — not `runs[0]` alone, which is what
// docket-spawn-guard-hook.sh resolved before this existed, leaving a second
// concurrent run's reap hold unasked.
func TestGuardSpawnActiveDeniesTheOlderRunWithAHold(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeLimitedSrc), "serialized.toml")

	olderIssue := createIssue(t, conn, "older", "body", "task", nil)
	older := startRun(t, conn, olderIssue)
	_, err := activate(conn, older.ID)
	testsupport.Must(t, err, "activate the older run: %v", err)

	newerIssue := createIssue(t, conn, "newer", "body", "task", nil)
	newer := startRun(t, conn, newerIssue)
	_, err = activate(conn, newer.ID)
	testsupport.Must(t, err, "activate the newer run: %v", err)

	// Only the OLDER run holds an unacknowledged write reap.
	reapOneWriter(t, conn, older.ID)

	verdict, err := GuardSpawnActive(conn, 0, 0, nowMS)
	testsupport.Must(t, err, "GuardSpawnActive: %v", err)
	if verdict.Allowed {
		t.Fatal("AC1: an unacknowledged reap on the older run did not deny --active")
	}
	if !strings.Contains(verdict.Reason, model.FormatRunID(older.ID)) {
		t.Errorf("the denial %q does not name the older run %s",
			verdict.Reason, model.FormatRunID(older.ID))
	}
	if strings.Contains(verdict.Reason, model.FormatRunID(newer.ID)) {
		t.Errorf("the denial %q wrongly names the newer, unheld run %s",
			verdict.Reason, model.FormatRunID(newer.ID))
	}
}

// TestGuardSpawnActiveAllowsWhenNoActiveRunHoldsAReap is --active's ordinary
// case: two active runs, neither holding a reap, allow.
func TestGuardSpawnActiveAllowsWhenNoActiveRunHoldsAReap(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeLimitedSrc), "serialized.toml")

	for _, name := range []string{"first", "second"} {
		issue := createIssue(t, conn, name, "body", "task", nil)
		run := startRun(t, conn, issue)
		_, err := activate(conn, run.ID)
		testsupport.Must(t, err, "activate %s: %v", name, err)
	}

	verdict, err := GuardSpawnActive(conn, 0, 0, nowMS)
	testsupport.Must(t, err, "GuardSpawnActive: %v", err)
	if !verdict.Allowed {
		t.Errorf("two unheld active runs were denied: %s", verdict.Reason)
	}
}

// TestGuardSpawnAckIsIdempotentAndUnforgeable is W8 and W9 through this verb's
// entry point.
//
// W8: a second ack of the same seq is a SUCCESS THAT CHANGES NOTHING, so a relay
// retrying its hook does not fail. W9: a bogus seq is a VALIDATION_ERROR — the
// forgery point, since an ack must name a REAL reap.
func TestGuardSpawnAckIsIdempotentAndUnforgeable(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)
	at, seq := reapOneWriter(t, conn, runID)

	for i := range 2 {
		verdict, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{
			AckSeqs: []int64{seq}, NowMS: at,
		})
		testsupport.Must(t, err, "W8: ack %d failed: %v", i+1, err)
		if !verdict.Allowed {
			t.Fatalf("W8: ack %d denied: %s", i+1, verdict.Reason)
		}
	}

	// W9: a seq that names no reap of this run. The message names the SEQ, so an
	// operator who mistyped can see what they typed.
	_, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{
		AckSeqs: []int64{999999}, NowMS: at,
	})
	if err == nil {
		t.Fatal("W9: a bogus seq was accepted; an ack must name a real reap")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("W9: the refusal is %q, want VALIDATION_ERROR", code)
	}
	if !strings.Contains(err.Error(), "999999") {
		t.Errorf("W9: the refusal %q does not name the seq", err.Error())
	}
}

// ---------------------------------------------------------------------------
// G11-G13 — what the guards write
// ---------------------------------------------------------------------------

// TestGuardsWriteNothing extends §10.3's standing assertion to group 3's rows:
// `guard record` and `guard spawn` WITHOUT `--ack-reap` leave the database
// byte-identical.
//
// It hashes the FILE rather than counting rows, for TestReadVerbsWriteNothing's
// reason: a row count would miss exactly the writes that matter — a reaped
// lease, a bumped attempt — all of which mutate rows in place.
//
// The fixture holds a LAPSED lease and an EXPIRED dispatch on purpose. Those are
// the two writes a guard is most likely to make by accident, because `next`
// makes both: G13 says neither guard reaps and neither auto-abandons, and this
// is that clause with something to catch.
func TestGuardsWriteNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "issues.db")
	conn, err := sql.Open("sqlite", path)
	testsupport.Must(t, err, "opening: %v", err)
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	err = db.Initialize(conn)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = db.Migrate(conn)
	testsupport.Must(t, err, "Migrate: %v", err)

	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	claimInstance(t, conn, "implement@0", nowMS)
	execSQL(t, conn, `UPDATE steps SET expires_ms = 1 WHERE instance = 'implement@0'`)

	// Past the dispatch TTL, so `next` in this position WOULD auto-abandon.
	at := nowMS + dispatchTTLMS(t, conn) + 1

	_, err = conn.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	testsupport.Must(t, err, "checkpointing: %v", err)
	before := hashFile(t, path)

	_, err = GuardRecord(conn, runID, 0, at)
	testsupport.Must(t, err, "GuardRecord: %v", err)
	_, err = NewEngine().GuardSpawn(conn, runID, SpawnOptions{NowMS: at})
	testsupport.Must(t, err, "GuardSpawn: %v", err)

	_, err = conn.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	testsupport.Must(t, err, "checkpointing after the guards: %v", err)
	if after := hashFile(t, path); after != before {
		t.Error("a guard changed the database; G11-G13 make both pure reads — " +
			"neither reaps and neither auto-abandons, because a hook's mere " +
			"presence must not change a run's behavior")
	}

	// And the dispatch is STILL OPEN afterwards, which is G13 stated positively:
	// the guard reported a state it did not resolve.
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	if _, err := db.OpenDispatchTx(tx, runID); err != nil {
		t.Errorf("the guards auto-abandoned the expired dispatch: %v", err)
	}
}

// TestGuardSpawnWritesOnlyTheAck is G12: `guard spawn` writes ONLY the
// `reap_acks` update, and only when `--ack-reap` is passed.
//
// The positive half of the previous test: the one write the verb IS allowed to
// make must actually happen, or the ack entry point would be a no-op that
// reported success.
func TestGuardSpawnWritesOnlyTheAck(t *testing.T) {
	conn := mustDB(t)
	runID := serializedRun(t, conn)
	at, seq := reapOneWriter(t, conn, runID)

	stepsBefore := stepStateFingerprint(t, conn, runID)

	_, err := NewEngine().GuardSpawn(conn, runID, SpawnOptions{
		AckSeqs: []int64{seq}, NowMS: at,
	})
	testsupport.Must(t, err, "GuardSpawn --ack-reap: %v", err)

	// The ack landed.
	if acks := openReapsOf(t, conn, runID); len(acks) != 0 {
		t.Errorf("G12: the ack did not land; %d reaps remain unacknowledged", len(acks))
	}
	// And NOTHING ELSE moved: no step was reaped, claimed, or re-statused by a
	// verb whose only permitted write is one column on one `reap_acks` row.
	if after := stepStateFingerprint(t, conn, runID); after != stepsBefore {
		t.Errorf("G12: `guard spawn --ack-reap` changed step state:\n before %s\n after  %s",
			stepsBefore, after)
	}
}

// dispatchGraceMS reads the configured grace window through the same accessor
// the engine uses, so a test's clock arithmetic follows a config change rather
// than restating a default that could drift from it. (`dispatchTTLMS` is group
// 2's, in dispatch_test.go, and does the same for the TTL.)
func dispatchGraceMS(t *testing.T, conn *sql.DB) int64 {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	grace, err := db.DispatchGraceTx(tx, 1)
	testsupport.Must(t, err, "DispatchGraceTx: %v", err)
	return grace.Milliseconds()
}

// stepStateFingerprint renders every step's scheduling-relevant state as one
// string, so a test can assert "nothing about the steps moved" without listing
// the columns at each call site.
func stepStateFingerprint(t *testing.T, conn *sql.DB, runID int) string {
	t.Helper()
	rows, err := conn.Query(
		`SELECT instance, status, attempt, owner, expires_ms
		   FROM steps WHERE run_id = ? ORDER BY instance`, runID)
	testsupport.Must(t, err, "reading step state: %v", err)
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var (
			instance, status string
			owner            sql.NullString
			attempt          int
			expires          sql.NullInt64
		)
		err := rows.Scan(&instance, &status, &attempt, &owner, &expires)
		testsupport.Must(t, err, "reading a step: %v", err)

		// `owner` and `expires_ms` are NULL on an unclaimed step, and both are
		// part of the fingerprint precisely because a reap CLEARS them: a
		// version that skipped the null columns would be blind to the write it
		// exists to detect.
		fmt.Fprintf(&b, "%s|%s|%d|%s|%d;",
			instance, status, attempt, owner.String, expires.Int64)
	}
	err = rows.Err()
	testsupport.Must(t, err, "reading step state: %v", err)
	return b.String()
}
