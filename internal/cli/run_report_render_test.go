package cli

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// DKT-403 — the rendered document, which is what a post-mortem reader has.
//
// RUN-32's "How steps ended" printed:
//
//	"reconcile@3": failed-routed — "waiting-human: loop 4 would exceed
//	 max_fix_loops = 3 on HRN-301; `docket step resolve --as fix-round`
//	 authorizes one more round"
//
// The operator had ruled on that park — `run abandon --issue`, with a written
// rationale — and the report said so nowhere. The next session read it, told
// the operator the gate was "never resolved", and re-asked the decision.

// abandonedRunReport is the RUN-32 shape, minimally: one parked step whose
// routing text outlived the park, and the ruling that ended it.
func abandonedRunReport() *engine.RunReport {
	return &engine.RunReport{
		Run: &model.Run{ID: 32, Status: model.RunDone},
		Steps: []model.StatusCount{
			{Status: db.StepDone, Count: 3},
			{Status: db.StepFailedRouted, Count: 1},
		},
		Attempts: []engine.StepAttempt{
			{
				Step: "STEP-1", Instance: "reconcile@3", Issue: "HRN-301",
				Status: db.StepFailedRouted, Attempts: 1,
				Routing: "waiting-human: loop 4 would exceed max_fix_loops = 3 " +
					"on HRN-301; `docket step resolve --as fix-round` authorizes " +
					"one more round",
			},
		},
		Issues: []engine.IssueDisposition{{
			Issue:       "HRN-301",
			Disposition: engine.DispositionAbandoned,
			Reason: "operator selected: Stop HRN-301, re-plan it later — " +
				"findings preserved as HRN-376",
		}},
	}
}

// TestReportRendersTheIssueDisposition is the first acceptance criterion: the
// abandonment, and its recorded reason, are in the document.
func TestReportRendersTheIssueDisposition(t *testing.T) {
	out := renderPlainRunReport(abandonedRunReport())

	if !strings.Contains(out, "How issues ended") {
		t.Fatalf("the report has no issue-disposition section:\n%s", out)
	}
	for _, needle := range []string{
		"HRN-301", "abandoned", "Stop HRN-301, re-plan it later",
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("the report never says %q:\n%s", needle, out)
		}
	}
}

// TestParkTextIsNotTheFinalWord is the second: the failed-routed line itself
// must not read as an open question.
func TestParkTextIsNotTheFinalWord(t *testing.T) {
	out := renderPlainRunReport(abandonedRunReport())

	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "reconcile@3") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("reconcile@3 is not in the report at all:\n%s", out)
	}
	if !strings.Contains(line, "later resolved") {
		t.Errorf("the failed-routed line still ends on its park text, so a "+
			"reader concludes the gate was never resolved:\n  %s", line)
	}
	if !strings.Contains(line, "abandoned") {
		t.Errorf("the resolution on %q does not say what happened", line)
	}
	if !strings.Contains(line, "Stop HRN-301") {
		t.Errorf("the resolution on %q quotes none of the ruling", line)
	}
	// exec.Render IS strconv.Quote (§5.7), so a caller that also applied %q
	// would hand the operator their own words as `"\"…\""`.
	if strings.Contains(line, `\"`) {
		t.Errorf("the annotation double-quotes the ruling:\n  %s", line)
	}
}

// TestResolvedStepIsNotAnnotatedTwice is the falsifier for the annotation.
//
// The `abandon-issue` ROUTING path already writes the disposition onto every
// step it terminated. Annotating those would print the same fact twice on one
// line, and a rule that fires on every row is not a rule.
func TestResolvedStepIsNotAnnotatedTwice(t *testing.T) {
	r := abandonedRunReport()
	r.Attempts = []engine.StepAttempt{
		{
			Step: "STEP-1", Instance: "verify@0", Issue: "HRN-301",
			Status: db.StepFailedRouted, Attempts: 0,
			Routing: "abandon-issue: cascade: HRN-301 was abandoned by " +
				"reconcile@3; this step was never measured",
		},
		{
			Step: "STEP-2", Instance: "report@0", Issue: "HRN-301",
			Status: db.StepSkipped, Attempts: 0, Routing: "skip: nothing to report",
		},
	}

	for _, line := range strings.Split(renderPlainRunReport(r), "\n") {
		if !strings.Contains(line, "verify@0") && !strings.Contains(line, "report@0") {
			continue
		}
		if strings.Contains(line, "later resolved") {
			t.Errorf("a step whose own record already carries its ending was "+
				"annotated anyway:\n  %s", line)
		}
	}
}

