package engine

import (
	"database/sql"
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

// TestRetryMeasuresTheRecordedChange is DKT-2548: when the DKT-259 guard drops
// an empty re-record because the issue already holds a non-empty `issue.diff`,
// the `diff.*` facts routing evaluates are measured from THAT recorded body —
// the object `issue.diff` resolves to and a review would read — not from the
// empty body the retry computed. Measuring the computed body decided
// `any(diff.empty == false)` false on every `--as retry` and skipped review
// over a change the ledger still held.
//
// The two boundaries pin the rule's narrowness: a FIRST empty diff has no
// recorded change to fall back to and still reads empty, and a real revision
// is measured from itself, never from the older record.
func TestRetryMeasuresTheRecordedChange(t *testing.T) {
	// recorded is the change the issue already holds: 25 content lines, 1 file.
	recorded := diffBodyOf(15, 10)

	src := func(predicate string) string {
		return fmt.Sprintf(`
[pipeline]
name = "diff-retry"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "implement"
emits = "change-summary"
threshold = { "review" = %q }

[[step]]
name = "review"
after = ["implement"]
executor = "review"
emits = "findings"
`, predicate)
	}

	recordDiff := func(t *testing.T, conn *sql.DB, runID int, body string) {
		t.Helper()
		// Resolved before the transaction opens: the pool holds one connection,
		// and a query inside the transaction deadlocks against it.
		stepID := stepIDByInstance(t, conn, "implement@0")
		tx, err := conn.Begin()
		testsupport.Must(t, err, "Begin: %v", err)
		_, err = db.InsertArtifactTx(tx, db.Artifact{
			RunID: runID, StepID: stepID, Kind: ArtifactKindIssueDiff, Body: body,
		}, nowMS)
		testsupport.Must(t, err, "InsertArtifactTx: %v", err)
		testsupport.Must(t, tx.Commit(), "Commit: %v", err)
	}

	cases := []struct {
		name      string
		predicate string
		// prior is what the issue already recorded; "" records nothing.
		prior string
		// computed is what this completion's diff came back as.
		computed   string
		wantReview bool
		// wantRecords is the issue.diff artifact count after the completion.
		wantRecords int
	}{
		{
			name:      "an empty re-record reads the recorded change as non-empty",
			predicate: "any(diff.empty == false)",
			prior:     recorded, computed: "", wantReview: true, wantRecords: 1,
		},
		{
			name:      "an empty re-record sizes lines from the recorded body",
			predicate: "any(diff.lines == 25)",
			prior:     recorded, computed: "", wantReview: true, wantRecords: 1,
		},
		{
			name:      "an empty re-record sizes files from the recorded body",
			predicate: "any(diff.files == 1)",
			prior:     recorded, computed: "", wantReview: true, wantRecords: 1,
		},
		{
			name:      "the empty computed body is not what routing measures",
			predicate: "any(diff.lines == 0)",
			prior:     recorded, computed: "", wantReview: false, wantRecords: 1,
		},
		{
			// DKT-259's narrowness: no recorded change to protect, so a genuine
			// "nothing changed" reads empty. No row is written either, and that
			// is the byte-identical guard, not this one: with nothing recorded
			// the newest body is "" and so is the computed one. That collides
			// with DKT-259's own stated intent that a first empty diff records;
			// the count here pins the behavior as found so a fix to that guard
			// shows up as this row changing, not as a silent drift.
			name:      "a first empty diff reads empty",
			predicate: "any(diff.empty == false)",
			prior:     "", computed: "", wantReview: false, wantRecords: 0,
		},
		{
			// An ordinary revision: measured from the diff it computed.
			name:      "a real revision is measured from itself",
			predicate: "any(diff.lines == 3)",
			prior:     recorded, computed: diffBodyOf(3, 0), wantReview: true, wantRecords: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			runID, _ := activateInterposed(t, conn, src(tc.predicate))
			if tc.prior != "" {
				recordDiff(t, conn, runID, tc.prior)
			}
			e := testEngine()
			e.DiffFn = func(_, _ string, _ []string) (string, error) {
				return tc.computed, nil
			}

			claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

			want := RoutingPass
			if tc.wantReview {
				want = "review"
			}
			if got := stepRouting(t, conn, "implement@0"); got != want {
				t.Errorf("routing = %q, want %q for %s with prior %d-line record "+
					"and computed %d-line diff", got, want, tc.predicate,
					measureDiff(tc.prior).Lines, measureDiff(tc.computed).Lines)
			}

			var records int
			err := conn.QueryRow(
				`SELECT COUNT(*) FROM artifacts WHERE run_id = ? AND kind = ?`,
				runID, ArtifactKindIssueDiff).Scan(&records)
			testsupport.Must(t, err, "counting issue.diff artifacts: %v", err)
			if records != tc.wantRecords {
				t.Errorf("issue.diff artifacts = %d, want %d — the guard's "+
					"record-or-drop decision must not change", records, tc.wantRecords)
			}
		})
	}
}

