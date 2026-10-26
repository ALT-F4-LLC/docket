package pgstore

import "github.com/ALT-F4-LLC/docket/internal/db"

// EventLowWatermarkTx reads the highest pruned event seq from the
// event_low_watermark row, never from the events table, so a seq gap that
// rolled-back inserts left reads as retained rather than pruned. The retained
// minimum is this value plus 1.
func (t *pgTx) EventLowWatermarkTx() (int64, error) {
	return db.EventLowWatermarkTx(t.tx)
}
