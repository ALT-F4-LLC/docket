package render

import (
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

func makeTestDoc(id int, title, docType, status, author string) *model.Doc {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &model.Doc{
		ID:        id,
		Type:      docType,
		Status:    status,
		Title:     title,
		Body:      "",
		Author:    author,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func makeTestRevision(docID, number int, kind, author string) *model.DocRevision {
	return &model.DocRevision{
		ID:             number,
		DocID:          docID,
		RevisionNumber: number,
		Body:           "rev body",
		ChangeKind:     kind,
		Author:         author,
		CreatedAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestRenderDocList_Empty(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	tests := []struct {
		name string
		rows []DocRow
	}{
		{name: "nil", rows: nil},
		{name: "empty", rows: []DocRow{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderDocList(tt.rows)
			if !strings.Contains(got, "No documents found.") {
				t.Errorf("expected empty-state message, got:\n%s", got)
			}
			if !strings.Contains(got, "docket doc create") {
				t.Errorf("expected create hint in empty state, got:\n%s", got)
			}
		})
	}
}

func TestRenderDocList_PlainColumnsAndRows(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	rows := []DocRow{
		{Doc: makeTestDoc(1, "Add docket doc CLI", "tdd", "draft", "Erik"), CurrentRevision: 3, RevisionsCount: 3},
		{Doc: makeTestDoc(2, "Vote weighting", "adr", "approved", "Alex"), CurrentRevision: 1, RevisionsCount: 1},
	}

	got := RenderDocList(rows)

	headers := []string{"ID", "Type", "Status", "Title", "Author", "Revisions", "Updated"}
	for _, h := range headers {
		if !strings.Contains(got, h) {
			t.Errorf("expected header %q in output, got:\n%s", h, got)
		}
	}

	for _, id := range []string{"DOC-1", "DOC-2"} {
		if !strings.Contains(got, id) {
			t.Errorf("expected %s in output, got:\n%s", id, got)
		}
	}

	for _, val := range []string{"tdd", "adr", "draft", "approved", "Erik", "Alex"} {
		if !strings.Contains(got, val) {
			t.Errorf("expected %q in output, got:\n%s", val, got)
		}
	}
}

func TestRenderDocList_TitleTruncated(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	long := strings.Repeat("A", 80)
	rows := []DocRow{
		{Doc: makeTestDoc(1, long, "tdd", "draft", "Erik"), CurrentRevision: 1, RevisionsCount: 1},
	}

	got := RenderDocList(rows)

	if strings.Contains(got, long) {
		t.Errorf("expected long title to be truncated, but full title present in output:\n%s", got)
	}
	if !strings.Contains(got, "...") {
		t.Errorf("expected truncated title to contain ellipsis, got:\n%s", got)
	}
}

func TestRenderDocList_ColorPathExecutes(t *testing.T) {
	rows := []DocRow{
		{Doc: makeTestDoc(1, "Color", "tdd", "draft", "Erik"), CurrentRevision: 1, RevisionsCount: 1},
	}
	got := RenderDocList(rows)
	if got == "" {
		t.Error("expected non-empty output from color list render")
	}
}

func TestRenderDocDetail_PlainAllSections(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	doc := makeTestDoc(42, "Add docket doc CLI", "tdd", "draft", "Erik")
	doc.Body = "doc body content"
	revisions := []*model.DocRevision{
		makeTestRevision(42, 1, "create", "Erik"),
		makeTestRevision(42, 2, "status", "vote-bot"),
	}
	comments := []*model.DocComment{
		{ID: 1, DocID: 42, Body: "Looks good", Author: "Alex", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	linkedIssues := []int{12}
	linkedProposals := []int{3}

	got := RenderDocDetail(doc, revisions, comments, linkedIssues, linkedProposals)

	wantSubstrings := []string{
		"DOC-42",
		"Add docket doc CLI",
		"tdd",
		"draft",
		"Erik",
		"Body",
		"doc body content",
		"Linked Issues",
		model.FormatID(12),
		"Linked Proposals",
		model.FormatProposalID(3),
		"Comments",
		"Looks good",
		"Revisions",
		"r1",
		"create",
		"r2",
		"status",
		"vote-bot",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in detail output, got:\n%s", want, got)
		}
	}
}

func TestRenderDocDetail_OmitsEmptySections(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	doc := makeTestDoc(7, "Minimal", "adr", "approved", "Erik")

	got := RenderDocDetail(doc, nil, nil, nil, nil)

	wantPresent := []string{"DOC-7", "Minimal", "adr", "approved", "Erik"}
	for _, w := range wantPresent {
		if !strings.Contains(got, w) {
			t.Errorf("expected %q in detail output, got:\n%s", w, got)
		}
	}

	wantAbsent := []string{"Body", "Linked Issues", "Linked Proposals", "Comments", "Revisions"}
	for _, w := range wantAbsent {
		if strings.Contains(got, w) {
			t.Errorf("expected %q NOT in detail output when section empty, got:\n%s", w, got)
		}
	}
}

func TestRenderDocDetail_AnonymousComment(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	doc := makeTestDoc(1, "Doc", "tdd", "draft", "Erik")
	comments := []*model.DocComment{
		{ID: 1, DocID: 1, Body: "no author", Author: "", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	got := RenderDocDetail(doc, nil, comments, nil, nil)

	if !strings.Contains(got, "anonymous") {
		t.Errorf("expected anonymous author fallback, got:\n%s", got)
	}
}

func TestRenderDocDetail_ColorPathExecutes(t *testing.T) {
	doc := makeTestDoc(1, "Color", "tdd", "draft", "Erik")
	doc.Body = "body"
	revisions := []*model.DocRevision{makeTestRevision(1, 1, "create", "Erik")}
	comments := []*model.DocComment{
		{ID: 1, DocID: 1, Body: "c", Author: "Alex", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	got := RenderDocDetail(doc, revisions, comments, []int{2}, []int{3})
	if got == "" {
		t.Error("expected non-empty output from color detail render")
	}
}

func TestRenderDocRevisionHistory_Empty(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	got := RenderDocRevisionHistory(nil)
	if !strings.Contains(got, "No revisions yet.") {
		t.Errorf("expected empty-state message, got:\n%s", got)
	}

	got = RenderDocRevisionHistory([]*model.DocRevision{})
	if !strings.Contains(got, "No revisions yet.") {
		t.Errorf("expected empty-state message for empty slice, got:\n%s", got)
	}
}

func TestRenderDocRevisionHistory_PlainOrdering(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	revisions := []*model.DocRevision{
		makeTestRevision(1, 1, "create", "Erik"),
		makeTestRevision(1, 2, "body", "Erik"),
		makeTestRevision(1, 3, "status", "vote-bot"),
	}

	got := RenderDocRevisionHistory(revisions)

	for _, rev := range []string{"r1", "r2", "r3"} {
		if !strings.Contains(got, rev) {
			t.Errorf("expected %q in revision history, got:\n%s", rev, got)
		}
	}
	for _, kind := range []string{"create", "body", "status"} {
		if !strings.Contains(got, kind) {
			t.Errorf("expected change_kind %q in revision history, got:\n%s", kind, got)
		}
	}

	idx1 := strings.Index(got, "r1")
	idx2 := strings.Index(got, "r2")
	idx3 := strings.Index(got, "r3")
	if !(idx1 >= 0 && idx2 > idx1 && idx3 > idx2) {
		t.Errorf("expected revisions rendered in input order, got positions r1=%d r2=%d r3=%d", idx1, idx2, idx3)
	}
}

func TestRenderDocRevisionHistory_EmptyAuthorFallsBackToSystem(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	revisions := []*model.DocRevision{
		makeTestRevision(1, 1, "create", ""),
	}

	got := RenderDocRevisionHistory(revisions)

	if !strings.Contains(got, "system") {
		t.Errorf("expected 'system' fallback for empty author, got:\n%s", got)
	}
}

func TestRenderDocRevisionHistory_ColorPathExecutes(t *testing.T) {
	revisions := []*model.DocRevision{
		makeTestRevision(1, 1, "create", "Erik"),
		makeTestRevision(1, 2, "status", "vote-bot"),
	}

	got := RenderDocRevisionHistory(revisions)
	if got == "" {
		t.Error("expected non-empty output from color revision history render")
	}
}
