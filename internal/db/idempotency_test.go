package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func newIdemDB(t *testing.T) *sql.DB {
	t.Helper()
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = Migrate(db)
	testsupport.Must(t, err, "Migrate: %v", err)
	return db
}

func probeIssue() *model.Issue {
	return &model.Issue{
		Title:    "idempotency subject",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}
}

func countIssues(t *testing.T, db *sql.DB) int {
	t.Helper()
	n, err := CountIssues(db, 0)
	testsupport.Must(t, err, "CountIssues: %v", err)
	return n
}

// The core contract: a retried create with the same key returns the original
// entity and inserts nothing.
func TestIdempotentCreateReplaysOriginal(t *testing.T) {
	db := newIdemDB(t)

	first, _, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, "key-1")
	testsupport.Must(t, err, "first create: %v", err)
	second, _, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, "key-1")
	testsupport.Must(t, err, "replay: %v", err)

	if first != second {
		t.Errorf("replay returned id %d, want the original %d", second, first)
	}
	if n := countIssues(t, db); n != 1 {
		t.Errorf("issue count = %d, want 1 — the replay inserted a duplicate", n)
	}
}

// secondProject returns a project other than the default one. The first
// identity claims the unclaimed default row, so the second inserts a new one.
func secondProject(t *testing.T, db *sql.DB) int {
	t.Helper()
	_, err := EnsureProject(db, "first-identity", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	id, err := EnsureProject(db, "second-identity", "second", 1)
	testsupport.Must(t, err, "creating a second project: %v", err)
	if id == DefaultProjectID {
		t.Fatalf("second project resolved to the default project %d", id)
	}
	return id
}

func countRuns(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n)
	testsupport.Must(t, err, "counting runs: %v", err)
	return n
}

// TestInsertRunIdempotentReplaysOriginal mirrors
// TestIdempotentCreateReplaysOriginal for `run start`: a retried call with the
// same key and the same request content returns the ORIGINAL run with
// inserted=false and inserts nothing. The retry starts later, so the start
// time is not part of the request content.
func TestInsertRunIdempotentReplaysOriginal(t *testing.T) {
	db := newIdemDB(t)
	issueID, err := CreateIssue(db, probeIssue(), nil, nil)
	testsupport.Must(t, err, "creating issue: %v", err)
	ctx := RunContext{ExecRoot: "/repo", Branch: "main", CommitSHA: "abc", Hostname: "h", UsageBudget: 3}

	first, _, err := InsertRunWithContextIdempotent(
		db, 0, "request", 10, 1000, ctx, "run-key-1", issueID)
	testsupport.Must(t, err, "first run start: %v", err)
	second, inserted, err := InsertRunWithContextIdempotent(
		db, 0, "request", 10, 2000, ctx, "run-key-1", issueID)
	testsupport.Must(t, err, "replay: %v", err)

	if inserted {
		t.Error("replay of an existing key reported inserted=true")
	}
	if first.ID != second.ID {
		t.Errorf("replay returned run %d, want the original %d", second.ID, first.ID)
	}
	if second.CreatedAtMS != 1000 {
		t.Errorf("replay created_at_ms = %d, want the original 1000", second.CreatedAtMS)
	}
	if n := countRuns(t, db); n != 1 {
		t.Errorf("run count = %d, want 1 — the replay inserted a duplicate", n)
	}
}

