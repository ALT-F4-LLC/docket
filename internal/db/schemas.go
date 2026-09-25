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
// registration or a builtin. A version of 0 selects the HIGHEST version still
// IN SERVICE, which is what `schema show NAME` without `@version` means —
// GetWorkflow's rule (v36 mirrors v11): a name whose every version is retired
// resolves to ErrSchemaNotFound, while an explicit version still resolves a
// retired row, because retired versions stay registered and reachable for the
// runs that pinned them.
func GetSchema(db *sql.DB, projectID int, name string, version int) (*model.Schema, error) {
	projectID = projectOrDefault(projectID)
	if version > 0 {
		return scanSchema(db.QueryRow(
			schemaSelect+` WHERE `+schemaProjectPredicate+` AND name = ? AND version = ?`,
			projectID, name, version))
	}
	return scanSchema(db.QueryRow(
		schemaSelect+` WHERE `+schemaProjectPredicate+` AND name = ? AND deprecated_at_ms IS NULL
		 ORDER BY version DESC LIMIT 1`,
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
	// ExcludeDeprecated drops retired versions (`deprecated_at_ms` set) from
	// both the rows and the pre-limit total. Zero value is false so every
	// existing caller — the registry audit, which asks what the registry
	// HOLDS — keeps seeing every version; only `schema list`'s default opts
	// this in.
	ExcludeDeprecated bool
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
	if opts.ExcludeDeprecated {
		clauses = append(clauses, `deprecated_at_ms IS NULL`)
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
       created_at_ms, row_version, deprecated_at_ms
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
		s            model.Schema
		sourcePath   sql.NullString
		builtin      int
		deprecatedAt sql.NullInt64
	)
	err := row.Scan(
		&s.ID, &s.ProjectID, &s.Name, &s.Version, &sourcePath, &s.SourceSHA256, &s.Body,
		&s.Ordered, &builtin, &s.CreatedAtMS, &s.RowVersion, &deprecatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("reading schema: %w", err)
	}
	s.SourcePath = sourcePath.String
	s.Builtin = builtin != 0
	// NULL means in service; 0 is the same fact in the model.
	s.DeprecatedAtMS = deprecatedAt.Int64
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

// Schema retirement sentinels (v36). They mirror the workflow ones, because the
// two registries share the retirement contract and a caller mapping errors to
// exit codes should not have to learn it twice.
var (
	// ErrSchemaAlreadyDeprecated means the version is already retired.
	// Surfaced as CONFLICT (exit 4), for ErrWorkflowAlreadyDeprecated's
	// reason: the second caller's "I am the one taking this out of service"
	// is wrong, and the timestamp they would expect to see is not the one
	// stored.
	ErrSchemaAlreadyDeprecated = errors.New("schema version is already deprecated")

	// ErrSchemaBuiltin means the version ships in the binary. It is refused
	// rather than retired because a builtin row is visible to EVERY project
	// through its flag, so retiring it in one project's registry would retire
	// it for the whole store — and nothing an operator can register replaces
	// what the binary seeds.
	ErrSchemaBuiltin = errors.New("schema is builtin and cannot be deprecated")
)

// ownSchemaTx reads one schema row that THIS project registered, builtins
// excluded. The retirement verbs use it in place of getSchemaTx because the
// visibility predicate admits the builtin row from whichever project seeded
// it, and a per-project retirement must never reach a row another project
// owns.
func ownSchemaTx(tx *sql.Tx, projectID int, name string, version int) (*model.Schema, error) {
	s, err := scanSchemaRows(tx.QueryRow(
		schemaSelect+` WHERE project_id = ? AND name = ? AND version = ?`,
		projectID, name, version))
	if errors.Is(err, sql.ErrNoRows) {
		// The builtin is the one row the visibility predicate would have
		// found here; name it rather than reporting it absent, so the operator
		// learns WHY it cannot be retired instead of that it does not exist.
		if b, berr := getSchemaTx(tx, projectID, name, version); berr == nil && b.Builtin {
			return nil, ErrSchemaBuiltin
		}
		return nil, ErrSchemaNotFound
	}
	if err != nil {
		return nil, err
	}
	// The builtin sits on whichever project row seeded it, so the project that
	// owns it by column is refused the same way as every other project.
	if s.Builtin {
		return nil, ErrSchemaBuiltin
	}
	return s, nil
}

// DeprecateSchema retires ONE registered schema version from service.
//
// It writes a timestamp and NOTHING ELSE, DeprecateWorkflow's contract applied
// to the other registry: the row, its bytes, its ordered index, and its hash
// are untouched, so `schema show name@n` still renders it, a run that pinned
// it keeps validating payloads against it, and the lineage stays legible. The
// only thing that changes is that a NEW `payload` reference to it is refused
// at registration.
//
// There is deliberately no delete verb, for the workflow registry's reason.
func DeprecateSchema(db *sql.DB, projectID int, name string, version int, nowMS int64) (*model.Schema, error) {
	projectID = projectOrDefault(projectID)
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	s, err := ownSchemaTx(tx, projectID, name, version)
	if err != nil {
		return nil, err
	}
	if s.Deprecated() {
		return nil, ErrSchemaAlreadyDeprecated
	}

	updated, err := setSchemaDeprecatedTx(tx, projectID, name, version, nowMS, "deprecating")
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}
	return updated, nil
}

// RestoreSchema clears a version's retirement, returning it to service. It is
// idempotent, as RestoreWorkflow is: a version that was never retired comes
// back unchanged.
func RestoreSchema(db *sql.DB, projectID int, name string, version int) (*model.Schema, error) {
	projectID = projectOrDefault(projectID)
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	s, err := ownSchemaTx(tx, projectID, name, version)
	if err != nil {
		return nil, err
	}
	if !s.Deprecated() {
		return s, nil // already in service; idempotent
	}

	updated, err := setSchemaDeprecatedTx(tx, projectID, name, version, nil, "restoring")
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}
	return updated, nil
}

// setSchemaDeprecatedTx is the UPDATE-and-re-read skeleton DeprecateSchema and
// RestoreSchema share, setWorkflowDeprecatedTx's shape: only the value written
// (a timestamp or NULL) and the verb in a wrapped error differ.
func setSchemaDeprecatedTx(
	tx *sql.Tx, projectID int, name string, version int, deprecatedAtMS any, verb string,
) (*model.Schema, error) {
	if _, err := tx.Exec(
		`UPDATE schemas SET deprecated_at_ms = ?, row_version = row_version + 1
		  WHERE project_id = ? AND name = ? AND version = ?`,
		deprecatedAtMS, projectID, name, version,
	); err != nil {
		return nil, fmt.Errorf("%s schema %s@%d: %w", verb, name, version, err)
	}
	return ownSchemaTx(tx, projectID, name, version)
}
