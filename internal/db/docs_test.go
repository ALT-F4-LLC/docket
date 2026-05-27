package db

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// mustCreateDoc creates a doc with sensible defaults and returns the new ID.
func mustCreateDoc(t *testing.T, db *sql.DB, title, typ, status, body string) int {
	t.Helper()
	id, err := CreateDoc(db, &model.Doc{
		Title:  title,
		Type:   typ,
		Status: status,
		Body:   body,
		Author: "tester",
	})
	if err != nil {
		t.Fatalf("CreateDoc(%q): %v", title, err)
	}
	return id
}

// --- CreateDoc / AppendDocRevision ---

func TestCreateDoc_InsertsRevision1(t *testing.T) {
	db := mustInitAndMigrate(t)

	id := mustCreateDoc(t, db, "first", "tdd", "draft", "body v1")

	revs, err := ListDocRevisions(db, id)
	if err != nil {
		t.Fatalf("ListDocRevisions: %v", err)
	}
	if len(revs) != 1 {
		t.Fatalf("len(revs) = %d, want 1", len(revs))
	}
	if revs[0].RevisionNumber != 1 {
		t.Errorf("RevisionNumber = %d, want 1", revs[0].RevisionNumber)
	}
	if revs[0].Body != "body v1" {
		t.Errorf("Body = %q, want %q", revs[0].Body, "body v1")
	}
}

func TestCreateDoc_ChangeKindCreate(t *testing.T) {
	db := mustInitAndMigrate(t)

	id := mustCreateDoc(t, db, "first", "tdd", "draft", "body v1")

	revs, _ := ListDocRevisions(db, id)
	if revs[0].ChangeKind != "create" {
		t.Errorf("ChangeKind = %q, want %q", revs[0].ChangeKind, "create")
	}
}

func TestCreateDoc_ParityWithDocBody(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "first", "tdd", "draft", "hello\n")

	d, err := GetDoc(db, id)
	if err != nil {
		t.Fatalf("GetDoc: %v", err)
	}
	revs, _ := ListDocRevisions(db, id)
	if d.Body != revs[0].Body {
		t.Errorf("doc.Body = %q, rev.Body = %q (must match)", d.Body, revs[0].Body)
	}
}

// --- UpdateDoc — each persisted field appends one revision ---

func updateBody(t *testing.T, db *sql.DB, id int, body string) int {
	t.Helper()
	rev, err := UpdateDoc(db, id, DocUpdate{Body: &body, Author: "editor"})
	if err != nil {
		t.Fatalf("UpdateDoc body: %v", err)
	}
	return rev
}

func TestUpdateDoc_AppendsRevisionOnBodyChange(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "first", "tdd", "draft", "v1")

	rev := updateBody(t, db, id, "v2")
	if rev != 2 {
		t.Fatalf("returned revision_number = %d, want 2", rev)
	}

	revs, _ := ListDocRevisions(db, id)
	if len(revs) != 2 {
		t.Fatalf("len(revs) = %d, want 2", len(revs))
	}
	if revs[1].ChangeKind != "body" {
		t.Errorf("revs[1].ChangeKind = %q, want body", revs[1].ChangeKind)
	}
}

func TestUpdateDoc_AppendsRevisionOnStatusChange(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "first", "tdd", "draft", "v1")
	newStatus := "approved"
	rev, err := UpdateDoc(db, id, DocUpdate{Status: &newStatus, Author: "voter"})
	if err != nil {
		t.Fatalf("UpdateDoc status: %v", err)
	}
	if rev != 2 {
		t.Fatalf("rev = %d, want 2", rev)
	}
	revs, _ := ListDocRevisions(db, id)
	if revs[1].ChangeKind != "status" {
		t.Errorf("ChangeKind = %q, want status", revs[1].ChangeKind)
	}
	// Body is duplicated from the prior revision (metadata-only edit, TDD §5.4 C8).
	if revs[1].Body != "v1" {
		t.Errorf("revs[1].Body = %q, want v1 (duplicated from prior)", revs[1].Body)
	}
}

