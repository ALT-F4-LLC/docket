package testsupport

import (
	"database/sql"
	"testing"
)

// EventWatermarkContract is the store-contract case for the event low
// watermark, shared by every store. On a freshly migrated store it checks that
// the migration seeded the one-row watermark at 0, and that the retained
// minimum the store's read yields is the row's seq plus 1 even when the event
// log holds a seq gap that no prune made.
//
// conn reaches the store's own tables and arranges the events and the row;
// readWatermark is the store's watermark read, called afresh for each check.
// The statements bind no placeholders, so either engine runs them unchanged.
func EventWatermarkContract(t *testing.T, conn *sql.DB, readWatermark func() (int64, error)) {
	t.Helper()
	rowSeq := func() int64 {
		t.Helper()
		var seq int64
		err := conn.QueryRow(`SELECT seq FROM event_low_watermark WHERE id = 1`).Scan(&seq)
		Must(t, err, "reading the event_low_watermark row: %v", err)
		return seq
	}
	checkRetainedMinimum := func(state string) {
		t.Helper()
		want := rowSeq() + 1
		watermark, err := readWatermark()
		Must(t, err, "%s: reading the watermark: %v", state, err)
		if got := watermark + 1; got != want {
			t.Errorf("%s: retained minimum = %d, want %d, the watermark row plus 1", state, got, want)
		}
	}

	if got := rowSeq(); got != 0 {
		t.Errorf("fresh store: watermark row seq = %d, want 0", got)
	}
	checkRetainedMinimum("fresh store")

	// Seq 1-4 and 6-8 were never committed, as rolled-back inserts leave them;
	// nothing was pruned, so the retained minimum stays 1.
	_, err := conn.Exec(`INSERT INTO events (seq, at_ms, kind) VALUES (5, 1, 'contract-gap'), (9, 1, 'contract-gap')`)
	Must(t, err, "inserting events around a seq gap: %v", err)
	checkRetainedMinimum("seq gap with nothing pruned")

	_, err = conn.Exec(`UPDATE event_low_watermark SET seq = 7 WHERE id = 1`)
	Must(t, err, "setting the watermark row: %v", err)
	checkRetainedMinimum("watermark row at 7")
}
