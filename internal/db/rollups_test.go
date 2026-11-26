package db

import (
	"reflect"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// reportEvent appends one event through the fixture's transaction.
func reportEvent(f *routingFixture, e NewEvent) {
	f.t.Helper()
	testsupport.Must(f.t, InsertEventTx(f.tx, e), "seeding a %s event", e.Kind)
}

// seedStepRulingEvents seeds the run's step-scoped rulings among events the
// step-event readers must skip, and returns what selecting step-resolved and
// lease-reaped yields. The first ruling names the later step, so seq order
// and step order disagree.
func seedStepRulingEvents(f *routingFixture) []StepEvent {
	f.t.Helper()
	first := f.add(StepRow{StepName: "implement", Status: StepDone})
	second := f.add(StepRow{StepName: "review", Status: StepDone})
	elsewhere := f.add(StepRow{RunID: f.otherRun, StepName: "implement", Status: StepDone})
	f.event("lease-reaped", second, `{"forced":true}`)
	f.event("step-claimed", first, `{}`)
	f.event("step-resolved", first, `{"detail":"override-pass"}`)
	reportEvent(f, NewEvent{AtMS: 1, Kind: "step-resolved", RunID: f.run, Data: `{"run":"level"}`})
	reportEvent(f, NewEvent{AtMS: 1, Kind: "step-resolved", RunID: f.otherRun, StepID: elsewhere, Data: `{}`})
	return []StepEvent{
		{StepID: second, Kind: "lease-reaped", Data: `{"forced":true}`},
		{StepID: first, Kind: "step-resolved", Data: `{"detail":"override-pass"}`},
	}
}

func TestReportQueriesRunStepEvents(t *testing.T) {
	f := newRoutingFixture(t)
	want := seedStepRulingEvents(f)
	db := sagaPool(f)

	got, err := RunStepEvents(db, f.run, "step-resolved", "lease-reaped")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("the run's step rulings = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	got, err = RunStepEvents(db, f.run, "step-approved")
	if err != nil || len(got) != 0 {
		t.Errorf("a kind the run never logged = (%+v, %v), want (none, nil)", got, err)
	}
	got, err = RunStepEvents(db, f.run)
	if err != nil || len(got) != 0 {
		t.Errorf("no kinds = (%+v, %v), want (none, nil)", got, err)
	}
}

func TestReportQueriesRunStepEventsTx(t *testing.T) {
	f := newRoutingFixture(t)
	want := seedStepRulingEvents(f)

	got, err := RunStepEventsTx(f.tx, f.run, "step-resolved", "lease-reaped")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("the run's step rulings = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	got, err = RunStepEventsTx(f.tx, f.run, "step-approved")
	if err != nil || len(got) != 0 {
		t.Errorf("a kind the run never logged = (%+v, %v), want (none, nil)", got, err)
	}
}

func TestReportQueriesRunEventDataTx(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "implement", Status: StepDone})
	reportEvent(f, NewEvent{AtMS: 1, Kind: "run-note-added", RunID: f.run, Data: `{"note":1}`})
	f.event("step-claimed", step, `{"authority":"skipped kind"}`)
	f.event("step-resolved", step, `{"authority":"operator"}`)
	reportEvent(f, NewEvent{AtMS: 1, Kind: "run-note-added", RunID: f.otherRun, Data: `{"note":2}`})

	got, err := RunEventDataTx(f.tx, f.run, "run-note-added", "step-resolved")
	want := []string{`{"note":1}`, `{"authority":"operator"}`}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("the run's notes and resolutions = (%q, %v), want (%q, nil)", got, err, want)
	}
	got, err = RunEventDataTx(f.tx, f.otherRun, "step-resolved")
	if err != nil || len(got) != 0 {
		t.Errorf("a kind the run never logged = (%q, %v), want (none, nil)", got, err)
	}
}

