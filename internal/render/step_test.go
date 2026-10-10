package render

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// DKT-862 — `step show` was ALREADY RIGHT, and had to stay byte-for-byte right.
//
// It was the only one of the three gate surfaces carrying the pre marker: `run
// report`'s Gates tally and `events list`'s gate-recorded lines rendered an
// advisory pre-gate failure identically to a blocking one, and the remedy was
// to teach those two what this function already knew. The risk in that remedy
// is a "shared helper" refactor that quietly restyles the surface that was
// working, so this pins the exact bytes.
//
// A GOLDEN STRING, not a set of Contains checks. The complaint DKT-862 fixes
// was about how two surfaces LOOKED beside a third; a test that accepted any
// output containing "[pre]" would not notice this one drifting away from the
// spelling the other two were just aligned to.
func TestStepShowGateSummaryBytesAreUnchanged(t *testing.T) {
	exitTwo := 2
	rows := []StepGateRow{
		// RUN-61 STEP-2745: the advisory failure that routed nothing.
		{Gate: "ac-commands", Verdict: "fail", Exit: &exitTwo, Pre: true},
		// A blocking gate failing the same way, and a re-run beside it, so the
		// ordinal and the marker are pinned together — they compose into one
		// name and a refactor could reorder them.
		{Gate: "build", Verdict: "fail", Exit: &exitTwo},
		{Gate: "build", Ordinal: 1, Verdict: "pass", Exit: new(int)},
	}

	const want = "  gates:\n" +
		"    fail       ac-commands [pre]  exit 2\n" +
		"    fail       build  exit 2\n" +
		"    pass       build (re-run 1)  exit 0\n" +
		"    2 gate(s) did not pass; reasons and output: docket step gates STEP-2745\n"

	if got := RenderStepGateSummary("STEP-2745", rows); got != want {
		t.Errorf("`step show`'s gate summary changed.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// The loop history a fix-loop exhaustion leaves on the row reaches `step show`
// as one labeled line per fact, and a row outside an exhaustion shows none of
// the three labels.
func TestRenderStepDetailLoopHistory(t *testing.T) {
	exhausted := model.StepRow{
		Step:              "STEP-7",
		Instance:          "fix@0#3",
		Status:            "waiting-human",
		LoopRoundsRun:     3,
		LoopTriggerStep:   "review@0",
		LoopLatestVerdict: "fix-loop",
	}
	got := RenderStepDetail(exhausted, "", "", "", 0)
	for _, want := range []string{
		"  loop rounds:  3\n",
		"  loop trigger: review@0\n",
		"  loop verdict: fix-loop\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("exhausted row: missing %q in:\n%s", want, got)
		}
	}

	bare := model.StepRow{Step: "STEP-8", Instance: "build@0", Status: "pending"}
	got = RenderStepDetail(bare, "", "", "", 0)
	for _, label := range []string{"loop rounds:", "loop trigger:", "loop verdict:"} {
		if strings.Contains(got, label) {
			t.Errorf("bare row: unexpected %q in:\n%s", label, got)
		}
	}
}
