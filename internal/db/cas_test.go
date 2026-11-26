package db

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func TestAllVersionedTablesHaveVersionColumn(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = Migrate(db)
	testsupport.Must(t, err, "Migrate: %v", err)

	for _, table := range versionedTables {
		var n int
		err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'version'`, table,
		).Scan(&n)
		testsupport.Must(t, err, "inspecting %s: %v", table, err)
		if n != 1 {
			t.Errorf("%s has %d version columns, want 1", table, n)
		}
	}
}