func TestUpdateDoc_AppendsRevisionOnTitleChange(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "first", "tdd", "draft", "v1")
	newTitle := "second title"
	rev, err := UpdateDoc(db, id, DocUpdate{Title: &newTitle, Author: "editor"})
	if err != nil {
		t.Fatalf("UpdateDoc title: %v", err)
	}
	if rev != 2 {
		t.Fatalf("rev = %d, want 2", rev)
	}
	revs, _ := ListDocRevisions(db, id)
	if revs[1].ChangeKind != "title" {
		t.Errorf("ChangeKind = %q, want title", revs[1].ChangeKind)
	}
}

func TestUpdateDoc_AppendsRevisionOnTypeChange(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "first", "tdd", "draft", "v1")
	newType := "adr"
	rev, err := UpdateDoc(db, id, DocUpdate{Type: &newType, Author: "editor"})
	if err != nil {
		t.Fatalf("UpdateDoc type: %v", err)
	}
	if rev != 2 {
		t.Fatalf("rev = %d, want 2", rev)
	}
	revs, _ := ListDocRevisions(db, id)
	if revs[1].ChangeKind != "type" {
		t.Errorf("ChangeKind = %q, want type", revs[1].ChangeKind)
	}
}

func TestUpdateDoc_CombinedChangeKind(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "first", "tdd", "draft", "v1")
	newStatus := "approved"
	newBody := "v2"
	rev, err := UpdateDoc(db, id, DocUpdate{
		Status: &newStatus,
		Body:   &newBody,
		Author: "voter",
	})
	if err != nil {
		t.Fatalf("UpdateDoc combined: %v", err)
	}
	if rev != 2 {
		t.Fatalf("rev = %d, want 2", rev)
	}
	revs, _ := ListDocRevisions(db, id)
	// UpdateDoc orders the change_kind by field-write order: title, type, status, body.
	if revs[1].ChangeKind != "status+body" {
		t.Errorf("ChangeKind = %q, want status+body", revs[1].ChangeKind)
	}
	if revs[1].Body != "v2" {
		t.Errorf("Body = %q, want v2", revs[1].Body)
	}
}

func TestUpdateDoc_NoRevisionOnNoOpEdit(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "first", "tdd", "draft", "v1\n")

	// Trailing-newline-only change; TrimRight equality treats this as no-op.
	bodyWithExtraNL := "v1\n\n\n"
	rev, err := UpdateDoc(db, id, DocUpdate{Body: &bodyWithExtraNL, Author: "editor"})
	if err != nil {
		t.Fatalf("UpdateDoc no-op: %v", err)
	}
	if rev != 0 {
		t.Fatalf("rev = %d, want 0 (no-op)", rev)
	}
	revs, _ := ListDocRevisions(db, id)
	if len(revs) != 1 {
		t.Fatalf("len(revs) = %d, want 1 (no revision appended on no-op)", len(revs))
	}
}

func TestUpdateDoc_BodyEqualityAfterTrimRight(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "first", "tdd", "draft", "abc")

	candidates := []string{"abc", "abc\n", "abc\n\n"}
	for _, c := range candidates {
		c := c
		rev, err := UpdateDoc(db, id, DocUpdate{Body: &c, Author: "editor"})
		if err != nil {
			t.Fatalf("UpdateDoc(%q): %v", c, err)
		}
		if rev != 0 {
			t.Errorf("UpdateDoc(%q): rev=%d, want 0", c, rev)
		}
	}

	revs, _ := ListDocRevisions(db, id)
	if len(revs) != 1 {
		t.Errorf("len(revs) = %d, want 1", len(revs))
	}
}

func TestUpdateDoc_BodyParity(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "first", "tdd", "draft", "v1")
	newBody := "v2 — different"
	if _, err := UpdateDoc(db, id, DocUpdate{Body: &newBody, Author: "editor"}); err != nil {
		t.Fatalf("UpdateDoc: %v", err)
	}

	d, _ := GetDoc(db, id)
	revs, _ := ListDocRevisions(db, id)
	if d.Body != revs[len(revs)-1].Body {
		t.Errorf("docs.body = %q, latest rev body = %q (must match)", d.Body, revs[len(revs)-1].Body)
	}
}

