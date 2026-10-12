package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// A fence completion gate records every harvested line as its own row under one
// gate name, with ascending ordinals. Each line is its own decision (§7.3 step
// 4), so the gate passes only when every line's last attempt passed. These
// tests put the failing line FIRST: a reduction that read only the gate's
// highest ordinal would see the passing last line and route the step as pass.

func gateExit(n int) *int { return &n }

func lineRow(ordinal int, verdict string, argv ...string) db.GateResultRow {
	r := db.GateResultRow{Gate: "checks", Ordinal: ordinal, Verdict: verdict}
	switch verdict {
	case db.GateVerdictPass:
		r.Argv, r.Exit = argv, gateExit(0)
	case db.GateVerdictFail:
		r.Argv, r.Exit = argv, gateExit(1)
	case db.GateVerdictUnmatched:
		r.Reason = "no trust entry matched"
	}
	return r
}

func TestFenceGateFailsWhenAnEarlierLineIsUnmatched(t *testing.T) {
	rows := []db.GateResultRow{
		lineRow(0, db.GateVerdictUnmatched),
		lineRow(1, db.GateVerdictPass, "go", "vet", "./..."),
	}
	verdict, _, unmatched := verdictOverRows(rows)
	if verdict != VerdictFail || !unmatched {
		t.Errorf("verdict = %q unmatched = %v, want %q true: the first line never "+
			"ran, and a later passing line does not stand in for it",
			verdict, unmatched, VerdictFail)
	}

	control := []db.GateResultRow{
		lineRow(0, db.GateVerdictPass, "go", "test", "./..."),
		lineRow(1, db.GateVerdictPass, "go", "vet", "./..."),
	}
	if verdict, _, unmatched := verdictOverRows(control); verdict != VerdictPass || unmatched {
		t.Errorf("control: verdict = %q unmatched = %v, want %q false",
			verdict, unmatched, VerdictPass)
	}
}

func TestFenceGateFailsWhenAnEarlierLineFailed(t *testing.T) {
	rows := []db.GateResultRow{
		lineRow(0, db.GateVerdictFail, "go", "test", "./..."),
		lineRow(1, db.GateVerdictPass, "go", "vet", "./..."),
	}
	verdict, _, unmatched := verdictOverRows(rows)
	if verdict != VerdictFail || unmatched {
		t.Errorf("verdict = %q unmatched = %v, want %q false: the first line ran "+
			"and failed", verdict, unmatched, VerdictFail)
	}
}

// F3/F4: a flaky line's retries share its argv and follow it directly, and the
// last attempt is the one that routes.
func TestFlakyRetryOfOneFenceLineStillPasses(t *testing.T) {
	rows := []db.GateResultRow{
		lineRow(0, db.GateVerdictFail, "go", "test", "./..."),
		lineRow(1, db.GateVerdictFail, "go", "test", "./..."),
		lineRow(2, db.GateVerdictPass, "go", "test", "./..."),
	}
	if verdict, _, _ := verdictOverRows(rows); verdict != VerdictPass {
		t.Errorf("verdict = %q, want %q: the line passed on its third attempt",
			verdict, VerdictPass)
	}

	// A later line running the same command is not a retry of the failure:
	// retries are adjacent.
	interleaved := []db.GateResultRow{
		lineRow(0, db.GateVerdictFail, "go", "test", "./..."),
		lineRow(1, db.GateVerdictPass, "go", "vet", "./..."),
		lineRow(2, db.GateVerdictPass, "go", "test", "./..."),
	}
	if verdict, _, _ := verdictOverRows(interleaved); verdict != VerdictFail {
		t.Errorf("interleaved: verdict = %q, want %q", verdict, VerdictFail)
	}
}

