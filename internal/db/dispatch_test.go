package db

import (
	"reflect"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// dispatchQueryRun seeds a run of the given project and status through the
// fixture's transaction, holding the fixture's `issue`, and returns its id.
func dispatchQueryRun(f *routingFixture, projectID int, status model.RunStatus) int {
	f.t.Helper()
	var id int
	testsupport.Must(f.t, f.tx.QueryRow(
		`INSERT INTO runs (project_id, request, status, budget, usage_budget,
		                   created_at_ms, updated_at_ms, row_version,
		                   exec_root, branch, commit_sha, hostname)
		 VALUES (?, 'req', ?, 0, 0, 1000, 1000, 1, '', '', '', '') RETURNING id`,
		projectID, status).Scan(&id), "seeding a %s run", status)
	f.exec(`INSERT INTO run_issues (run_id, issue_id) VALUES (?, ?)`, id, f.issue)
	return id
}

// dispatchQueryProject seeds a second project and returns its id.
func dispatchQueryProject(f *routingFixture) int {
	f.t.Helper()
	var id int
	testsupport.Must(f.t, f.tx.QueryRow(
		`INSERT INTO projects (identity, name, prefix) VALUES ('/repo/other', 'other', 'OTH') RETURNING id`).
		Scan(&id), "seeding a project")
	return id
}

func dispatchQueryOpen(f *routingFixture, runID int) {
	f.t.Helper()
	_, err := InsertDispatchTx(f.tx, runID, 0, 5000, 1000)
	testsupport.Must(f.t, err, "opening a dispatch: %v", err)
}

func TestDispatchQueriesLastEventSeqTx(t *testing.T) {
	f := newRoutingFixture(t)

	seq, err := LastEventSeqTx(f.tx)
	if err != nil || seq != 0 {
		t.Fatalf("an empty event log's cursor = (%d, %v), want (0, nil)", seq, err)
	}

	f.event("step-claimed", 0, `{}`)
	reportEvent(f, NewEvent{AtMS: 1, Kind: "run-note-added", RunID: f.otherRun, Data: `{}`})
	var want int64
	testsupport.Must(t, f.tx.QueryRow(
		`SELECT seq FROM events WHERE run_id = ? AND kind = 'run-note-added'`, f.otherRun).Scan(&want),
		"reading the newest seq")

	seq, err = LastEventSeqTx(f.tx)
	if err != nil || seq != want {
		t.Errorf("the cursor over every run's events = (%d, %v), want (%d, nil)", seq, err, want)
	}
}

func TestDispatchQueriesRunEventIssueIDsTx(t *testing.T) {
	f := newRoutingFixture(t)
	reportEvent(f, NewEvent{AtMS: 1, Kind: "issue-abandoned", RunID: f.run, IssueID: f.issue, Data: `{}`})
	reportEvent(f, NewEvent{AtMS: 2, Kind: "issue-abandoned", RunID: f.run, IssueID: f.issue, Data: `{}`})
	reportEvent(f, NewEvent{AtMS: 3, Kind: "issue-abandoned", RunID: f.run, Data: `{"no":"issue"}`})
	reportEvent(f, NewEvent{AtMS: 4, Kind: "issue-reopened", RunID: f.run, IssueID: f.otherIssue, Data: `{}`})
	reportEvent(f, NewEvent{AtMS: 5, Kind: "issue-abandoned", RunID: f.otherRun, IssueID: f.otherIssue,
		Data: `{}`})

	got, err := RunEventIssueIDsTx(f.tx, f.run, "issue-abandoned")
	want := map[int]bool{f.issue: true}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("the run's abandoned issues = (%v, %v), want (%v, nil)", got, err, want)
	}
	got, err = RunEventIssueIDsTx(f.tx, f.otherRun, "issue-reopened")
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("a kind the run never logged = (%v, %v), want (an empty set, nil)", got, err)
	}
}

func TestDispatchQueriesPriorDispatchClosesTx(t *testing.T) {
	f := newRoutingFixture(t)
	reportEvent(f, NewEvent{AtMS: 1, Kind: "dispatch-closed", RunID: f.run, Data: `{"dispatch":"DISPATCH-1"}`})
	reportEvent(f, NewEvent{AtMS: 2, Kind: "dispatch-opened", RunID: f.run, Data: `{"dispatch":"DISPATCH-2"}`})
	reportEvent(f, NewEvent{AtMS: 3, Kind: "dispatch-closed", RunID: f.otherRun,
		Data: `{"dispatch":"DISPATCH-3"}`})
	reportEvent(f, NewEvent{AtMS: 4, Kind: "dispatch-closed", RunID: f.run, Data: `{"dispatch":"DISPATCH-2"}`})

	got, err := PriorDispatchClosesTx(f.tx, f.run, "dispatch-closed")
	want := []string{`{"dispatch":"DISPATCH-1"}`, `{"dispatch":"DISPATCH-2"}`}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("the run's closes = (%q, %v), want (%q, nil)", got, err, want)
	}
	got, err = PriorDispatchClosesTx(f.tx, f.otherRun, "dispatch-abandoned")
	if err != nil || len(got) != 0 {
		t.Errorf("a run that never logged the kind = (%q, %v), want (none, nil)", got, err)
	}
}

