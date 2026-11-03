package db

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
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

func TestRunSideQueriesEventDataTx(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "implement", Status: StepClaimed})
	forced := f.event("lease-reaped", step, `{"forced":true}`)
	expired := f.event("lease-reaped", step, `{"forced":false}`)

	for seq, want := range map[int64]string{forced: `{"forced":true}`, expired: `{"forced":false}`} {
		got, err := EventDataTx(f.tx, seq)
		if err != nil || got != want {
			t.Errorf("the payload at seq %d = (%q, %v), want (%q, nil)", seq, got, err, want)
		}
	}
	got, err := EventDataTx(f.tx, expired+1)
	if !errors.Is(err, ErrNotFound) || got != "" {
		t.Errorf("a seq the log never reached = (%q, %v), want (\"\", ErrNotFound)", got, err)
	}
}

func TestRunSideQueriesClaimAdmittedByDispatchTx(t *testing.T) {
	f := newRoutingFixture(t)
	admitted := f.add(StepRow{StepName: "implement", Status: StepClaimed})
	early := f.add(StepRow{StepName: "review", Status: StepClaimed})
	offManifest := f.add(StepRow{StepName: "verify", Status: StepClaimed})
	unclaimed := f.add(StepRow{StepName: "fix", Status: StepPending})
	f.event("step-claimed", early, `{}`)
	opened := lifecycleEvent(f, NewEvent{AtMS: 1, Kind: "dispatch-opened", RunID: f.run, Data: `{}`})
	claim := f.event("step-claimed", admitted, `{}`)
	f.event("step-claimed", offManifest, `{}`)
	f.event("step-recorded", early, `{}`)
	reaped := f.event("lease-reaped", admitted, `{}`)

	dispatchID, err := InsertDispatchTx(f.tx, f.run, opened, 99999, 1000)
	testsupport.Must(t, err, "InsertDispatchTx: %v", err)
	for i, stepID := range []int{admitted, early, unclaimed} {
		testsupport.Must(t, InsertDispatchRowTx(f.tx, dispatchID, DispatchRow{
			Position: i, StepID: stepID, Instance: "row", RowJSON: `{}`, RowSHA256: "sha",
		}), "InsertDispatchRowTx %d", i)
	}

	ask := func(name string, dispatchID, stepID int, reapedSeq int64, want bool) {
		t.Helper()
		got, err := ClaimAdmittedByDispatchTx(f.tx, dispatchID, opened, stepID, "step-claimed", reapedSeq)
		if err != nil || got != want {
			t.Errorf("%s = (%v, %v), want (%v, nil)", name, got, err, want)
		}
	}
	ask("a manifest step claimed after the open", dispatchID, admitted, reaped, true)
	ask("a reap at the claim's own seq", dispatchID, admitted, claim, false)
	ask("a manifest step last claimed before the open", dispatchID, early, reaped, false)
	ask("a step claimed after the open but off the manifest", dispatchID, offManifest, reaped, false)
	ask("a manifest step never claimed", dispatchID, unclaimed, reaped, false)
	ask("a dispatch that does not exist", 999, admitted, reaped, false)
}

// seedSideQueryVotes stores proposals 7 and 8 and their casts: seats a and b
// on 7, seat c on 8.
func seedSideQueryVotes(f *routingFixture) {
	f.t.Helper()
	now := time.Unix(1000, 0)
	for _, id := range []int{7, 8} {
		_, err := InsertProposalWithID(f.tx, &model.Proposal{
			ID: id, Description: "vote", Criticality: model.CriticalityLow,
			Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5,
			CreatedAt: now, UpdatedAt: now,
		})
		testsupport.Must(f.t, err, "InsertProposalWithID %d: %v", id, err)
	}
	for _, v := range []struct {
		id, proposal int
		voter        string
	}{{70, 7, "seat-a"}, {71, 7, "seat-b"}, {80, 8, "seat-c"}} {
		_, err := InsertVoteWithID(f.tx, &model.Vote{
			ID: v.id, ProposalID: v.proposal, VoterName: v.voter,
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9, CreatedAt: now,
		})
		testsupport.Must(f.t, err, "InsertVoteWithID %d: %v", v.id, err)
	}
}

func TestRunSideQueriesProposalExistsTx(t *testing.T) {
	f := newRoutingFixture(t)
	seedSideQueryVotes(f)

	for id, want := range map[int]bool{7: true, 8: true, 999: false} {
		got, err := ProposalExistsTx(f.tx, id)
		if err != nil || got != want {
			t.Errorf("proposal %d exists = (%v, %v), want (%v, nil)", id, got, err, want)
		}
	}
}

