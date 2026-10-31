package pgstore

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// testEventWatermarkAndGap runs the shared watermark-and-gap case on a fresh
// schema, arranging through the store's own pool and reading the watermark
// through a read snapshot.
func testEventWatermarkAndGap(t *testing.T) {
	st := openTestStore(t)
	testsupport.EventWatermarkContract(t, st.(*pgStore).db, func() (int64, error) {
		snap, err := st.BeginReadSnapshot()
		if err != nil {
			return 0, err
		}
		defer snap.Rollback()
		return snap.EventLowWatermarkTx()
	})
}

// testRaiseEventLowWatermark raises the watermark over events at seq 5 and 9:
// from 0 it climbs to 4, just below the lowest survivor, and from 7 it stays,
// because a raise never lowers it.
func testRaiseEventLowWatermark(t *testing.T) {
	st := openTestStore(t)
	conn := st.(*pgStore).db
	_, err := conn.Exec(`INSERT INTO events (seq, at_ms, kind) VALUES (5, 1, 'raise'), (9, 1, 'raise')`)
	testsupport.Must(t, err, "inserting events: %v", err)

	for _, tc := range []struct {
		name      string
		from      int64
		watermark int64
	}{
		{"row at 0 climbs below the lowest event", 0, 4},
		{"row at 7 is never lowered", 7, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := conn.Exec(`UPDATE event_low_watermark SET seq = ? WHERE id = 1`, tc.from)
			testsupport.Must(t, err, "setting the watermark row: %v", err)

			tx, err := st.Begin()
			testsupport.Must(t, err, "Begin: %v", err)
			defer tx.Rollback()
			testsupport.Must(t, tx.RaiseEventLowWatermarkTx(), "RaiseEventLowWatermarkTx")
			testsupport.Must(t, tx.Commit(), "Commit")

			var got int64
			err = conn.QueryRow(`SELECT seq FROM event_low_watermark WHERE id = 1`).Scan(&got)
			testsupport.Must(t, err, "reading the watermark row: %v", err)
			if got != tc.watermark {
				t.Errorf("watermark after raising from %d = %d, want %d", tc.from, got, tc.watermark)
			}
		})
	}
}