func TestReportQueriesRunIssueEventsTx(t *testing.T) {
	f := newRoutingFixture(t)
	decider := f.add(StepRow{StepName: "review", Status: StepFailedRouted})
	f.exec(`UPDATE steps SET routing = ? WHERE id = ?`, "abandon-issue: superseded", decider)
	reportEvent(f, NewEvent{AtMS: 20, Kind: "issue-abandoned", RunID: f.run, IssueID: f.otherIssue,
		Data: `{"reason":"run-level"}`})
	reportEvent(f, NewEvent{AtMS: 10, Kind: "issue-abandoned", RunID: f.run, IssueID: f.issue,
		StepID: decider, Data: `{"instance":"review@0"}`})
	reportEvent(f, NewEvent{AtMS: 30, Kind: "issue-abandoned", RunID: f.run, Data: `{"no":"issue"}`})
	reportEvent(f, NewEvent{AtMS: 40, Kind: "issue-abandoned", RunID: f.otherRun, IssueID: f.issue,
		Data: `{"other":"run"}`})
	reportEvent(f, NewEvent{AtMS: 50, Kind: "issue-reopened", RunID: f.run, IssueID: f.issue, Data: `{}`})

	got, err := RunIssueEventsTx(f.tx, f.run, "issue-abandoned")
	want := []IssueEvent{
		{IssueID: f.otherIssue, RunID: f.run, AtMS: 20, Data: `{"reason":"run-level"}`},
		{IssueID: f.issue, RunID: f.run, AtMS: 10, Data: `{"instance":"review@0"}`,
			StepRouting: "abandon-issue: superseded"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("the run's abandonments = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	got, err = RunIssueEventsTx(f.tx, f.otherRun, "issue-reopened")
	if err != nil || len(got) != 0 {
		t.Errorf("a kind the run never logged = (%+v, %v), want (none, nil)", got, err)
	}
}

func TestReportQueriesLatestIssueEvent(t *testing.T) {
	f := newRoutingFixture(t)
	decider := f.add(StepRow{IssueID: f.otherIssue, StepName: "review", Status: StepFailedRouted})
	f.exec(`UPDATE steps SET routing = ? WHERE id = ?`, "abandon-issue: replanned", decider)
	reportEvent(f, NewEvent{AtMS: 10, Kind: "issue-abandoned", RunID: f.run, IssueID: f.issue,
		Data: `{"first":true}`})
	reportEvent(f, NewEvent{AtMS: 5, Kind: "issue-abandoned", IssueID: f.issue, Data: `{"runless":true}`})
	reportEvent(f, NewEvent{AtMS: 30, Kind: "issue-reopened", RunID: f.run, IssueID: f.issue, Data: `{}`})
	reportEvent(f, NewEvent{AtMS: 40, Kind: "issue-abandoned", RunID: f.run, IssueID: f.otherIssue,
		StepID: decider, Data: `{"instance":"review@0"}`})
	db := sagaPool(f)

	got, found, err := LatestIssueEvent(db, f.issue, "issue-abandoned")
	want := IssueEvent{IssueID: f.issue, AtMS: 5, Data: `{"runless":true}`}
	if err != nil || !found || got != want {
		t.Errorf("the issue's newest abandonment = (%+v, %v, %v), want (%+v, true, nil)", got, found, err, want)
	}
	got, found, err = LatestIssueEvent(db, f.otherIssue, "issue-abandoned")
	want = IssueEvent{IssueID: f.otherIssue, RunID: f.run, AtMS: 40, Data: `{"instance":"review@0"}`,
		StepRouting: "abandon-issue: replanned"}
	if err != nil || !found || got != want {
		t.Errorf("the other issue's abandonment = (%+v, %v, %v), want (%+v, true, nil)", got, found, err, want)
	}
	got, found, err = LatestIssueEvent(db, f.otherIssue, "issue-reopened")
	if err != nil || found || got != (IssueEvent{}) {
		t.Errorf("a kind the issue never logged = (%+v, %v, %v), want (zero, false, nil)", got, found, err)
	}
}

func TestReportQueriesRunEventSpanMS(t *testing.T) {
	f := newRoutingFixture(t)
	for _, e := range []NewEvent{
		{AtMS: 100, Kind: "run-activated", RunID: f.run},
		{AtMS: 50, Kind: "run-activated", RunID: f.run},
		{AtMS: 300, Kind: "run-done", RunID: f.run},
		{AtMS: 200, Kind: "run-abandoned", RunID: f.run},
		{AtMS: 999, Kind: "run-paused", RunID: f.run},
		{AtMS: 5000, Kind: "run-done", RunID: f.otherRun},
	} {
		e.Data = `{}`
		reportEvent(f, e)
	}
	db := sagaPool(f)

	ask := func(name string, runID int, kinds []string, wantFirst, wantLast int64, wantFound bool) {
		t.Helper()
		first, last, found, err := RunEventSpanMS(db, runID, kinds...)
		if err != nil || first != wantFirst || last != wantLast || found != wantFound {
			t.Errorf("%s = (%d, %d, %v, %v), want (%d, %d, %v, nil)",
				name, first, last, found, err, wantFirst, wantLast, wantFound)
		}
	}
	ask("the activations", f.run, []string{"run-activated"}, 50, 100, true)
	ask("the ends", f.run, []string{"run-done", "run-abandoned"}, 200, 300, true)
	ask("a run never activated", f.otherRun, []string{"run-activated"}, 0, 0, false)
	ask("no kinds", f.run, nil, 0, 0, false)
}
