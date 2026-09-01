package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Run sentinels.
var (
	// ErrRunNotFound means no `runs` row matches. NOT_FOUND (exit 2).
	ErrRunNotFound = errors.New("run not found")
)

// InsertRun creates a run in `planning` — `docket run start` (TDD §5.2).
//
// `budget` is STORED and enforces nothing until S6. Accepting it now means the
// S6 upgrade adds enforcement rather than a flag: a flag appearing later would
// break the `run start` invocation an S3-era harness scripted.
func InsertRun(db *sql.DB, projectID int, request string, budget float64, nowMS int64) (*model.Run, error) {
	return InsertRunWithContext(db, projectID, request, budget, nowMS, RunContext{})
}

// RunContext is the execution context stamped on a run at creation (G8):
// which checkout, on which branch and commit, on which machine. Empty fields
// record as empty — a run started outside a checkout has no branch, and
// inventing one would make the record an opinion.
type RunContext struct {
	ExecRoot  string
	Branch    string
	CommitSHA string
	Hostname  string
	// UsageBudget is the cap over MEASURED usage (DKT-238), resolved at
	// `run start` from `--usage-budget` or `budget.usage.default` and pinned
	// on the row for the same reason `budget` is: a config change must not
	// re-cap a live run.
	UsageBudget float64
}

// InsertRunWithContext is InsertRun carrying the invocation's context.
func InsertRunWithContext(db *sql.DB, projectID int, request string, budget float64, nowMS int64, ctx RunContext) (*model.Run, error) {
	return InsertRunWithContextIdempotent(db, projectID, request, budget, nowMS, ctx, "")
}

// InsertRunWithContextIdempotent is InsertRunWithContext with an optional
// idempotency key — `docket run start --idempotency-key`.
//
// A repeat call with the same key returns the ORIGINAL run unchanged and
// creates nothing, the same create-verb replay-protection pattern
// CreateIssueIdempotent, CreateDocIdempotent, and CreateProposalIdempotent use
// (internal/db/idempotency.go): a `run start` that commits but dies before its
// response must be safe to retry, or the key protects nothing. The key record
// and the insert commit in the SAME transaction, so a crash between them
// cannot orphan either.
func InsertRunWithContextIdempotent(db *sql.DB, projectID int, request string, budget float64, nowMS int64, ctx RunContext, idempotencyKey string) (*model.Run, error) {
	existingID, hit, tx, err := beginIdempotentCreate(db, ScopeRunStart, idempotencyKey)
	if err != nil {
		return nil, err
	}
	if hit {
		return GetRun(db, existingID)
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO runs (project_id, request, status, budget, usage_budget,
		                   created_at_ms, updated_at_ms, row_version,
		                   exec_root, branch, commit_sha, hostname)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
		projectOrDefault(projectID), request, model.RunPlanning, budget,
		ctx.UsageBudget, nowMS, nowMS,
		ctx.ExecRoot, ctx.Branch, ctx.CommitSHA, ctx.Hostname,
	)
	if err != nil {
		return nil, fmt.Errorf("creating run: %w", err)
	}
	id64, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("reading created run id: %w", err)
	}
	id := int(id64)

	if idempotencyKey != "" {
		if err := RecordIdempotencyKeyTx(tx, ScopeRunStart, idempotencyKey, id); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	return GetRun(db, id)
}

// runSelect names the columns in a fixed order so every scan agrees.
const runSelect = `
SELECT id, project_id, request, status, reason, budget, usage_budget,
       activated_at_ms, created_at_ms,
       updated_at_ms, row_version, exec_root, branch, commit_sha, hostname
  FROM runs`

// GetRun reads one run.
func GetRun(db *sql.DB, id int) (*model.Run, error) {
	return scanRun(db.QueryRow(runSelect+` WHERE id = ?`, id))
}

// GetRunTx is GetRun inside a transaction — the fat transaction's own reader.
func GetRunTx(tx *sql.Tx, id int) (*model.Run, error) {
	return scanRun(tx.QueryRow(runSelect+` WHERE id = ?`, id))
}