// TestInsertRunIdempotentRejectsChangedContent: reusing a key with any
// different piece of request content (request, budget, project, context or
// issue ids) is a conflict naming the key, and creates nothing.
func TestInsertRunIdempotentRejectsChangedContent(t *testing.T) {
	type request struct {
		project  int
		text     string
		budget   float64
		ctx      RunContext
		issueIDs []int
	}
	cases := []struct {
		name   string
		change func(r *request, otherProject, otherIssue int)
	}{
		{"request", func(r *request, _, _ int) { r.text = "other request" }},
		{"budget", func(r *request, _, _ int) { r.budget = 99 }},
		{"project", func(r *request, p, _ int) { r.project = p }},
		{"usage budget", func(r *request, _, _ int) { r.ctx.UsageBudget = 7 }},
		{"exec root", func(r *request, _, _ int) { r.ctx.ExecRoot = "/elsewhere" }},
		{"commit", func(r *request, _, _ int) { r.ctx.CommitSHA = "def" }},
		{"issue ids", func(r *request, _, i int) { r.issueIDs = []int{i} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newIdemDB(t)
			issueID, err := CreateIssue(db, probeIssue(), nil, nil)
			testsupport.Must(t, err, "creating issue: %v", err)
			otherIssue, err := CreateIssue(db, probeIssue(), nil, nil)
			testsupport.Must(t, err, "creating other issue: %v", err)
			otherProject := secondProject(t, db)

			original := request{
				text: "request", budget: 10, issueIDs: []int{issueID},
				ctx: RunContext{ExecRoot: "/repo", Branch: "main", CommitSHA: "abc", Hostname: "h", UsageBudget: 3},
			}
			start := func(r request) (*model.Run, error) {
				run, _, err := InsertRunWithContextIdempotent(
					db, r.project, r.text, r.budget, 1000, r.ctx, "run-key", r.issueIDs...)
				return run, err
			}
			_, err = start(original)
			testsupport.Must(t, err, "first run start: %v", err)

			changed := original
			tc.change(&changed, otherProject, otherIssue)
			_, err = start(changed)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("reuse with a different %s returned %v, want an ErrConflict", tc.name, err)
			}
			if !strings.Contains(err.Error(), "run-key") {
				t.Errorf("conflict %q does not name the key", err)
			}
			if n := countRuns(t, db); n != 1 {
				t.Errorf("run count = %d, want 1 — the rejected reuse created a run", n)
			}
		})
	}
}

// TestCreateDocIdempotentReplaysOriginal: the same key with the same doc
// content returns the original doc and inserts nothing.
func TestCreateDocIdempotentReplaysOriginal(t *testing.T) {
	db := newIdemDB(t)
	doc := func() *model.Doc {
		return &model.Doc{Title: "t", Type: "adr", Status: "draft", Body: "b", Author: "a"}
	}

	first, err := CreateDocIdempotent(db, doc(), "doc-key")
	testsupport.Must(t, err, "first doc create: %v", err)
	second, err := CreateDocIdempotent(db, doc(), "doc-key")
	testsupport.Must(t, err, "replay: %v", err)

	if second != first {
		t.Errorf("replay returned doc %d, want the original %d", second, first)
	}
	docs, _, err := ListDocs(db, DocListOptions{})
	testsupport.Must(t, err, "ListDocs: %v", err)
	if len(docs) != 1 {
		t.Errorf("doc count = %d, want 1 — the replay inserted a duplicate", len(docs))
	}
}

// TestCreateDocIdempotentRejectsChangedContent: reusing a key with any
// different doc field is a conflict naming the key, and creates nothing.
func TestCreateDocIdempotentRejectsChangedContent(t *testing.T) {
	cases := []struct {
		name   string
		change func(d *model.Doc, otherProject int)
	}{
		{"title", func(d *model.Doc, _ int) { d.Title = "other" }},
		{"type", func(d *model.Doc, _ int) { d.Type = "tdd" }},
		{"status", func(d *model.Doc, _ int) { d.Status = "accepted" }},
		{"body", func(d *model.Doc, _ int) { d.Body = "other body" }},
		{"author", func(d *model.Doc, _ int) { d.Author = "someone else" }},
		{"project", func(d *model.Doc, p int) { d.ProjectID = p }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newIdemDB(t)
			otherProject := secondProject(t, db)
			original := model.Doc{Title: "t", Type: "adr", Status: "draft", Body: "b", Author: "a"}

			first := original
			_, err := CreateDocIdempotent(db, &first, "doc-key")
			testsupport.Must(t, err, "first doc create: %v", err)

			changed := original
			tc.change(&changed, otherProject)
			_, err = CreateDocIdempotent(db, &changed, "doc-key")
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("reuse with a different %s returned %v, want an ErrConflict", tc.name, err)
			}
			if !strings.Contains(err.Error(), "doc-key") {
				t.Errorf("conflict %q does not name the key", err)
			}
			docs, _, err := ListDocs(db, DocListOptions{})
			testsupport.Must(t, err, "ListDocs: %v", err)
			if len(docs) != 1 {
				t.Errorf("doc count = %d, want 1 — the rejected reuse created a doc", len(docs))
			}
		})
	}
}

