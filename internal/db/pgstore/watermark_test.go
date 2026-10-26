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
