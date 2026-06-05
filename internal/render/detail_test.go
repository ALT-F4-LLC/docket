package render

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

func issueWithDocs(docs []model.DocRef) *model.Issue {
	i := makeTestIssue(1, "Issue", model.StatusTodo, model.PriorityHigh, model.IssueKindFeature, nil)
	i.Docs = docs
	return i
}

func TestRenderDetail_PlainLinkedDocsAfterFilesBeforeDescription(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	issue := issueWithDocs([]model.DocRef{
		{ID: 3, Type: "tdd", Status: "approved", Title: "Docket Doc CLI"},
	})
	issue.Files = []string{"internal/db/doc_links.go"}
	issue.Description = "the description"

	out := RenderDetail(issue, nil, nil, nil, nil)

	if !strings.Contains(out, "\nLinked Docs\n") {
		t.Fatalf("missing Linked Docs header:\n%s", out)
	}
	if !strings.Contains(out, "  > DOC-3   tdd   approved   Docket Doc CLI") {
		t.Errorf("plain doc line wrong:\n%s", out)
	}
	files := strings.Index(out, "Files")
	docs := strings.Index(out, "Linked Docs")
	desc := strings.Index(out, "Description")
	if !(files < docs && docs < desc) {
		t.Errorf("section order wrong: Files=%d Linked Docs=%d Description=%d", files, docs, desc)
	}
}

func TestRenderDetail_PlainLinkedDocsAligned(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	issue := issueWithDocs([]model.DocRef{
		{ID: 3, Type: "tdd", Status: "approved", Title: "Alpha"},
		{ID: 100, Type: "ux", Status: "draft", Title: "Beta"},
	})

	out := RenderDetail(issue, nil, nil, nil, nil)

	wantLines := []string{
		"  > DOC-3     tdd   approved   Alpha",
		"  > DOC-100   ux    draft      Beta",
	}
	for _, w := range wantLines {
		if !strings.Contains(out, w) {
			t.Errorf("missing aligned line %q:\n%s", w, out)
		}
	}
}

func TestRenderDetail_PlainOmitsLinkedDocsWhenEmpty(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	issue := issueWithDocs(nil)
	out := RenderDetail(issue, nil, nil, nil, nil)
	if strings.Contains(out, "Linked Docs") {
		t.Errorf("empty docs should omit section:\n%s", out)
	}
}

func TestRenderDetail_StyledLinkedDocsUsesArrowGlyph(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	issue := issueWithDocs([]model.DocRef{
		{ID: 3, Type: "tdd", Status: "approved", Title: "Docket Doc CLI"},
	})

	out := RenderDetail(issue, nil, nil, nil, nil)

	if !strings.Contains(out, "Linked Docs") {
		t.Fatalf("missing Linked Docs header:\n%s", out)
	}
	if !strings.Contains(out, "▸") {
		t.Errorf("styled output missing ▸ glyph:\n%s", out)
	}
	if strings.Contains(out, "  > DOC-3") {
		t.Errorf("styled output used plain > prefix:\n%s", out)
	}
	for _, want := range []string{"DOC-3", "tdd", "approved", "Docket Doc CLI"} {
		if !strings.Contains(out, want) {
			t.Errorf("styled output missing %q:\n%s", want, out)
		}
	}
}
