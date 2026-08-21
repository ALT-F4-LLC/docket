package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `events prune`'s refusals, which are the verb (docs/tdd/events-follow.md §5.6).
//
// The tests are weighted the way the design is: one test for what it deletes,
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

// TestPruneRefusalMatrix is §5.6's matrix: every way the verb says no, with the
// code and the reason each one carries.
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
			// P1: no target. "Prune everything" is not expressible, because a
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
			// P1: two targets. They name different things — a seq boundary and a
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
			// §5.2, THE HEADLINE REFUSAL: a run that has not finished.
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

// TestRetentionWindowHoldsTheBoundary is §5.3: `events.retain` protects recent
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
	// P14: HELD BACK, AND SAID SO. An operator told only "0" would conclude the
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

// TestRetentionDefaultRetainsEverything is P13 and D1: the window defaults to 0,
// and 0 means EVERYTHING IS PROTECTED.
//
// The direction matters more than the number. A retention key defaulting to
// "keep nothing" would make the first `events prune --before` an operator ever
// typed delete their whole log — the opposite of the posture operations.md §2
// documents, where Docket deletes nothing it was not asked to delete.
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

// TestPruneDeletesOnlyEvents is P16 and P17. The verb's blast radius is one
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
			t.Errorf("%s went from %d rows to %d: the prune reached outside `events` (P16)",
				table, n, after[table])
		}
	}
}

// TestPruningALiveRunWouldMoveTheFloor is §5.2's ARGUMENT, executed.
//
// The refusal is not fastidiousness about audit trails. The budget floor is a
// SUM over a run's `step-claimed` events (§4.3), so deleting them does not lose
// a record of what the run spent — it changes what the engine BELIEVES the run
// spent, downward, which is the direction that lets a capped run overspend.
//
// The test proves that by doing the forbidden thing directly, the same way S6's
// GONE test constructed a state no product path could reach: refuse through the
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
		t.Fatal("a live run's events were pruned; §5.2 refuses them")
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

// TestPruneDryRunWritesNothing is P5: `--dry-run` answers the question without
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

// TestPruneIsEventLogged is P18: the prune records itself, ABOVE what it
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
		t.Fatal("the prune wrote no `events-pruned` event (P18)")
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

	// And it is ATTRIBUTABLE, which is what keeps §9 item 2 checkable over a
	// log that has been trimmed.
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
