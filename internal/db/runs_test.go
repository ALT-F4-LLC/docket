package db

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestPinContentSurvivesReopen pins the pinned file's bytes to the store, not
// the process: a second machine opening the same database renders from them.
func TestPinContentSurvivesReopen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		// A NUL and an invalid UTF-8 byte prove the column round-trips raw
		// bytes rather than text.
		{"binary", "# Charter\x00\xff\nWork as a senior engineer.\n"},
		// An empty file is still recorded content, distinct from none.
		{"empty file", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := migratedStoreFile(t)
			want := sql.NullString{String: tc.content, Valid: true}

			writer, err := Open(path)
			testsupport.Must(t, err, "Open: %v", err)
			run, err := InsertRun(writer, 1, "pin content", 0, 1)
			testsupport.Must(t, err, "InsertRun: %v", err)
			tx, err := writer.Begin()
			testsupport.Must(t, err, "Begin: %v", err)
			err = InsertPinTx(tx, Pin{
				RunID: run.ID, Kind: PinKindFile, Ref: "contracts/implement.md",
				SHA256: "b52312cc", Content: want,
			})
			testsupport.Must(t, err, "InsertPinTx: %v", err)
			err = tx.Commit()
			testsupport.Must(t, err, "Commit: %v", err)
			err = writer.Close()
			testsupport.Must(t, err, "Close: %v", err)

			reader, err := Open(path)
			testsupport.Must(t, err, "reopening: %v", err)
			t.Cleanup(func() { reader.Close() })

			pins, err := ListPins(reader, run.ID)
			testsupport.Must(t, err, "ListPins: %v", err)
			if len(pins) != 1 || pins[0].Content != want {
				t.Errorf("ListPins = %+v, want one pin with content %+v", pins, want)
			}

			var storage string
			err = reader.QueryRow(`SELECT typeof(content) FROM pins WHERE run_id = ?`,
				run.ID).Scan(&storage)
			testsupport.Must(t, err, "reading storage class: %v", err)
			if storage != "blob" {
				t.Errorf("pins.content stored as %s, want blob", storage)
			}

			tx, err = reader.Begin()
			testsupport.Must(t, err, "Begin: %v", err)
			defer tx.Rollback()
			txPins, err := ListPinsTx(tx, run.ID)
			testsupport.Must(t, err, "ListPinsTx: %v", err)
			if len(txPins) != 1 || txPins[0].Content != want {
				t.Errorf("ListPinsTx = %+v, want one pin with content %+v", txPins, want)
			}
		})
	}
}

// lifecycleEvent appends one event through the fixture's transaction and
// returns its seq.
func lifecycleEvent(f *routingFixture, e NewEvent) int64 {
	f.t.Helper()
	testsupport.Must(f.t, InsertEventTx(f.tx, e), "seeding a %s event", e.Kind)
	var seq int64
	testsupport.Must(f.t, f.tx.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&seq), "reading the seq")
	return seq
}

// stepState is the columns an abandonment writes.
type stepState struct {
	Status                  string
	Routing                 string
	UpdatedAtMS, RecordedMS int64
	RowVersion              int
}

func readStepState(f *routingFixture, id int) stepState {
	f.t.Helper()
	var s stepState
	testsupport.Must(f.t, f.tx.QueryRow(
		`SELECT status, COALESCE(routing, ''), updated_at_ms, COALESCE(recorded_at_ms, 0), row_version
		   FROM steps WHERE id = ?`, id).Scan(
		&s.Status, &s.Routing, &s.UpdatedAtMS, &s.RecordedMS, &s.RowVersion),
		"reading step %d", id)
	return s
}

