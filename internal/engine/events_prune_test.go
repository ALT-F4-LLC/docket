package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `events prune`'s refusals, which are the verb.
//
// The tests are weighted the way the verb is: one test for what it deletes,
// and the rest for what it will not.

// pruneFixture is a run with events, moved to a terminal status so the prune's
// live-run refusal is not what a test about something else trips over.
func pruneFixture(t *testing.T, conn *sql.DB) int {
	t.Helper()
	runID, _ := budgetRun(t, conn, 0)
	claimInstance(t, conn, "implement@0", nowMS)
	execSQL(t, conn, `UPDATE runs SET status = 'done' WHERE id = ?`, runID)
	return runID
}

func eventCount(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	err := conn.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	testsupport.Must(t, err, "counting events: %v", err)
	return n
}

// TestPruneRefusalMatrix is the refusal matrix: every way the verb says no,
// with the code and the reason each one carries.
//
// They are ONE TEST because they are one property — a destructive verb refuses
// unless everything is right — and because a matrix makes the omission of a case
// visible in a way six sibling functions would not.
func TestPruneRefusalMatrix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, conn *sql.DB) PruneQuery
		code  ErrorCode
		// says is a phrase the refusal must contain. A code alone would let a
		// message degrade into "invalid input" while the test kept passing, and
		// an operator hitting a refusal needs to know WHICH one.
		says string
	}{
		{
			// No target. "Prune everything" is not expressible, because a
			// destructive verb with a default target is how a log gets deleted
			// by a typo.
			name: "no target",
			setup: func(t *testing.T, conn *sql.DB) PruneQuery {
				pruneFixture(t, conn)
				return PruneQuery{NowMS: nowMS}
			},
			code: CodeValidation,
			says: "needs a target",
		},
		{
			// Two targets. They name different things — a seq boundary and a
			// whole run — so honoring both would mean guessing which was meant.
			name: "both targets",
			setup: func(t *testing.T, conn *sql.DB) PruneQuery {
				runID := pruneFixture(t, conn)
				return PruneQuery{Before: 2, BeforeRun: runID, NowMS: nowMS}
			},
			code: CodeValidation,
			says: "pass one",
		},
		{
			name: "negative before",
			setup: func(t *testing.T, conn *sql.DB) PruneQuery {
				pruneFixture(t, conn)
				return PruneQuery{Before: -1, NowMS: nowMS}
			},
			code: CodeValidation,
			says: "must not be negative",
		},
		{
			// THE HEADLINE REFUSAL: a run that has not finished.
			name: "a live run's events",
			setup: func(t *testing.T, conn *sql.DB) PruneQuery {
				runID, _ := budgetRun(t, conn, 0)
				claimInstance(t, conn, "implement@0", nowMS)
				// Deliberately NOT moved to a terminal status.
				var maxSeq int64
				if err := conn.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&maxSeq); err != nil {
					t.Fatalf("reading MAX(seq): %v", err)
				}
				_ = runID
				return PruneQuery{Before: maxSeq, NowMS: nowMS}
			},
			code: CodeConflict,
			// The message must explain WHY, not merely that. "Refused" would
			// leave an operator to conclude the verb is fussy; the reason is
			// that pruning a live run changes what the engine computes.
			says: "budget floor",
		},
		{
			name: "a live run named directly",
			setup: func(t *testing.T, conn *sql.DB) PruneQuery {
				runID, _ := budgetRun(t, conn, 0)
				claimInstance(t, conn, "implement@0", nowMS)
				return PruneQuery{BeforeRun: runID, NowMS: nowMS}
			},
			code: CodeConflict,
			says: "have not finished",
		},
		{
			name: "a run that does not exist",
			setup: func(t *testing.T, conn *sql.DB) PruneQuery {
				pruneFixture(t, conn)
				return PruneQuery{BeforeRun: 9999, NowMS: nowMS}
			},
			code: CodeNotFound,
			says: "not found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			q := tc.setup(t, conn)
			before := eventCount(t, conn)

			_, err := PruneEvents(conn, q)
			if err == nil {
				t.Fatal("the prune was accepted; this case must refuse")
			}
			code, ok := CodeOf(err)
			if !ok || code != tc.code {
				t.Errorf("the refusal is %q, want %q: %v", code, tc.code, err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal %q does not mention %q", err.Error(), tc.says)
			}

			// EVERY REFUSAL IS TOTAL. A prune that refused and had already
			// deleted half its range would be the worst of both answers, so the
			// count is asserted for each case rather than for the mechanism in
			// general.
			if after := eventCount(t, conn); after != before {
				t.Errorf("a refused prune deleted %d events", before-after)
			}
		})
	}
}