// TestReportOmitsTheSectionWithoutADisposition: a section on every run says
// nothing.
func TestReportOmitsTheSectionWithoutADisposition(t *testing.T) {
	r := abandonedRunReport()
	r.Issues = nil

	out := renderPlainRunReport(r)
	if strings.Contains(out, "How issues ended") {
		t.Errorf("a run with no issue-level ruling still prints the section:\n%s", out)
	}
	if strings.Contains(out, "later resolved") {
		t.Errorf("a step with no disposition to join was annotated anyway:\n%s", out)
	}
}

// TestReasonHeadTrimsOnRuneBoundaries: an operator's ruling is prose, and a
// byte cut through a multi-byte character emits a replacement character in the
// middle of their own words.
func TestReasonHeadTrimsOnRuneBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		max            int
	}{
		{name: "short", in: "stop it", max: 20, want: "stop it"},
		{name: "trimmed", in: "abcdefghij", max: 4, want: "abcd…"},
		{name: "multibyte", in: "—————", max: 2, want: "——…"},
		{name: "multiline", in: "head\n\nbody", max: 40, want: "head …"},
		{name: "trailing blank", in: "head\n\n", max: 40, want: "head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reasonHead(tc.in, tc.max); got != tc.want {
				t.Errorf("reasonHead(%q, %d) = %q, want %q",
					tc.in, tc.max, got, tc.want)
			}
		})
	}
}

// DKT-405 item 2 — step lines that say WHICH ISSUE they are about.
//
// Instance labels are unique within an issue and repeat across them. RUN-32
// carried four issues on one workflow, and its report printed:
//
//	Attempts
//	  "implement@0": 2
//	  "implement@0": 2
//
//	How steps ended
//	  "reconcile@0": done — "fix-loop"
//	  "reconcile@0": done — "fix-loop"
//	  "reconcile@0": done — "fix-loop"
//
// Nothing on those rows says which issue retried, and a reader cannot even
// tell a repeated row from a duplicate-rendering bug.

// multiIssueRunReport is that shape: two issues on one workflow, sharing every
// instance name, each with a retry and a non-pass ending.
func multiIssueRunReport() *engine.RunReport {
	return &engine.RunReport{
		Run: &model.Run{ID: 32, Status: model.RunDone},
		Steps: []model.StatusCount{
			{Status: db.StepDone, Count: 2},
			{Status: db.StepFailedRouted, Count: 2},
		},
		Attempts: []engine.StepAttempt{
			{
				Step: "STEP-1", Instance: "implement@0", Issue: "HRN-300",
				Status: db.StepFailedRouted, Attempts: 2, Routing: "fix-loop",
			},
			{
				Step: "STEP-2", Instance: "implement@0", Issue: "HRN-301",
				Status: db.StepFailedRouted, Attempts: 2, Routing: "fix-loop",
			},
		},
	}
}

// TestMultiIssueStepLinesCarryTheIssue is the acceptance criterion: on a run
// with more than one issue, every step line names its issue, in both sections
// that print one.
func TestMultiIssueStepLinesCarryTheIssue(t *testing.T) {
	out := renderPlainRunReport(multiIssueRunReport())

	for _, section := range []string{"Attempts", "How steps ended"} {
		lines := sectionLines(t, out, section)
		if len(lines) != 2 {
			t.Fatalf("%s printed %d rows, want the two steps:\n%s",
				section, len(lines), out)
		}
		for _, want := range []string{"HRN-300 implement@0", "HRN-301 implement@0"} {
			if !linesContain(lines, want) {
				t.Errorf("%s has no row for %q — the rows are indistinguishable:\n  %s",
					section, want, strings.Join(lines, "\n  "))
			}
		}
	}
}

// TestSingleIssueStepLinesAreUnchanged is the falsifier for the rule. On a
// one-issue run the id would be the same constant on every line: it
// disambiguates nothing and would only churn the document.
func TestSingleIssueStepLinesAreUnchanged(t *testing.T) {
	r := multiIssueRunReport()
	r.Attempts = r.Attempts[:1]

	lines := sectionLines(t, renderPlainRunReport(r), "How steps ended")
	if len(lines) != 1 {
		t.Fatalf("How steps ended printed %d rows, want one: %v", len(lines), lines)
	}
	if strings.Contains(lines[0], "HRN-300") {
		t.Errorf("a single-issue run's step line carries an id that "+
			"disambiguates nothing:\n  %s", lines[0])
	}
	if !strings.Contains(lines[0], `"implement@0":`) {
		t.Errorf("the single-issue label is no longer the bare instance:\n  %s", lines[0])
	}
}

