package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// The projects dimension (v12).
//
// A store used to BE a project: one directory, one database, no question of
// which rows belong to whom. The shared store makes tenancy a row, and this
// file is the only place that decides which row an invocation belongs to.

// DefaultProjectID is the row every pre-v12 datum backfilled to, and the row
// an invocation with no resolvable identity falls back to.
const DefaultProjectID = 1

// UnregisteredProjectID is the id an invocation resolves to when its identity
// has NO project row and this invocation is not allowed to create one (DKT-58:
// a read verb, or an identity that is not a repository).
//
// It is negative so it references nothing: every scoped SELECT returns empty
// and every scoped INSERT fails its foreign key rather than silently landing in
// someone else's project. That is the honest answer to "show me this
// directory's issues" when the directory has no project — the pre-DKT-58 code
// answered it by MINTING one, and a read that creates permanent state is how
// the store filled with rows nobody asked for.
//
// It is deliberately NOT DefaultProjectID: falling back to project 1 would show
// a legacy store's issues under an unrelated repository and, worse, let a write
// from an unregistered directory land in that project's history.
const UnregisteredProjectID = -1

// projectOrDefault normalizes a model's zero ProjectID to the default row —
// what keeps every pre-v12 caller and fixture writing valid rows without
// naming a project.
func projectOrDefault(id int) int {
	if id == 0 {
		return DefaultProjectID
	}
	return id
}

// DefaultProjectIDOr is projectOrDefault for callers outside this package
// that render their own SQL and need the same zero-means-default rule.
func DefaultProjectIDOr(id int) int {
	return projectOrDefault(id)
}

// EnsureProject resolves identity to a project id, creating the row on first
// contact.
//
// The resolution ladder:
//  1. A row already bound to this identity wins.
//  2. The UNCLAIMED default row — id 1 with an empty identity, seeded by the
//     v12 migration — is claimed in place. A legacy store holds exactly one
//     project's history under project 1, and the first repository to open it
//     is that project; claiming rather than inserting is what keeps that
//     history attached to its repo.
//  3. Otherwise a new row is inserted.
//
// An EMPTY identity never claims and never inserts: it reads as "this
// invocation could not be resolved to a project" and falls back to the
// default row, which is the pre-v12 behavior exactly.
func EnsureProject(conn *sql.DB, identity, name string, nowMS int64) (int, error) {
	id, _, err := ensureProject(conn, identity, name, nowMS)
	return id, err
}

// EnsureProjectCreated is EnsureProject with the fact the caller needs in order
// to report a first contact: whether this call is what brought the row into
// being. The root hook writes a `project-registered` event on true (DKT-61).
func EnsureProjectCreated(conn *sql.DB, identity, name string, nowMS int64) (int, bool, error) {
	return ensureProject(conn, identity, name, nowMS)
}

// LookupProject resolves an identity to its project WITHOUT creating anything.
//
// It is the read half EnsureProject used to keep private, split out for DKT-58:
// every invocation must be able to ask "which project is this?", and only some
// of them may answer "a new one".
func LookupProject(conn *sql.DB, identity string) (int, bool, error) {
	if identity == "" {
		return 0, false, nil
	}
	var id int
	err := conn.QueryRow(
		`SELECT id FROM projects WHERE identity = ?`, identity).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("resolving project for %s: %w", identity, err)
	}
	return id, true, nil
}

