package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Schema registration sentinels. They mirror the workflow ones exactly, because
// the two registries have the same immutability contract (TDD §4.4) and a
// caller mapping errors to exit codes should not have to learn it twice.
var (
	// ErrSchemaConflict means `name@version` is already registered with
	// DIFFERENT bytes. Surfaced as CONFLICT (exit 4), naming both hashes.
	//
	// Why a conflict and not an overwrite: engine-core §4's pinning property —
	// "editing a pipeline never changes an in-flight run" — is worth nothing if
	// the pinned bytes can be swapped underneath the run. A schema decides
	// whether a worker's payload is ACCEPTED, so a mutable findings@1 means a
	// run's acceptance criteria change mid-flight. Bump the version.
	ErrSchemaConflict = errors.New("schema already registered with different content")

	// ErrSchemaNotFound means no row matches the requested name/version.
	// Surfaced as NOT_FOUND (exit 2).
	ErrSchemaNotFound = errors.New("schema not found")
)

// InsertSchema registers a schema document, or returns the existing row when
// the same bytes are already registered at that `name@version`.
//
// The three outcomes are `InsertWorkflow`'s, verbatim in behavior (§4.4):
//
//   - no row at name@version               -> insert, created = true
//   - a row with the SAME source_sha256    -> return it, created = false
//   - a row with a DIFFERENT source_sha256 -> ErrSchemaConflict
//
// Idempotency is decided on the CONTENT HASH, not on a normalized form: two
// documents that validate identically but differ in whitespace or key order are
// different registered bytes, because `source_sha256` is what pins refer to
// (§4.7) and what a run reproduces against.
func InsertSchema(db *sql.DB, s *model.Schema, nowMS int64) (stored *model.Schema, created bool, err error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	inserted, created, err := InsertSchemaTx(tx, s, nowMS)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("committing transaction: %w", err)
	}
	return inserted, created, nil
}