func scanRun(row rowScanner) (*model.Run, error) {
	var (
		run      model.Run
		reason   sql.NullString
		actAtMS  sql.NullInt64
		rowError = func(err error) (*model.Run, error) {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrRunNotFound
			}
			return nil, fmt.Errorf("reading run: %w", err)
		}
	)

	err := row.Scan(
		&run.ID, &run.ProjectID, &run.Request, &run.Status, &reason, &run.Budget,
		&run.UsageBudget,
		&actAtMS, &run.CreatedAtMS, &run.UpdatedAtMS, &run.RowVersion,
		&run.ExecRoot, &run.Branch, &run.CommitSHA, &run.Hostname,
	)
	if err != nil {
		return rowError(err)
	}
	run.Reason = reason.String
	if actAtMS.Valid {
		v := actAtMS.Int64
		run.ActivatedAtMS = &v
	}
	return &run, nil
}

// RunListOptions filters `run status` without an id.
type RunListOptions struct {
	// ProjectID scopes the list to one project (v12); 0 = every project.
	ProjectID int
	// ActiveOnly restricts the list to runs that are not terminal — the
	// `--active` flag. `planning` counts as active: a run that exists but has
	// not been activated is still live work an operator is mid-way through.
	ActiveOnly bool
	Limit      int
}

// ListRuns returns runs newest first, and the TRUE total before the limit —
// the Collection contract (reliability-delta §4.1) needs a total a limit
// cannot distort, so truncation is computable rather than guessed.
func ListRuns(db *sql.DB, opts RunListOptions) ([]*model.Run, int, error) {
	var clauses []string
	var args []any
	if opts.ProjectID != 0 {
		clauses = append(clauses, `project_id = ?`)
		args = append(args, opts.ProjectID)
	}
	if opts.ActiveOnly {
		clauses = append(clauses, fmt.Sprintf(`status NOT IN ('%s', '%s')`,
			model.RunDone, model.RunAbandoned))
	}
	where := ``
	if len(clauses) > 0 {
		where = ` WHERE ` + strings.Join(clauses, ` AND `)
	}

	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting runs: %w", err)
	}

	query := runSelect + where + ` ORDER BY id DESC`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing runs: %w", err)
	}
	runs, err := scanRows(rows, "runs", func(r *sql.Rows) (*model.Run, error) { return scanRun(r) })
	if err != nil {
		return nil, 0, err
	}
	return runs, total, nil
}

// SetRunStatus moves a run's status and stamps `updated_at_ms`, bumping the CAS
// column. `reason` records why a run is parked or abandoned (engine-core §1.1);
// passing "" leaves the existing reason alone rather than clearing it, so a
// resume does not erase why the run was paused.
func SetRunStatus(db *sql.DB, id int, status model.RunStatus, reason string, nowMS int64) error {
	return setRunStatus(db, id, status, reason, nowMS)
}

// SetRunStatusTx is SetRunStatus inside a caller's transaction, for verbs that
// must write the transition and its event atomically (DKT-86): a status write
// that commits while its event does not is a transition the ledger never saw.
func SetRunStatusTx(tx *sql.Tx, id int, status model.RunStatus, reason string, nowMS int64) error {
	return setRunStatus(tx, id, status, reason, nowMS)
}

func setRunStatus(e execer, id int, status model.RunStatus, reason string, nowMS int64) error {
	query := `UPDATE runs SET status = ?, updated_at_ms = ?, row_version = row_version + 1`
	args := []any{string(status), nowMS}
	if reason != "" {
		query += `, reason = ?`
		args = append(args, reason)
	}
	query += ` WHERE id = ?`
	args = append(args, id)

	res, err := e.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("updating run status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("updating run status: %w", err)
	}
	if n == 0 {
		return ErrRunNotFound
	}
	return nil
}