// inScopePortion is the text measureDiff sizes: the cumulative diff before
// either trailer marker. Spelled out here rather than exported from saga.go so
// the agreement assertion below reads the body independently of the code it
// checks.
func inScopePortion(body string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, roundDeltaMarker) ||
			strings.HasPrefix(line, outOfScopeMarker) {
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestMeasureDiffEmptyAgreesWithRecordedChange is DKT-2549: a body the ledger
// keeps as a real change — one diffRecordsNoChange answers false for — reads
// `diff.empty == false`, whatever its hunks look like. Rename-, mode-, and
// binary-only diffs carry no `+`/`-` content line, and sizing `Empty` by those
// lines alone recorded the change, showed it to a reviewer, and then skipped the
// review keyed on `any(diff.empty == false)`.
func TestMeasureDiffEmptyAgreesWithRecordedChange(t *testing.T) {
	cases := []struct {
		name string
		body string
		want DiffFacts
	}{
		{
			name: "pure rename",
			body: "diff --git a/old.go b/new.go\nsimilarity index 100%\n" +
				"rename from old.go\nrename to new.go\n",
			want: DiffFacts{Files: 1},
		},
		{
			name: "binary change",
			body: "diff --git a/x.bin b/x.bin\nindex 0123456..89abcde 100644\n" +
				"Binary files a/x.bin and b/x.bin differ\n",
			want: DiffFacts{Files: 1},
		},
		{
			name: "mode-only change",
			body: "diff --git a/x.sh b/x.sh\nold mode 100644\nnew mode 100755\n",
			want: DiffFacts{Files: 1},
		},
		{
			name: "content lines",
			body: diffBodyOf(2, 1),
			want: DiffFacts{Lines: 3, Files: 1},
		},
		{
			name: "empty",
			body: "",
			want: DiffFacts{Empty: true},
		},
		{
			name: "comment only",
			body: "# warning\n",
			want: DiffFacts{Empty: true},
		},
		{
			// Hunks disclosed behind a trailer are not this issue's change, so
			// a body that is ONLY a trailer measured nothing in scope.
			name: "content behind a trailer only",
			body: outOfScopeMarker + " their hunks follow (DKT-86) ===\n" + diffBodyOf(2, 0),
			want: DiffFacts{Empty: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := measureDiff(tc.body)
			if got != tc.want {
				t.Errorf("measureDiff = %+v, want %+v", got, tc.want)
			}
			if ledger := diffRecordsNoChange(inScopePortion(tc.body)); got.Empty != ledger {
				t.Errorf("measureDiff.Empty = %v but diffRecordsNoChange over the "+
					"in-scope portion = %v — the two readers of one body disagree "+
					"about whether it holds a change", got.Empty, ledger)
			}
		})
	}
}

// TestMeasureDiffHeaderCollision is DKT-2550: `--- `/`+++ ` are file headers
// only between a block's `diff --git ` line and its first `@@`. Inside a hunk
// they are content — a removed `-- banner`, an added `++x` — and dropping them
// under-sized every change that touched such a line.
func TestMeasureDiffHeaderCollision(t *testing.T) {
	cases := []struct {
		name string
		body string
		want DiffFacts
	}{
		{
			name: "hunk lines with header prefixes are content",
			body: "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n" +
				"-plain\n--- banner\n+++x\n+other\n",
			want: DiffFacts{Lines: 4, Files: 1},
		},
		{
			// The second block is `diff.noprefix` style for a new file, whose
			// headers do not carry the `a/`/`b/` spellings a narrower match
			// would key on.
			name: "genuine headers are still excluded",
			body: "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n-one\n+two\n" +
				"diff --git a/f b/f\nnew file mode 100644\n--- /dev/null\n+++ f\n" +
				"@@ -0,0 +1,2 @@\n+three\n+four\n",
			want: DiffFacts{Lines: 4, Files: 2},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := measureDiff(tc.body); got != tc.want {
				t.Errorf("measureDiff = %+v, want %+v", got, tc.want)
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
