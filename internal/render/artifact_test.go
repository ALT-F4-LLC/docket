package render

import (
	"strings"
	"testing"
)

// Artifact rendering — the human half of the read surface.
//
// A rendered frame cannot be reviewed from a diff, which is why render-verify
// requires a test beside every file exporting Render*. These pin the two
// properties that make the output usable rather than merely present: the
// listing never carries a body, and the fetch never alters one.

func TestRenderStepArtifactsReportsSizesNotBodies(t *testing.T) {
	rows := []StepArtifactRow{
		{
			Artifact: "ARTIFACT-3", Kind: "findings",
			Bytes: 8204, PayloadBytes: 2685,
			SHA256: "61bafd433ebd9c1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a",
		},
		{
			Artifact: "ARTIFACT-4", Kind: "issue.diff",
			Bytes: 6912, PayloadBytes: 0,
			SHA256: "b83d2df7fcd21122334455667788990011223344556677889900aabbccddeeff",
		},
	}

	got := RenderStepArtifacts("STEP-4", rows)

	for _, want := range []string{"ARTIFACT-3", "ARTIFACT-4", "findings", "issue.diff"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendering omits %q:\n%s", want, got)
		}
	}
	// Sizes are the point of the listing — they are how an operator decides
	// which artifact to fetch.
	if !strings.Contains(got, "8204") || !strings.Contains(got, "2685") {
		t.Errorf("rendering omits the sizes:\n%s", got)
	}
	// An artifact with no payload reads as "-", not as a bare 0, so "absent"
	// and "empty" stay distinguishable at a glance.
	if !strings.Contains(got, "-") {
		t.Errorf("an artifact with no payload should render a dash:\n%s", got)
	}
	// The next command is named, so the listing is a step in a workflow rather
	// than a dead end.
	if !strings.Contains(got, "docket step artifact ARTIFACT-3") {
		t.Errorf("rendering does not name how to read one in full:\n%s", got)
	}
}

// TestRenderStepArtifactsMarksStubs keeps a stub from reading as a result.
//
// A stub artifact records that a computation did NOT run. Rendering it
// identically to a real one would make an absent result look like a present
// one, which is the confusion db.Artifact.Stub exists to prevent.
func TestRenderStepArtifactsMarksStubs(t *testing.T) {
	got := RenderStepArtifacts("STEP-9", []StepArtifactRow{
		{Artifact: "ARTIFACT-1", Kind: "findings", Bytes: 12, Stub: true, SHA256: "abc123"},
	})

	if !strings.Contains(got, "stub") {
		t.Errorf("a stub artifact is not marked as one:\n%s", got)
	}
}

// TestRenderStepArtifactsEmptyStateIsNotAFailure pins the wording for a step
// that produced nothing. Many steps legitimately produce no artifact, and an
// empty state that reads as an error sends an operator hunting for a problem
// that is not there.
func TestRenderStepArtifactsEmptyState(t *testing.T) {
	got := RenderStepArtifacts("STEP-7", nil)

	if !strings.Contains(got, "STEP-7") {
		t.Errorf("the empty state does not name the step:\n%s", got)
	}
	if !strings.Contains(got, "not a failure") {
		t.Errorf("the empty state should say producing nothing is ordinary:\n%s", got)
	}
}

// TestRenderArtifactPrintsTheBodyVerbatim is the property that makes the fetch
// usable in a pipeline.
//
// The body must survive rendering BYTE FOR BYTE — no wrapping, no indenting,
// no truncation — because an operator redirects it to a file or pipes it
// onward. A renderer that reflowed it would silently corrupt a diff or a
// patch.
func TestRenderArtifactPrintsTheBodyVerbatim(t *testing.T) {
	const body = "line one\n    indented two\n\nline four with trailing spaces   \n"

	got := RenderArtifact("ARTIFACT-3", "findings", "review@0#2", "abc123", body, "")

	if !strings.Contains(got, body) {
		t.Errorf("the body was altered in rendering.\nwant to contain:\n%q\ngot:\n%q",
			body, got)
	}
	// The metadata goes ABOVE the body, so it never interleaves with content
	// a caller is capturing.
	bodyAt := strings.Index(got, "--- body ---")
	shaAt := strings.Index(got, "abc123")
	if bodyAt < 0 || shaAt < 0 || shaAt > bodyAt {
		t.Errorf("metadata must precede the body:\n%s", got)
	}
}

func TestRenderArtifactShowsPayloadWhenPresent(t *testing.T) {
	const payload = `[{"id":"C-1","severity":"low"}]`

	got := RenderArtifact("ARTIFACT-3", "findings", "review@0#2", "abc123", "the body", payload)
	if !strings.Contains(got, payload) {
		t.Errorf("the payload is missing:\n%s", got)
	}

	// An artifact with no payload renders no payload section at all, rather
	// than an empty one that reads as a payload that exists and is blank.
	without := RenderArtifact("ARTIFACT-4", "issue.diff", "", "def456", "the body", "")
	if strings.Contains(without, "--- payload ---") {
		t.Errorf("an artifact with no payload rendered a payload section:\n%s", without)
	}
	// With no producing step, no producer line is invented.
	if strings.Contains(without, "producer:") {
		t.Errorf("a run-scoped artifact rendered a producer:\n%s", without)
	}
}

func TestShortHashLeavesShortValuesAlone(t *testing.T) {
	if got := shortHash("abc123"); got != "abc123" {
		t.Errorf("shortHash(abc123) = %q, want it unchanged", got)
	}
	long := "61bafd433ebd9c1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a"
	if got := shortHash(long); len(got) != 12 || !strings.HasPrefix(long, got) {
		t.Errorf("shortHash(%q) = %q, want a 12-char prefix", long, got)
	}
}
