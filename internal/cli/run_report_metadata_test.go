package cli

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// DKT-868 — the rendered document, which is what an operator who never passes
// `--json` has.
//
// The "Metadata" rollup published RUN-51's anomaly and could not name it: an
// `effort_resolved` value of `low` under a key whose partner `effort_requested`
// never showed one. Grouping by key is what discards the pairing, so the only
// remaining reader was `docket step show`, one step at a time.

// tieredRunReport is the RUN-51 shape, minimally: three steps, one of which was
// served at a tier other than the one it was dispatched at, and a fourth that
// failed before it could report what it resolved to.
func tieredRunReport() *engine.RunReport {
	return &engine.RunReport{
		Run: &model.Run{ID: 51, Status: model.RunDone},
		Steps: []model.StatusCount{
			{Status: db.StepDone, Count: 3},
			{Status: db.StepFailedRouted, Count: 1},
		},
		Attempts: []engine.StepAttempt{
			{
				Step: "STEP-1", Instance: "implement@0", Status: db.StepDone,
				Attempts: 1,
				Metadata: map[string]any{
					"effort_requested": "high", "effort_resolved": "high",
				},
			},
			{
				Step: "STEP-2", Instance: "review@0", Status: db.StepDone,
				Attempts: 1,
				Metadata: map[string]any{
					"effort_requested": "high", "effort_resolved": "low",
				},
			},
			{
				Step: "STEP-3", Instance: "reconcile@0", Status: db.StepDone,
				Attempts: 1,
				Metadata: map[string]any{
					"effort_requested": "high", "effort_resolved": "high",
				},
			},
			{
				Step: "STEP-4", Instance: "verify@0", Status: db.StepFailedRouted,
				Attempts: 1,
				Metadata: map[string]any{"effort_requested": "high"},
			},
		},
		// The rollup as it always rendered: the anomaly, unattributable.
		Metadata: []db.MetadataKeyRollup{
			{Key: "effort_requested", Values: []db.MetadataValueCount{
				{Value: "high", Count: 4}}},
			{Key: "effort_resolved", Values: []db.MetadataValueCount{
				{Value: "high", Count: 2}, {Value: "low", Count: 1}}},
		},
	}
}

// TestReportPairsEachStepsKeysOnOneLine is the acceptance criterion: the two
// keys the rollup separated appear together, on the line that names the step.
func TestReportPairsEachStepsKeysOnOneLine(t *testing.T) {
	out := renderPlainRunReport(tieredRunReport())

	if !strings.Contains(out, "Step metadata") {
		t.Fatalf("the report has no per-step metadata section:\n%s", out)
	}

	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "review@0") && strings.Contains(l, "effort_resolved") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no line pairs review@0 with its bag; the drifted step is "+
			"still recoverable only by `step show`:\n%s", out)
	}
	for _, needle := range []string{"effort_requested", "high", "effort_resolved", "low"} {
		if !strings.Contains(line, needle) {
			t.Errorf("the review@0 line %q never says %q", line, needle)
		}
	}
	// The rollup is still there. Both questions are real — "which values did
	// this key take across the run" and "which values did two keys take on one
	// step" — and the fix adds the second reader rather than trading one for
	// the other.
	if !strings.Contains(out, "\nMetadata\n") {
		t.Errorf("the key rollup is gone from the document:\n%s", out)
	}
}

// TestFailedStepsHalfBagIsAttributable is the issue's stated consequence: a
// step that failed before reporting a resolution must not read as one whose
// tiers agreed.
func TestFailedStepsHalfBagIsAttributable(t *testing.T) {
	out := renderPlainRunReport(tieredRunReport())

	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "verify@0") && strings.Contains(l, "effort_requested") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("the failed step's dispatch bag is in no line of the "+
			"document:\n%s", out)
	}
	if !strings.Contains(line, db.StepFailedRouted) {
		t.Errorf("the verify@0 line %q does not say the step failed, so its "+
			"missing resolution reads as agreement", line)
	}
	if strings.Contains(line, "effort_resolved") {
		t.Errorf("the verify@0 line %q invents a resolution the step never "+
			"reported", line)
	}
}

// TestStepMetadataLineIsDeterministic is R9 through a map-valued field: two
// renders of one report are byte-identical. A bare range over the bag would
// emit a different document per invocation.
func TestStepMetadataLineIsDeterministic(t *testing.T) {
	first := renderPlainRunReport(tieredRunReport())
	for i := 0; i < 20; i++ {
		if again := renderPlainRunReport(tieredRunReport()); again != first {
			t.Fatalf("render %d differs from the first:\n%s\n---\n%s",
				i, first, again)
		}
	}
}

// TestReportWithoutStepBagsIsUnchanged is the regression half: a run whose
// steps carried no metadata gains no section, no header, and no blank line.
func TestReportWithoutStepBagsIsUnchanged(t *testing.T) {
	report := tieredRunReport()
	for i := range report.Attempts {
		report.Attempts[i].Metadata = nil
	}
	report.Metadata = nil

	out := renderPlainRunReport(report)
	if strings.Contains(out, "Step metadata") {
		t.Errorf("a run with no step bags grew a per-step metadata "+
			"section:\n%s", out)
	}
}

// TestUnreadableBagSaysSoInTheDocument keeps R10's tolerance from reading as an
// absence: bytes that do not decode are reported as such, not skipped into a
// silence a reader would take for "the dispatcher recorded nothing".
func TestUnreadableBagSaysSoInTheDocument(t *testing.T) {
	report := tieredRunReport()
	report.Attempts[0].Metadata = nil
	report.Attempts[0].MetadataUnreadable = true

	out := renderPlainRunReport(report)

	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "implement@0") && strings.Contains(l, "decode") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("a stored bag that does not decode is reported nowhere:\n%s", out)
	}
	if !strings.Contains(line, "step show") {
		t.Errorf("the line %q says the bag is unreadable and not where the raw "+
			"bytes are", line)
	}
}
