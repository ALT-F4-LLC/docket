package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Workflow registration sentinels.
var (
	// ErrWorkflowConflict means `name@version` is already registered with
	// DIFFERENT bytes. Surfaced as CONFLICT (exit 4), naming both hashes.
	//
	// A registered name@version is frozen: re-registering identical bytes is
	// an idempotent success, and re-registering different bytes is this. The
	// version pinning engine-core §4 requires ("editing a pipeline never
	// changes an in-flight run") is worth nothing if the pinned bytes can be
	// swapped underneath a run.
	ErrWorkflowConflict = errors.New("workflow already registered with different content")

	// ErrWorkflowNotFound means no row matches the requested name/version.
	// Surfaced as NOT_FOUND (exit 2).
	ErrWorkflowNotFound = errors.New("workflow not found")
)

// InsertWorkflow registers a definition, or returns the existing row when the
// same bytes are already registered at that `name@version`.
//
// The three outcomes, per §4.1:
//
//   - no row at name@version              -> insert, created = true
//   - a row with the SAME source_sha256   -> return it, created = false
//   - a row with a DIFFERENT source_sha256 -> ErrWorkflowConflict
//
// Idempotency is decided on the CONTENT HASH rather than on the parsed form:
// two files that parse identically but differ in comments are different
// registered bytes, and `source_sha256` is what pins and audits refer to.
func InsertWorkflow(db *sql.DB, wf *model.Workflow, nowMS int64) (stored *model.Workflow, created bool, err error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	inserted, created, err := InsertWorkflowTx(tx, wf, nowMS)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("committing transaction: %w", err)
	}
	return inserted, created, nil
}