// SetRunPauseOriginTx records WHERE a run's park was decided, and clears the
// record — `model.RunPauseOriginNone` — when the park ends (DKT-305).
//
// It is written beside a status transition, never on its own: `origin` is only
// meaningful for a run that IS `waiting-human`, and a stale origin on a running
// run would make the rollup decline to resume a run nobody parked. Every caller
// therefore pairs a set with the pause it describes and a clear with the move
// that ends it, in the same transaction.
//
// It does NOT bump `row_version`, for the same reason `CacheRunFloorTx` does
// not: the caller's own status write in this transaction already bumped it, and
// a second bump for one fact would make `--if-version` collide with itself.
func SetRunPauseOriginTx(tx *sql.Tx, id int, origin model.RunPauseOrigin) error {
	if _, err := tx.Exec(
		`UPDATE runs SET pause_origin = ? WHERE id = ?`, string(origin), id,
	); err != nil {
		return fmt.Errorf("recording the pause origin for %s: %w",
			model.FormatRunID(id), err)
	}
	return nil
}

// RunPauseOriginTx reads where a run's park was decided. A run that is not
// parked, and one parked by its own steps, both read
// `model.RunPauseOriginNone`.
func RunPauseOriginTx(tx *sql.Tx, id int) (model.RunPauseOrigin, error) {
	var origin sql.NullString
	err := tx.QueryRow(`SELECT pause_origin FROM runs WHERE id = ?`, id).Scan(&origin)
	if errors.Is(err, sql.ErrNoRows) {
		return model.RunPauseOriginNone, ErrRunNotFound
	}
	if err != nil {
		return model.RunPauseOriginNone, fmt.Errorf("reading the pause origin for %s: %w",
			model.FormatRunID(id), err)
	}
	return model.RunPauseOrigin(origin.String), nil
}

// SetRunBudgetTx writes a run's cap — `docket run budget --set`
// (docs/tdd/events-follow.md §7.2).
//
// IT ALWAYS BUMPS `row_version`, whether or not a precondition was given (B-7).
// Before this verb existed, operations.md §4's runbook told an operator to raise
// a cap with `sqlite3` and reminded them to increment the column by hand — "or a
// concurrent `--if-version` check will pass against a row that changed
// underneath it". Making that structural rather than a step somebody remembers
// is most of why the verb is better than the edit.
//
// `ifVersion` is the optimistic-concurrency precondition (B-6): non-nil means
// the UPDATE also requires that version, and a mismatch affects zero rows and
// becomes ErrVersionConflict. That is the same shape every other CAS verb uses,
// so `--if-version` behaves identically here and there.
//
// It runs in the CALLER'S transaction because the write and its event are one
// fact: a cap that moved with nothing in the log explaining why is exactly the
// gap operations.md §4 warned about when the manual edit was the only option.
func SetRunBudgetTx(tx *sql.Tx, runID int, budget float64, ifVersion *int, nowMS int64) error {
	query := `UPDATE runs SET budget = ?, updated_at_ms = ?,
	                 row_version = row_version + 1
	           WHERE id = ?`
	args := []any{budget, nowMS, runID}
	if ifVersion != nil {
		query += ` AND row_version = ?`
		args = append(args, *ifVersion)
	}

	res, err := tx.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("setting the budget for %s: %w", model.FormatRunID(runID), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("setting the budget for %s: %w", model.FormatRunID(runID), err)
	}
	if n == 0 {
		// Zero rows means one of two things, and they are DIFFERENT ANSWERS to
		// the caller: the run is gone (NOT_FOUND) or somebody else wrote it
		// (CONFLICT). Collapsing them would report a live race as a missing row.
		if ifVersion != nil {
			return ErrVersionConflict
		}
		return ErrRunNotFound
	}
	return nil
}

// AddRunIssue attaches an issue to a run before activation. The binding,
// snapshots, and expansion timestamp are all NULL until activation fills them —
// a run in `planning` carries its issue list and nothing else.
func AddRunIssue(db *sql.DB, runID, issueID int) error {
	_, err := db.Exec(
		`INSERT OR IGNORE INTO run_issues (run_id, issue_id) VALUES (?, ?)`,
		runID, issueID,
	)
	if err != nil {
		return fmt.Errorf("adding issue to run: %w", err)
	}
	return nil
}