// TestInsertRunIdempotentReportsInsert mirrors
// TestCreateIssueIdempotentReportsInsert for `run start`: the CLI attaches
// --issue only when the create itself says it inserted.
func TestInsertRunIdempotentReportsInsert(t *testing.T) {
	db := newIdemDB(t)

	first, inserted, err := InsertRunWithContextIdempotent(
		db, 0, "req", 0, model.NowMS(), RunContext{}, "run-key-1")
	testsupport.Must(t, err, "first run start: %v", err)
	if !inserted {
		t.Error("run start under a fresh key reported inserted=false")
	}

	replay, inserted, err := InsertRunWithContextIdempotent(
		db, 0, "req", 0, model.NowMS(), RunContext{}, "run-key-1")
	testsupport.Must(t, err, "replay: %v", err)
	if inserted {
		t.Error("replay of an existing key reported inserted=true")
	}
	if replay.ID != first.ID {
		t.Errorf("replay returned run %d, want the original %d", replay.ID, first.ID)
	}
}

// TestInsertRunIdempotentCommitsRosterWithRun: the roster commits in the same
// transaction as the run and its key, so a retry that replays the key finds
// the roster already written instead of an empty one.
func TestInsertRunIdempotentCommitsRosterWithRun(t *testing.T) {
	db := newIdemDB(t)

	first, err := CreateIssue(db, probeIssue(), nil, nil)
	testsupport.Must(t, err, "creating first issue: %v", err)
	second, err := CreateIssue(db, probeIssue(), nil, nil)
	testsupport.Must(t, err, "creating second issue: %v", err)
	want := []int{first, second}

	rosterOf := func(runID int) []int {
		t.Helper()
		rows, err := ListRunIssues(db, runID)
		testsupport.Must(t, err, "ListRunIssues: %v", err)
		ids := make([]int, 0, len(rows))
		for _, ri := range rows {
			ids = append(ids, ri.IssueID)
		}
		return ids
	}

	run, inserted, err := InsertRunWithContextIdempotent(
		db, 0, "req", 0, model.NowMS(), RunContext{}, "run-key-roster", first, second)
	testsupport.Must(t, err, "first run start: %v", err)
	if !inserted {
		t.Fatal("run start under a fresh key reported inserted=false")
	}
	if got := rosterOf(run.ID); !slices.Equal(got, want) {
		t.Errorf("roster after insert = %v, want %v", got, want)
	}

	replay, inserted, err := InsertRunWithContextIdempotent(
		db, 0, "req", 0, model.NowMS(), RunContext{}, "run-key-roster", first, second)
	testsupport.Must(t, err, "replay: %v", err)
	if inserted {
		t.Error("replay of an existing key reported inserted=true")
	}
	if replay.ID != run.ID {
		t.Fatalf("replay returned run %d, want the original %d", replay.ID, run.ID)
	}
	if got := rosterOf(run.ID); !slices.Equal(got, want) {
		t.Errorf("roster after replay = %v, want %v", got, want)
	}
}

// TestInsertRunWithoutKeyNeverDeduplicates mirrors
// TestCreateWithoutKeyNeverDeduplicates: an empty key always skips the
// lookup, matching every other create verb's keyless behavior.
func TestInsertRunWithoutKeyNeverDeduplicates(t *testing.T) {
	db := newIdemDB(t)

	for range 3 {
		_, err := InsertRun(db, 0, "req", 0, model.NowMS())
		testsupport.Must(t, err, "run start: %v", err)
	}
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n)
	testsupport.Must(t, err, "counting runs: %v", err)
	if n != 3 {
		t.Errorf("run count = %d, want 3 — keyless creates must not dedupe", n)
	}
}

