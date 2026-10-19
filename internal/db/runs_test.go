package db

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestPinContentSurvivesReopen pins the pinned file's bytes to the store, not
// the process: a second machine opening the same database renders from them.
func TestPinContentSurvivesReopen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		// A NUL and an invalid UTF-8 byte prove the column round-trips raw
		// bytes rather than text.
		{"binary", "# Charter\x00\xff\nWork as a senior engineer.\n"},
		// An empty file is still recorded content, distinct from none.
		{"empty file", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := migratedStoreFile(t)
			want := sql.NullString{String: tc.content, Valid: true}

			writer, err := Open(path)
			testsupport.Must(t, err, "Open: %v", err)
			run, err := InsertRun(writer, 1, "pin content", 0, 1)
			testsupport.Must(t, err, "InsertRun: %v", err)
			tx, err := writer.Begin()
			testsupport.Must(t, err, "Begin: %v", err)
			err = InsertPinTx(tx, Pin{
				RunID: run.ID, Kind: PinKindFile, Ref: "contracts/implement.md",
				SHA256: "b52312cc", Content: want,
			})
			testsupport.Must(t, err, "InsertPinTx: %v", err)
			err = tx.Commit()
			testsupport.Must(t, err, "Commit: %v", err)
			err = writer.Close()
			testsupport.Must(t, err, "Close: %v", err)

			reader, err := Open(path)
			testsupport.Must(t, err, "reopening: %v", err)
			t.Cleanup(func() { reader.Close() })

			pins, err := ListPins(reader, run.ID)
			testsupport.Must(t, err, "ListPins: %v", err)
			if len(pins) != 1 || pins[0].Content != want {
				t.Errorf("ListPins = %+v, want one pin with content %+v", pins, want)
			}

			var storage string
			err = reader.QueryRow(`SELECT typeof(content) FROM pins WHERE run_id = ?`,
				run.ID).Scan(&storage)
			testsupport.Must(t, err, "reading storage class: %v", err)
			if storage != "blob" {
				t.Errorf("pins.content stored as %s, want blob", storage)
			}

			tx, err = reader.Begin()
			testsupport.Must(t, err, "Begin: %v", err)
			defer tx.Rollback()
			txPins, err := ListPinsTx(tx, run.ID)
			testsupport.Must(t, err, "ListPinsTx: %v", err)
			if len(txPins) != 1 || txPins[0].Content != want {
				t.Errorf("ListPinsTx = %+v, want one pin with content %+v", txPins, want)
			}
		})
	}
}
