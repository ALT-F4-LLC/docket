package engine

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// readEventLowWatermark reads the watermark row directly, so a test asserts
// the stored fact rather than the engine's reading of it.
func readEventLowWatermark(t *testing.T, conn *sql.DB) int64 {
	t.Helper()
	var watermark int64
	err := conn.QueryRow(`SELECT seq FROM event_low_watermark WHERE id = 1`).Scan(&watermark)
	testsupport.Must(t, err, "reading the event low watermark: %v", err)
	return watermark
}

func readMinSeq(t *testing.T, conn *sql.DB) int64 {
	t.Helper()
	var minSeq int64
	err := conn.QueryRow(`SELECT MIN(seq) FROM events`).Scan(&minSeq)
	testsupport.Must(t, err, "reading MIN(seq): %v", err)
	return minSeq
}

// TestWatermarkMigrationSeedsFromAPrunedStore migrates a store whose oldest
// events were already deleted, and asserts the watermark is the highest pruned
// seq: one below the lowest survivor.
func TestWatermarkMigrationSeedsFromAPrunedStore(t *testing.T) {
	conn := mustDB(t)
	budgetRun(t, conn, 0)
	claimInstance(t, conn, "implement@0", nowMS)

	// A pre-watermark store that pruned seq 1 and 2.
	const prunedThrough = 2
	execSQL(t, conn, `DELETE FROM events WHERE seq <= ?`, prunedThrough)
	if got := readMinSeq(t, conn); got != prunedThrough+1 {
		t.Fatalf("fixture MIN(seq) = %d, want %d", got, prunedThrough+1)
	}
	execSQL(t, conn, `DROP TABLE event_low_watermark`)
	execSQL(t, conn, `UPDATE meta SET value = '45' WHERE key = 'schema_version'`)

	err := db.Migrate(conn)
	testsupport.Must(t, err, "Migrate from v45: %v", err)

	if got := readEventLowWatermark(t, conn); got != prunedThrough {
		t.Fatalf("migrated watermark = %d, want %d (the highest pruned seq)",
			got, prunedThrough)
	}

	_, err = ListEvents(conn, EventQuery{Since: prunedThrough - 1})
	if code, ok := CodeOf(err); !ok || code != CodeGone {
		t.Errorf("a cursor below the migrated watermark answered %v; want GONE", err)
	}
	if _, err := ListEvents(conn, EventQuery{Since: prunedThrough}); err != nil {
		t.Errorf("a cursor at the migrated watermark was refused: %v", err)
	}
}

// TestWatermarkIgnoresSeqGaps builds the gap a PostgreSQL rollback leaves:
// seqs below the first committed row were consumed and never written. Nothing
// was pruned, so no cursor is GONE and the retained minimum is still 1.
func TestWatermarkIgnoresSeqGaps(t *testing.T) {
	conn := mustDB(t)

	var existing int
	err := conn.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&existing)
	testsupport.Must(t, err, "counting events: %v", err)
	if existing != 0 {
		t.Fatalf("a fresh store holds %d events; the gap fixture needs none", existing)
	}
	execSQL(t, conn, `INSERT INTO events (seq, at_ms, kind) VALUES (41, ?, ?)`,
		nowMS, EventStepHeartbeat)
	budgetRun(t, conn, 0)

	if got := readMinSeq(t, conn); got <= 1 {
		t.Fatalf("fixture MIN(seq) = %d; the gap needs MIN(seq) > 1", got)
	}
	if got := readEventLowWatermark(t, conn); got != 0 {
		t.Fatalf("watermark = %d with nothing pruned, want 0", got)
	}

	page, err := ListEvents(conn, EventQuery{Since: 0})
	testsupport.Must(t, err, "a seq gap answered %v; nothing was pruned, so nothing is GONE", err)
	if len(page.Events) == 0 || page.Events[0].Seq != 41 {
		t.Errorf("the feed from cursor 0 does not start at the first committed seq 41: %+v",
			page.Events)
	}

	result, err := PruneEvents(conn, PruneQuery{Before: 1, DryRun: true, NowMS: nowMS})
	testsupport.Must(t, err, "dry-run PruneEvents: %v", err)
	if result.RetainedMinimum != 1 {
		t.Errorf("RetainedMinimum = %d with nothing pruned, want 1", result.RetainedMinimum)
	}

	execSQL(t, conn, `DELETE FROM events`)
	if _, err := ListEvents(conn, EventQuery{Since: 99}); err != nil {
		t.Errorf("an empty event table answered GONE: %v", err)
	}
}