// TestCreateIssueIdempotentReportsInsert: the caller learns from the create
// itself whether it inserted, so post-insert writes (the CLI's --scope) can key
// on that answer instead of a separate lookup another process can race.
func TestCreateIssueIdempotentReportsInsert(t *testing.T) {
	db := newIdemDB(t)

	first, inserted, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, "key-1")
	testsupport.Must(t, err, "first create: %v", err)
	if !inserted {
		t.Error("create under a fresh key reported inserted=false")
	}
	if n := countIssues(t, db); n != 1 {
		t.Fatalf("issue count after fresh create = %d, want 1", n)
	}

	second, inserted, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, "key-1")
	testsupport.Must(t, err, "replay: %v", err)
	if inserted {
		t.Error("create under a recorded key reported inserted=true")
	}
	if second != first {
		t.Errorf("replay returned id %d, want the recorded %d", second, first)
	}
	if n := countIssues(t, db); n != 1 {
		t.Errorf("issue count after replay = %d, want 1", n)
	}
}

func TestIdempotentCreateDistinctKeys(t *testing.T) {
	db := newIdemDB(t)

	first, _, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, "key-a")
	testsupport.Must(t, err, "create a: %v", err)
	second, _, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, "key-b")
	testsupport.Must(t, err, "create b: %v", err)

	if first == second {
		t.Error("distinct keys returned the same id")
	}
	if n := countIssues(t, db); n != 2 {
		t.Errorf("issue count = %d, want 2", n)
	}
}

func TestCreateWithoutKeyNeverDeduplicates(t *testing.T) {
	db := newIdemDB(t)

	for range 3 {
		_, err := CreateIssue(db, probeIssue(), nil, nil)
		testsupport.Must(t, err, "create: %v", err)
	}
	if n := countIssues(t, db); n != 3 {
		t.Errorf("issue count = %d, want 3 — keyless creates must not dedupe", n)
	}
}

// Keys are scoped per verb, so the same key on two verbs is independent.
func TestIdempotencyKeysAreScopedPerVerb(t *testing.T) {
	db := newIdemDB(t)

	issueID, _, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, "shared")
	testsupport.Must(t, err, "issue create: %v", err)
	docID, err := CreateDocIdempotent(db, &model.Doc{Title: "doc", Type: "adr"}, "shared")
	testsupport.Must(t, err, "doc create: %v", err)

	if n := countIssues(t, db); n != 1 {
		t.Errorf("issue count = %d, want 1", n)
	}
	if docID == 0 || issueID == 0 {
		t.Error("expected both entities to be created under the same key in different scopes")
	}

	// And each replays within its own scope.
	replayDoc, err := CreateDocIdempotent(db, &model.Doc{Title: "doc", Type: "adr"}, "shared")
	testsupport.Must(t, err, "doc replay: %v", err)
	if replayDoc != docID {
		t.Errorf("doc replay = %d, want %d", replayDoc, docID)
	}

	// A third pair, reusing the same key on the two comment scopes
	// (beginIdempotentCreate's other two callers): each entity is created,
	// and each replays only within its own scope — a doc-comment key must
	// never satisfy a lookup against ScopeIssueComment or vice versa.
	commentID, err := CreateCommentIdempotent(db, &model.Comment{
		IssueID: issueID, Body: "comment body", Author: "u",
	}, "shared")
	testsupport.Must(t, err, "comment create: %v", err)
	docCommentID, err := CreateDocCommentIdempotent(db, &model.DocComment{
		DocID: docID, Body: "doc comment body", Author: "u",
	}, "shared")
	testsupport.Must(t, err, "doc comment create: %v", err)
	if commentID == 0 || docCommentID == 0 {
		t.Error("expected both comment entities to be created under the same key in different scopes")
	}
	// A collapsed scope would make the doc-comment call a "replay" of the
	// issue comment's key — a hit — and never insert a row at all.
	docComments, err := ListDocComments(db, docID)
	testsupport.Must(t, err, "ListDocComments: %v", err)
	if len(docComments) != 1 {
		t.Errorf("doc comment count = %d, want 1 (a collapsed scope would make this a no-op replay)", len(docComments))
	}

	replayComment, err := CreateCommentIdempotent(db, &model.Comment{
		IssueID: issueID, Body: "comment body", Author: "u",
	}, "shared")
	testsupport.Must(t, err, "comment replay: %v", err)
	if replayComment != commentID {
		t.Errorf("comment replay = %d, want %d", replayComment, commentID)
	}
	replayDocComment, err := CreateDocCommentIdempotent(db, &model.DocComment{
		DocID: docID, Body: "doc comment body", Author: "u",
	}, "shared")
	testsupport.Must(t, err, "doc comment replay: %v", err)
	if replayDocComment != docCommentID {
		t.Errorf("doc comment replay = %d, want %d", replayDocComment, docCommentID)
	}
}

