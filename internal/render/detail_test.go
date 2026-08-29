package render

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"os"
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

	out := RenderDetail(issue, nil, nil, nil, nil, nil, nil)

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

	out := RenderDetail(issue, nil, nil, nil, nil, nil, nil)

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
	out := RenderDetail(issue, nil, nil, nil, nil, nil, nil)
	if strings.Contains(out, "Linked Docs") {
		t.Errorf("empty docs should omit section:\n%s", out)
	}
}

func unsetNoColor(t *testing.T) {
	t.Helper()
	old, ok := os.LookupEnv("NO_COLOR")
	if err := os.Unsetenv("NO_COLOR"); err != nil {
		t.Fatalf("Unsetenv(NO_COLOR): %v", err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv("NO_COLOR", old)
		} else {
			_ = os.Unsetenv("NO_COLOR")
		}
	})
}

func TestRenderDetail_StyledLinkedDocsUsesArrowGlyph(t *testing.T) {
	unsetNoColor(t)
	t.Setenv("TERM", "xterm-256color")
	issue := issueWithDocs([]model.DocRef{
		{ID: 3, Type: "tdd", Status: "approved", Title: "Docket Doc CLI"},
	})

	out := RenderDetail(issue, nil, nil, nil, nil, nil, nil)

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

// TestRenderDetail_PlainCarriesTheResolution closes half of DKT-245 that only
// the styled renderer had.
//
// DKT-245's own comment says `issue show` prints the status AND the
// resolution; renderPlainDetail printed only the status. NO_COLOR is what
// every piped reader sees — CI, and an agent shelling out to `docket issue
// show` — so the one surface that dropped the marker was the one read by the
// readers DKT-404 caught misreading abandoned issues as pending.
func TestRenderDetail_PlainCarriesTheResolution(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	issue := makeTestIssue(1, "Issue", model.StatusReview, model.PriorityHigh,
		model.IssueKindFeature, nil)
	issue.Resolution = model.ResolutionAbandoned

	out := RenderDetail(issue, nil, nil, nil, nil, nil, nil)

	if !strings.Contains(out, model.ResolutionAbandoned) {
		t.Errorf("plain detail drops the resolution; the row reads as an "+
			"ordinary issue in review:\n%s", out)
	}
	// The STATUS is still there. The two are one fact each and the detail view
	// has room for both — dropping either is what sends a reader to the wrong
	// conclusion.
	if !strings.Contains(out, string(model.StatusReview)) {
		t.Errorf("plain detail drops the status:\n%s", out)
	}
}

// TestRenderDetail_RunDispositionIsAboveTheDescription is DKT-404's placement
// argument: a reader who stops at the top of this view is exactly the reader
// who misreads a frozen status, so the ruling sits with the metadata that
// frames it rather than below a description and a comment thread.
func TestRenderDetail_RunDispositionIsAboveTheDescription(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	issue := makeTestIssue(1, "Issue", model.StatusTodo, model.PriorityHigh,
		model.IssueKindFeature, nil)
	issue.Description = "the description"

	out := RenderDetail(issue, nil, nil, nil, nil, nil, &model.IssueRunDisposition{
		RunID: 14, Disposition: "abandoned", By: "implement@0",
		Reason: "pin drift\nre-planned as HRN-300", AtMS: 1755000000000,
	})

	want := []string{
		RunDispositionHeading,
		"work abandoned in RUN-14 by implement@0 (2025-08-12)",
		"reason: pin drift",
		// The ruling VERBATIM: a second line is a second line, not a "…".
		"re-planned as HRN-300",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("plain detail missing %q:\n%s", w, out)
		}
	}
	if disposition, desc := strings.Index(out, RunDispositionHeading),
		strings.Index(out, "Description"); disposition > desc {
		t.Errorf("the run disposition renders below the description "+
			"(%d > %d):\n%s", disposition, desc, out)
	}
}