func TestDispatchQueriesProposalStatusTx(t *testing.T) {
	f := newRoutingFixture(t)
	now := time.Unix(1000, 0)
	for id, status := range map[int]model.ProposalStatus{
		7: model.ProposalStatusOpen, 8: model.ProposalStatusCommitted,
	} {
		_, err := InsertProposalWithID(f.tx, &model.Proposal{
			ID: id, Description: "vote", Criticality: model.CriticalityLow, Status: status,
			RequiredVoters: 1, Threshold: 0.5, CreatedAt: now, UpdatedAt: now,
		})
		testsupport.Must(t, err, "InsertProposalWithID %d: %v", id, err)
	}

	for id, want := range map[int]model.ProposalStatus{
		7: model.ProposalStatusOpen, 8: model.ProposalStatusCommitted,
	} {
		got, found, err := ProposalStatusTx(f.tx, id)
		if err != nil || !found || got != want {
			t.Errorf("proposal %d's status = (%q, %v, %v), want (%q, true, nil)", id, got, found, err, want)
		}
	}
	got, found, err := ProposalStatusTx(f.tx, 999)
	if err != nil || found || got != "" {
		t.Errorf("a missing proposal's status = (%q, %v, %v), want (\"\", false, nil)", got, found, err)
	}
}

func TestDispatchQueriesActiveStartedRunIDs(t *testing.T) {
	f := newRoutingFixture(t)
	dispatchQueryOpen(f, f.run)
	f.add(StepRow{RunID: f.otherRun, StepName: "implement", Status: StepPending})

	started := dispatchQueryRun(f, 1, model.RunActive)
	f.add(StepRow{RunID: started, StepName: "implement", Status: StepRunning})
	f.add(StepRow{RunID: started, StepName: "review", Status: StepPending})

	done := dispatchQueryRun(f, 1, model.RunDone)
	dispatchQueryOpen(f, done)
	abandoned := dispatchQueryRun(f, 1, model.RunAbandoned)
	f.add(StepRow{RunID: abandoned, StepName: "implement", Status: StepRunning})

	project := dispatchQueryProject(f)
	elsewhere := dispatchQueryRun(f, project, model.RunWaitingHuman)
	dispatchQueryOpen(f, elsewhere)
	db := sagaPool(f)

	got, err := ActiveStartedRunIDs(db, 0)
	want := []int{f.run, started, elsewhere}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("every project's started runs = (%v, %v), want (%v, nil)", got, err, want)
	}
	got, err = ActiveStartedRunIDs(db, 1)
	want = []int{f.run, started}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("the default project's started runs = (%v, %v), want (%v, nil)", got, err, want)
	}
	got, err = ActiveStartedRunIDs(db, project+1)
	if err != nil || len(got) != 0 {
		t.Errorf("a project with no runs = (%v, %v), want (none, nil)", got, err)
	}
}

func TestDispatchQueriesActiveGateSteps(t *testing.T) {
	f := newRoutingFixture(t)
	f.add(StepRow{StepName: "approve", Kind: "human", Status: StepPending})
	passed := f.add(StepRow{StepName: "approve", Ordinal: 1, Kind: "vote", Status: StepDone})
	f.exec(`UPDATE steps SET routing = 'pass: tallied' WHERE id = ?`, passed)
	f.add(StepRow{StepName: "approve", Ordinal: 2, Kind: "executor", Status: StepDone})
	f.add(StepRow{StepName: "ship", Kind: "human", Status: StepDone})
	f.add(StepRow{RunID: f.otherRun, StepName: "approve", Kind: "human", Status: StepWaitingHuman})

	done := dispatchQueryRun(f, 1, model.RunDone)
	f.add(StepRow{RunID: done, StepName: "approve", Kind: "human", Status: StepDone})

	project := dispatchQueryProject(f)
	elsewhere := dispatchQueryRun(f, project, model.RunActive)
	f.add(StepRow{RunID: elsewhere, StepName: "approve", Kind: "human", Status: StepFailedRouted})
	db := sagaPool(f)

	kinds := []string{"human", "vote"}
	inRun := []GateStepState{{Status: StepPending}, {Status: StepDone, Routing: "pass: tallied"}}
	inOtherRun := GateStepState{Status: StepWaitingHuman}
	inOtherProject := GateStepState{Status: StepFailedRouted}

	cases := []struct {
		name string
		q    GateStepQuery
		want []GateStepState
	}{
		{"every project", GateStepQuery{StepName: "approve", Kinds: kinds},
			append(append([]GateStepState{}, inRun...), inOtherRun, inOtherProject)},
		{"one project", GateStepQuery{StepName: "approve", Kinds: kinds, ProjectID: 1},
			append(append([]GateStepState{}, inRun...), inOtherRun)},
		{"one run", GateStepQuery{StepName: "approve", Kinds: kinds, RunID: f.run, ProjectID: 1}, inRun},
		{"a named run outside the project", GateStepQuery{StepName: "approve", Kinds: kinds,
			RunID: elsewhere, ProjectID: 1}, []GateStepState{inOtherProject}},
		{"one kind", GateStepQuery{StepName: "approve", Kinds: []string{"vote"}, RunID: f.run}, inRun[1:]},
	}
	for _, c := range cases {
		got, err := ActiveGateSteps(db, c.q)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: gate steps = (%+v, %v), want (%+v, nil)", c.name, got, err, c.want)
		}
	}

	got, err := ActiveGateSteps(db, GateStepQuery{StepName: "absent", Kinds: kinds})
	if err != nil || len(got) != 0 {
		t.Errorf("a gate no run declares = (%+v, %v), want (none, nil)", got, err)
	}
	got, err = ActiveGateSteps(db, GateStepQuery{StepName: "approve", Kinds: kinds, RunID: done})
	if err != nil || len(got) != 0 {
		t.Errorf("an ended run's gate = (%+v, %v), want (none, nil)", got, err)
	}
	got, err = ActiveGateSteps(db, GateStepQuery{StepName: "approve", RunID: f.run})
	if err != nil || len(got) != 0 {
		t.Errorf("no kinds = (%+v, %v), want (none, nil)", got, err)
	}
}
