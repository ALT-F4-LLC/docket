package render

import "testing"

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