func TestRunSideQueriesVoteIDTx(t *testing.T) {
	f := newRoutingFixture(t)
	seedSideQueryVotes(f)

	ask := func(name string, proposalID int, voter string, wantID int64, wantFound bool) {
		t.Helper()
		id, found, err := VoteIDTx(f.tx, proposalID, voter)
		if err != nil || found != wantFound || id != wantID {
			t.Errorf("%s = (%d, %v, %v), want (%d, %v, nil)", name, id, found, err, wantID, wantFound)
		}
	}
	ask("seat a's cast", 7, "seat-a", 70, true)
	ask("seat b's cast", 7, "seat-b", 71, true)
	ask("a seat that cast only on another proposal", 7, "seat-c", 0, false)
	ask("a proposal that does not exist", 999, "seat-a", 0, false)
}

func TestRunSideQueriesListFencesTx(t *testing.T) {
	f := newRoutingFixture(t)
	for _, fence := range []RunFence{
		{RunID: f.run, IssueID: f.otherIssue, Tag: "ac", Ordinal: 0, Command: "make c", SHA256: "c"},
		{RunID: f.run, IssueID: f.issue, Tag: "tests", Ordinal: 1, Command: "make b2", SHA256: "b2"},
		{RunID: f.run, IssueID: f.issue, Tag: "ac", Ordinal: 0, Command: "make a", SHA256: "a"},
		{RunID: f.run, IssueID: f.issue, Tag: "tests", Ordinal: 0, Command: "make b1", SHA256: "b1"},
		{RunID: f.otherRun, IssueID: f.issue, Tag: "ac", Ordinal: 0, Command: "make z", SHA256: "z"},
	} {
		testsupport.Must(t, InsertFenceTx(f.tx, fence), "InsertFenceTx %s#%d", fence.Tag, fence.Ordinal)
	}
	lo, hi := f.issue, f.otherIssue
	if lo > hi {
		lo, hi = hi, lo
	}
	byIssue := map[int][]RunFence{
		f.issue: {
			{RunID: f.run, IssueID: f.issue, Tag: "ac", Ordinal: 0, Command: "make a", SHA256: "a"},
			{RunID: f.run, IssueID: f.issue, Tag: "tests", Ordinal: 0, Command: "make b1", SHA256: "b1"},
			{RunID: f.run, IssueID: f.issue, Tag: "tests", Ordinal: 1, Command: "make b2", SHA256: "b2"},
		},
		f.otherIssue: {
			{RunID: f.run, IssueID: f.otherIssue, Tag: "ac", Ordinal: 0, Command: "make c", SHA256: "c"},
		},
	}
	want := append(append([]RunFence{}, byIssue[lo]...), byIssue[hi]...)

	got, err := ListFencesTx(f.tx, f.run)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("the run's fences = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	got, err = ListFencesTx(f.tx, 999)
	if err != nil || len(got) != 0 {
		t.Errorf("a run with no fences = (%+v, %v), want (none, nil)", got, err)
	}
}

func TestRunSideQueriesRunEventDetailExistsTx(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "review", Status: StepDone})
	lifecycleEvent(f, NewEvent{AtMS: 1, Kind: "vote-opened", RunID: f.run, Data: `{"detail":"DKT-V7"}`})
	lifecycleEvent(f, NewEvent{AtMS: 1, Kind: "vote-tallied", RunID: f.run, Data: `{"detail":"DKT-V8"}`})
	lifecycleEvent(f, NewEvent{AtMS: 1, Kind: "vote-opened", RunID: f.run, StepID: step, Data: `{"detail":"DKT-V9"}`})
	lifecycleEvent(f, NewEvent{AtMS: 1, Kind: "vote-opened", RunID: f.otherRun, Data: `{"detail":"DKT-V10"}`})

	ask := func(name string, runID int, detail string, want bool) {
		t.Helper()
		got, err := RunEventDetailExistsTx(f.tx, runID, "vote-opened", detail)
		if err != nil || got != want {
			t.Errorf("%s = (%v, %v), want (%v, nil)", name, got, err, want)
		}
	}
	ask("the run's opened panel", f.run, "DKT-V7", true)
	ask("a panel the run only tallied", f.run, "DKT-V8", false)
	ask("a panel opened on a step", f.run, "DKT-V9", false)
	ask("a panel opened on another run", f.run, "DKT-V10", false)
	ask("the other run's opened panel", f.otherRun, "DKT-V10", true)
	ask("a panel no run opened", f.run, "DKT-V11", false)
}