// InsertSchemaTx is InsertSchema inside a CALLER'S transaction — the schema
// half of what S6's auto-registration needs (docs/tdd/runs-dispatch.md §9.2 F8).
//
// It mirrors InsertWorkflowTx exactly, because the two registries have the same
// immutability contract and a caller should not have to learn it twice. The
// reason both are needed inside a transaction is F8's: auto-registration runs in
// activation's fat transaction, so a failure refuses the whole activation and
// leaves no definitions behind from a run that never started.
func InsertSchemaTx(
	tx *sql.Tx, s *model.Schema, nowMS int64,
) (stored *model.Schema, created bool, err error) {
	projectID := projectOrDefault(s.ProjectID)
	existing, err := getSchemaTx(tx, projectID, s.Name, s.Version)
	if err != nil && !errors.Is(err, ErrSchemaNotFound) {
		return nil, false, err
	}
	if existing != nil {
		if existing.SourceSHA256 != s.SourceSHA256 {
			return nil, false, fmt.Errorf(
				"%w: %s is registered as %s, these bytes are %s",
				ErrSchemaConflict, s.Ref(), existing.SourceSHA256, s.SourceSHA256)
		}
		// Identical bytes: an idempotent success returning the existing row.
		// Nothing is inserted and nothing is updated — re-registering must not
		// bump row_version, or `--if-version` would fail for a caller that
		// changed nothing.
		return existing, false, nil
	}

	builtin := 0
	if s.Builtin {
		builtin = 1
	}
	res, err := tx.Exec(
		`INSERT INTO schemas
		   (project_id, name, version, source_path, source_sha256, body, ordered, builtin,
		    created_at_ms, row_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		projectID, s.Name, s.Version, nullable(s.SourcePath), s.SourceSHA256, s.Body,
		s.Ordered, builtin, nowMS,
	)
	if err != nil {
		return nil, false, fmt.Errorf("inserting schema: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, false, fmt.Errorf("reading inserted schema id: %w", err)
	}

	inserted, err := getSchemaTx(tx, projectID, s.Name, s.Version)
	if err != nil {
		return nil, false, err
	}
	inserted.ID = int(id)
	return inserted, true, nil
}

// GetSchema returns one registered schema visible to a project — its own
// registration or a builtin. A version of 0 selects the HIGHEST registered
// version, which is what `schema show NAME` without `@version` means.
func GetSchema(db *sql.DB, projectID int, name string, version int) (*model.Schema, error) {
	projectID = projectOrDefault(projectID)
	if version > 0 {
		return scanSchema(db.QueryRow(
			schemaSelect+` WHERE `+schemaProjectPredicate+` AND name = ? AND version = ?`,
			projectID, name, version))
	}
	return scanSchema(db.QueryRow(
		schemaSelect+` WHERE `+schemaProjectPredicate+` AND name = ? ORDER BY version DESC LIMIT 1`,
		projectID, name))
}

// GetSchemaTx is GetSchema at an exact version, inside a transaction. It is
// what activation's pin stage reads, so the hash it records and the row it
// checked are the same read (§4.7 P1).
func GetSchemaTx(tx *sql.Tx, projectID int, name string, version int) (*model.Schema, error) {
	return getSchemaTx(tx, projectID, name, version)
}

// SchemaListOptions filters `schema list`.
type SchemaListOptions struct {
	// ProjectID scopes the list to one project's visible schemas — its own
	// plus builtins (v12); 0 = every project's.
	ProjectID int
	Name      string
	Limit     int
}

// ListSchemas returns registered schemas and the TRUE total before the limit —
// the Collection contract (reliability-delta §4.1) requires a total a limit
// cannot distort.
func ListSchemas(db *sql.DB, opts SchemaListOptions) ([]*model.Schema, int, error) {
	var clauses []string
	var args []any
	if opts.ProjectID != 0 {
		clauses = append(clauses, schemaProjectPredicate)
		args = append(args, opts.ProjectID)
	}
	if opts.Name != "" {
		clauses = append(clauses, `name = ?`)
		args = append(args, opts.Name)
	}
	where := ``
	if len(clauses) > 0 {
		where = ` WHERE ` + strings.Join(clauses, ` AND `)
	}

	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schemas`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting schemas: %w", err)
	}

	query := schemaSelect + where + ` ORDER BY name ASC, version DESC`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing schemas: %w", err)
	}
	schemas, err := scanRows(rows, "schemas", func(r *sql.Rows) (*model.Schema, error) {
		return scanSchemaRows(r)
	})
	if err != nil {
		return nil, 0, err
	}

	return schemas, total, nil
}

// schemaSelect names the columns in a fixed order, so the two scan helpers
// cannot drift apart.
const schemaSelect = `
SELECT id, project_id, name, version, source_path, source_sha256, body, ordered, builtin,
       created_at_ms, row_version
  FROM schemas`

// schemaProjectPredicate is the visibility rule every schema lookup shares:
// a project sees its own registrations plus the builtin rows. `aggregate@1`
// ships in the binary and sits on whichever project row seeded it; the
// builtin FLAG, not its project, is what makes it everyone's (v12).
const schemaProjectPredicate = `(project_id = ? OR builtin = 1)`

func scanSchema(row rowScanner) (*model.Schema, error) {
	s, err := scanSchemaRows(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSchemaNotFound
	}
	return s, err
}

func scanSchemaRows(row rowScanner) (*model.Schema, error) {
	var (
		s          model.Schema
		sourcePath sql.NullString
		builtin    int
	)
	err := row.Scan(
		&s.ID, &s.ProjectID, &s.Name, &s.Version, &sourcePath, &s.SourceSHA256, &s.Body,
		&s.Ordered, &builtin, &s.CreatedAtMS, &s.RowVersion,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("reading schema: %w", err)
	}
	s.SourcePath = sourcePath.String
	s.Builtin = builtin != 0
	return &s, nil
}

func getSchemaTx(tx *sql.Tx, projectID int, name string, version int) (*model.Schema, error) {
	s, err := scanSchemaRows(tx.QueryRow(
		schemaSelect+` WHERE `+schemaProjectPredicate+` AND name = ? AND version = ?`,
		projectOrDefault(projectID), name, version))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSchemaNotFound
	}
	return s, err
}
