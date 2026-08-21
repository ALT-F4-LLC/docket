package db

import (
	"database/sql"
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

	first, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, "key-1")
	testsupport.Must(t, err, "first create: %v", err)
	second, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, "key-1")
	testsupport.Must(t, err, "replay: %v", err)

	if first != second {
		t.Errorf("replay returned id %d, want the original %d", second, first)
	}
	if n := countIssues(t, db); n != 1 {
		t.Errorf("issue count = %d, want 1 — the replay inserted a duplicate", n)
	}
}

// TestInsertRunIdempotentReplaysOriginal mirrors
// TestIdempotentCreateReplaysOriginal for `run start` (DKT-416): a retried
// call with the same key returns the ORIGINAL run and inserts nothing, even
// when the retry's own parameters differ — matching CreateIssueIdempotent and
// CreateDocIdempotent, which never compare params and simply return the
// original entity on any repeat of the key.
func TestInsertRunIdempotentReplaysOriginal(t *testing.T) {
	db := newIdemDB(t)

	first, err := InsertRunWithContextIdempotent(
		db, 0, "first request", 10, model.NowMS(), RunContext{}, "run-key-1")
	testsupport.Must(t, err, "first run start: %v", err)
	second, err := InsertRunWithContextIdempotent(
		db, 0, "different request", 99, model.NowMS(), RunContext{}, "run-key-1")
	testsupport.Must(t, err, "replay: %v", err)

	if first.ID != second.ID {
		t.Errorf("replay returned run %d, want the original %d", second.ID, first.ID)
	}
	if second.Budget != 10 {
		t.Errorf("replay budget = %g, want the original 10 (the replay must not overwrite it)", second.Budget)
	}
	if second.Request != "first request" {
		t.Errorf("replay request = %q, want the original %q", second.Request, "first request")
	}

	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n)
	testsupport.Must(t, err, "counting runs: %v", err)
	if n != 1 {
		t.Errorf("run count = %d, want 1 — the replay inserted a duplicate", n)
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

func TestIdempotentCreateDistinctKeys(t *testing.T) {
	db := newIdemDB(t)

	first, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, "key-a")
	testsupport.Must(t, err, "create a: %v", err)
	second, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, "key-b")
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

	issueID, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, "shared")
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

	id, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, "atomic")
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
		_, err := CreateIssueIdempotent(db, probeIssue(), nil, nil, key)
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
