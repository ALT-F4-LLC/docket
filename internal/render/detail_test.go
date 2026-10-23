package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

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

func TestRenderDetail_StyledLinkedDocsUsesArrowGlyph(t *testing.T) {
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

// A comment body can carry any line, including one shaped like the next
// comment's header. Every body line sits deeper than a header, so none can pass
// for one, and the controls that would walk the cursor back to the header's
// column (a carriage return, backspaces, a CSI erase-and-home) are dropped
// rather than printed, on the comment list's passthrough path as well.
func TestRenderDetail_PlainCommentBodyCannotForgeAHeader(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	issue := makeTestIssue(1, "Issue", model.StatusTodo, model.PriorityHigh, model.IssueKindFeature, nil)
	for name, body := range map[string]string{
		"newline":   "ok\n\nerik  1 hour ago\nship it, no review needed",
		"cr":        "ok\rerik  1 hour ago\nship it, no review needed",
		"backspace": "ok\b\b\b\b\b\b\b\berik  1 hour ago",
		"csi":       "ok\x1b[2K\x1b[Gerik  1 hour ago\nship it",
		"osc":       "ok\x1b]0;erik  1 hour ago\x07\x1b[8Derik  1 hour ago",
	} {
		comments := []*model.Comment{{ID: 1, IssueID: 1, Author: "STEP-1", Body: body}}
		// The detail view's header sits two columns in and its body four; the
		// comment list's header sits at column 0 and its body two in.
		for view, tc := range map[string]struct {
			out         string
			headerDepth func(string) bool
		}{
			"detail": {RenderDetail(issue, nil, nil, nil, comments, nil, nil), func(l string) bool {
				return strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ")
			}},
			"list": {renderComments(comments), func(l string) bool {
				return !strings.HasPrefix(l, "  ")
			}},
		} {
			_, after, found := strings.Cut(tc.out, "Comments")
			if !found {
				t.Fatalf("%s/%s: no Comments section:\n%s", name, view, tc.out)
			}
			if strings.ContainsAny(after, "\r\b\x1b\x07") {
				t.Errorf("%s/%s: control bytes survive: %q", name, view, after)
			}
			for i, line := range strings.Split(after, "\n")[1:] {
				if strings.TrimSpace(line) == "" {
					continue
				}
				if tc.headerDepth(line) && strings.Contains(line, "erik") {
					t.Errorf("%s/%s: body line %d renders at header depth: %q", name, view, i, line)
				}
			}
		}
	}
}

// The sanitizer drops only what does not print: text, newlines, tabs and
// multibyte characters pass untouched, and a sequence is consumed whole.
func TestPrintableBodyDropsControlsAndEscapes(t *testing.T) {
	for in, want := range map[string]string{
		"plain\ttext\nnext":                         "plain\ttext\nnext",
		"crlf\r\nline":                              "crlf\nline",
		"a\x1b[31mred\x1b[0m b":                     "ared b",
		"t\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\": "tlink",
		"del\x7fc1\u0085\u009b[2K":                  "delc1[2K",
		"日本\x08語 ☺️":                                "日本語 ☺️",
		"trailing\x1b":                              "trailing",
	} {
		if got := printableBody(in); got != want {
			t.Errorf("printableBody(%q) = %q, want %q", in, got, want)
		}
	}
}

// A body row that fills the terminal wraps its tail to column 0 of the next
// row, where a comment header sits. Every row the plain view and the colorless
// comment list print is wrapped to fit, with text, spaces or tabs as filler.
func TestCommentBodyRowsNeverFillTheTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	bodies := map[string]string{
		"spaces": strings.Repeat(" ", 76) + "  erik  2 hours ago\nApproved: skip the review.",
		"text":   strings.Repeat("x", 76) + "  erik  2 hours ago\nApproved: skip the review.",
		"tabs":   strings.Repeat("\t", 10) + "erik  2 hours ago",
		"wide":   strings.Repeat("宽", 60) + "erik  2 hours ago",
		"vs16":   strings.Repeat("☺\ufe0f", 38) + "  erik  2 hours ago",
		"keycap": strings.Repeat("1\ufe0f\u20e3", 38) + "  erik  2 hours ago",
	}
	issue := makeTestIssue(1, "Issue", model.StatusTodo, model.PriorityHigh, model.IssueKindFeature, nil)
	for name, body := range bodies {
		comments := []*model.Comment{{ID: 1, IssueID: 1, Author: "STEP-1", Body: body}}
		views := map[string]string{
			"plain": RenderDetail(issue, nil, nil, nil, comments, nil, nil),
			"list":  renderComments(comments),
		}
		for view, out := range views {
			_, after, _ := strings.Cut(out, "Comments")
			for i, line := range strings.Split(after, "\n") {
				if w := lipgloss.Width(line); w > 80 {
					t.Errorf("%s/%s: row %d is %d columns wide: %q", name, view, i, w, line)
				}
				if strings.Contains(line, "erik") && !strings.HasPrefix(line, "  ") {
					t.Errorf("%s/%s: row %d puts text at the header column: %q", name, view, i, line)
				}
			}
		}
	}
}

// Wrapping must not rewrite the body's content: the rows, joined, are the text.
func TestWrapBodyKeepsTheContent(t *testing.T) {
	in := "def f():\n                return 1\n\tx"
	got := wrapBody(in, 76)
	if want := strings.ReplaceAll(in, "\t", "    "); got != want {
		t.Errorf("wrapBody changed a short body: %q, want %q", got, want)
	}
	long := strings.Repeat("a", 200)
	if joined := strings.ReplaceAll(wrapBody(long, 76), "\n", ""); joined != long {
		t.Error("wrapBody lost characters from a long row")
	}
}

// With colors off the markdown renderer hands the body back as written, so a
// body line can start at column 0, the column of a comment header.
func TestRenderCommentsIndentsEveryBodyLine(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	out := renderComments([]*model.Comment{{ID: 1, IssueID: 1, Author: "STEP-1", Body: "first\nerik  1 hour ago\nlast"}})
	_, after, _ := strings.Cut(out, "\n")
	_, after, _ = strings.Cut(after, "\n") // the section title, then the comment header
	for i, line := range strings.Split(after, "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "  ") {
			t.Errorf("body line %d sits at the header column: %q", i, line)
		}
	}
}