// TestRetentionWindowHoldsTheBoundary: `events.retain` protects recent
// events whatever `--before` says, and the answer SAYS SO rather than silently
// returning a smaller number.
func TestRetentionWindowHoldsTheBoundary(t *testing.T) {
	conn := mustDB(t)
	pruneFixture(t, conn)

	// The fixture's events carry WALL-CLOCK timestamps: `recordEvent` falls back
	// to `time.Now()` for any call site that passes no clock, and most of the
	// activation path does. So the rows are re-stamped to the test's own `nowMS`
	// first, which makes the window arithmetic below deterministic rather than
	// dependent on the gap between the machine's clock and a constant.
	//
	// NOTHING HERE SLEEPS. The window is placed relative to the clock the CALLER
	// passes — which is exactly why PruneQuery takes one — so "an hour later" is
	// an argument rather than an hour.
	execSQL(t, conn, `UPDATE events SET at_ms = ?`, nowMS)

	err := db.SetConfig(conn, 0, db.KeyEventsRetain, "1h")
	testsupport.Must(t, err, "SetConfig: %v", err)

	var maxSeq int64
	err = conn.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&maxSeq)
	testsupport.Must(t, err, "reading MAX(seq): %v", err)
	before := eventCount(t, conn)

	result, err := PruneEvents(conn, PruneQuery{Before: maxSeq + 1, NowMS: nowMS})
	testsupport.Must(t, err, "PruneEvents: %v", err)
	if result.Pruned != 0 {
		t.Errorf("the prune deleted %d events inside the retention window", result.Pruned)
	}
	// HELD BACK, AND SAID SO. An operator told only "0" would conclude the
	// verb is broken; told that a window held N, they know it is policy.
	if result.HeldByRetention != before {
		t.Errorf("held_by_retention = %d, want %d — the answer must name what "+
			"the boundary protected", result.HeldByRetention, before)
	}
	if after := eventCount(t, conn); after != before {
		t.Errorf("%d events were deleted despite the window", before-after)
	}

	// Moving the CLOCK past the window — not the rows — releases them. This is
	// the same test from the other side, and it shows the boundary is a window
	// rather than a refusal to prune at all.
	result, err = PruneEvents(conn, PruneQuery{
		Before: maxSeq + 1, NowMS: nowMS + (2 * 60 * 60 * 1000),
	})
	testsupport.Must(t, err, "PruneEvents past the window: %v", err)
	if result.Pruned != before {
		t.Errorf("past the window the prune deleted %d of %d events",
			result.Pruned, before)
	}
}

// TestRetentionDefaultRetainsEverything: the window defaults to 0, and 0
// means EVERYTHING IS PROTECTED.
//
// The direction matters more than the number. A retention key defaulting to
// "keep nothing" would make the first `events prune --before` an operator ever
// typed delete their whole log — the opposite of the posture where Docket
// deletes nothing it was not asked to delete.
func TestRetentionDefaultRetainsEverything(t *testing.T) {
	conn := mustDB(t)

	entry, err := db.GetConfig(conn, 1, db.KeyEventsRetain)
	testsupport.Must(t, err, "GetConfig: %v", err)
	if entry.Value != "0" || entry.Source != "default" {
		t.Errorf("events.retain defaults to %q (%s), want \"0\" (default)",
			entry.Value, entry.Source)
	}

	// And 0 means "retain everything" in the sense that matters: it imposes NO
	// boundary of its own, so a prune below a terminal run's cursor proceeds.
	// (A key whose default silently blocked every prune would be a different
	// bug wearing the same number.)
	pruneFixture(t, conn)
	var maxSeq int64
	err = conn.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&maxSeq)
	testsupport.Must(t, err, "reading MAX(seq): %v", err)
	result, err := PruneEvents(conn, PruneQuery{Before: maxSeq, NowMS: nowMS})
	testsupport.Must(t, err, "PruneEvents: %v", err)
	if result.Pruned == 0 {
		t.Error("nothing was pruned at the default window; 0 means the window " +
			"imposes no boundary, not that prune never works")
	}
	if result.HeldByRetention != 0 {
		t.Errorf("held_by_retention = %d at a zero window", result.HeldByRetention)
	}
}

