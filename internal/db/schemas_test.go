package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/schema"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// aSchema is a registration's inputs, as `docket schema register` assembles
// them. The `ordered` index is a plain string here because this package stores
// it rather than deriving it — the derivation is internal/schema's, and its
// round-trip is asserted there.
func aSchema(name string, version int, body, ordered string) *model.Schema {
	return &model.Schema{
		Name: name, Version: version,
		SourcePath:   name + ".json",
		SourceSHA256: sha256Of(body),
		Body:         body,
		Ordered:      ordered,
	}
}

// sha256Of is the content hash a registration records — the real one, because
// the CONFLICT case below asserts that the refusal names BOTH hashes, and a
// stand-in would let that assertion pass against a message naming neither.
func sha256Of(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func migratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = Migrate(db)
	testsupport.Must(t, err, "Migrate: %v", err)
	return db
}

// TestInsertSchemaThreeOutcomes is §4.4's immutability table, verbatim in
// behavior with `InsertWorkflow`'s. Two registries with the same contract that
// behaved differently would be two implementations of one rule.
func TestInsertSchemaThreeOutcomes(t *testing.T) {
	db := migratedDB(t)
	const body = `{"type":"array"}`

	// (1) No row at name@version: insert, created = true.
	stored, created, err := InsertSchema(db, aSchema("findings", 1, body, `{}`), 1)
	testsupport.Must(t, err, "first insert: %v", err)
	if !created {
		t.Error("created = false on a first registration")
	}
	if stored.Ref() != "findings@1" || stored.Body != body {
		t.Errorf("stored = %+v, want the bytes as given", stored)
	}
	if stored.RowVersion != 1 {
		t.Errorf("row_version = %d on insert, want 1", stored.RowVersion)
	}

	// (2) The SAME source_sha256: return the existing row, created = false,
	// insert nothing, and DO NOT bump row_version — or `--if-version` would
	// fail for a caller that changed nothing.
	again, created, err := InsertSchema(db, aSchema("findings", 1, body, `{}`), 2)
	testsupport.Must(t, err, "idempotent re-register: %v", err)
	if created {
		t.Error("created = true on identical bytes")
	}
	if again.RowVersion != 1 {
		t.Errorf("row_version = %d after an idempotent re-register, want 1", again.RowVersion)
	}
	if again.CreatedAtMS != stored.CreatedAtMS {
		t.Errorf("created_at_ms moved from %d to %d; nothing was supposed to be written",
			stored.CreatedAtMS, again.CreatedAtMS)
	}

	// (3) A DIFFERENT source_sha256: CONFLICT naming BOTH hashes.
	_, _, err = InsertSchema(db, aSchema("findings", 1, `{"type":"object","x":1}`, `{}`), 3)
	if !errors.Is(err, ErrSchemaConflict) {
		t.Fatalf("differing bytes gave %v, want ErrSchemaConflict", err)
	}
	if !strings.Contains(err.Error(), stored.SourceSHA256) {
		t.Errorf("the refusal does not name the REGISTERED hash: %v", err)
	}
	if !strings.Contains(err.Error(), sha256Of(`{"type":"object","x":1}`)) {
		t.Errorf("the refusal does not name the OFFERED hash: %v", err)
	}

	// A new version is an ordinary registration, which is the remedy the
	// CONFLICT exists to push an author toward.
	if _, created, err := InsertSchema(db, aSchema("findings", 2, `{"type":"object"}`, `{}`), 4); err != nil || !created {
		t.Errorf("registering at a new version: created=%v err=%v", created, err)
	}

	// One builtin plus findings@1 and findings@2.
	if n := countSchemas(t, db); n != 3 {
		t.Errorf("schemas holds %d rows, want 3", n)
	}
}

// TestGetSchemaWithoutAVersionTakesTheHighest is `schema show NAME`'s rule. It
// is a READ verb's convenience and is never what a `payload` declaration or a
// pin may mean — those name an exact version, which is what a run reproduces
// against.
func TestGetSchemaWithoutAVersionTakesTheHighest(t *testing.T) {
	db := migratedDB(t)
	for _, v := range []int{1, 3, 2} {
		_, _, err := InsertSchema(db, aSchema("findings", v, `{"type":"array","v":`+strconv.Itoa(v)+`}`, `{}`), 1)
		testsupport.Must(t, err, "registering findings@%d: %v", v, err)
	}

	got, err := GetSchema(db, 1, "findings", 0)
	testsupport.Must(t, err, "GetSchema: %v", err)
	if got.Version != 3 {
		t.Errorf("GetSchema(findings, 0) = v%d, want the highest registered (3)", got.Version)
	}

	if _, err := GetSchema(db, 1, "findings", 9); !errors.Is(err, ErrSchemaNotFound) {
		t.Errorf("an absent version gave %v, want ErrSchemaNotFound", err)
	}
	if _, err := GetSchema(db, 1, "absent", 0); !errors.Is(err, ErrSchemaNotFound) {
		t.Errorf("an absent name gave %v, want ErrSchemaNotFound", err)
	}
}

// TestListSchemasReportsTheTrueTotal is the Collection contract
// (reliability-delta §4.1): the total ignores the limit, so truncation is
// computable rather than guessed.
func TestListSchemasReportsTheTrueTotal(t *testing.T) {
	db := migratedDB(t)
	for _, v := range []int{1, 2, 3} {
		_, _, err := InsertSchema(db, aSchema("findings", v, `{"type":"array","v":`+strconv.Itoa(v)+`}`, `{}`), 1)
		testsupport.Must(t, err, "registering findings@%d: %v", v, err)
	}

	rows, total, err := ListSchemas(db, SchemaListOptions{Limit: 2})
	testsupport.Must(t, err, "ListSchemas: %v", err)
	if len(rows) != 2 {
		t.Errorf("returned %d rows under --limit 2", len(rows))
	}
	// Three findings plus the builtin.
	if total != 4 {
		t.Errorf("total = %d, want 4 — the count must ignore the limit", total)
	}

	// The name filter narrows both the rows AND the total.
	rows, total, err = ListSchemas(db, SchemaListOptions{Name: "findings"})
	testsupport.Must(t, err, "ListSchemas(name): %v", err)
	if len(rows) != 3 || total != 3 {
		t.Errorf("filtered list = %d rows / total %d, want 3/3", len(rows), total)
	}
	// Newest version first, so `schema list` reads top-down as an author
	// expects.
	if rows[0].Version != 3 {
		t.Errorf("first row is v%d, want the highest version", rows[0].Version)
	}
}

// TestGetSchemaTxSeesTheOpenTransaction is what activation's pin stage needs:
// the hash it records and the row it checked are one read, inside the fat
// transaction.
func TestGetSchemaTxSeesTheOpenTransaction(t *testing.T) {
	db := migratedDB(t)

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()

	got, err := GetSchemaTx(tx, 1, schema.AggregateName, schema.AggregateVersion)
	testsupport.Must(t, err, "GetSchemaTx(%s): %v", schema.AggregateRef(), err)
	if got.SourceSHA256 != schema.AggregateSHA256() {
		t.Errorf("hash = %q, want the embedded document's", got.SourceSHA256)
	}

	if _, err := GetSchemaTx(tx, 1, "absent", 1); !errors.Is(err, ErrSchemaNotFound) {
		t.Errorf("an absent schema gave %v, want ErrSchemaNotFound", err)
	}
}