func TestUpdateDoc_NotFound(t *testing.T) {
	db := mustInitAndMigrate(t)
	newTitle := "nope"
	_, err := UpdateDoc(db, 999, DocUpdate{Title: &newTitle, Author: "x"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// --- ListDocsWithCounts — single-JOIN strategy (S1) ---

func TestListDocsWithCounts_NoNPlusOne(t *testing.T) {
	// TDD §5.4 S1 — must be a single statement, no per-row sub-selects.
	// We assert this by running EXPLAIN QUERY PLAN against the SQL the
	// function uses and verifying it touches docs and doc_revisions exactly
	// once each as JOIN partners. If a future refactor introduces N+1
	// (e.g. a correlated subquery in SELECT), this test fails.
	db := mustInitAndMigrate(t)
	for i := 0; i < 5; i++ {
		id := mustCreateDoc(t, db, "title", "tdd", "draft", "body")
		// Multiple revisions per doc to exercise the COUNT/MAX aggregation.
		b1 := "body+r2"
		updateBody(t, db, id, b1)
	}

	// Assert the exact query the function uses produces a single-pass plan.
	rows, err := db.Query("EXPLAIN QUERY PLAN " + listDocsWithCountsBaseSQL + " GROUP BY d.id")
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var selectID, order, from int
		var detail string
		if err := rows.Scan(&selectID, &order, &from, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows.Err: %v", err)
	}
	if len(plan) == 0 {
		t.Fatalf("EXPLAIN QUERY PLAN returned no rows")
	}

	// Verify the plan covers both tables via the LEFT-JOIN partner and contains
	// no CORRELATED SUBQUERY / EXECUTE LIST SUBQUERY marker (which would
	// indicate N+1). SQLite reports the docs alias as "d" and the
	// doc_revisions side via its autoindex name.
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "SCAN d") {
		t.Errorf("plan does not scan docs (alias d):\n%s", joined)
	}
	if !strings.Contains(joined, "doc_revisions") {
		t.Errorf("plan does not reference doc_revisions:\n%s", joined)
	}
	if !strings.Contains(joined, "LEFT-JOIN") {
		t.Errorf("plan does not use a LEFT-JOIN:\n%s", joined)
	}
	if strings.Contains(joined, "CORRELATED") || strings.Contains(joined, "EXECUTE LIST SUBQUERY") {
		t.Errorf("plan contains N+1 marker:\n%s", joined)
	}
	// The plan should have exactly two rows: one SCAN over docs and one
	// SEARCH against doc_revisions. Any extra rows indicate per-doc sub-work.
	if len(plan) > 2 {
		t.Errorf("plan has %d rows, want <= 2 (one per table):\n%s", len(plan), joined)
	}

	// Behavioural check: the call returns the right counts for each doc.
	summaries, total, err := ListDocsWithCounts(db, DocListOptions{Sort: "id", SortDir: "asc"})
	if err != nil {
		t.Fatalf("ListDocsWithCounts: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(summaries) != 5 {
		t.Fatalf("len(summaries) = %d, want 5", len(summaries))
	}
	for i, s := range summaries {
		if s.RevisionsCount != 2 {
			t.Errorf("summaries[%d].RevisionsCount = %d, want 2", i, s.RevisionsCount)
		}
		if s.CurrentRevision != 2 {
			t.Errorf("summaries[%d].CurrentRevision = %d, want 2", i, s.CurrentRevision)
		}
	}
}

func TestListDocsWithCounts_FilterByType(t *testing.T) {
	db := mustInitAndMigrate(t)
	mustCreateDoc(t, db, "a", "tdd", "draft", "x")
	mustCreateDoc(t, db, "b", "adr", "draft", "x")
	mustCreateDoc(t, db, "c", "tdd", "draft", "x")

	summaries, total, err := ListDocsWithCounts(db, DocListOptions{Types: []string{"tdd"}})
	if err != nil {
		t.Fatalf("ListDocsWithCounts: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(summaries) != 2 {
		t.Errorf("len(summaries) = %d, want 2", len(summaries))
	}
	for _, s := range summaries {
		if s.Doc.Type != "tdd" {
			t.Errorf("returned non-tdd doc: %q", s.Doc.Type)
		}
	}
}

func TestListDocsWithCounts_FilterByStatus(t *testing.T) {
	db := mustInitAndMigrate(t)
	mustCreateDoc(t, db, "a", "tdd", "draft", "x")
	mustCreateDoc(t, db, "b", "tdd", "approved", "x")
	mustCreateDoc(t, db, "c", "tdd", "draft", "x")

	summaries, total, err := ListDocsWithCounts(db, DocListOptions{Statuses: []string{"approved"}})
	if err != nil {
		t.Fatalf("ListDocsWithCounts: %v", err)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	if len(summaries) != 1 {
		t.Fatalf("len(summaries) = %d, want 1", len(summaries))
	}
	if summaries[0].Doc.Status != "approved" {
		t.Errorf("status = %q, want approved", summaries[0].Doc.Status)
	}
}

// --- GetDocRevision (TDD §3.3 Q1 semantics) ---

func TestGetDocRevision(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "first", "tdd", "draft", "v1")
	updateBody(t, db, id, "v2")
	updateBody(t, db, id, "v3")

	r, err := GetDocRevision(db, id, 2)
	if err != nil {
		t.Fatalf("GetDocRevision rev=2: %v", err)
	}
	if r.RevisionNumber != 2 {
		t.Errorf("RevisionNumber = %d, want 2", r.RevisionNumber)
	}
	if r.Body != "v2" {
		t.Errorf("Body = %q, want v2", r.Body)
	}

	// rev == 0 returns the current revision.
	r0, err := GetDocRevision(db, id, 0)
	if err != nil {
		t.Fatalf("GetDocRevision rev=0: %v", err)
	}
	if r0.RevisionNumber != 3 {
		t.Errorf("rev=0 RevisionNumber = %d, want 3 (current)", r0.RevisionNumber)
	}
}

func TestGetDocRevision_OutOfRange_ErrNotFound(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "first", "tdd", "draft", "v1")
	// Only revision 1 exists.
	_, err := GetDocRevision(db, id, 999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestGetDocRevision_NegativeRev_ErrValidation(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "first", "tdd", "draft", "v1")
	_, err := GetDocRevision(db, id, -1)
	if !errors.Is(err, ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}

func TestGetDocRevision_DocNotFound(t *testing.T) {
	db := mustInitAndMigrate(t)
	_, err := GetDocRevision(db, 999, 1)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// --- DeleteDoc + cascade behaviour (AC9) ---

func TestDeleteDoc_CascadesRevisions(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "first", "tdd", "draft", "v1")
	updateBody(t, db, id, "v2")

	if err := DeleteDoc(db, id, true); err != nil {
		t.Fatalf("DeleteDoc: %v", err)
	}

	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM doc_revisions WHERE doc_id = ?", id).Scan(&n); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	if n != 0 {
		t.Errorf("revisions remaining = %d, want 0", n)
	}
}

func TestDeleteDoc_CascadesLinks(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "first", "tdd", "draft", "v1")
	issueID := createTestIssue(t, db, "an issue", model.StatusTodo, model.PriorityMedium)

	if err := LinkDocIssue(db, docID, issueID); err != nil {
		t.Fatalf("LinkDocIssue: %v", err)
	}
	if err := DeleteDoc(db, docID, true); err != nil {
		t.Fatalf("DeleteDoc cascade: %v", err)
	}

	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM doc_issue_links WHERE doc_id = ?", docID).Scan(&n); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if n != 0 {
		t.Errorf("links remaining = %d, want 0", n)
	}
}

func TestDeleteDoc_NoCascade_BlocksOnLinks(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "first", "tdd", "draft", "v1")
	issueID := createTestIssue(t, db, "an issue", model.StatusTodo, model.PriorityMedium)

	if err := LinkDocIssue(db, docID, issueID); err != nil {
		t.Fatalf("LinkDocIssue: %v", err)
	}
	err := DeleteDoc(db, docID, false)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}

	// Doc must still exist.
	if _, err := GetDoc(db, docID); err != nil {
		t.Errorf("doc was unexpectedly deleted: %v", err)
	}
}