// TestPruneDeletesOnlyEvents: the verb's blast radius is one
// table, and the test asserts it over EVERY OTHER TABLE rather than over the
// handful a reviewer would think to name.
func TestPruneDeletesOnlyEvents(t *testing.T) {
	conn := mustDB(t)
	pruneFixture(t, conn)

	before := allTableCounts(t, conn)

	var maxSeq int64
	err := conn.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&maxSeq)
	testsupport.Must(t, err, "reading MAX(seq): %v", err)
	_, err = PruneEvents(conn, PruneQuery{Before: maxSeq, NowMS: nowMS})
	testsupport.Must(t, err, "PruneEvents: %v", err)

	after := allTableCounts(t, conn)
	for table, n := range before {
		if table == "events" {
			if after[table] >= n {
				t.Errorf("events went from %d to %d; the prune deleted nothing", n, after[table])
			}
			continue
		}
		if after[table] != n {
			t.Errorf("%s went from %d rows to %d: the prune reached outside `events`",
				table, n, after[table])
		}
	}
}

// TestPruningALiveRunWouldMoveTheFloor is the live-run refusal's ARGUMENT,
// executed.
//
// The refusal is not fastidiousness about audit trails. The budget floor is a
// SUM over a run's `step-claimed` events, so deleting them does not lose a
// record of what the run spent — it changes what the engine BELIEVES the run
// spent, downward, which is the direction that lets a capped run overspend.
//
// The test proves that by doing the forbidden thing directly, the same way the
// GONE tests construct a state no product path could reach: refuse through the
// verb, then delete by hand and watch the number move.
func TestPruningALiveRunWouldMoveTheFloor(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 100)
	claimInstance(t, conn, "implement@0", nowMS)

	floorBefore := runFloor(t, conn, runID)
	if floorBefore == 0 {
		t.Fatal("the fixture accrued no floor; this test needs a claim with a cost")
	}

	var maxSeq int64
	err := conn.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&maxSeq)
	testsupport.Must(t, err, "reading MAX(seq): %v", err)

	// The verb refuses.
	if _, err := PruneEvents(conn, PruneQuery{Before: maxSeq + 1, NowMS: nowMS}); err == nil {
		t.Fatal("a live run's events were pruned; the live-run refusal forbids that")
	}
	if floor := runFloor(t, conn, runID); floor != floorBefore {
		t.Errorf("the refused prune moved the floor from %g to %g", floorBefore, floor)
	}

	// And this is WHY. Doing it by hand — which is what an operator with
	// sqlite3 and no verb would do — moves the number enforcement decides on.
	execSQL(t, conn, `DELETE FROM events WHERE kind = ?`, EventStepClaimed)
	if floor := runFloor(t, conn, runID); floor >= floorBefore {
		t.Errorf("deleting the claim events left the floor at %g (was %g); this "+
			"test's premise is that they ARE the floor", floor, floorBefore)
	}
}

// TestPruneDryRunWritesNothing: `--dry-run` answers the question without
// answering it destructively.
func TestPruneDryRunWritesNothing(t *testing.T) {
	conn := mustDB(t)
	pruneFixture(t, conn)

	before := eventCount(t, conn)
	var maxSeq int64
	err := conn.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&maxSeq)
	testsupport.Must(t, err, "reading MAX(seq): %v", err)

	result, err := PruneEvents(conn, PruneQuery{
		Before: maxSeq, DryRun: true, NowMS: nowMS,
	})
	testsupport.Must(t, err, "PruneEvents --dry-run: %v", err)
	if result.Pruned == 0 {
		t.Error("--dry-run reported 0; it must report what the real call would delete")
	}
	if !result.DryRun {
		t.Error("--dry-run did not mark its answer as one")
	}
	if after := eventCount(t, conn); after != before {
		t.Errorf("--dry-run deleted %d events", before-after)
	}

	// The real call then deletes exactly what the rehearsal promised, which is
	// the only property that makes a dry run worth having.
	real, err := PruneEvents(conn, PruneQuery{Before: maxSeq, NowMS: nowMS})
	testsupport.Must(t, err, "PruneEvents: %v", err)
	if real.Pruned != result.Pruned {
		t.Errorf("--dry-run promised %d and the prune deleted %d",
			result.Pruned, real.Pruned)
	}
}