// TestUnattributedStepKeepsTheBareLabel: a row carrying no issue at all — a
// pre-DKT-403 store, or a step expanded for the run rather than an issue —
// renders as it always did rather than being given an id it does not have.
func TestUnattributedStepKeepsTheBareLabel(t *testing.T) {
	r := multiIssueRunReport()
	r.Attempts = append(r.Attempts, engine.StepAttempt{
		Step: "STEP-3", Instance: "rollup@0", Status: db.StepFailedRouted,
		Attempts: 1, Routing: "fix-loop",
	})

	for _, line := range sectionLines(t, renderPlainRunReport(r), "How steps ended") {
		if !strings.Contains(line, "rollup@0") {
			continue
		}
		if !strings.Contains(line, `"rollup@0":`) {
			t.Errorf("an unattributed step was given an issue prefix:\n  %s", line)
		}
	}
}

// sectionLines returns the indented rows under one section heading of the
// plain report.
func sectionLines(t *testing.T, out, section string) []string {
	t.Helper()
	var (
		lines []string
		in    bool
	)
	for _, line := range strings.Split(out, "\n") {
		switch {
		case line == section:
			in = true
		case in && strings.HasPrefix(line, "  "):
			lines = append(lines, strings.TrimSpace(line))
		case in:
			return lines
		}
	}
	return lines
}