// The key record must be committed atomically with the insert.
func TestIdempotencyKeyRecordedWithEntity(t *testing.T) {
	db := newIdemDB(t)

	id, _, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, "atomic")
	testsupport.Must(t, err, "create: %v", err)

	got, found, err := LookupIdempotencyKey(db, ScopeIssueCreate, "atomic")
	testsupport.Must(t, err, "LookupIdempotencyKey: %v", err)
	if !found {
		t.Fatal("key not recorded alongside the insert")
	}
	if got != id {
		t.Errorf("recorded entity_id = %d, want %d", got, id)
	}
}

func TestLookupMissingKey(t *testing.T) {
	db := newIdemDB(t)

	if _, found, err := LookupIdempotencyKey(db, ScopeIssueCreate, "never-used"); err != nil {
		t.Fatalf("LookupIdempotencyKey: %v", err)
	} else if found {
		t.Error("found = true for a key that was never recorded")
	}
}

// seq is monotonic and created_at_ms is populated — both are v5-only fields
// that exist so later stages can order records without touching the existing
// RFC3339 columns.
func TestIdempotencySeqIsMonotonic(t *testing.T) {
	db := newIdemDB(t)

	for _, key := range []string{"k1", "k2", "k3"} {
		_, _, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, key)
		testsupport.Must(t, err, "create %s: %v", key, err)
	}

	rows, err := db.Query(`SELECT seq, created_at_ms FROM idempotency_keys ORDER BY seq`)
	testsupport.Must(t, err, "querying keys: %v", err)
	defer rows.Close()

	prev := int64(0)
	n := 0
	for rows.Next() {
		var seq, createdAtMS int64
		err := rows.Scan(&seq, &createdAtMS)
		testsupport.Must(t, err, "scan: %v", err)
		if seq <= prev {
			t.Errorf("seq %d is not greater than previous %d", seq, prev)
		}
		if createdAtMS <= 0 {
			t.Errorf("created_at_ms = %d, want a positive epoch-ms value", createdAtMS)
		}
		prev = seq
		n++
	}
	if n != 3 {
		t.Errorf("recorded %d keys, want 3", n)
	}
}

// fingerprintScope is a scope no create verb uses, so these tests exercise the
// fingerprint helpers directly rather than through any one verb.
const fingerprintScope = "test.fingerprint"