// InsertWorkflowTx is InsertWorkflow inside a CALLER'S transaction.
//
// It exists for S6's auto-registration, which runs inside activation's FAT
// TRANSACTION (docs/tdd/runs-dispatch.md §9.2 F8): a registration failure must
// refuse the whole activation and write nothing, and a registration that
// committed on its own could not be rolled back with the binding that followed
// it. It is also the only correct shape against a one-connection pool — the
// self-committing version would deadlock if called from inside a transaction.
//
// The three outcomes and the immutability contract are §4.1's, unchanged: this
// is the SAME body InsertWorkflow ran, lifted so both callers share it rather
// than a second path drifting from the first (F7: "no `auto` variant with looser
// rules").
func InsertWorkflowTx(
	tx *sql.Tx, wf *model.Workflow, nowMS int64,
) (stored *model.Workflow, created bool, err error) {
	projectID := projectOrDefault(wf.ProjectID)
	existing, err := getWorkflowTx(tx, projectID, wf.Name, wf.Version)
	if err != nil && !errors.Is(err, ErrWorkflowNotFound) {
		return nil, false, err
	}
	if existing != nil {
		if existing.SourceSHA256 != wf.SourceSHA256 {
			return nil, false, fmt.Errorf(
				"%w: %s@%d is registered as %s, these bytes are %s",
				ErrWorkflowConflict, wf.Name, wf.Version,
				existing.SourceSHA256, wf.SourceSHA256)
		}
		// Identical bytes: an idempotent success returning the existing row.
		// Nothing is inserted and nothing is updated — re-registering must not
		// bump row_version, or `--if-version` would fail for a caller that
		// changed nothing.
		return existing, false, nil
	}

	res, err := tx.Exec(
		`INSERT INTO workflows
		   (project_id, name, version, description, source_path, source_sha256, body, parsed, created_at_ms, row_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		projectID, wf.Name, wf.Version, nullable(wf.Description), nullable(wf.SourcePath),
		wf.SourceSHA256, wf.Body, wf.Parsed, nowMS,
	)
	if err != nil {
		return nil, false, fmt.Errorf("inserting workflow: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, false, fmt.Errorf("reading inserted workflow id: %w", err)
	}

	inserted, err := getWorkflowTx(tx, projectID, wf.Name, wf.Version)
	if err != nil {
		return nil, false, err
	}
	inserted.ID = int(id)
	return inserted, true, nil
}

// GetWorkflow returns one registered workflow WITHIN ONE PROJECT (v12 — a
// name@version is a per-project registration). A version of 0 selects the
// HIGHEST NON-DEPRECATED registered version, which is what `workflow show
// NAME` without `@version` means — and, by design, the same version
// `run activate` binds (engine.bindableDefinitions retires deprecated rows
// first, then takes the highest of what remains; DKT-616). A name whose every
// version is deprecated therefore resolves to ErrWorkflowNotFound, mirroring
// binding removing the name from routing altogether. An explicit version
// still resolves a deprecated row: retired versions stay registered and
// reachable for the runs that pinned them.
func GetWorkflow(db *sql.DB, projectID int, name string, version int) (*model.Workflow, error) {
	projectID = projectOrDefault(projectID)
	if version > 0 {
		return scanWorkflow(db.QueryRow(
			workflowSelect+` WHERE project_id = ? AND name = ? AND version = ?`,
			projectID, name, version))
	}
	return scanWorkflow(db.QueryRow(
		workflowSelect+` WHERE project_id = ? AND name = ? AND deprecated_at_ms IS NULL
		 ORDER BY version DESC LIMIT 1`,
		projectID, name))
}

// WorkflowListOptions filters `workflow list`.
type WorkflowListOptions struct {
	// ProjectID scopes the list to one project (v12); 0 = every project.
	ProjectID int
	Name      string
	Limit     int
	// ExcludeDeprecated drops retired versions (`deprecated_at_ms` set) from
	// both the rows and the pre-limit total. Zero value is false so every
	// existing caller — binding readers that need the full lineage to judge
	// staleness — keeps seeing every version; only `workflow list`'s default
	// opts this in.
	ExcludeDeprecated bool
}

// ListWorkflows returns registered workflows, newest registration first, and
// the TRUE total before the limit — the Collection contract (reliability-delta
// §4.1) requires a total that a limit cannot distort, so truncation is
// computable rather than guessed.
func ListWorkflows(db *sql.DB, opts WorkflowListOptions) ([]*model.Workflow, int, error) {
	var clauses []string
	var args []any
	if opts.ProjectID != 0 {
		clauses = append(clauses, `project_id = ?`)
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
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflows`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting workflows: %w", err)
	}

	query := workflowSelect + where + ` ORDER BY name ASC, version DESC`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing workflows: %w", err)
	}
	workflows, err := scanRows(rows, "workflows", func(r *sql.Rows) (*model.Workflow, error) {
		return scanWorkflowRows(r)
	})
	if err != nil {
		return nil, 0, err
	}

	return workflows, total, nil
}

// WorkflowVersion is one registered version's IDENTITY, without its bytes
// (DKT-594).
//
// It exists because the staleness question — "how many versions has this name
// advanced since the run pinned it" — is answered by the version column alone,
// and the rows that answer it carry a `body` and a `parsed` each. A corpus with
// 41 commits in four days has names registered a dozen deep; loading every one
// of their bodies to subtract two integers would make a read verb's cost scale
// with the size of the definitions it is not reading.
type WorkflowVersion struct {
	Name    string
	Version int
	// Binds is false for a version RETIRED from binding (`deprecated_at_ms` set).
	//
	// The distinction is the same one bindableDefinitions makes: retirement is a
	// binding-time filter, so the version a fresh run would pin is the highest
	// one that still binds, and a staleness count computed over retired rows
	// would tell an operator to chase a version nothing can bind.
	Binds bool
}

// WorkflowVersionsFor returns every registered version of the NAMED workflows
// within one project, ordered by (name, version) so a caller's reduction is
// deterministic.
//
// An empty `names` returns no rows and makes no query: the caller has nothing
// to ask about, and an unfiltered scan of the whole registry is never what
// "these names" means.
func WorkflowVersionsFor(conn *sql.DB, projectID int, names []string) ([]WorkflowVersion, error) {
	if len(names) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(names))
	args := []any{projectOrDefault(projectID)}
	for i, n := range names {
		placeholders[i] = "?"
		args = append(args, n)
	}
	rows, err := conn.Query(
		`SELECT name, version, deprecated_at_ms FROM workflows
		  WHERE project_id = ? AND name IN (`+strings.Join(placeholders, ", ")+`)
		  ORDER BY name ASC, version ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing registered workflow versions: %w", err)
	}
	return scanRows(rows, "workflow versions", func(r *sql.Rows) (WorkflowVersion, error) {
		var (
			v            WorkflowVersion
			deprecatedAt sql.NullInt64
		)
		if err := r.Scan(&v.Name, &v.Version, &deprecatedAt); err != nil {
			return WorkflowVersion{}, fmt.Errorf("reading a workflow version: %w", err)
		}
		v.Binds = !deprecatedAt.Valid || deprecatedAt.Int64 == 0
		return v, nil
	})
}

// workflowSelect names the columns in a fixed order, so the two scan helpers
// cannot drift apart.
const workflowSelect = `
SELECT id, project_id, name, version, description, source_path, source_sha256, body, parsed,
       created_at_ms, row_version, deprecated_at_ms
  FROM workflows`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanWorkflow(row rowScanner) (*model.Workflow, error) {
	wf, err := scanWorkflowRows(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWorkflowNotFound
	}
	return wf, err
}

func scanWorkflowRows(row rowScanner) (*model.Workflow, error) {
	var (
		wf           model.Workflow
		description  sql.NullString
		sourcePath   sql.NullString
		deprecatedAt sql.NullInt64
	)
	err := row.Scan(
		&wf.ID, &wf.ProjectID, &wf.Name, &wf.Version, &description, &sourcePath,
		&wf.SourceSHA256, &wf.Body, &wf.Parsed, &wf.CreatedAtMS, &wf.RowVersion,
		&deprecatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("reading workflow: %w", err)
	}
	wf.Description = description.String
	wf.SourcePath = sourcePath.String
	// NULL means binding; 0 is the same fact in the model.
	wf.DeprecatedAtMS = deprecatedAt.Int64
	return &wf, nil
}

// getWorkflowTx is GetWorkflow inside a transaction, at an exact version.
func getWorkflowTx(tx *sql.Tx, projectID int, name string, version int) (*model.Workflow, error) {
	wf, err := scanWorkflowRows(tx.QueryRow(
		workflowSelect+` WHERE project_id = ? AND name = ? AND version = ?`,
		projectOrDefault(projectID), name, version))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWorkflowNotFound
	}
	return wf, err
}

// ErrWorkflowAlreadyDeprecated means the version is already retired. Surfaced
// as CONFLICT (exit 4): retiring twice is not a silent success, because the
// second caller's mental model ("I am the one taking this out of service") is
// wrong and the timestamp they would expect to see is not the one stored.
var ErrWorkflowAlreadyDeprecated = errors.New("workflow version is already deprecated")

// DeprecateWorkflow retires ONE registered version from binding.
//
// It writes a timestamp and NOTHING ELSE. The row, its body, its parsed form,
// and its hash are untouched, so:
//
//   - `workflow show name@n` still renders it, and `--source` still emits the
//     exact registered bytes;
//   - definitionByID still resolves it, so a run that pinned this version
//     before it was retired continues to completion — retirement is a
//     binding-time filter, not a retraction;
//   - the lineage stays legible: `workflow list` shows the version with its
//     retirement date rather than a gap where a version used to be.
//
// There is deliberately no delete verb. The operator was offered one and
// rejected it: old versions stay registered, and that is the point.
func DeprecateWorkflow(db *sql.DB, projectID int, name string, version int, nowMS int64) (*model.Workflow, error) {
	projectID = projectOrDefault(projectID)
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	wf, err := getWorkflowTx(tx, projectID, name, version)
	if err != nil {
		return nil, err
	}
	if wf.Deprecated() {
		return nil, ErrWorkflowAlreadyDeprecated
	}

	updated, err := setWorkflowDeprecatedTx(tx, projectID, name, version, nowMS, "deprecating")
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}
	return updated, nil
}

