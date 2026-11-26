// Package storetest hands a test a migrated SQLite store without migrating
// one for it. The store is built once per test binary, the way a first use of
// docket builds it, and each caller gets a private copy.
//
// It is test-only, like internal/testsupport, and reaches the database through
// internal/store alone. internal/db and internal/store cannot import it from
// their in-package tests, since it imports internal/store; they keep images of
// their own.
package storetest

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

var migrated testsupport.SQLiteImage

// SeedFile writes a migrated store to path, which must not exist yet. Opening
// path afterwards finds the store at the current schema version, so
// Initialize and Migrate on it change nothing.
func SeedFile(t *testing.T, path string) {
	t.Helper()
	migrated.File(t, path, func(path string) error {
		st, err := store.OpenSQLite(path)
		if err != nil {
			return err
		}
		if err := migrate(st); err != nil {
			st.Close()
			return err
		}
		return st.Close()
	})
}

// SeedMemory replaces the contents of conn, an in-memory database opened by
// store.OpenSQLiteDB or db.Open, with a migrated store.
func SeedMemory(t *testing.T, conn *sql.DB) {
	t.Helper()
	migrated.Memory(t, conn, func() (*sql.DB, error) {
		source, err := store.OpenSQLiteDB(":memory:")
		if err != nil {
			return nil, err
		}
		if err := migrate(store.StoreOn(source)); err != nil {
			source.Close()
			return nil, err
		}
		return source, nil
	})
}

func migrate(st store.Store) error {
	if err := st.Initialize(); err != nil {
		return err
	}
	return st.Migrate()
}