// TestPruneIsEventLogged: the prune records itself, ABOVE what it
// deleted.
//
// The ordering is the point. An `events-pruned` event with a seq inside the
// deleted range would be deleted by the prune that wrote it, and a log that
// erased the record of its own erasure is the failure mode this kind exists to
// prevent.
func TestPruneIsEventLogged(t *testing.T) {
	conn := mustDB(t)
	pruneFixture(t, conn)

	var maxSeq int64
	err := conn.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&maxSeq)
	testsupport.Must(t, err, "reading MAX(seq): %v", err)
	result, err := PruneEvents(conn, PruneQuery{Before: maxSeq, NowMS: nowMS})
	testsupport.Must(t, err, "PruneEvents: %v", err)

	page, err := ListEvents(conn, EventQuery{Since: result.RetainedMinimum - 1})
	testsupport.Must(t, err, "ListEvents: %v", err)

	var pruneEvent *Event
	for i := range page.Events {
		if page.Events[i].Kind == EventEventsPruned {
			pruneEvent = &page.Events[i]
		}
	}
	if pruneEvent == nil {
		t.Fatal("the prune wrote no `events-pruned` event")
	}
	if pruneEvent.Seq < maxSeq {
		t.Errorf("the `events-pruned` event is at seq %d, inside the deleted "+
			"range (< %d): a prune must not be able to erase its own record",
			pruneEvent.Seq, maxSeq)
	}
	for _, want := range []string{"deleted", "before"} {
		if !strings.Contains(string(pruneEvent.Data), want) {
			t.Errorf("the `events-pruned` data %s does not carry %q",
				pruneEvent.Data, want)
		}
	}

	// And it is ATTRIBUTABLE, so a trimmed log still says who trimmed it.
	actor, ok := ActorFor(EventEventsPruned)
	if !ok || actor != ActorHuman {
		t.Errorf("`events-pruned` maps to %q; nothing in the engine prunes, so "+
			"it is a human's act", actor)
	}
}

// allTableCounts snapshots every table's row count, for the blast-radius test.
func allTableCounts(t *testing.T, conn *sql.DB) map[string]int {
	t.Helper()
	rows, err := conn.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	testsupport.Must(t, err, "listing tables: %v", err)
	var tables []string
	for rows.Next() {
		var name string
		err := rows.Scan(&name)
		testsupport.Must(t, err, "reading a table name: %v", err)
		tables = append(tables, name)
	}
	rows.Close()

	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var n int
		err := conn.QueryRow(`SELECT COUNT(*) FROM "` + table + `"`).Scan(&n)
		testsupport.Must(t, err, "counting %s: %v", table, err)
		counts[table] = n
	}
	return counts
}