// RestoreWorkflow clears a version's retirement, returning it to binding.
//
// It exists because retirement is a routing decision and routing decisions get
// reversed. Without it the only way back would be to re-register the same name
// at a HIGHER version, which changes what runs pin for a change the operator
// did not intend to make.
func RestoreWorkflow(db *sql.DB, projectID int, name string, version int) (*model.Workflow, error) {
	projectID = projectOrDefault(projectID)
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	wf, err := getWorkflowTx(tx, projectID, name, version)
	if err != nil {
		return nil, err
	}
	if !wf.Deprecated() {
		return wf, nil // already binding; idempotent
	}

	updated, err := setWorkflowDeprecatedTx(tx, projectID, name, version, nil, "restoring")
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}
	return updated, nil
}

// setWorkflowDeprecatedTx is the UPDATE-and-re-read skeleton DeprecateWorkflow
// and RestoreWorkflow share: only the value written to deprecated_at_ms (a
// timestamp or NULL) and the verb naming the operation in a wrapped error
// differ. The state check that decides WHETHER to call it stays with each
// caller, because that check's meaning (already deprecated vs. already
// binding) is not shared.
func setWorkflowDeprecatedTx(
	tx *sql.Tx, projectID int, name string, version int, deprecatedAtMS any, verb string,
) (*model.Workflow, error) {
	if _, err := tx.Exec(
		`UPDATE workflows SET deprecated_at_ms = ?, row_version = row_version + 1
		  WHERE project_id = ? AND name = ? AND version = ?`,
		deprecatedAtMS, projectID, name, version,
	); err != nil {
		return nil, fmt.Errorf("%s %s@%d: %w", verb, name, version, err)
	}
	return getWorkflowTx(tx, projectID, name, version)
}

// nullable stores "" as SQL NULL, so an absent description reads back as NULL
// rather than as an empty string that a later NOT NULL check would accept.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
