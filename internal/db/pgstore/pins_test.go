package pgstore

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestStoreContract holds the PostgreSQL store's own contract cases, each on
// a fresh schema. It skips without DOCKET_TEST_POSTGRES_DSN.
func TestStoreContract(t *testing.T) {
	t.Run("PinContentRoundTrips", testPinContentRoundTrips)
	t.Run("EventWatermarkAndGap", testEventWatermarkAndGap)
}

// testPinContentRoundTrips inserts a pin with its file's bytes and reads the
// same bytes back, outside and inside a transaction.
func testPinContentRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		// A NUL and an invalid UTF-8 byte prove the column carries raw bytes
		// rather than text.
		{"binary", "# Charter\x00\xff\nWork as a senior engineer.\n"},
		// An empty file is still recorded content, distinct from none.
		{"empty file", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openTestStore(t)
			project, err := st.EnsureProject("/repo/pins", "pins", 1)
			testsupport.Must(t, err, "EnsureProject: %v", err)
			run, _, err := st.InsertRunWithContextIdempotent(project, "pin content", 0, 1, db.RunContext{}, "")
			testsupport.Must(t, err, "InsertRunWithContextIdempotent: %v", err)
			want := sql.NullString{String: tc.content, Valid: true}

			tx, err := st.Begin()
			testsupport.Must(t, err, "Begin: %v", err)
			err = tx.InsertPinTx(db.Pin{
				RunID: run.ID, Kind: db.PinKindFile, Ref: "contracts/implement.md",
				SHA256: "b52312cc", Content: want,
			})
			testsupport.Must(t, err, "InsertPinTx: %v", err)
			err = tx.Commit()
			testsupport.Must(t, err, "Commit: %v", err)

			pins, err := st.ListPins(run.ID)
			testsupport.Must(t, err, "ListPins: %v", err)
			if len(pins) != 1 || pins[0].Content != want {
				t.Errorf("ListPins = %+v, want one pin with content %+v", pins, want)
			}

			snap, err := st.BeginReadSnapshot()
			testsupport.Must(t, err, "BeginReadSnapshot: %v", err)
			defer snap.Rollback()
			txPins, err := snap.ListPinsTx(run.ID)
			testsupport.Must(t, err, "ListPinsTx: %v", err)
			if len(txPins) != 1 || txPins[0].Content != want {
				t.Errorf("ListPinsTx = %+v, want one pin with content %+v", txPins, want)
			}
		})
	}
}