// RemoveRunIssue detaches an issue from a run. The caller enforces WHEN this
// is legal (DKT-53: only while the run is in `planning` — after activation the
// issue is bound, snapshotted, and possibly scheduled, and a bare row delete
// would strand all three). Returns ErrNotFound when the issue was not
// attached, so a typo reads as a miss rather than a success.
func RemoveRunIssue(db *sql.DB, runID, issueID int) error {
	res, err := db.Exec(
		`DELETE FROM run_issues WHERE run_id = ? AND issue_id = ?`,
		runID, issueID,
	)
	if err != nil {
		return fmt.Errorf("removing issue from run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("removing issue from run: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RunIssue is one `run_issues` row: an issue's membership in a run, its
// binding, and the activation-time snapshots the context bundle reads.
type RunIssue struct {
	RunID         int
	IssueID       int
	WorkflowID    *int
	BodySnapshot  string
	BodySHA256    string
	IssueSnapshot string
	ExpandedAtMS  *int64
	// LoopCount is the ISSUE's loop counter (§11.3 (1), TDD §7.1). It lives here
	// rather than on `steps` because the spec says "the issue's loop counter":
	// one issue loops, and `max_fix_loops` bounds that one number.
	LoopCount int
}

// Expanded reports whether this issue's phase has been expanded. Re-activation
// expands ONLY issues for which this is false (RA1), which is what makes
// expansion idempotent and re-entrant.
func (ri *RunIssue) Expanded() bool { return ri.ExpandedAtMS != nil }

const runIssueSelect = `
SELECT run_id, issue_id, workflow_id, body_snapshot, body_sha256, issue_snapshot,
       expanded_at_ms, loop_count
  FROM run_issues`

// ListRunIssues returns a run's issues in ascending issue order, so every
// caller walks them the same way and two activations of the same inputs agree.
func ListRunIssues(db *sql.DB, runID int) ([]*RunIssue, error) {
	return scanRunIssues(db.Query(runIssueSelect+` WHERE run_id = ? ORDER BY issue_id`, runID))
}

// ListRunIssuesTx is ListRunIssues inside a transaction.
func ListRunIssuesTx(tx *sql.Tx, runID int) ([]*RunIssue, error) {
	return scanRunIssues(tx.Query(runIssueSelect+` WHERE run_id = ? ORDER BY issue_id`, runID))
}

func scanRunIssues(rows *sql.Rows, err error) ([]*RunIssue, error) {
	if err != nil {
		return nil, fmt.Errorf("listing run issues: %w", err)
	}
	return scanRows(rows, "run issues", func(r *sql.Rows) (*RunIssue, error) {
		var (
			ri         RunIssue
			workflowID sql.NullInt64
			body       sql.NullString
			sha        sql.NullString
			snapshot   sql.NullString
			expanded   sql.NullInt64
		)
		if err := r.Scan(
			&ri.RunID, &ri.IssueID, &workflowID, &body, &sha, &snapshot, &expanded,
			&ri.LoopCount,
		); err != nil {
			return nil, fmt.Errorf("reading run issue: %w", err)
		}
		if workflowID.Valid {
			v := int(workflowID.Int64)
			ri.WorkflowID = &v
		}
		if expanded.Valid {
			v := expanded.Int64
			ri.ExpandedAtMS = &v
		}
		ri.BodySnapshot, ri.BodySHA256, ri.IssueSnapshot = body.String, sha.String, snapshot.String
		return &ri, nil
	})
}

// BindRunIssueTx records an issue's binding and its activation-time snapshots —
// stages 1 and 4 of the fat transaction, written together because they are one
// fact: this issue, bound to this workflow, as it read at this moment.
func BindRunIssueTx(tx *sql.Tx, ri *RunIssue) error {
	_, err := tx.Exec(
		`UPDATE run_issues
		    SET workflow_id = ?, body_snapshot = ?, body_sha256 = ?, issue_snapshot = ?
		  WHERE run_id = ? AND issue_id = ?`,
		ri.WorkflowID, ri.BodySnapshot, ri.BodySHA256, ri.IssueSnapshot,
		ri.RunID, ri.IssueID,
	)
	if err != nil {
		return fmt.Errorf("binding issue %d: %w", ri.IssueID, err)
	}
	return nil
}

// SetRunIssueSnapshotTx rewrites ONE run-issue's `issue_snapshot` blob and
// nothing else (DKT-869).
//
// It is deliberately narrower than BindRunIssueTx, which writes the binding and
// all three snapshot columns together because activation produces them as one
// fact. The scope refresh is not that fact: it re-reads exactly one field of an
// already-frozen snapshot and must leave `workflow_id`, `body_snapshot`, and
// `body_sha256` untouched — a refresh that re-bound the workflow, or rewrote
// the description snapshot from the live issue, would smuggle §9 item 5's
// mid-run edit immunity out through a verb that says it is about scope.
//
// A miss is a caller error rather than a no-op: the row was read moments ago
// inside this same transaction, so zero rows affected means the membership the
// decision was made on no longer stands.
func SetRunIssueSnapshotTx(tx *sql.Tx, runID, issueID int, snapshot string) error {
	res, err := tx.Exec(
		`UPDATE run_issues SET issue_snapshot = ? WHERE run_id = ? AND issue_id = ?`,
		snapshot, runID, issueID,
	)
	if err != nil {
		return fmt.Errorf("rewriting issue %d's snapshot: %w", issueID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("issue %d is no longer part of run %d", issueID, runID)
	}
	return nil
}

// MarkExpandedTx stamps an issue as expanded — stage 6's record that this
// issue's phase is done, so re-activation (RA1) skips it.
func MarkExpandedTx(tx *sql.Tx, runID, issueID int, nowMS int64) error {
	_, err := tx.Exec(
		`UPDATE run_issues SET expanded_at_ms = ? WHERE run_id = ? AND issue_id = ?`,
		nowMS, runID, issueID,
	)
	if err != nil {
		return fmt.Errorf("marking issue %d expanded: %w", issueID, err)
	}
	return nil
}

// GetRunIssueTx reads one `run_issues` row — the issue's binding, snapshots,
// and loop counter — inside a transaction.
//
// The loop routing needs the counter under the SAME lock that increments it, so
// this reads inside the caller's transaction rather than through a *sql.DB
// helper. (It also must: internal/db caps the pool at one connection, so a pool
// read from inside a transaction deadlocks — the failure
// TestNoPoolReadsInsideTransactions exists to prevent.)
func GetRunIssueTx(tx *sql.Tx, runID, issueID int) (*RunIssue, error) {
	issues, err := scanRunIssues(tx.Query(
		runIssueSelect+` WHERE run_id = ? AND issue_id = ?`, runID, issueID))
	if err != nil {
		return nil, err
	}
	if len(issues) == 0 {
		return nil, fmt.Errorf("issue %d is not in run %d", issueID, runID)
	}
	return issues[0], nil
}

// IncrementLoopCountTx raises the issue's loop counter by one and returns the
// NEW value (§11.3 (1)).
//
// The read-back is in the same statement's transaction rather than a separate
// SELECT so the value returned is the one this UPDATE wrote. Two concurrent
// routings incrementing the same issue would otherwise both read the same
// "new" count and both believe they were loop k+1 — and `max_fix_loops` would
// bound nothing.
func IncrementLoopCountTx(tx *sql.Tx, runID, issueID int) (int, error) {
	_, err := tx.Exec(
		`UPDATE run_issues SET loop_count = loop_count + 1
		  WHERE run_id = ? AND issue_id = ?`,
		runID, issueID,
	)
	if err != nil {
		return 0, fmt.Errorf("incrementing the loop counter for issue %d: %w", issueID, err)
	}

	var count int
	err = tx.QueryRow(
		`SELECT loop_count FROM run_issues WHERE run_id = ? AND issue_id = ?`,
		runID, issueID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("reading the loop counter for issue %d: %w", issueID, err)
	}
	return count, nil
}

// LoopGrantsTx reads how many ADDITIONAL fix loops an operator has authorized
// for one issue in one run (DKT-237). Zero on every issue nobody has reopened.
func LoopGrantsTx(tx *sql.Tx, runID, issueID int) (int, error) {
	var grants int
	err := tx.QueryRow(
		`SELECT loop_grants FROM run_issues WHERE run_id = ? AND issue_id = ?`,
		runID, issueID,
	).Scan(&grants)
	if err != nil {
		return 0, fmt.Errorf("reading the loop grants for issue %d: %w", issueID, err)
	}
	return grants, nil
}

// GrantLoopTx authorizes ONE more fix loop for an issue and returns the new
// total.
//
// It RAISES A GRANT rather than editing `max_fix_loops`, because the two say
// different things: the workflow's bound is the author's standing policy over
// every issue it matches, while this is one operator's decision about one
// issue on one occasion. Editing the bound to unstick a single issue would
// quietly loosen it for every issue after.
func GrantLoopTx(tx *sql.Tx, runID, issueID int) (int, error) {
	res, err := tx.Exec(
		`UPDATE run_issues SET loop_grants = loop_grants + 1
		  WHERE run_id = ? AND issue_id = ?`,
		runID, issueID,
	)
	if err != nil {
		return 0, fmt.Errorf("granting a fix loop for issue %d: %w", issueID, err)
	}
	// A miss means the issue is not in the run at all, which is a caller error
	// rather than a no-op: silently granting nothing would leave the resolve
	// that asked for it reporting success over a bound that never moved.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return 0, fmt.Errorf(
			"issue %d is not part of %s", issueID, model.FormatRunID(runID))
	}
	return LoopGrantsTx(tx, runID, issueID)
}

// Pin kinds (TDD §5.1). `workflow` pins a registered `name@version` by its
// source hash; `file` pins an arbitrary operator-supplied path by its content
// hash — "how the reference instance pins its contracts, fragments, and policy
// without core knowing what they are" (engine-spec §2). Core reads bytes,
// hashes them, stores the path, and never opens the content again.
const (
	PinKindWorkflow = "workflow"
	PinKindFile     = "file"
	// PinKindSchema pins a registered payload schema by its source hash
	// (docs/tdd/payloads-thresholds.md §4.7 P1). A schema is a registered
	// object, and §2's pinning clause is "registered objects by version" —
	// therefore it pins. What a run validates its payloads against is a fact
	// about the run, not about the table's current contents.
	PinKindSchema = "schema"
)

// Pin is one `pins` row.
type Pin struct {
	RunID  int
	Kind   string
	Ref    string
	SHA256 string
	// Bytes is the pinned content's size, carried IN MEMORY ONLY during
	// activation — it is not a column and is not persisted.
	//
	// It exists so the closure-size arithmetic (§1.5) can count a
	// declared packet file without opening it a second time: the activation
	// scan already read the bytes to hash them, and re-reading at expansion
	// would both cost an extra pass and open a window where the two reads
	// disagree.
	Bytes int
}

// InsertPinTx records a pin. `INSERT OR IGNORE` on the UNIQUE(run_id, kind, ref)
// key makes pinning the same workflow for two issues in one run a single row
// rather than a conflict — a run pins a workflow once, however many issues bind
// to it.
func InsertPinTx(tx *sql.Tx, p Pin) error {
	_, err := tx.Exec(
		`INSERT OR IGNORE INTO pins (run_id, kind, ref, sha256) VALUES (?, ?, ?, ?)`,
		p.RunID, p.Kind, p.Ref, p.SHA256,
	)
	if err != nil {
		return fmt.Errorf("pinning %s %s: %w", p.Kind, p.Ref, err)
	}
	return nil
}

// ListPins returns a run's pins, ordered so the set is stable across reads —
// `context.pins` is golden-diffed byte for byte (§8.3), so an unordered read
// would make the goldens flap.
func ListPins(db *sql.DB, runID int) ([]Pin, error) {
	rows, err := db.Query(
		`SELECT run_id, kind, ref, sha256 FROM pins WHERE run_id = ? ORDER BY kind, ref`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing pins: %w", err)
	}
	return scanRows(rows, "pins", scanPin)
}

// ListPinsTx is ListPins inside a transaction — RA2's reader. Re-activation
// INHERITS the pin set rather than recomputing it, so an in-flight run cannot
// silently adopt an edited workflow (engine-core §4).
func ListPinsTx(tx *sql.Tx, runID int) ([]Pin, error) {
	rows, err := tx.Query(
		`SELECT run_id, kind, ref, sha256 FROM pins WHERE run_id = ? ORDER BY kind, ref`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing pins: %w", err)
	}
	return scanRows(rows, "pins", scanPin)
}

// scanPin reads one pins row. Shared by ListPins and ListPinsTx (*sql.DB and
// *sql.Tx read the same table through the same shape).
func scanPin(r *sql.Rows) (Pin, error) {
	var p Pin
	if err := r.Scan(&p.RunID, &p.Kind, &p.Ref, &p.SHA256); err != nil {
		return Pin{}, fmt.Errorf("reading pin: %w", err)
	}
	return p, nil
}

// RunFence is one harvested fenced command (TDD §5.1).
type RunFence struct {
	RunID   int
	IssueID int
	Tag     string
	Ordinal int
	Command string
	SHA256  string
}

// InsertFenceTx records one harvested command. Harvesting happens at
// ACTIVATION so "post-activation edits cannot inject" (engine-spec §4) — the
// hash is of the command as it read when the operator approved the plan.
func InsertFenceTx(tx *sql.Tx, f RunFence) error {
	_, err := tx.Exec(
		`INSERT OR REPLACE INTO run_fences (run_id, issue_id, tag, ordinal, command, sha256)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		f.RunID, f.IssueID, f.Tag, f.Ordinal, f.Command, f.SHA256,
	)
	if err != nil {
		return fmt.Errorf("recording fence %s#%d: %w", f.Tag, f.Ordinal, err)
	}
	return nil
}

// ListFences returns a run's harvested commands in a stable order.
func ListFences(db *sql.DB, runID int) ([]RunFence, error) {
	rows, err := db.Query(
		`SELECT run_id, issue_id, tag, ordinal, command, sha256
		   FROM run_fences WHERE run_id = ? ORDER BY issue_id, tag, ordinal`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing fences: %w", err)
	}
	return scanRows(rows, "fences", func(r *sql.Rows) (RunFence, error) {
		var f RunFence
		if err := r.Scan(
			&f.RunID, &f.IssueID, &f.Tag, &f.Ordinal, &f.Command, &f.SHA256,
		); err != nil {
			return RunFence{}, fmt.Errorf("reading fence: %w", err)
		}
		return f, nil
	})
}

// StepRow is one `steps` row as activation writes it. Phase 3 adds the lease,
// saga, and routing readers; expansion fills only the identity, the shape, and
// the initial status.
type StepRow struct {
	ID           int
	RunID        int
	IssueID      int
	WorkflowID   int
	StepName     string
	Ordinal      int
	SiblingIndex *int
	Instance     string
	Kind         string
	Executor     string
	Class        string
	Status       string
	MaxAttempts  *int
	ExpectedCost float64
	Metadata     string
	ContextBytes int
	// Materialized marks a step the engine minted rather than one expansion
	// read out of the pinned definition (payloads-thresholds §7.7 H4). Ordinary
	// expansion never sets it, so every row activation writes reads 0.
	Materialized bool
	CreatedAtMS  int64
}

// InsertStepTx writes one expanded step. The UNIQUE(run_id, issue_id, instance)
// index is the loop/fanout correctness guard — two rows claiming the same
// identity is the bug §11.3 exists to prevent — so a duplicate is an error
// here rather than a silently-ignored insert.
func InsertStepTx(tx *sql.Tx, s StepRow, nowMS int64) error {
	_, err := tx.Exec(
		`INSERT INTO steps
		   (run_id, issue_id, workflow_id, step_name, ordinal, sibling_index, instance,
		    kind, executor, class, status, attempt, max_attempts, expected_cost,
		    metadata, context_bytes, materialized, created_at_ms, updated_at_ms,
		    row_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, 1)`,
		s.RunID, s.IssueID, s.WorkflowID, s.StepName, s.Ordinal, s.SiblingIndex,
		s.Instance, s.Kind, nullable(s.Executor), nullable(s.Class), s.Status,
		s.MaxAttempts, s.ExpectedCost, nullable(s.Metadata), s.ContextBytes,
		boolToInt(s.Materialized), nowMS, nowMS,
	)
	if err != nil {
		return fmt.Errorf("creating step %s: %w", s.Instance, err)
	}
	return nil
}

const stepSelect = `
SELECT id, run_id, issue_id, workflow_id, step_name, ordinal, sibling_index, instance,
       kind, executor, class, status, max_attempts, expected_cost, metadata,
       context_bytes, created_at_ms
  FROM steps`

// ListSteps returns a run's steps ordered by (issue, id) — creation order
// within an issue, which is declaration order, which is what the topology
// goldens compare against.
func ListSteps(db *sql.DB, runID int) ([]*StepRow, error) {
	rows, err := db.Query(stepSelect+` WHERE run_id = ? ORDER BY issue_id, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("listing steps: %w", err)
	}
	return scanRows(rows, "steps", func(r *sql.Rows) (*StepRow, error) {
		var (
			s        StepRow
			sibling  sql.NullInt64
			executor sql.NullString
			class    sql.NullString
			maxAtt   sql.NullInt64
			metadata sql.NullString
			ctxBytes sql.NullInt64
		)
		if err := r.Scan(
			&s.ID, &s.RunID, &s.IssueID, &s.WorkflowID, &s.StepName, &s.Ordinal,
			&sibling, &s.Instance, &s.Kind, &executor, &class, &s.Status,
			&maxAtt, &s.ExpectedCost, &metadata, &ctxBytes, &s.CreatedAtMS,
		); err != nil {
			return nil, fmt.Errorf("reading step: %w", err)
		}
		if sibling.Valid {
			v := int(sibling.Int64)
			s.SiblingIndex = &v
		}
		if maxAtt.Valid {
			v := int(maxAtt.Int64)
			s.MaxAttempts = &v
		}
		s.Executor, s.Class, s.Metadata = executor.String, class.String, metadata.String
		s.ContextBytes = int(ctxBytes.Int64)
		return &s, nil
	})
}

// CountSteps returns a run's step count, for `run status` without paging the
// whole table.
func CountSteps(db *sql.DB, runID int) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM steps WHERE run_id = ?`, runID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting steps: %w", err)
	}
	return n, nil
}

// StepStatusCounts returns a run's steps grouped by status, sorted by status so
// the rendering is stable.
func StepStatusCounts(db *sql.DB, runID int) ([]model.StatusCount, error) {
	rows, err := db.Query(
		`SELECT status, COUNT(*) FROM steps WHERE run_id = ? GROUP BY status`, runID)
	if err != nil {
		return nil, fmt.Errorf("counting step statuses: %w", err)
	}
	out, err := scanRows(rows, "step statuses", func(r *sql.Rows) (model.StatusCount, error) {
		var sc model.StatusCount
		if err := r.Scan(&sc.Status, &sc.Count); err != nil {
			return model.StatusCount{}, fmt.Errorf("reading step status count: %w", err)
		}
		return sc, nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Status < out[j].Status })
	return out, nil
}

// SetIssueScopeGlobs stores an issue's declared scope as a JSON array, or NULL
// when the list is empty. NULL and `[]` are different facts — "no scope
// declared" versus "declared to touch nothing" — and only NULL is the dormant
// default every pre-existing row carries.
func SetIssueScopeGlobs(db *sql.DB, issueID int, globsJSON string) error {
	var value any
	if globsJSON != "" {
		value = globsJSON
	}
	_, err := db.Exec(`UPDATE issues SET scope_globs = ? WHERE id = ?`, value, issueID)
	if err != nil {
		return fmt.Errorf("setting scope on issue %d: %w", issueID, err)
	}
	return nil
}

// IssueScopeGlobs reads an issue's declared scope as stored JSON, or "" when
// none is declared.
func IssueScopeGlobs(db *sql.DB, issueID int) (string, error) {
	var globs sql.NullString
	err := db.QueryRow(`SELECT scope_globs FROM issues WHERE id = ?`, issueID).Scan(&globs)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading scope for issue %d: %w", issueID, err)
	}
	return globs.String, nil
}

// IssueScopeGlobsTx is IssueScopeGlobs inside a transaction — the snapshot's
// reader at activation stage 4.
func IssueScopeGlobsTx(tx *sql.Tx, issueID int) (string, error) {
	var globs sql.NullString
	err := tx.QueryRow(`SELECT scope_globs FROM issues WHERE id = ?`, issueID).Scan(&globs)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading scope for issue %d: %w", issueID, err)
	}
	return globs.String, nil
}