func ensureProject(conn *sql.DB, identity, name string, nowMS int64) (int, bool, error) {
	if identity == "" {
		return DefaultProjectID, false, nil
	}

	if id, found, err := LookupProject(conn, identity); err != nil || found {
		return id, false, err
	}

	res, err := conn.Exec(
		`UPDATE projects SET identity = ?, name = ?, created_at_ms = ?
		  WHERE id = ? AND identity = ''`,
		identity, name, nowMS, DefaultProjectID)
	if err != nil {
		return 0, false, fmt.Errorf("claiming the default project for %s: %w", identity, err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return DefaultProjectID, true, nil
	}

	// The prefix is DERIVED and UNIQUE (DKT-60). It used to be the literal
	// 'DKT' for every row, so the second project to register displayed its
	// issues under the first project's prefix — and since the prefix is the
	// only project discriminator any listing carries, every id in the store
	// became ambiguous about who owned it.
	prefix, err := availablePrefix(conn, name)
	if err != nil {
		return 0, false, err
	}

	_, err = conn.Exec(
		`INSERT INTO projects (identity, name, prefix, created_at_ms)
		 VALUES (?, ?, ?, ?)`,
		identity, name, prefix, nowMS)
	if err != nil {
		// A concurrent invocation may have inserted or claimed between the
		// probes; the identity's UNIQUE constraint makes the re-read
		// authoritative either way. The loser reports created=false — the row
		// exists, but this call is not the first contact that made it.
		var raced int
		if raceErr := conn.QueryRow(
			`SELECT id FROM projects WHERE identity = ?`, identity).Scan(&raced); raceErr == nil {
			return raced, false, nil
		}
		return 0, false, fmt.Errorf("creating project for %s: %w", identity, err)
	}

	var id int
	err = conn.QueryRow(
		`SELECT id FROM projects WHERE identity = ?`, identity).Scan(&id)
	if err != nil {
		return 0, false, fmt.Errorf("re-reading project for %s: %w", identity, err)
	}
	return id, true, nil
}

// DerivePrefix proposes a display prefix from a project's name (DKT-60).
//
// A multi-word name becomes its INITIALS — `agentic-mcp-services` reads as
// `AMS` — because the alternative, the first three letters, collapses whole
// families of sibling repositories onto the same three characters. A
// single-word name takes its first three letters instead, since `D` alone
// carries nothing.
//
// The result is always a legal prefix per model.ValidateProjectPrefix (letters
// only, 1-8) or empty when the name yields no letters at all; uniqueness and
// the reserved names are availablePrefix's job, because both are facts about
// the store rather than about the name.
func DerivePrefix(name string) string {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".git")

	// Split on anything that is not a letter, so `docket.git`, `docket_v2` and
	// `agentic mcp services` all word-split the way a reader would say them.
	words := strings.FieldsFunc(name, func(r rune) bool {
		return r < 'a' || r > 'z'
	})
	if len(words) == 0 {
		return ""
	}

	var out string
	if len(words) == 1 {
		out = words[0]
		if len(out) > 3 {
			out = out[:3]
		}
	} else {
		for _, w := range words {
			out += w[:1]
			if len(out) == 8 {
				break
			}
		}
	}
	return strings.ToUpper(out)
}