// createFingerprinted is the shape every fingerprint-aware create verb takes:
// begin, replay on a hit, otherwise insert an entity and record the key in the
// same transaction. The entity is a probe_entities row when that table exists,
// else the fixed id 42.
func createFingerprinted(db *sql.DB, key, fingerprint string) (int, error) {
	existingID, hit, tx, err := beginIdempotentCreateFingerprinted(db, fingerprintScope, key, fingerprint)
	if err != nil {
		return 0, err
	}
	if hit {
		return existingID, nil
	}
	defer tx.Rollback()

	id := 42
	var hasProbe bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master
	                        WHERE type='table' AND name='probe_entities')`).Scan(&hasProbe); err != nil {
		return 0, err
	}
	if hasProbe {
		res, err := tx.Exec(`INSERT INTO probe_entities DEFAULT VALUES`)
		if err != nil {
			return 0, err
		}
		id64, err := res.LastInsertId()
		if err != nil {
			return 0, err
		}
		id = int(id64)
	}
	if err := recordIdempotencyKeyFingerprintTx(tx, fingerprintScope, key, fingerprint, id); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func storedFingerprint(t *testing.T, db *sql.DB, scope, key string) sql.NullString {
	t.Helper()
	var fp sql.NullString
	err := db.QueryRow(`SELECT fingerprint FROM idempotency_keys WHERE scope = ? AND key = ?`,
		scope, key).Scan(&fp)
	testsupport.Must(t, err, "reading fingerprint of %s/%s: %v", scope, key, err)
	return fp
}

// TestMigrateIdempotencyFingerprint: a store from before the fingerprint column
// upgrades cleanly, and a key it already recorded reads NULL and still replays.
func TestMigrateIdempotencyFingerprint(t *testing.T) {
	db := newIdemDB(t)

	// A v46 store: the column absent, the stamp one back, and a key recorded
	// in the v46 shape.
	mustExec(t, db, `ALTER TABLE idempotency_keys DROP COLUMN fingerprint`)
	mustExec(t, db, `UPDATE meta SET value = '46' WHERE key = 'schema_version'`)
	mustExec(t, db, `INSERT INTO idempotency_keys (scope, key, entity_id, created_at_ms, seq)
	                 VALUES (?, 'old-key', 7, 1, 1)`, fingerprintScope)

	err := Migrate(db)
	testsupport.Must(t, err, "Migrate from v46: %v", err)
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Errorf("schema_version after migrating from v46 = %d, want %d", v, currentSchemaVersion)
	}
	if exists, err := hasColumnDB(db, "idempotency_keys", "fingerprint"); err != nil || !exists {
		t.Fatalf("idempotency_keys.fingerprint missing after migrating from v46 (err %v)", err)
	}
	if fp := storedFingerprint(t, db, fingerprintScope, "old-key"); fp.Valid {
		t.Errorf("pre-migration key reads fingerprint %q, want NULL", fp.String)
	}

	id, err := createFingerprinted(db, "old-key", "fp-any")
	testsupport.Must(t, err, "replaying the pre-migration key: %v", err)
	if id != 7 {
		t.Errorf("pre-migration key replayed entity %d, want 7", id)
	}
}

// TestIdempotencyFingerprintRewindGuard drops the column while leaving the
// stamp current and asserts Migrate converges it.
func TestIdempotencyFingerprintRewindGuard(t *testing.T) {
	db := newIdemDB(t)

	mustExec(t, db, `ALTER TABLE idempotency_keys DROP COLUMN fingerprint`)
	err := Migrate(db)
	testsupport.Must(t, err, "re-running Migrate on the stamped store: %v", err)
	if exists, err := hasColumnDB(db, "idempotency_keys", "fingerprint"); err != nil || !exists {
		t.Fatalf("the rewind guard did not converge idempotency_keys.fingerprint (err %v)", err)
	}
}

// TestIdempotencyMismatch: the same key with the same fingerprint replays the
// recorded entity; with a different fingerprint it is a conflict naming the key.
func TestIdempotencyMismatch(t *testing.T) {
	db := newIdemDB(t)

	first, err := createFingerprinted(db, "key-1", "fp-a")
	testsupport.Must(t, err, "first create: %v", err)
	if fp := storedFingerprint(t, db, fingerprintScope, "key-1"); !fp.Valid || fp.String != "fp-a" {
		t.Errorf("recorded fingerprint = %+v, want fp-a", fp)
	}

	replay, err := createFingerprinted(db, "key-1", "fp-a")
	testsupport.Must(t, err, "same-fingerprint replay: %v", err)
	if replay != first {
		t.Errorf("same-fingerprint replay returned %d, want the recorded %d", replay, first)
	}

	_, err = createFingerprinted(db, "key-1", "fp-b")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("different-fingerprint reuse returned %v, want an ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "key-1") {
		t.Errorf("conflict %q does not name the key key-1", err)
	}
	if fp := storedFingerprint(t, db, fingerprintScope, "key-1"); fp.String != "fp-a" {
		t.Errorf("fingerprint after the rejected reuse = %q, want fp-a unchanged", fp.String)
	}
}

// TestIdempotencyLegacyNull: a key recorded without a fingerprint matches any
// fingerprint and stays NULL after the replay.
func TestIdempotencyLegacyNull(t *testing.T) {
	db := newIdemDB(t)

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	err = RecordIdempotencyKeyTx(tx, fingerprintScope, "legacy", 9)
	testsupport.Must(t, err, "RecordIdempotencyKeyTx: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	id, err := createFingerprinted(db, "legacy", "fp-new")
	testsupport.Must(t, err, "fingerprinted replay of a legacy key: %v", err)
	if id != 9 {
		t.Errorf("legacy key replayed entity %d, want 9", id)
	}
	if fp := storedFingerprint(t, db, fingerprintScope, "legacy"); fp.Valid {
		t.Errorf("legacy key fingerprint = %q after replay, want NULL (no backfill)", fp.String)
	}
}

// TestIdempotencyConcurrent races callers sharing one key, each on its own
// connection as separate processes would be. Equal fingerprints converge on one
// entity; differing fingerprints yield one success and conflicts for the rest,
// never a raw constraint error.
func TestIdempotencyConcurrent(t *testing.T) {
	const callers = 8

	race := func(t *testing.T, fingerprintOf func(i int) string) (ids []int, errs []error, entities int) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "idem.db")
		setup, err := Open(path)
		testsupport.Must(t, err, "Open: %v", err)
		defer setup.Close()
		err = Initialize(setup)
		testsupport.Must(t, err, "Initialize: %v", err)
		err = Migrate(setup)
		testsupport.Must(t, err, "Migrate: %v", err)
		mustExec(t, setup, `CREATE TABLE probe_entities (id INTEGER PRIMARY KEY AUTOINCREMENT)`)

		handles := make([]*sql.DB, callers)
		for i := range handles {
			h, err := Open(path)
			testsupport.Must(t, err, "Open caller %d: %v", i, err)
			defer h.Close()
			handles[i] = h
		}

		ids = make([]int, callers)
		errs = make([]error, callers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ids[i], errs[i] = createFingerprinted(handles[i], "shared", fingerprintOf(i))
			}()
		}
		close(start)
		wg.Wait()

		err = setup.QueryRow(`SELECT COUNT(*) FROM probe_entities`).Scan(&entities)
		testsupport.Must(t, err, "counting entities: %v", err)
		return ids, errs, entities
	}

	t.Run("equal fingerprints", func(t *testing.T) {
		ids, errs, entities := race(t, func(int) string { return "fp-same" })
		for i, err := range errs {
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
			}
		}
		for i, id := range ids {
			if id != ids[0] {
				t.Errorf("caller %d got entity %d, caller 0 got %d", i, id, ids[0])
			}
		}
		if entities != 1 {
			t.Errorf("created %d entities, want 1", entities)
		}
	})

	t.Run("differing fingerprints", func(t *testing.T) {
		_, errs, entities := race(t, func(i int) string { return "fp-" + string(rune('a'+i)) })
		successes := 0
		for i, err := range errs {
			switch {
			case err == nil:
				successes++
			case !errors.Is(err, ErrConflict):
				t.Errorf("caller %d: %v, want nil or an ErrConflict", i, err)
			}
		}
		if successes != 1 {
			t.Errorf("%d callers succeeded, want exactly 1", successes)
		}
		if entities != 1 {
			t.Errorf("created %d entities, want 1", entities)
		}
	})
}