// bodyRefreshRun activates the two-step body fixture: `before` and `after` are
// the steps a test claims on either side of a body refresh.
func bodyRefreshRun(t *testing.T) (conn *sql.DB, runID, issue, before, after int) {
	t.Helper()
	conn = mustDB(t)
	registerSource(t, conn, []byte(bodyInputWorkflow), "bodies.toml")
	issue = createIssue(t, conn, "amend me", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	before = stepIDOf(t, conn, run.ID, issue, "first@0")
	after = stepIDOf(t, conn, run.ID, issue, "second@0")
	return conn, run.ID, issue, before, after
}

func refreshRunBody(t *testing.T, conn *sql.DB, runID, issue int, body string, atMS int64) {
	t.Helper()
	amend(t, conn, issue, body)
	_, err := RefreshIssueBodyInRun(conn, runID, issue, "operator amended", testBy, testConductorToken, atMS)
	testsupport.Must(t, err, "refreshing: %v", err)
}

// recordClaimEvent writes the `step-claimed` row a real claim leaves in the
// log, keyed to the step's own step_id, so a prune test can watch that row go.
func recordClaimEvent(t *testing.T, conn *sql.DB, runID, issue int, instance string, atMS int64) {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	err = recordEvent(tx, eventRecord{
		Kind: EventStepClaimed, RunID: runID, Instance: instance, IssueID: issue, AtMS: atMS,
	})
	testsupport.Must(t, err, "recording the claim event: %v", err)
	testsupport.Must(t, tx.Commit(), "committing the claim event")
}

// claimEventCount counts the `step-claimed` rows naming stepID.
func claimEventCount(t *testing.T, conn *sql.DB, stepID int) int {
	t.Helper()
	var n int
	err := conn.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND step_id = ?`,
		EventStepClaimed, stepID).Scan(&n)
	testsupport.Must(t, err, "counting claim events: %v", err)
	return n
}

// TestPrunedRunReportsRecordedBodyUnavailable: once a done run's events are
// pruned, the refresh rows the read-back reconstructs from may be gone, so
// every claim that preceded the prune reports its body unavailable rather than
// the live description, whether or not a refresh followed that claim.
func TestPrunedRunReportsRecordedBodyUnavailable(t *testing.T) {
	conn, runID, issue, before, after := bodyRefreshRun(t)
	recordClaim(t, conn, before, nowMS)
	recordClaimEvent(t, conn, runID, issue, "first@0", nowMS)
	refreshRunBody(t, conn, runID, issue, "body, amended", nowMS+1)
	recordClaim(t, conn, after, nowMS+2)
	recordClaimEvent(t, conn, runID, issue, "second@0", nowMS+2)
	execSQL(t, conn, `UPDATE runs SET status = 'done' WHERE id = ?`, runID)

	steps := map[string]int{
		"claimed before the refresh": before,
		"claimed after the refresh":  after,
	}
	for name, stepID := range steps {
		if n := claimEventCount(t, conn, stepID); n != 1 {
			t.Fatalf("premise: step %s has %d step-claimed row(s) before the "+
				"prune, want 1", name, n)
		}
	}

	_, err := PruneEvents(conn, PruneQuery{BeforeRun: runID, NowMS: nowMS + 3})
	testsupport.Must(t, err, "PruneEvents: %v", err)

	for name, stepID := range steps {
		if n := claimEventCount(t, conn, stepID); n != 0 {
			t.Errorf("step %s: %d step-claimed row(s) survived the prune, want 0",
				name, n)
		}
		bundle, err := ReadContext(conn, stepID, nowMS+4)
		if err == nil {
			t.Errorf("step %s: read-back returned body %q; want it reported "+
				"unavailable after the prune", name, bundle.Issue.BodySnapshot)
			continue
		}
		if code, _ := CodeOf(err); code != CodeGone {
			t.Errorf("step %s: error code %q, want %q: %v", name, code, CodeGone, err)
		}
		if !strings.Contains(err.Error(), "pruned") {
			t.Errorf("step %s: error %q does not name the prune", name, err)
		}
	}
}

// TestRecordedBodyUnprunedRunReadsRecordedBody is the same fixture without the
// prune: each step reads the body its own claim was handed.
func TestRecordedBodyUnprunedRunReadsRecordedBody(t *testing.T) {
	conn, runID, issue, before, after := bodyRefreshRun(t)
	recordClaim(t, conn, before, nowMS)
	amended := "body, amended"
	refreshRunBody(t, conn, runID, issue, amended, nowMS+1)
	recordClaim(t, conn, after, nowMS+2)
	execSQL(t, conn, `UPDATE runs SET status = 'done' WHERE id = ?`, runID)

	if got := recordedBodyOf(t, conn, before); got != "body" {
		t.Errorf("pre-refresh step read %q, want the refresh row's from_body %q", got, "body")
	}
	if got := recordedBodyOf(t, conn, after); got != amended {
		t.Errorf("post-refresh step read %q, want the refreshed body %q", got, amended)
	}
}

// TestPruneBelowAClaimKeepsItsRecordedBody: a `--before` boundary that does not
// reach a step's claim event leaves every later event, the refresh row
// included, so that step's read-back is still served.
func TestPruneBelowAClaimKeepsItsRecordedBody(t *testing.T) {
	conn, runID, issue, before, _ := bodyRefreshRun(t)
	recordClaim(t, conn, before, nowMS)
	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	err = recordEvent(tx, eventRecord{
		Kind: EventStepClaimed, RunID: runID, Instance: "first@0", IssueID: issue, AtMS: nowMS,
	})
	testsupport.Must(t, err, "recording the claim event: %v", err)
	testsupport.Must(t, tx.Commit(), "committing the claim event")
	refreshRunBody(t, conn, runID, issue, "body, amended", nowMS+1)
	execSQL(t, conn, `UPDATE runs SET status = 'done' WHERE id = ?`, runID)

	var claimSeq int64
	err = conn.QueryRow(`SELECT seq FROM events WHERE kind = ? AND step_id = ?`,
		EventStepClaimed, before).Scan(&claimSeq)
	testsupport.Must(t, err, "reading the claim seq: %v", err)
	result, err := PruneEvents(conn, PruneQuery{Before: claimSeq, NowMS: nowMS + 3})
	testsupport.Must(t, err, "PruneEvents: %v", err)
	if result.Pruned == 0 {
		t.Fatal("the fixture pruned nothing below the claim; the case needs a prune")
	}

	if got := recordedBodyOf(t, conn, before); got != "body" {
		t.Errorf("read-back = %q, want the recorded body %q", got, "body")
	}
}

// TestRepoWidePruneAboveAClaimReportsRecordedBodyGone: a `--before` prune with
// no run named writes its `events-pruned` row with a NULL run_id. That row
// covers every run, so once it has removed a step's claim row the read-back
// reports the body GONE rather than reading the live columns.
func TestRepoWidePruneAboveAClaimReportsRecordedBodyGone(t *testing.T) {
	conn, runID, issue, before, _ := bodyRefreshRun(t)
	recordClaim(t, conn, before, nowMS)
	recordClaimEvent(t, conn, runID, issue, "first@0", nowMS)
	refreshRunBody(t, conn, runID, issue, "body, amended", nowMS+1)
	execSQL(t, conn, `UPDATE runs SET status = 'done' WHERE id = ?`, runID)

	var claimSeq int64
	err := conn.QueryRow(`SELECT seq FROM events WHERE kind = ? AND step_id = ?`,
		EventStepClaimed, before).Scan(&claimSeq)
	testsupport.Must(t, err, "reading the claim seq: %v", err)
	_, err = PruneEvents(conn, PruneQuery{Before: claimSeq + 1, NowMS: nowMS + 3})
	testsupport.Must(t, err, "PruneEvents: %v", err)

	if n := claimEventCount(t, conn, before); n != 0 {
		t.Fatalf("premise: %d step-claimed row(s) survived the prune, want 0", n)
	}
	var repoWide int
	err = conn.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND run_id IS NULL`,
		EventEventsPruned).Scan(&repoWide)
	testsupport.Must(t, err, "counting repo-wide prune rows: %v", err)
	if repoWide != 1 {
		t.Fatalf("premise: %d events-pruned row(s) with a NULL run_id, want 1", repoWide)
	}

	bundle, err := ReadContext(conn, before, nowMS+4)
	if err == nil {
		t.Fatalf("read-back returned body %q; want it reported unavailable "+
			"after the repo-wide prune", bundle.Issue.BodySnapshot)
	}
	if code, _ := CodeOf(err); code != CodeGone {
		t.Errorf("error code %q, want %q: %v", code, CodeGone, err)
	}
	if !strings.Contains(err.Error(), "pruned") {
		t.Errorf("error %q does not name the prune", err)
	}
}