// availablePrefix returns a prefix no project already holds.
//
// The derived candidate is tried first; a collision — with another project or
// with a reserved entity prefix like DOC — appends a discriminating LETTER
// (`DOCA`, `DOCB`, …), never a digit: model.ValidateProjectPrefix accepts
// letters only, so a numbered ladder would reject every rung it generated and
// loop forever. Failing the whole alphabet it lengthens the suffix, so
// registration can never be blocked by prefix exhaustion — a project with an
// awkward prefix is a cosmetic problem, a project that cannot be created is
// not.
func availablePrefix(conn *sql.DB, name string) (string, error) {
	taken := map[string]bool{}
	rows, err := conn.Query(`SELECT UPPER(prefix) FROM projects`)
	if err != nil {
		return "", fmt.Errorf("reading the projects' prefixes: %w", err)
	}
	held, err := scanRows(rows, "project prefixes", func(r *sql.Rows) (string, error) {
		var p string
		return p, r.Scan(&p)
	})
	if err != nil {
		return "", err
	}
	for _, p := range held {
		taken[p] = true
	}

	free := func(candidate string) bool {
		return candidate != "" && !taken[candidate] &&
			model.ValidateProjectPrefix(candidate) == nil
	}

	base := DerivePrefix(name)
	if free(base) {
		return base, nil
	}
	// `base` may be empty (a name with no letters) or reserved; either way the
	// numbered ladder needs a letter root to hang off, and DKT is the historical
	// default.
	root := base
	if root == "" || model.ValidateProjectPrefix(root) != nil {
		root = "DKT"
	}
	if len(root) > 7 {
		root = root[:7]
	}
	for suffix := "A"; len(root)+len(suffix) <= 8; suffix += "A" {
		for c := byte('A'); c <= 'Z'; c++ {
			candidate := root + suffix[:len(suffix)-1] + string(c)
			if free(candidate) {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf(
		"no display prefix is available for %q: every candidate under %s is "+
			"already held; free one with `docket project set-prefix`", name, root)
}

// ErrPrefixTaken refuses a prefix another project already holds (DKT-60). The
// prefix is a project's only discriminator in a listing or an event feed, so
// two projects sharing one makes every id in the store ambiguous about its
// owner.
var ErrPrefixTaken = errors.New("prefix already held by another project")

// PrefixHolder reports the id of the project holding prefix, other than
// `exclude`. It returns 0 when the prefix is free.
func PrefixHolder(conn *sql.DB, prefix string, exclude int) (int, error) {
	var id int
	err := conn.QueryRow(
		`SELECT id FROM projects WHERE UPPER(prefix) = UPPER(?) AND id != ?`,
		prefix, exclude).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("checking who holds the prefix %q: %w", prefix, err)
	}
	return id, nil
}

// GetProject reads one project row.
func GetProject(conn *sql.DB, id int) (*model.Project, error) {
	p := &model.Project{}
	err := conn.QueryRow(
		`SELECT id, identity, name, prefix, created_at_ms
		   FROM projects WHERE id = ?`, id).
		Scan(&p.ID, &p.Identity, &p.Name, &p.Prefix, &p.CreatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading project %d: %w", id, err)
	}
	return p, nil
}

// ListProjects returns every project, default row first, then by id.
func ListProjects(conn *sql.DB) ([]*model.Project, error) {
	rows, err := conn.Query(
		`SELECT id, identity, name, prefix, created_at_ms FROM projects ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	return scanRows(rows, "project rows", func(r *sql.Rows) (*model.Project, error) {
		p := &model.Project{}
		if err := r.Scan(&p.ID, &p.Identity, &p.Name, &p.Prefix, &p.CreatedAtMS); err != nil {
			return nil, fmt.Errorf("reading a project row: %w", err)
		}
		return p, nil
	})
}

// ErrProjectInUse refuses the deletion of a project anything still references
// (DKT-59).
var ErrProjectInUse = errors.New("project still has rows")

// ErrProjectIsDefault refuses the deletion of the default row.
var ErrProjectIsDefault = errors.New("the default project cannot be deleted")

// projectRefTables are every table carrying a project_id, in the order a
// message should name them. Deletion counts all of them.
//
// It is a LIST rather than a schema query because the check must fail CLOSED:
// a table added later and forgotten here would let its rows be orphaned
// silently, and a test enumerating the schema against this list is what catches
// that (the same discipline eventKinds uses for the closed set).
var projectRefTables = []string{
	"issues", "runs", "docs", "proposals", "workflows", "schemas", "labels",
}

// ProjectRefCounts reports how many rows in each project-scoped table belong to
// a project. Tables with no rows are omitted.
func ProjectRefCounts(conn *sql.DB, id int) (map[string]int, error) {
	counts := map[string]int{}
	for _, table := range projectRefTables {
		var n int
		if err := conn.QueryRow(
			`SELECT COUNT(*) FROM `+table+` WHERE project_id = ?`, id).Scan(&n); err != nil {
			return nil, fmt.Errorf("counting %s for project %d: %w", table, id, err)
		}
		if n > 0 {
			counts[table] = n
		}
	}
	return counts, nil
}

// DeleteProject removes an EMPTY project row (DKT-59).
//
// It exists because the auto-registration defect (DKT-58) minted permanent
// rows nobody asked for, and `docket project` had no verb that could take one
// back out: the operator's only remedy was a raw sqlite DELETE against a store
// shared by every repository on the machine.
//
// It REFUSES a project any row references, and refuses the default row
// outright. That is what makes it safe to expose: the verb can remove junk and
// cannot remove history, so there is no version of "I meant the other project"
// that costs anything. Re-homing real rows is `issue move --project`'s job, and
// a project emptied that way becomes deletable by this verb afterwards.
func DeleteProject(conn *sql.DB, id int) error {
	if id == DefaultProjectID {
		return ErrProjectIsDefault
	}
	if _, err := GetProject(conn, id); err != nil {
		return err
	}
	counts, err := ProjectRefCounts(conn, id)
	if err != nil {
		return err
	}
	if len(counts) > 0 {
		return ErrProjectInUse
	}
	res, err := conn.Exec(`DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting project %d: %w", id, err)
	}
	return requireAffected(res)
}

// ProjectPrefix reads one project's display prefix — the root hook's second
// query, feeding model.SetDisplayPrefix before any command runs.
func ProjectPrefix(conn *sql.DB, id int) (string, error) {
	var prefix string
	err := conn.QueryRow(
		`SELECT prefix FROM projects WHERE id = ?`, projectOrDefault(id)).Scan(&prefix)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the prefix for project %d: %w", id, err)
	}
	return prefix, nil
}

// AllProjectPrefixes reads every prefix registered in the store — the root
// hook's roster for model.SetKnownProjectPrefixes (DKT-110), so id parsing
// accepts exactly the prefixes some project actually holds and refuses the
// rest (`ANSI-16` resolving issue 16 was the defect).
func AllProjectPrefixes(conn *sql.DB) ([]string, error) {
	rows, err := conn.Query(
		`SELECT DISTINCT prefix FROM projects WHERE prefix != ''`)
	if err != nil {
		return nil, fmt.Errorf("reading the project prefixes: %w", err)
	}
	defer rows.Close()

	var prefixes []string
	for rows.Next() {
		var prefix string
		if err := rows.Scan(&prefix); err != nil {
			return nil, fmt.Errorf("scanning a project prefix: %w", err)
		}
		prefixes = append(prefixes, prefix)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the project prefixes: %w", err)
	}
	return prefixes, nil
}

// SetProjectPrefix stores a project's display prefix, uppercased. Validation
// is the caller's (model.ValidateProjectPrefix); this only writes.
func SetProjectPrefix(conn *sql.DB, id int, prefix string) error {
	res, err := conn.Exec(
		`UPDATE projects SET prefix = ? WHERE id = ?`,
		strings.ToUpper(prefix), projectOrDefault(id))
	if err != nil {
		return fmt.Errorf("setting the prefix for project %d: %w", id, err)
	}
	return requireAffected(res)
}

// ErrIssueHasParent refuses a project migration of a non-root issue: sub-issues
// follow their root, so the root is what migrates (or the issue is reparented
// first).
var ErrIssueHasParent = errors.New("issue has a parent")

// ErrIssueInRun refuses a project migration of an issue any run holds: the
// run's snapshots, steps, and events are project-scoped bookkeeping, and an
// issue migrated from under them would strand every record.
var ErrIssueInRun = errors.New("issue belongs to a run")

// MoveIssueProject migrates a ROOT issue — and its entire sub-issue tree — to
// another project (DKT-27).
//
// Gaps recorded by `step complete --gap-file` land in the run's own project
// unconditionally, because cwd is the record's only routing; when the surfaced
// work belongs to another repository, this verb is how the residue gets
// re-homed without export/import. Labels are re-mapped by NAME into the target
// project (created there when missing, color preserved) because label rows are
// per-project; relations and comments ride along untouched — ids are
// store-wide, so cross-project relations stay resolvable.
//
// It returns the migrated issue ids, the root first.
func MoveIssueProject(
	conn *sql.DB, issueID, targetProjectID int, author string, nowMS int64,
) ([]int, error) {
	targetProjectID = projectOrDefault(targetProjectID)

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning the migration: %w", err)
	}
	defer tx.Rollback()

	var (
		fromProject int
		parentID    sql.NullInt64
	)
	err = tx.QueryRow(
		`SELECT project_id, parent_id FROM issues WHERE id = ?`, issueID).
		Scan(&fromProject, &parentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading issue %d: %w", issueID, err)
	}
	if parentID.Valid {
		return nil, ErrIssueHasParent
	}

	// The subtree, root first — sub-issues follow their root.
	rows, err := tx.Query(
		`WITH RECURSIVE tree(id) AS (
		   SELECT id FROM issues WHERE id = ?
		   UNION ALL
		   SELECT i.id FROM issues i JOIN tree t ON i.parent_id = t.id
		 ) SELECT id FROM tree`, issueID)
	if err != nil {
		return nil, fmt.Errorf("collecting the sub-issue tree: %w", err)
	}
	ids, err := scanRows(rows, "sub-issue tree", func(r *sql.Rows) (int, error) {
		var id int
		return id, r.Scan(&id)
	})
	if err != nil {
		return nil, err
	}

	if fromProject == targetProjectID {
		return ids, tx.Commit() // Already home; nothing to write.
	}

	placeholders := makePlaceholders(len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}

	var inRuns int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM run_issues WHERE issue_id IN (`+placeholders+`)`,
		args...).Scan(&inRuns); err != nil {
		return nil, fmt.Errorf("checking run membership: %w", err)
	}
	if inRuns > 0 {
		return nil, ErrIssueInRun
	}

	// Labels re-map by name: same name in the target project, created there
	// when missing with the color carried over.
	labelRows, err := tx.Query(
		`SELECT DISTINCT l.id, l.name, COALESCE(l.color, '')
		   FROM labels l JOIN issue_labels il ON il.label_id = l.id
		  WHERE il.issue_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading the issues' labels: %w", err)
	}
	type labelRef struct {
		id    int
		name  string
		color string
	}
	labels, err := scanRows(labelRows, "label rows", func(r *sql.Rows) (labelRef, error) {
		var l labelRef
		return l, r.Scan(&l.id, &l.name, &l.color)
	})
	if err != nil {
		return nil, err
	}
	for _, l := range labels {
		var newID int
		err := tx.QueryRow(
			`SELECT id FROM labels WHERE project_id = ? AND name = ?`,
			targetProjectID, l.name).Scan(&newID)
		if errors.Is(err, sql.ErrNoRows) {
			res, insErr := tx.Exec(
				`INSERT INTO labels (project_id, name, color) VALUES (?, ?, NULLIF(?, ''))`,
				targetProjectID, l.name, l.color)
			if insErr != nil {
				return nil, fmt.Errorf("creating label %q in the target project: %w", l.name, insErr)
			}
			id64, insErr := res.LastInsertId()
			if insErr != nil {
				return nil, fmt.Errorf("reading the new label id: %w", insErr)
			}
			newID = int(id64)
		} else if err != nil {
			return nil, fmt.Errorf("resolving label %q in the target project: %w", l.name, err)
		}
		// OR IGNORE + sweep rather than a bare UPDATE: an issue somehow already
		// holding the target row must not abort the whole migration on the
		// primary key.
		if _, err := tx.Exec(
			`UPDATE OR IGNORE issue_labels SET label_id = ?
			  WHERE label_id = ? AND issue_id IN (`+placeholders+`)`,
			append([]any{newID, l.id}, args...)...); err != nil {
			return nil, fmt.Errorf("re-mapping label %q: %w", l.name, err)
		}
		if _, err := tx.Exec(
			`DELETE FROM issue_labels WHERE label_id = ? AND issue_id IN (`+placeholders+`)`,
			append([]any{l.id}, args...)...); err != nil {
			return nil, fmt.Errorf("sweeping label %q: %w", l.name, err)
		}
	}

	now := time.UnixMilli(nowMS).UTC().Format(time.RFC3339)
	if _, err := tx.Exec(
		`UPDATE issues SET project_id = ?, updated_at = ?, version = version + 1
		  WHERE id IN (`+placeholders+`)`,
		append([]any{targetProjectID, now}, args...)...); err != nil {
		return nil, fmt.Errorf("migrating the issues: %w", err)
	}

	// One activity row per migrated issue, identity to identity — the durable
	// names, so the trail reads the same from either side of the move.
	var fromIdentity, toIdentity string
	if err := tx.QueryRow(
		`SELECT identity FROM projects WHERE id = ?`, fromProject).Scan(&fromIdentity); err != nil {
		return nil, fmt.Errorf("reading the source project: %w", err)
	}
	if err := tx.QueryRow(
		`SELECT identity FROM projects WHERE id = ?`, targetProjectID).Scan(&toIdentity); err != nil {
		return nil, fmt.Errorf("reading the target project: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.Exec(
			`INSERT INTO activity_log (issue_id, field_changed, old_value, new_value, changed_by, created_at)
			 VALUES (?, 'project', ?, ?, ?, ?)`,
			id, fromIdentity, toIdentity, author, now); err != nil {
			return nil, fmt.Errorf("recording the migration of issue %d: %w", id, err)
		}
	}

	return ids, tx.Commit()
}

