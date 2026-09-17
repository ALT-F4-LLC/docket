package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
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

// TestMeasureDiffCountsTheInScopeCumulativeDiffOnly pins both trailer
// boundaries: each repeats or adds hunks after the object a review reads, and
// counting either would size this issue's change by bytes it does not claim.
func TestMeasureDiffCountsTheInScopeCumulativeDiffOnly(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			// The loop re-entry trailer: the same work again under a second base.
			name: "round delta",
			body: diffBodyOf(3, 2) +
				"\n" + roundDeltaMarker + " changes since abc — this round's work alone, unscoped ===\n" +
				diffBodyOf(3, 2),
		},
		{
			// The out-of-scope trailer: hunks disclosed as evidence and
			// explicitly not claimed as this issue's change.
			name: "out of scope",
			body: diffBodyOf(3, 2) +
				outOfScopeMarker + " their hunks follow (DKT-86) ===\n" +
				diffBodyOf(9, 9),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := DiffFacts{Lines: 5, Files: 1}
			if got := measureDiff(tc.body); got != want {
				t.Errorf("measureDiff = %+v, want %+v", got, want)
			}
		})
	}
}

// evalDiff evaluates one predicate against a measurement, through the exported
// seam a routing step uses.
func evalDiff(
	t *testing.T, facts DiffFacts, predicate string,
) (ThresholdResult, error) {
	t.Helper()
	threshold := map[string]string{workflow.OnFailFixLoop: predicate}
	return EvaluateThresholdOverDiff(
		"implement@0", threshold, ThresholdOrder(threshold), nil, nil, &facts)
}

// TestDiffFieldsEvaluateAgainstTheMeasurement covers the family's remaining
// surface: `diff.files`, equality on a count, and the refusals a predicate that
// cannot mean anything must produce instead of a silent non-match.
func TestDiffFieldsEvaluateAgainstTheMeasurement(t *testing.T) {
	facts := DiffFacts{Lines: 40, Files: 3}

	matches := []struct {
		predicate string
		want      bool
	}{
		{"any(diff.files >= 2)", true},
		{"any(diff.files > 3)", false},
		{"all(diff.files == 3)", true},
		{"any(diff.files != 3)", false},
		{"any(diff.lines >= 40)", true},
		{"any(diff.lines < 40)", false},
		{"any(diff.empty == false)", true},
		{"any(diff.empty == true)", false},
		{"any(diff.empty != true)", true},
	}
	for _, tc := range matches {
		result, err := evalDiff(t, facts, tc.predicate)
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.predicate, err)
			continue
		}
		routed := result.Routing == workflow.OnFailFixLoop
		if routed != tc.want {
			t.Errorf("%s over %+v routed %q, want matched=%v",
				tc.predicate, facts, result.Routing, tc.want)
		}
		if result.Parked {
			t.Errorf("%s parked; a diff.* comparison is over integers the "+
				"engine measured and has no order to guess", tc.predicate)
		}
	}

	// A literal that is not a count, and an order asked of a boolean, are
	// DECLARATION errors: the engine knows the comparison perfectly and the
	// author wrote something it cannot mean. Refusing names the predicate; a
	// park would ask an operator to resolve a typo.
	refusals := []string{
		"any(diff.lines > twenty)",
		"any(diff.files == none)",
		"any(diff.empty == maybe)",
		"any(diff.empty > 1)",
	}
	for _, predicate := range refusals {
		if _, err := evalDiff(t, facts, predicate); err == nil {
			t.Errorf("%s evaluated without error, want a refusal naming the "+
				"predicate", predicate)
		}
	}
}

// TestDiffFieldsAreOrdinaryWithoutAMeasurement is AC3's structural half: on a
// step that holds no tree there are no facts, and `diff.lines` is an unknown
// payload field evaluated exactly as any other — which for an ordered operator
// with no registered schema is T3's park, unchanged.
func TestDiffFieldsAreOrdinaryWithoutAMeasurement(t *testing.T) {
	threshold := map[string]string{workflow.OnFailFixLoop: "any(diff.lines > 20)"}
	result, err := EvaluateThreshold(
		"read@0", threshold, ThresholdOrder(threshold),
		[]map[string]any{{"other": "value"}}, nil)
	testsupport.Must(t, err, "EvaluateThreshold: %v", err)

	if !result.Parked || result.Routing != workflow.OnFailWaitingHuman {
		t.Errorf("routing = %q parked = %v, want the unchanged T3 park — with no "+
			"measurement `diff.lines` is an ordinary undeclared field",
			result.Routing, result.Parked)
	}
}