func TestDeleteDoc_NotFound(t *testing.T) {
	db := mustInitAndMigrate(t)
	err := DeleteDoc(db, 999, true)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// --- InsertDocWithID / InsertDocRevisionWithID round-trip helpers ---

func TestInsertDocWithID_RoundTrip(t *testing.T) {
	db := mustInitAndMigrate(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	d := &model.Doc{ID: 42, Type: "tdd", Status: "draft", Title: "imported", Body: "x", Author: "operator"}
	inserted, err := InsertDocWithID(tx, d)
	if err != nil {
		t.Fatalf("InsertDocWithID: %v", err)
	}
	if !inserted {
		t.Error("first call: inserted = false, want true")
	}
	// Second call with same ID: skipped.
	inserted2, err := InsertDocWithID(tx, d)
	if err != nil {
		t.Fatalf("InsertDocWithID 2: %v", err)
	}
	if inserted2 {
		t.Error("second call: inserted = true, want false")
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := GetDoc(db, 42)
	if err != nil {
		t.Fatalf("GetDoc(42): %v", err)
	}
	if got.Title != "imported" {
		t.Errorf("Title = %q, want imported", got.Title)
	}
}

func TestInsertDocRevisionWithID_RoundTrip(t *testing.T) {
	db := mustInitAndMigrate(t)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	d := &model.Doc{ID: 1, Type: "tdd", Status: "draft", Title: "t", Body: "b", Author: "a"}
	if _, err := InsertDocWithID(tx, d); err != nil {
		t.Fatalf("InsertDocWithID: %v", err)
	}
	r := &model.DocRevision{
		ID:             10,
		DocID:          1,
		RevisionNumber: 1,
		Body:           "b",
		ChangeKind:     "create",
		Author:         "a",
	}
	inserted, err := InsertDocRevisionWithID(tx, r)
	if err != nil {
		t.Fatalf("InsertDocRevisionWithID: %v", err)
	}
	if !inserted {
		t.Error("inserted = false, want true")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	revs, _ := ListDocRevisions(db, 1)
	if len(revs) != 1 || revs[0].ID != 10 {
		t.Errorf("revs[0] = %+v, want ID=10", revs[0])
	}
}

// --- ClearAllData covering BOTH new doc tables AND pre-existing proposals ---

func TestClearAllData_DropsProposalsAndDocs(t *testing.T) {
	db := mustInitAndMigrate(t)

	// Seed an issue, proposal, vote, proposal_issue link, doc, comment, link.
	issueID := createTestIssue(t, db, "i", model.StatusTodo, model.PriorityMedium)
	pID, err := CreateProposal(db, &model.Proposal{
		Description: "p", Criticality: model.CriticalityHigh,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5,
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if err := LinkProposalIssue(db, pID, issueID); err != nil {
		t.Fatalf("LinkProposalIssue: %v", err)
	}

	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "body")
	if _, err := CreateDocComment(db, &model.DocComment{DocID: docID, Body: "c", Author: "a"}); err != nil {
		t.Fatalf("CreateDocComment: %v", err)
	}
	if err := LinkDocIssue(db, docID, issueID); err != nil {
		t.Fatalf("LinkDocIssue: %v", err)
	}
	if err := LinkProposalDoc(db, pID, docID); err != nil {
		t.Fatalf("LinkProposalDoc: %v", err)
	}

	if err := ClearAllData(db); err != nil {
		t.Fatalf("ClearAllData: %v", err)
	}

	tables := []string{
		"docs", "doc_revisions", "doc_comments", "doc_issue_links", "proposal_docs",
		"proposals", "votes", "proposal_issues",
		"issues", "comments", "labels", "issue_labels", "issue_relations", "issue_files", "activity_log",
	}
	for _, table := range tables {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Errorf("counting %s: %v", table, err)
			continue
		}
		if n != 0 {
			t.Errorf("expected 0 rows in %s after ClearAllData, got %d", table, n)
		}
	}
}

func TestUpdateDoc_CombinedChangeKind_AllFields(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "initial title", "tdd", "draft", "body v1")

	newTitle := "second title"
	newType := "adr"
	newStatus := "approved"
	newBody := "body v2"

	rev, err := UpdateDoc(db, id, DocUpdate{
		Title:  &newTitle,
		Type:   &newType,
		Status: &newStatus,
		Body:   &newBody,
		Author: "editor",
	})
	if err != nil {
		t.Fatalf("UpdateDoc all-fields combined: %v", err)
	}
	if rev != 2 {
		t.Fatalf("rev = %d, want 2", rev)
	}

	revs, err := ListDocRevisions(db, id)
	if err != nil {
		t.Fatalf("ListDocRevisions: %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("len(revs) = %d, want 2", len(revs))
	}
	wantChangeKind := "title+type+status+body"
	if revs[1].ChangeKind != wantChangeKind {
		t.Errorf("ChangeKind = %q, want %q", revs[1].ChangeKind, wantChangeKind)
	}
	if revs[1].Body != newBody {
		t.Errorf("revs[1].Body = %q, want %q", revs[1].Body, newBody)
	}
}

func TestDeleteDoc_NoCascade_DeletesRevisionsAndComments(t *testing.T) {
	db := mustInitAndMigrate(t)
	id := mustCreateDoc(t, db, "first", "tdd", "draft", "v1")

	updateBody(t, db, id, "v2")
	newStatus := "approved"
	if _, err := UpdateDoc(db, id, DocUpdate{Status: &newStatus, Author: "editor"}); err != nil {
		t.Fatalf("UpdateDoc status: %v", err)
	}

	revsBefore, err := ListDocRevisions(db, id)
	if err != nil {
		t.Fatalf("ListDocRevisions: %v", err)
	}
	if len(revsBefore) < 3 {
		t.Fatalf("expected >=3 revisions before delete, got %d", len(revsBefore))
	}

	for _, body := range []string{"first comment", "second comment"} {
		if _, err := CreateDocComment(db, &model.DocComment{
			DocID:  id,
			Body:   body,
			Author: "tester",
		}); err != nil {
			t.Fatalf("CreateDocComment %q: %v", body, err)
		}
	}

	var nLinks int
	if err := db.QueryRow(
		`SELECT (SELECT COUNT(*) FROM doc_issue_links WHERE doc_id = ?) +
		        (SELECT COUNT(*) FROM proposal_docs   WHERE doc_id = ?)`,
		id, id,
	).Scan(&nLinks); err != nil {
		t.Fatalf("count external links: %v", err)
	}
	if nLinks != 0 {
		t.Fatalf("precondition: external link count = %d, want 0", nLinks)
	}

	if err := DeleteDoc(db, id, false); err != nil {
		t.Fatalf("DeleteDoc(cascade=false) with no external links: %v", err)
	}

	if _, err := GetDoc(db, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetDoc after delete: err = %v, want ErrNotFound", err)
	}

	var nRevs int
	if err := db.QueryRow("SELECT COUNT(*) FROM doc_revisions WHERE doc_id = ?", id).Scan(&nRevs); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	if nRevs != 0 {
		t.Errorf("revisions remaining = %d, want 0", nRevs)
	}

	var nComments int
	if err := db.QueryRow("SELECT COUNT(*) FROM doc_comments WHERE doc_id = ?", id).Scan(&nComments); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if nComments != 0 {
		t.Errorf("comments remaining = %d, want 0", nComments)
	}
}