func TestRunLifecycleQueriesRunWorktreesTx(t *testing.T) {
	f := newRoutingFixture(t)
	for _, seed := range []struct {
		run, issue int
		name, root string
	}{
		{f.run, f.issue, "implement", "/wt/b"},
		{f.run, f.issue, "fix", "/wt/a"},
		{f.run, f.issue, "review", "/wt/a"},
		{f.run, f.issue, "verify", ""},
		{f.run, f.otherIssue, "implement", "/wt/c"},
		{f.otherRun, f.issue, "implement", "/wt/z"},
	} {
		id := f.add(StepRow{RunID: seed.run, IssueID: seed.issue, StepName: seed.name, Status: StepDone})
		f.exec(`UPDATE steps SET work_root = ? WHERE id = ?`, seed.root, id)
	}

	ask := func(name string, runID, issueID int, want []string) {
		t.Helper()
		got, err := RunWorktreesTx(f.tx, runID, issueID)
		if err != nil || len(got) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
			t.Errorf("%s = (%q, %v), want (%q, nil)", name, got, err, want)
		}
	}
	ask("the whole run's worktrees", f.run, 0, []string{"/wt/a", "/wt/b", "/wt/c"})
	ask("one issue's worktrees", f.run, f.issue, []string{"/wt/a", "/wt/b"})
	ask("an issue that recorded none", f.otherRun, f.otherIssue, nil)
	ask("a run with no steps", 999, 0, nil)
}

func TestRunLifecycleQueriesRunHasIssueTx(t *testing.T) {
	f := newRoutingFixture(t)

	ask := func(name string, runID, issueID int, want bool) {
		t.Helper()
		got, err := RunHasIssueTx(f.tx, runID, issueID)
		if err != nil || got != want {
			t.Errorf("%s = (%v, %v), want (%v, nil)", name, got, err, want)
		}
	}
	ask("a member issue", f.run, f.otherIssue, true)
	ask("the other run's member", f.otherRun, f.issue, true)
	ask("an issue outside the run", f.otherRun, f.otherIssue, false)
	ask("a run that does not exist", 999, f.issue, false)
}