// `step resolve --as rerun-gates` runs the gate again on the same step, and its
// rows continue the ordinals. Only the latest execution routes.
func TestRerunOfAFenceGateRoutesOnTheLatestExecution(t *testing.T) {
	at := func(ms int64, r db.GateResultRow) db.GateResultRow {
		r.CreatedAtMS = ms
		return r
	}
	failedThenFixed := []db.GateResultRow{
		at(100, lineRow(0, db.GateVerdictFail, "go", "test", "./...")),
		at(100, lineRow(1, db.GateVerdictPass, "go", "vet", "./...")),
		at(200, lineRow(2, db.GateVerdictPass, "go", "test", "./...")),
		at(200, lineRow(3, db.GateVerdictPass, "go", "vet", "./...")),
	}
	if verdict, _, _ := verdictOverRows(failedThenFixed); verdict != VerdictPass {
		t.Errorf("verdict = %q, want %q: the re-run passed every line",
			verdict, VerdictPass)
	}

	fixedThenBroken := []db.GateResultRow{
		at(100, lineRow(0, db.GateVerdictPass, "go", "test", "./...")),
		at(100, lineRow(1, db.GateVerdictPass, "go", "vet", "./...")),
		at(200, lineRow(2, db.GateVerdictUnmatched)),
		at(200, lineRow(3, db.GateVerdictPass, "go", "vet", "./...")),
	}
	verdict, _, unmatched := verdictOverRows(fixedThenBroken)
	if verdict != VerdictFail || !unmatched {
		t.Errorf("verdict = %q unmatched = %v, want %q true: the re-run's first "+
			"line never ran", verdict, unmatched, VerdictFail)
	}
}

// A batch override grant must cover EVERY failing line, so the cover sees the
// fence gate's failing first line as well as the other failing gate.
func TestFailingCompletionRowsIncludesAnEarlierFailingFenceLine(t *testing.T) {
	rows := []db.GateResultRow{
		lineRow(0, db.GateVerdictFail, "go", "test", "./..."),
		lineRow(1, db.GateVerdictPass, "go", "vet", "./..."),
		{Gate: "lint", Ordinal: 0, Verdict: db.GateVerdictFail, Exit: gateExit(1),
			Argv: []string{"golangci-lint", "run"}},
	}
	failing := failingCompletionRows(rows)
	var got []string
	for _, r := range failing {
		got = append(got, fmt.Sprintf("%s#%d", r.Gate, r.Ordinal))
	}
	want := []string{"checks#0", "lint#0"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("failing rows = %v, want %v", got, want)
	}
}

// fenceLines is a gate runner that reports each gate as a block of lines, one
// row per line, in the shape runFenceGate records them. lines holds each line's
// verdict for the next execution.
type fenceLines struct {
	mu    sync.Mutex
	lines []string
}

func (f *fenceLines) Run(context.Context, GateSpec, StepContext) (GateResult, error) {
	return GateResult{}, fmt.Errorf("fenceLines runs only through Execute")
}

func (f *fenceLines) Execute(_ context.Context, g GateSpec, _ StepContext) (GateExecution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := GateExecution{Verdict: VerdictPass}
	for i, verdict := range f.lines {
		row := GateResultRow{Gate: g.Name, Ordinal: i, Verdict: verdict}
		switch verdict {
		case VerdictPass:
			row.Argv, row.Exit = []string{"line", fmt.Sprint(i)}, gateExit(0)
		case VerdictFail:
			row.Argv, row.Exit = []string{"line", fmt.Sprint(i)}, gateExit(1)
		default:
			row.Reason = "no trust entry matched"
		}
		if verdict != VerdictPass {
			out.Verdict = VerdictFail
		}
		out.Results = append(out.Results, row)
	}
	return out, nil
}

func TestFenceGateWithAFailingFirstLineParksAndRerunRecovers(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	gates := &fenceLines{lines: []string{VerdictUnmatched, VerdictPass}}
	e := testEngine()
	e.Gates = gates

	id := parkedByFailingGate(t, conn, e)
	if got := stepStatus(t, conn, "implement@0"); got != db.StepWaitingHuman {
		t.Fatalf("status = %q, want %q: the gate's first line never ran",
			got, db.StepWaitingHuman)
	}
	failed, err := FailedGates(conn, id)
	testsupport.Must(t, err, "FailedGates: %v", err)
	if len(failed) == 0 || failed[0].Verdict != db.GateVerdictUnmatched {
		t.Errorf("FailedGates = %+v, want the gate reported unmatched", failed)
	}

	gates.mu.Lock()
	gates.lines = []string{VerdictPass, VerdictPass}
	gates.mu.Unlock()
	err = e.ResolveStep(conn, id, ResolveRerunGates, "trust entry added", nowMS+1)
	testsupport.Must(t, err, "resolve --as rerun-gates: %v", err)

	if got := stepStatus(t, conn, "implement@0"); got == db.StepWaitingHuman {
		t.Errorf("status = %q after every line passed on the re-run", got)
	}
}
