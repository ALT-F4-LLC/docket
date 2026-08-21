package cli

import "testing"

// DKT-113: the human renderers used to hard-cut the stored Request at the
// first newline with no marker, so `run status RUN-19` printed
// `Request: "# Original request"` and nothing else while --json carried the
// complete multi-paragraph text. The stored data was always complete; only
// the rendering summarizes, and now it says so.

func TestRequestSummary(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"single line", "fix the bug", "fix the bug"},
		{"trailing newline only", "fix the bug\n", "fix the bug"},
		{"multi-line names the cut",
			"# Original request\n\nParagraph one.\nParagraph two.",
			"# Original request … (+3 more lines; --json carries the full text)"},
		{"two lines",
			"first\nsecond",
			"first … (+1 more lines; --json carries the full text)"},
	}
	for _, tc := range cases {
		if got := requestSummary(tc.in); got != tc.want {
			t.Errorf("%s: requestSummary(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestFirstLineMarksTheCut(t *testing.T) {
	if got := firstLine("one line"); got != "one line" {
		t.Errorf("firstLine(single) = %q, want unchanged", got)
	}
	if got := firstLine("first\nrest"); got != "first …" {
		t.Errorf("firstLine(multi) = %q, want the first line plus a marker", got)
	}
	if got := firstLine("first\n"); got != "first" {
		t.Errorf("firstLine(trailing newline) = %q, want %q", got, "first")
	}
}