// RunProjectID resolves the project a run belongs to — the engine's way into
// the dimension: an engine verb operates on a run or step it was handed, and
// the run row, not any ambient state, says whose project that work is.
func RunProjectID(conn *sql.DB, runID int) (int, error) {
	return runProjectID(conn, runID)
}

// RunProjectIDTx is RunProjectID inside a caller's transaction.
func RunProjectIDTx(tx *sql.Tx, runID int) (int, error) {
	return runProjectID(tx, runID)
}

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func runProjectID(q rowQuerier, runID int) (int, error) {
	var id int
	err := q.QueryRow(
		`SELECT project_id FROM runs WHERE id = ?`, runID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrRunNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("resolving the project for run %d: %w", runID, err)
	}
	return id, nil
}

// IssueOwnerPrefix reports the prefix of the project that OWNS an issue, or ""
// when the issue does not exist or its project has none (DKT-256).
//
// One indexed lookup joining the issue to its project. It is the single-id form
// deliberately: issue ids are store-wide and a rendering pass touches whichever
// handful it happens to render, so a caller that preloaded the whole store
// would pay for every issue to display five.
//
// A missing row is "" AND NO ERROR. The caller is a renderer, and a renderer
// that failed because an id it was handed does not exist would turn a cosmetic
// question into a broken command — the fallback is the reader's own prefix,
// which is exactly what it rendered before this existed.
func IssueOwnerPrefix(conn *sql.DB, issueID int) (string, error) {
	var prefix string
	err := conn.QueryRow(
		`SELECT p.prefix FROM issues i JOIN projects p ON p.id = i.project_id
		  WHERE i.id = ?`, issueID).Scan(&prefix)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the owning prefix for issue %d: %w", issueID, err)
	}
	return prefix, nil
}