// TestPruningAnotherRunKeepsThisRunsRecordedBody: a `--before-run` prune's
// `events-pruned` row names its run, and covers no other. Run A's step carries
// no `step-claimed` row, so only the prune row's run scoping keeps a prune of
// run B from reporting run A's body GONE.
func TestPruningAnotherRunKeepsThisRunsRecordedBody(t *testing.T) {
	conn, runA, issueA, stepA, _ := bodyRefreshRun(t)
	recordClaim(t, conn, stepA, nowMS)
	amended := "body, amended"
	refreshRunBody(t, conn, runA, issueA, amended, nowMS+1)
	execSQL(t, conn, `UPDATE runs SET status = 'done' WHERE id = ?`, runA)

	issueB := createIssue(t, conn, "other work", "other body", "task", nil)
	runB := startRun(t, conn, issueB)
	_, err := activate(conn, runB.ID)
	testsupport.Must(t, err, "activate run B: %v", err)
	execSQL(t, conn, `UPDATE runs SET status = 'done' WHERE id = ?`, runB.ID)

	result, err := PruneEvents(conn, PruneQuery{BeforeRun: runB.ID, NowMS: nowMS + 3})
	testsupport.Must(t, err, "PruneEvents: %v", err)
	if result.Pruned == 0 {
		t.Fatal("premise: pruning run B deleted nothing; the case needs a prune row")
	}

	if got := recordedBodyOf(t, conn, stepA); got != "body" {
		t.Errorf("run A's read-back = %q, want its recorded body %q", got, "body")
	}
}
