package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
)

// DKT-2063: a change track sized by the change `implement` actually recorded.
//
// The fixture is the corpus shape the issue names: a write step whose threshold
// interposes two review stages on the reserved `diff.*` family, and a `report`
// behind the heavier one by `after_fired`, so a skipped review cascades rather
// than running over an unreviewed object.
const diffRoutedSrc = `
[pipeline]
name = "diff-routed"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "implement"
emits = "change-summary"
threshold = { "review" = "any(diff.lines > 20)", "review-lite" = "all(diff.lines <= 20)" }

[[step]]
name = "review"
after = ["implement"]
executor = "review"
emits = "findings"

[[step]]
name = "review-lite"
after = ["implement"]
executor = "review"
emits = "findings"

[[step]]
name = "report"
after = ["review"]
after_fired = ["review"]
executor = "report"
emits = "report"
`

// diffBodyOf renders a unified diff over one file with the requested number of
// added and removed content lines, so a case states its size directly.
func diffBodyOf(added, removed int) string {
	var b strings.Builder
	b.WriteString("diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -1 +1 @@\n")
	for i := 0; i < added; i++ {
		fmt.Fprintf(&b, "+added %d\n", i)
	}
	for i := 0; i < removed; i++ {
		fmt.Fprintf(&b, "-removed %d\n", i)
	}
	return b.String()
}

// TestThresholdRoutesOnRecordedDiff is AC1 and AC2: `diff.*` predicates route a
// write step on the size of the diff its completion measured, and a step that
// recorded no diff evaluates as empty rather than as an error.
func TestThresholdRoutesOnRecordedDiff(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		fired   string
		skipped string
	}{
		{
			// The boundary itself: 20 changed lines satisfies `<= 20`.
			name: "at the boundary routes to the lighter review",
			body: diffBodyOf(20, 0), fired: "review-lite", skipped: "review",
		},
		{
			// 11 added + 10 removed. Counting added lines ALONE would read 11
			// and route to `review-lite`; `diff.lines` is added plus removed.
			name: "added plus removed crosses the boundary",
			body: diffBodyOf(11, 10), fired: "review", skipped: "review-lite",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			activateInterposed(t, conn, diffRoutedSrc)
			e := testEngine()
			e.DiffFn = func(_, _ string, _ []string) (string, error) {
				return tc.body, nil
			}

			claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

			if got := stepRouting(t, conn, "implement@0"); got != tc.fired {
				t.Fatalf("routing = %q, want %q for a %d-line diff",
					got, tc.fired, measureDiff(tc.body).Lines)
			}
			if got := stepStatus(t, conn, tc.skipped+"@0"); got != db.StepSkipped {
				t.Errorf("%s@0 = %q, want %q — the review the threshold did not "+
					"route to is skipped in the routing transaction",
					tc.skipped, got, db.StepSkipped)
			}
			if got := stepStatus(t, conn, tc.fired+"@0"); got != db.StepPending {
				t.Errorf("%s@0 = %q, want pending — the routed review is live",
					tc.fired, got)
			}
		})
	}

	// AC2: no recorded diff evaluates as `diff.empty == true`, `diff.lines == 0`
	// and `diff.files == 0`, so a review keyed on a non-empty change is skipped
	// and the skip cascades through `after_fired`.
	t.Run("an absent round record is empty, not an error", func(t *testing.T) {
		const emptySrc = `
[pipeline]
name = "diff-empty"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "implement"
emits = "change-summary"
threshold = { "review" = "any(diff.empty == false)" }

[[step]]
name = "review"
after = ["implement"]
executor = "review"
emits = "findings"

[[step]]
name = "report"
after = ["review"]
after_fired = ["review"]
executor = "report"
emits = "report"
`
		conn := mustDB(t)
		activateInterposed(t, conn, emptySrc)
		e := testEngine()
		e.DiffFn = func(_, _ string, _ []string) (string, error) { return "", nil }

		claimAndComplete(t, conn, e, "implement@0", "nothing to do", "")

		if got := stepRouting(t, conn, "implement@0"); got != RoutingPass {
			t.Fatalf("routing = %q, want %q — an absent round record must DECIDE "+
				"the predicate false, not park or error", got, RoutingPass)
		}
		if got := stepStatus(t, conn, "review@0"); got != db.StepSkipped {
			t.Errorf("review@0 = %q, want %q", got, db.StepSkipped)
		}
		if got := stepStatus(t, conn, "report@0"); got != db.StepSkipped {
			t.Errorf("report@0 = %q, want %q — the skip cascades through "+
				"after_fired", got, db.StepSkipped)
		}

		if facts := measureDiff(""); facts != (DiffFacts{Empty: true}) {
			t.Errorf("measureDiff(\"\") = %+v, want lines 0, files 0, empty true", facts)
		}
	})
}

// TestMeasureDiffCountsTheCumulativeDiffOnly pins the round-delta boundary: the
// trailer repeats this round's work under a second base, and counting it would
// double every loop round's size.
func TestMeasureDiffCountsTheCumulativeDiffOnly(t *testing.T) {
	body := diffBodyOf(3, 2) +
		roundDeltaMarker + " changes since abc — this round's work alone, unscoped ===\n" +
		diffBodyOf(3, 2)

	got := measureDiff(body)
	want := DiffFacts{Lines: 5, Files: 1}
	if got != want {
		t.Errorf("measureDiff = %+v, want %+v", got, want)
	}
}
