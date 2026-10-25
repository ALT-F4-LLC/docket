package db_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db/pgstore"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// contractStore is one Store implementation under the contract suite. open
// returns a fresh, migrated store that no other case shares and that closes
// when the test ends.
type contractStore struct {
	name string
	open func(t *testing.T) store.Store
}

// contractCase is one behavior every Store implementation must show. It
// reaches the store only through the Store and Tx interfaces, so the same
// case runs unchanged against each implementation.
type contractCase struct {
	name string
	run  func(t *testing.T, st store.Store)
}

// TestStoreContract runs every contract case against every store, each case
// on a store of its own.
func TestStoreContract(t *testing.T) {
	stores := []contractStore{
		{name: "sqlite", open: openSQLiteContractStore},
		{name: "postgres", open: openPostgresContractStore},
	}

	var cases []contractCase
	cases = append(cases, versionContract...)
	cases = append(cases, leaseContract...)
	cases = append(cases, commentContract...)
	cases = append(cases, relationContract...)
	cases = append(cases, issueContract...)

	for _, s := range stores {
		t.Run(s.name, func(t *testing.T) {
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					c.run(t, s.open(t))
				})
			}
		})
	}
}

// openSQLiteContractStore opens the SQLite store on a file rather than
// :memory:, because the store's reader is a second connection and must see
// the writer's database.
func openSQLiteContractStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "issues.db"))
	testsupport.Must(t, err, "OpenSQLite: %v", err)
	t.Cleanup(func() { st.Close() })
	err = st.Initialize()
	testsupport.Must(t, err, "Initialize: %v", err)
	err = st.Migrate()
	testsupport.Must(t, err, "Migrate: %v", err)
	return st
}

// openPostgresContractStore opens the PostgreSQL store in a schema made for
// this case alone and dropped when it ends. It skips without
// DOCKET_TEST_POSTGRES_DSN.
func openPostgresContractStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()
	dsn, passwordFile := testsupport.PostgresDSN(t), testsupport.PostgresPasswordFile()

	admin, err := pgstore.Open(ctx, dsn, passwordFile)
	testsupport.Must(t, err, "pgstore.Open: %v", err)
	t.Cleanup(func() { admin.Close() })
	suffix := make([]byte, 8)
	_, err = rand.Read(suffix)
	testsupport.Must(t, err, "rand.Read: %v", err)
	schema := "docket_contract_" + hex.EncodeToString(suffix)
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	testsupport.Must(t, err, "creating schema %s: %v", schema, err)
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Errorf("dropping schema %s: %v", schema, err)
		}
	})

	st, err := pgstore.OpenStore(ctx, dsn, passwordFile, schema)
	testsupport.Must(t, err, "pgstore.OpenStore: %v", err)
	// Registered after the schema drop, so it runs first and the drop waits
	// on no open connection.
	t.Cleanup(func() { st.Close() })
	err = st.Initialize()
	testsupport.Must(t, err, "Initialize: %v", err)
	err = st.Migrate()
	testsupport.Must(t, err, "Migrate: %v", err)
	return st
}

// createContractIssue creates a todo task and returns its id.
func createContractIssue(t *testing.T, st store.Store, title string) int {
	t.Helper()
	id, _, err := st.CreateIssueIdempotent(&model.Issue{
		Title:    title,
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}, nil, nil, "")
	testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)
	return id
}