func linesContain(lines []string, needle string) bool {
	for _, l := range lines {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}

// DKT-733 — RUN-51's coverage line said 12 of 57 seats reported nothing and
// named neither the seats nor the seating paths, so the gap could not be
// diagnosed or back-filled from the report.

// silentSeatsRunReport is the RUN-51 shape, minimally: casts on both seating
// paths, some of them silent.
func silentSeatsRunReport() *engine.RunReport {
	return &engine.RunReport{
		Run:               &model.Run{ID: 51, Status: model.RunDone},
		VoteUsageCoverage: db.VoteUsageCoverage{Casts: 5, Reported: 2},
		SilentVoteSeats: []engine.SilentVoteSeat{
			{Proposal: "DKT-V218", Voter: "sec-arch", Role: "judge",
				Path: engine.SeatPathConversationalGate},
			{Proposal: "DKT-V219", Voter: "sec-crypto",
				Path: engine.SeatPathConversationalGate},
			{Proposal: "DKT-V220", Voter: "verify-seat", Role: "verifier",
				Path: engine.SeatPathVoteStep},
		},
	}
}

// TestCoverageLineNamesSilentSeatPaths is DKT-733's AC3: the coverage line
// itself names the seating path(s) of the missing seats, with counts.
func TestCoverageLineNamesSilentSeatPaths(t *testing.T) {
	lines := sectionLines(t, renderPlainRunReport(silentSeatsRunReport()), "Vote usage")

	var coverage string
	for _, l := range lines {
		if strings.HasPrefix(l, "Coverage:") {
			coverage = l
		}
	}
	if coverage == "" {
		t.Fatalf("no coverage line rendered:\n%v", lines)
	}
	if !strings.Contains(coverage, "3 reported NOTHING") {
		t.Errorf("the coverage line lost its silent count:\n  %s", coverage)
	}
	if !strings.Contains(coverage, "conversational-gate: 2, vote-step: 1") {
		t.Errorf("the coverage line does not name the seating paths of the "+
			"missing seats:\n  %s", coverage)
	}
}

// TestSilentSeatsAreEnumeratedWithABackfillPointer is DKT-733's AC1 rendered:
// each silent seat is one line naming its proposal (the backfill verb's
// argument), its voter, and its path — and the verb itself is named once.
func TestSilentSeatsAreEnumeratedWithABackfillPointer(t *testing.T) {
	lines := sectionLines(t, renderPlainRunReport(silentSeatsRunReport()), "Vote usage")

	for _, needle := range []string{
		`DKT-V218 seat "sec-arch" as "judge"  (conversational-gate)`,
		`DKT-V219 seat "sec-crypto"  (conversational-gate)`,
		`DKT-V220 seat "verify-seat" as "verifier"  (vote-step)`,
		"vote backfill-usage",
	} {
		if !linesContain(lines, needle) {
			t.Errorf("the Vote usage section never says %q:\n%v", needle, lines)
		}
	}
}

// TestFullyReportedRunListsNoSilentSeats: a run whose every seat reported
// renders the coverage line alone — no Silent rows, no backfill pointer.
func TestFullyReportedRunListsNoSilentSeats(t *testing.T) {
	r := silentSeatsRunReport()
	r.VoteUsageCoverage = db.VoteUsageCoverage{Casts: 5, Reported: 5}
	r.SilentVoteSeats = nil

	lines := sectionLines(t, renderPlainRunReport(r), "Vote usage")
	if !linesContain(lines, "5 of 5 seat(s) reported spend") {
		t.Fatalf("no coverage line on a fully-reported run:\n%v", lines)
	}
	for _, forbidden := range []string{"Silent:", "Backfill:", "NOTHING"} {
		if linesContain(lines, forbidden) {
			t.Errorf("a fully-reported run still renders %q:\n%v", forbidden, lines)
		}
	}
}

// ---------------------------------------------------------------------------
// DKT-862 — the Gates tally distinguishes a pre-gate from a blocking one
// ---------------------------------------------------------------------------

// The section rendered `ac-commands: pass 0, fail 1` whether the failure
// BLOCKED the step or was an advisory input to it. A `pre = true` gate never
// routes — §11.1 runs it at claim, PG4 keeps it out of the saga's verdict — so
// on RUN-61 three of them failed and every step routed on its executor's own
// verdict anyway. Only `step show` carried the marker, and a conductor reading
// this document nearly reported a fix round as burned on a gate artifact.

// mixedGateReport is RUN-61's shape: one advisory gate, one blocking gate, and
// one name that ran BOTH ways across the run's workflows.
func mixedGateReport() *engine.RunReport {
	return &engine.RunReport{
		Run: &model.Run{ID: 61, Status: model.RunDone},
		Gates: []db.VerdictCount{
			// The pre-gate RUN-61 kept failing: every row advisory.
			{Name: "ac-commands", Fail: 1, Pre: 1},
			// A blocking gate that failed identically — the row whose bytes
			// were indistinguishable from the one above.
			{Name: "build", Pass: 2, Fail: 1},
			// One name, both declarations: `design-qa` declares render-verify
			// `pre`, another workflow gates on it.
			{Name: "render-verify", Pass: 1, Fail: 1, Pre: 1},
		},
	}
}

// TestPreGateTallyCarriesTheMarker is DKT-862's AC1.
//
// The marker sits OUTSIDE the rendered name's quotes: `exec.Render` escapes a
// stored string, and the marker is core's own word about it — quoting it would
// claim a workflow author had written it.
func TestPreGateTallyCarriesTheMarker(t *testing.T) {
	lines := sectionLines(t, renderPlainRunReport(mixedGateReport()), "Gates")

	if !linesContain(lines, `"ac-commands" [pre]: pass 0, fail 1`) {
		t.Errorf("the advisory gate's tally carries no [pre] marker, so it "+
			"reads as a gate that blocked the step:\n%v", lines)
	}
}

// TestBlockingGateTallyIsUnmarked is the other half of AC1, and the half that
// makes the marker mean something: a gate that DID route must not carry it.
func TestBlockingGateTallyIsUnmarked(t *testing.T) {
	lines := sectionLines(t, renderPlainRunReport(mixedGateReport()), "Gates")

	for _, l := range lines {
		if strings.Contains(l, `"build"`) && strings.Contains(l, "[pre]") {
			t.Errorf("a blocking gate's tally carries the advisory marker:\n  %s", l)
		}
	}
	if !linesContain(lines, `"build":`) || !linesContain(lines, "pass 2, fail 1") {
		t.Errorf("the blocking gate's line changed:\n%v", lines)
	}
}

// TestSplitGateNameReportsTheRatioRatherThanTheMarker: one gate name declared
// `pre` by one workflow and blocking by another cannot take the marker without
// claiming the whole tally was advisory, so the ratio says which part was.
func TestSplitGateNameReportsTheRatioRatherThanTheMarker(t *testing.T) {
	lines := sectionLines(t, renderPlainRunReport(mixedGateReport()), "Gates")

	for _, l := range lines {
		if !strings.Contains(l, `"render-verify"`) {
			continue
		}
		if strings.Contains(l, "[pre]") {
			t.Errorf("a name that also ran as a blocking gate takes the "+
				"whole-tally marker:\n  %s", l)
		}
		if !strings.Contains(l, "1 of 2 ran as a pre-gate") {
			t.Errorf("a split gate name says nothing about which rows "+
				"routed:\n  %s", l)
		}
		return
	}
	t.Fatalf("render-verify is not in the Gates section at all:\n%v", lines)
}

// TestActionsTallyIsUnaffected: `action_results` has no `pre` column, and the
// shared rollup body must render actions exactly as before rather than growing
// a marker off an honest zero.
func TestActionsTallyIsUnaffected(t *testing.T) {
	r := mixedGateReport()
	r.Actions = []db.VerdictCount{{Name: "reconcile", Pass: 1}}

	lines := sectionLines(t, renderPlainRunReport(r), "Actions")
	if !linesContain(lines, `"reconcile":   pass 1, fail 0`) {
		t.Errorf("the Actions section changed:\n%v", lines)
	}
	if linesContain(lines, "[pre]") || linesContain(lines, "pre-gate") {
		t.Errorf("an action tally grew a pre marker off a column its table "+
			"does not have:\n%v", lines)
	}
}