func TestRunLifecycleQueriesIssueUnfinishedStepsTx(t *testing.T) {
	f := newRoutingFixture(t)
	parked := f.add(StepRow{StepName: "review", Status: StepWaitingHuman})
	for _, status := range []string{StepDone, StepSkipped, StepSuperseded, StepFailedRouted} {
		f.add(StepRow{StepName: "done-" + status, Status: status})
	}
	pending := f.add(StepRow{StepName: "verify", Status: StepPending})
	claimed := f.add(StepRow{StepName: "implement", Status: StepClaimed})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "implement", Status: StepPending})
	f.add(StepRow{RunID: f.otherRun, StepName: "implement", Status: StepPending})
	f.add(StepRow{RunID: f.otherRun, IssueID: f.otherIssue, StepName: "implement", Status: StepDone})

	got, err := IssueUnfinishedStepsTx(f.tx, f.run, f.issue)
	want := []UnfinishedStep{
		{ID: parked, Instance: "review@0", Status: StepWaitingHuman},
		{ID: pending, Instance: "verify@0", Status: StepPending},
		{ID: claimed, Instance: "implement@0", Status: StepClaimed},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("the issue's unfinished steps = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	got, err = IssueUnfinishedStepsTx(f.tx, f.otherRun, f.otherIssue)
	if err != nil || len(got) != 0 {
		t.Errorf("an issue whose steps are all terminal = (%+v, %v), want (none, nil)", got, err)
	}
}

func TestRunLifecycleQueriesFailRouteIssueStepsTx(t *testing.T) {
	f := newRoutingFixture(t)
	pending := f.add(StepRow{StepName: "verify", Status: StepPending})
	f.exec(`UPDATE steps SET routing = 'retry: flaky' WHERE id = ?`, pending)
	parked := f.add(StepRow{StepName: "review", Status: StepWaitingHuman})
	done := f.add(StepRow{StepName: "implement", Status: StepDone})
	sibling := f.add(StepRow{IssueID: f.otherIssue, StepName: "implement", Status: StepPending})
	elsewhere := f.add(StepRow{RunID: f.otherRun, StepName: "implement", Status: StepPending})
	before := map[int]stepState{}
	for _, id := range []int{done, sibling, elsewhere} {
		before[id] = readStepState(f, id)
	}

	n, err := FailRouteIssueStepsTx(f.tx, f.run, f.issue, 5000)
	if err != nil || n != 2 {
		t.Fatalf("abandoning the issue = (%d, %v), want (2, nil)", n, err)
	}
	if got, want := readStepState(f, pending), (stepState{
		Status: StepFailedRouted, Routing: "retry: flaky", UpdatedAtMS: 5000, RecordedMS: 5000, RowVersion: 2,
	}); got != want {
		t.Errorf("the pending step = %+v, want %+v", got, want)
	}
	if got, want := readStepState(f, parked), (stepState{
		Status: StepFailedRouted, UpdatedAtMS: 5000, RecordedMS: 5000, RowVersion: 2,
	}); got != want {
		t.Errorf("the parked step = %+v, want %+v", got, want)
	}
	for id, want := range before {
		if got := readStepState(f, id); got != want {
			t.Errorf("step %d = %+v, want it untouched at %+v", id, got, want)
		}
	}

	n, err = FailRouteIssueStepsTx(f.tx, f.run, f.issue, 6000)
	if err != nil || n != 0 {
		t.Errorf("abandoning an issue with nothing left = (%d, %v), want (0, nil)", n, err)
	}
}

func TestRunLifecycleQueriesRunEventsTx(t *testing.T) {
	f := newRoutingFixture(t)
	activated := lifecycleEvent(f, NewEvent{AtMS: 10, Kind: "run-activated", RunID: f.run, Data: `{"a":1}`})
	lifecycleEvent(f, NewEvent{AtMS: 15, Kind: "run-paused", RunID: f.run, Data: `{}`})
	repinned := lifecycleEvent(f, NewEvent{AtMS: 20, Kind: "run-repinned", RunID: f.run, Data: `{"ref":"x"}`})
	lifecycleEvent(f, NewEvent{AtMS: 25, Kind: "run-repinned", RunID: f.otherRun, Data: `{"ref":"y"}`})

	got, err := RunEventsTx(f.tx, f.run, "run-activated", "run-repinned")
	want := []RunEvent{
		{Seq: activated, AtMS: 10, Kind: "run-activated", Data: `{"a":1}`},
		{Seq: repinned, AtMS: 20, Kind: "run-repinned", Data: `{"ref":"x"}`},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("the run's pin history = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	got, err = RunEventsTx(f.tx, f.otherRun, "run-activated")
	if err != nil || len(got) != 0 {
		t.Errorf("a kind the run never logged = (%+v, %v), want (none, nil)", got, err)
	}
	got, err = RunEventsTx(f.tx, f.run)
	if err != nil || len(got) != 0 {
		t.Errorf("no kinds = (%+v, %v), want (none, nil)", got, err)
	}
}

func TestRunLifecycleQueriesRunStepEventSeqsTx(t *testing.T) {
	f := newRoutingFixture(t)
	first := f.add(StepRow{StepName: "implement", Status: StepDone})
	second := f.add(StepRow{StepName: "review", Status: StepDone})
	elsewhere := f.add(StepRow{RunID: f.otherRun, StepName: "implement", Status: StepDone})
	claimed := f.event("step-claimed", second, `{}`)
	lifecycleEvent(f, NewEvent{AtMS: 1, Kind: "run-paused", RunID: f.run, Data: `{}`})
	recorded := f.event("step-recorded", first, `{}`)
	lifecycleEvent(f, NewEvent{AtMS: 1, Kind: "step-claimed", RunID: f.otherRun, StepID: elsewhere, Data: `{}`})

	got, err := RunStepEventSeqsTx(f.tx, f.run)
	want := []StepEventSeq{
		{StepID: second, Kind: "step-claimed", Seq: claimed},
		{StepID: first, Kind: "step-recorded", Seq: recorded},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("the run's step events = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	got, err = RunStepEventSeqsTx(f.tx, 999)
	if err != nil || len(got) != 0 {
		t.Errorf("a run with no step events = (%+v, %v), want (none, nil)", got, err)
	}
}

// seedLifecyclePins pins one ref in both fixture runs, with stored bytes.
func seedLifecyclePins(f *routingFixture) {
	f.t.Helper()
	for _, runID := range []int{f.run, f.otherRun} {
		testsupport.Must(f.t, InsertPinTx(f.tx, Pin{
			RunID: runID, Kind: PinKindFile, Ref: "contracts/implement.md", SHA256: "old",
			Content: sql.NullString{String: "old bytes", Valid: true},
		}), "InsertPinTx")
	}
}

func listLifecyclePins(f *routingFixture, runID int) []Pin {
	f.t.Helper()
	pins, err := ListPinsTx(f.tx, runID)
	testsupport.Must(f.t, err, "ListPinsTx: %v", err)
	return pins
}

func TestRunLifecycleQueriesRepinPinTx(t *testing.T) {
	f := newRoutingFixture(t)
	seedLifecyclePins(f)
	untouched := listLifecyclePins(f, f.otherRun)

	n, err := RepinPinTx(f.tx, f.run, PinKindFile, "contracts/implement.md", "stale", "new")
	if err != nil || n != 0 {
		t.Errorf("repinning from a hash the run does not hold = (%d, %v), want (0, nil)", n, err)
	}
	n, err = RepinPinTx(f.tx, f.run, PinKindFile, "contracts/implement.md", "old", "new")
	if err != nil || n != 1 {
		t.Errorf("repinning from the held hash = (%d, %v), want (1, nil)", n, err)
	}
	want := []Pin{{RunID: f.run, Kind: PinKindFile, Ref: "contracts/implement.md", SHA256: "new"}}
	if got := listLifecyclePins(f, f.run); !reflect.DeepEqual(got, want) {
		t.Errorf("the repinned run's pins = %+v, want %+v", got, want)
	}
	if got := listLifecyclePins(f, f.otherRun); !reflect.DeepEqual(got, untouched) {
		t.Errorf("the other run's pins = %+v, want them untouched at %+v", got, untouched)
	}
}

func TestRunLifecycleQueriesDropPinTx(t *testing.T) {
	f := newRoutingFixture(t)
	seedLifecyclePins(f)
	untouched := listLifecyclePins(f, f.otherRun)

	n, err := DropPinTx(f.tx, f.run, PinKindFile, "contracts/implement.md", "stale")
	if err != nil || n != 0 {
		t.Errorf("dropping from a hash the run does not hold = (%d, %v), want (0, nil)", n, err)
	}
	n, err = DropPinTx(f.tx, f.run, PinKindFile, "contracts/implement.md", "old")
	if err != nil || n != 1 {
		t.Errorf("dropping the held hash = (%d, %v), want (1, nil)", n, err)
	}
	if got := listLifecyclePins(f, f.run); len(got) != 0 {
		t.Errorf("the dropped run's pins = %+v, want none", got)
	}
	if got := listLifecyclePins(f, f.otherRun); !reflect.DeepEqual(got, untouched) {
		t.Errorf("the other run's pins = %+v, want them untouched at %+v", got, untouched)
	}
}

func TestRunLifecycleQueriesAddPinTx(t *testing.T) {
	f := newRoutingFixture(t)
	seedLifecyclePins(f)

	n, err := AddPinTx(f.tx, Pin{RunID: f.run, Kind: PinKindFile, Ref: "contracts/fix.md", SHA256: "added"})
	if err != nil || n != 1 {
		t.Errorf("pinning a new ref = (%d, %v), want (1, nil)", n, err)
	}
	n, err = AddPinTx(f.tx, Pin{RunID: f.run, Kind: PinKindFile, Ref: "contracts/implement.md", SHA256: "other"})
	if err != nil || n != 0 {
		t.Errorf("pinning a ref the run already holds = (%d, %v), want (0, nil)", n, err)
	}
	want := []Pin{
		{RunID: f.run, Kind: PinKindFile, Ref: "contracts/fix.md", SHA256: "added"},
		{RunID: f.run, Kind: PinKindFile, Ref: "contracts/implement.md", SHA256: "old",
			Content: sql.NullString{String: "old bytes", Valid: true}},
	}
	if got := listLifecyclePins(f, f.run); !reflect.DeepEqual(got, want) {
		t.Errorf("the run's pins = %+v, want %+v", got, want)
	}
}

func TestRunLifecycleQueriesRunUnfinishedStepCountTx(t *testing.T) {
	f := newRoutingFixture(t)
	f.add(StepRow{StepName: "review", Status: StepWaitingHuman})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "implement", Status: StepPending})
	for _, status := range []string{StepDone, StepSkipped, StepSuperseded, StepFailedRouted} {
		f.add(StepRow{StepName: "done-" + status, Status: status})
	}
	f.add(StepRow{RunID: f.otherRun, StepName: "implement", Status: StepDone})

	ask := func(name string, runID, want int) {
		t.Helper()
		got, err := RunUnfinishedStepCountTx(f.tx, runID)
		if err != nil || got != want {
			t.Errorf("%s = (%d, %v), want (%d, nil)", name, got, err, want)
		}
	}
	ask("the run's unfinished steps", f.run, 2)
	ask("a run whose steps are all terminal", f.otherRun, 0)
	ask("a run with no steps", 999, 0)
}
