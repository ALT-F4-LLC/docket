package db

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

const sagaDiffKind = "issue.diff"

// sagaArtifact records one artifact for a step through the fixture's
// transaction and returns its id.
func sagaArtifact(f *routingFixture, stepID int, kind, body, payload string) int {
	f.t.Helper()
	var runID int
	testsupport.Must(f.t, f.tx.QueryRow(`SELECT run_id FROM steps WHERE id = ?`, stepID).Scan(&runID),
		"reading step %d's run", stepID)
	id, err := InsertArtifactTx(f.tx, Artifact{
		RunID: runID, StepID: stepID, Kind: kind, Body: body, Payload: payload, SHA256: "sha",
	}, 1000)
	testsupport.Must(f.t, err, "InsertArtifactTx: %v", err)
	return id
}

// sagaPool commits what the fixture seeded and hands back the pool, for the
// helpers that read outside a transaction: the pool's one connection belongs
// to the fixture's transaction until it ends.
func sagaPool(f *routingFixture) *sql.DB {
	f.t.Helper()
	testsupport.Must(f.t, f.tx.Commit(), "committing the seeded rows")
	return f.db
}

func TestSagaQueriesSetStepWorkRootTx(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "implement", Status: StepRunning})
	other := f.add(StepRow{StepName: "review", Status: StepPending})

	testsupport.Must(t, SetStepWorkRootTx(f.tx, step, "/work/tree"), "SetStepWorkRootTx")

	read := func(id int) string {
		t.Helper()
		var root sql.NullString
		testsupport.Must(t, f.tx.QueryRow(`SELECT work_root FROM steps WHERE id = ?`, id).Scan(&root),
			"reading step %d's work_root", id)
		return root.String
	}
	if got := read(step); got != "/work/tree" {
		t.Errorf("the recorded work_root = %q, want /work/tree", got)
	}
	if got := read(other); got != "" {
		t.Errorf("another step's work_root = %q, want it untouched", got)
	}
	if err := SetStepWorkRootTx(f.tx, 999, "/work/tree"); !errors.Is(err, ErrStepNotFound) {
		t.Errorf("a missing step's error = %v, want ErrStepNotFound", err)
	}
}

func TestSagaQueriesStepArtifactCount(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "implement", Status: StepDone})
	other := f.add(StepRow{StepName: "review", Status: StepDone})
	sagaArtifact(f, step, "gap", "one", "")
	sagaArtifact(f, step, "gap", "two", "")
	sagaArtifact(f, step, "change-summary", "emit", "")
	sagaArtifact(f, other, "gap", "elsewhere", "")
	db := sagaPool(f)

	got, err := StepArtifactCount(db, step, "gap")
	if err != nil || got != 2 {
		t.Errorf("the step's gaps = (%d, %v), want (2, nil)", got, err)
	}
	got, err = StepArtifactCount(db, step, "absent")
	if err != nil || got != 0 {
		t.Errorf("a kind the step never recorded = (%d, %v), want (0, nil)", got, err)
	}
}

func TestSagaQueriesNewestStepArtifactBody(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "implement", Status: StepDone})
	other := f.add(StepRow{StepName: "review", Status: StepDone})
	sagaArtifact(f, step, "change-summary", "first attempt", "")
	sagaArtifact(f, step, "change-summary", "second attempt", "")
	sagaArtifact(f, step, "gap", "a gap", "")
	sagaArtifact(f, other, "change-summary", "another step", "")
	db := sagaPool(f)

	body, found, err := NewestStepArtifactBody(db, step, "change-summary")
	if err != nil || !found || body != "second attempt" {
		t.Errorf("the newest emit = (%q, %v, %v), want (\"second attempt\", true, nil)", body, found, err)
	}
	body, found, err = NewestStepArtifactBody(db, step, "absent")
	if err != nil || found || body != "" {
		t.Errorf("a kind the step never recorded = (%q, %v, %v), want (\"\", false, nil)", body, found, err)
	}
}

func TestSagaQueriesNextGateOrdinalTx(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "implement", Status: StepRunning})
	other := f.add(StepRow{StepName: "review", Status: StepRunning})
	for _, r := range []GateResultRow{
		{StepID: step, Gate: "tests", Ordinal: 0},
		{StepID: step, Gate: "tests", Ordinal: 2},
		{StepID: step, Gate: "build", Ordinal: 5},
		{StepID: other, Gate: "tests", Ordinal: 7},
	} {
		r.RunID, r.Verdict = f.run, GateVerdictPass
		testsupport.Must(t, InsertGateResultTx(f.tx, r), "InsertGateResultTx")
	}

	got, err := NextGateOrdinalTx(f.tx, step, "tests")
	if err != nil || got != 3 {
		t.Errorf("the next tests ordinal = (%d, %v), want (3, nil)", got, err)
	}
	got, err = NextGateOrdinalTx(f.tx, step, "lint")
	if err != nil || got != 0 {
		t.Errorf("a gate with no result = (%d, %v), want (0, nil)", got, err)
	}
}

func TestSagaQueriesNewestStepPayload(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "review", Status: StepDone})
	bare := f.add(StepRow{StepName: "verify", Status: StepDone})
	empty := f.add(StepRow{StepName: "aggregate", Status: StepDone})
	sagaArtifact(f, step, "findings", "", `{"old":true}`)
	sagaArtifact(f, step, "findings", "", `{"new":true}`)
	sagaArtifact(f, step, "gap", "a gap", "")
	sagaArtifact(f, bare, "findings", "no payload", "")
	db := sagaPool(f)

	payload, found, err := NewestStepPayload(db, step, "findings")
	if err != nil || !found || payload != `{"new":true}` {
		t.Errorf("the newest findings payload = (%q, %v, %v), want ({\"new\":true}, true, nil)", payload, found, err)
	}
	payload, found, err = NewestStepPayload(db, step, "")
	if err != nil || !found || payload != "" {
		t.Errorf("any kind's newest payload = (%q, %v, %v), want the gap's (\"\", true, nil)", payload, found, err)
	}
	payload, found, err = NewestStepPayload(db, bare, "findings")
	if err != nil || !found || payload != "" {
		t.Errorf("an artifact with no payload = (%q, %v, %v), want (\"\", true, nil)", payload, found, err)
	}
	payload, found, err = NewestStepPayload(db, empty, "")
	if err != nil || found || payload != "" {
		t.Errorf("a step with no artifact = (%q, %v, %v), want (\"\", false, nil)", payload, found, err)
	}
}

func TestSagaQueriesRunIssueSnapshot(t *testing.T) {
	f := newRoutingFixture(t)
	f.exec(`UPDATE run_issues SET issue_snapshot = ? WHERE run_id = ? AND issue_id = ?`,
		`{"scope":["a/**"]}`, f.run, f.issue)
	db := sagaPool(f)

	got, err := RunIssueSnapshot(db, f.run, f.issue)
	if err != nil || got != `{"scope":["a/**"]}` {
		t.Errorf("the frozen snapshot = (%q, %v), want ({\"scope\":[\"a/**\"]}, nil)", got, err)
	}
	got, err = RunIssueSnapshot(db, f.otherRun, f.issue)
	if err != nil || got != "" {
		t.Errorf("an unfrozen snapshot = (%q, %v), want (\"\", nil)", got, err)
	}
	if _, err := RunIssueSnapshot(db, f.otherRun, f.otherIssue); !errors.Is(err, ErrNotFound) {
		t.Errorf("an issue outside the run = %v, want ErrNotFound", err)
	}
}

func TestSagaQueriesNewestIssueArtifactPayloadExcept(t *testing.T) {
	f := newRoutingFixture(t)
	first := f.add(StepRow{StepName: "implement", Status: StepDone})
	second := f.add(StepRow{StepName: "fix", Ordinal: 1, Status: StepDone})
	elsewhere := f.add(StepRow{IssueID: f.otherIssue, StepName: "implement", Status: StepDone})
	otherRun := f.add(StepRow{RunID: f.otherRun, StepName: "implement", Status: StepDone})
	sagaArtifact(f, first, sagaDiffKind, "d1", `{"head":"one"}`)
	sagaArtifact(f, second, sagaDiffKind, "d2", `{"head":"two"}`)
	sagaArtifact(f, second, "change-summary", "s", `{"head":"summary"}`)
	sagaArtifact(f, elsewhere, sagaDiffKind, "dx", `{"head":"other issue"}`)
	sagaArtifact(f, otherRun, sagaDiffKind, "dr", `{"head":"other run"}`)
	db := sagaPool(f)

	got, found, err := NewestIssueArtifactPayloadExcept(db, f.run, f.issue, sagaDiffKind, second)
	if err != nil || !found || got != `{"head":"one"}` {
		t.Errorf("the newest diff but the second step's = (%q, %v, %v), want the first's", got, found, err)
	}
	got, found, err = NewestIssueArtifactPayloadExcept(db, f.run, f.issue, sagaDiffKind, first)
	if err != nil || !found || got != `{"head":"two"}` {
		t.Errorf("the newest diff but the first step's = (%q, %v, %v), want the second's", got, found, err)
	}
	got, found, err = NewestIssueArtifactPayloadExcept(db, f.run, f.otherIssue, sagaDiffKind, elsewhere)
	if err != nil || found || got != "" {
		t.Errorf("an issue whose only diff is excepted = (%q, %v, %v), want (\"\", false, nil)", got, found, err)
	}
}

func TestSagaQueriesNewestIssueArtifactConsumedByDoneStep(t *testing.T) {
	f := newRoutingFixture(t)
	producer := f.add(StepRow{StepName: "implement", Status: StepDone})
	judged := sagaArtifact(f, producer, sagaDiffKind, "d1", `{"head":"judged"}`)
	newer := sagaArtifact(f, producer, sagaDiffKind, "d2", `{"head":"unjudged"}`)

	reviewer := f.add(StepRow{StepName: "review", Status: StepDone})
	testsupport.Must(t, InsertStepInputTx(f.tx, reviewer, 0, judged), "InsertStepInputTx")
	running := f.add(StepRow{StepName: "review", Ordinal: 1, Status: StepRunning})
	testsupport.Must(t, InsertStepInputTx(f.tx, running, 0, newer), "InsertStepInputTx")
	fixer := f.add(StepRow{StepName: "fix", Ordinal: 1, Status: StepDone})
	testsupport.Must(t, InsertStepInputTx(f.tx, fixer, 0, newer), "InsertStepInputTx")
	sagaArtifact(f, fixer, sagaDiffKind, "d3", `{"head":"fixer"}`)
	db := sagaPool(f)

	id, payload, found, err := NewestIssueArtifactConsumedByDoneStep(db, f.run, f.issue, sagaDiffKind)
	if err != nil || !found || id != judged || payload != `{"head":"judged"}` {
		t.Errorf("the newest diff a done reader took = (%d, %q, %v, %v), want (%d, {\"head\":\"judged\"}, true, nil)",
			id, payload, found, err, judged)
	}
	id, payload, found, err = NewestIssueArtifactConsumedByDoneStep(db, f.run, f.otherIssue, sagaDiffKind)
	if err != nil || found || id != 0 || payload != "" {
		t.Errorf("an issue no done step read = (%d, %q, %v, %v), want (0, \"\", false, nil)", id, payload, found, err)
	}
}

func TestSagaQueriesFirstRecorderAfterMetadata(t *testing.T) {
	f := newRoutingFixture(t)
	producer := f.add(StepRow{StepName: "implement", Status: StepDone})
	judged := sagaArtifact(f, producer, sagaDiffKind, "d1", "")
	excepted := f.add(StepRow{StepName: "fix", Ordinal: 2, Status: StepRunning, Metadata: `{"who":"excepted"}`})
	sagaArtifact(f, producer, sagaDiffKind, "re-pin", "")
	sagaArtifact(f, excepted, sagaDiffKind, "d2", "")
	round := f.add(StepRow{StepName: "fix", Ordinal: 1, Status: StepDone, Metadata: `{"who":"round"}`})
	sagaArtifact(f, round, sagaDiffKind, "d3", "")
	bare := f.add(StepRow{StepName: "fix", Ordinal: 3, Status: StepDone})
	last := sagaArtifact(f, bare, sagaDiffKind, "d4", "")
	db := sagaPool(f)

	q := FirstRecorderAfterQuery{
		RunID: f.run, IssueID: f.issue, Kind: sagaDiffKind,
		AfterArtifactID: judged, ExceptStepID: excepted,
	}
	got, found, err := FirstRecorderAfterMetadata(db, q)
	if err != nil || !found || got != `{"who":"round"}` {
		t.Errorf("the first new recorder = (%q, %v, %v), want the round's metadata", got, found, err)
	}

	q.ExceptStepID = 0
	got, found, err = FirstRecorderAfterMetadata(db, q)
	if err != nil || !found || got != `{"who":"excepted"}` {
		t.Errorf("with nothing excepted = (%q, %v, %v), want the excepted step's metadata", got, found, err)
	}

	q.AfterArtifactID = last - 1
	got, found, err = FirstRecorderAfterMetadata(db, q)
	if err != nil || !found || got != "" {
		t.Errorf("a recorder with no metadata = (%q, %v, %v), want (\"\", true, nil)", got, found, err)
	}

	q.AfterArtifactID = last
	got, found, err = FirstRecorderAfterMetadata(db, q)
	if err != nil || found || got != "" {
		t.Errorf("nothing recorded after the newest = (%q, %v, %v), want (\"\", false, nil)", got, found, err)
	}
}

func TestSagaQueriesNewestStepNamePayloadBelowOrdinal(t *testing.T) {
	f := newRoutingFixture(t)
	for ordinal, head := range []string{"zero", "one", "two"} {
		step := f.add(StepRow{StepName: "fix", Ordinal: ordinal, Status: StepDone})
		sagaArtifact(f, step, sagaDiffKind, "d", `{"head":"`+head+`"}`)
	}
	other := f.add(StepRow{StepName: "review", Ordinal: 0, Status: StepDone})
	sagaArtifact(f, other, sagaDiffKind, "d", `{"head":"review"}`)
	db := sagaPool(f)

	got, found, err := NewestStepNamePayloadBelowOrdinal(db, f.run, f.issue, "fix", 2, sagaDiffKind)
	if err != nil || !found || got != `{"head":"one"}` {
		t.Errorf("fix's newest diff below ordinal 2 = (%q, %v, %v), want ordinal 1's", got, found, err)
	}
	got, found, err = NewestStepNamePayloadBelowOrdinal(db, f.run, f.issue, "fix", 0, sagaDiffKind)
	if err != nil || found || got != "" {
		t.Errorf("nothing below ordinal 0 = (%q, %v, %v), want (\"\", false, nil)", got, found, err)
	}
}

func TestSagaQueriesRunFenceCommands(t *testing.T) {
	f := newRoutingFixture(t)
	for _, fence := range []RunFence{
		{RunID: f.run, IssueID: f.issue, Tag: "ac", Ordinal: 1, Command: "second", SHA256: "s2"},
		{RunID: f.run, IssueID: f.issue, Tag: "ac", Ordinal: 0, Command: "first", SHA256: "s1"},
		{RunID: f.run, IssueID: f.issue, Tag: "other", Ordinal: 0, Command: "other tag", SHA256: "s3"},
		{RunID: f.run, IssueID: f.otherIssue, Tag: "ac", Ordinal: 0, Command: "other issue", SHA256: "s4"},
	} {
		testsupport.Must(t, InsertFenceTx(f.tx, fence), "InsertFenceTx")
	}
	db := sagaPool(f)

	got, found, err := RunFenceCommands(db, f.run, f.issue, "ac")
	want := []FenceCommand{{Command: "first", SHA256: "s1"}, {Command: "second", SHA256: "s2"}}
	if err != nil || !found || !reflect.DeepEqual(got, want) {
		t.Errorf("the ac fence = (%+v, %v, %v), want (%+v, true, nil)", got, found, err, want)
	}
	got, found, err = RunFenceCommands(db, f.run, f.issue, "absent")
	if err != nil || found || len(got) != 0 {
		t.Errorf("an unharvested tag = (%+v, %v, %v), want (none, false, nil)", got, found, err)
	}
}

func TestSagaQueriesNewestIssueArtifact(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "implement", Status: StepDone})
	elsewhere := f.add(StepRow{IssueID: f.otherIssue, StepName: "implement", Status: StepDone})
	sagaArtifact(f, step, sagaDiffKind, "older", `{"head":"old"}`)
	sagaArtifact(f, step, sagaDiffKind, "newer", "")
	sagaArtifact(f, elsewhere, sagaDiffKind, "other issue", `{"head":"x"}`)
	db := sagaPool(f)

	body, payload, found, err := NewestIssueArtifact(db, f.run, f.issue, sagaDiffKind)
	if err != nil || !found || body != "newer" || payload != "" {
		t.Errorf("the newest diff = (%q, %q, %v, %v), want (\"newer\", \"\", true, nil)", body, payload, found, err)
	}
	body, payload, found, err = NewestIssueArtifact(db, f.otherRun, f.issue, sagaDiffKind)
	if err != nil || found || body != "" || payload != "" {
		t.Errorf("a run with no diff = (%q, %q, %v, %v), want (\"\", \"\", false, nil)", body, payload, found, err)
	}
}

func TestSagaQueriesNewestIssueArtifactBodies(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "implement", Status: StepDone})
	elsewhere := f.add(StepRow{IssueID: f.otherIssue, StepName: "implement", Status: StepDone})
	sagaArtifact(f, step, sagaDiffKind, "b1", "")
	sagaArtifact(f, step, sagaDiffKind, "b2", "")
	sagaArtifact(f, step, "gap", "not a diff", "")
	sagaArtifact(f, step, sagaDiffKind, "b3", "")
	sagaArtifact(f, elsewhere, sagaDiffKind, "other issue", "")
	db := sagaPool(f)

	got, err := NewestIssueArtifactBodies(db, f.run, f.issue, sagaDiffKind, 2)
	if err != nil || !reflect.DeepEqual(got, []string{"b3", "b2"}) {
		t.Errorf("the two newest diffs = (%q, %v), want ([b3 b2], nil)", got, err)
	}
	got, err = NewestIssueArtifactBodies(db, f.otherRun, f.issue, sagaDiffKind, 50)
	if err != nil || len(got) != 0 {
		t.Errorf("a run with no diff = (%q, %v), want (none, nil)", got, err)
	}
}

func TestSagaQueriesFirstStepEventData(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "implement", Status: StepRunning})
	other := f.add(StepRow{StepName: "review", Status: StepRunning})
	f.event("step-noted", step, `{"n":0}`)
	f.event("step-claimed", step, `{"claim_head":"first"}`)
	f.event("step-claimed", step, `{"claim_head":"second"}`)
	f.event("step-claimed", other, `{"claim_head":"other"}`)
	db := sagaPool(f)

	got, found, err := FirstStepEventData(db, step, "step-claimed")
	if err != nil || !found || got != `{"claim_head":"first"}` {
		t.Errorf("the first claim = (%q, %v, %v), want the first claim's data", got, found, err)
	}
	got, found, err = FirstStepEventData(db, step, "step-failed")
	if err != nil || found || got != "" {
		t.Errorf("a kind the step never logged = (%q, %v, %v), want (\"\", false, nil)", got, found, err)
	}
}

func TestSagaQueriesStepEventDataContains(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "implement", Status: StepRunning})
	other := f.add(StepRow{StepName: "review", Status: StepRunning})
	f.event("gate-started", step, `{"gate":"tests"}`)
	f.event("gate-finished", step, `{"gate":"build"}`)
	f.event("gate-started", other, `{"gate":"lint"}`)
	db := sagaPool(f)

	ask := func(name, kind, text string, want bool) {
		t.Helper()
		got, err := StepEventDataContains(db, f.run, step, kind, text)
		if err != nil || got != want {
			t.Errorf("%s = (%v, %v), want (%v, nil)", name, got, err, want)
		}
	}
	ask("a started gate", "gate-started", "tests", true)
	ask("a gate only another kind names", "gate-started", "build", false)
	ask("a gate only another step started", "gate-started", "lint", false)
}

func TestSagaQueriesStrandedSagaStepIDs(t *testing.T) {
	f := newRoutingFixture(t)
	stage := func(id int, stage any, updatedAtMS int64) {
		f.exec(`UPDATE steps SET saga_stage = ?, updated_at_ms = ? WHERE id = ?`, stage, updatedAtMS, id)
	}
	stranded := f.add(StepRow{StepName: "implement", Status: StepDone})
	stage(stranded, SagaGatePrefix+"tests", 1000)
	routing := f.add(StepRow{StepName: "fix", Status: StepDone})
	stage(routing, SagaRouting, 1000)
	held := f.add(StepRow{StepName: "review", Status: StepDone})
	stage(held, SagaHeld, 1000)
	blank := f.add(StepRow{StepName: "verify", Status: StepDone})
	stage(blank, "", 1000)
	f.add(StepRow{StepName: "plan", Status: StepDone})
	action := f.add(StepRow{StepName: "aggregate", Kind: "action", Status: StepDone})
	stage(action, SagaRouting, 1000)
	recent := f.add(StepRow{StepName: "triage", Status: StepDone})
	stage(recent, SagaRouting, 5000)
	otherRun := f.add(StepRow{RunID: f.otherRun, StepName: "implement", Status: StepDone})
	stage(otherRun, SagaRouting, 1000)
	db := sagaPool(f)

	got, err := StrandedSagaStepIDs(db, f.run, "action", 2000)
	testsupport.Must(t, err, "StrandedSagaStepIDs: %v", err)
	ids := map[int]bool{}
	for _, id := range got {
		ids[id] = true
	}
	want := map[int]bool{stranded: true, routing: true}
	if !reflect.DeepEqual(ids, want) || len(got) != len(want) {
		t.Errorf("stranded sagas = %v, want ids %v", got, want)
	}

	got, err = StrandedSagaStepIDs(db, f.run, "action", 0)
	if err != nil || len(got) != 0 {
		t.Errorf("nothing older than 0 = (%v, %v), want (none, nil)", got, err)
	}
}

func TestSagaQueriesRunCostFloorTx(t *testing.T) {
	f := newRoutingFixture(t)
	floor, err := RunCostFloorTx(f.tx, f.run, "step-claimed", "vote")
	if err != nil || floor != 0 {
		t.Errorf("a run with no claims or votes = (%v, %v), want (0, nil)", floor, err)
	}

	cheap := f.add(StepRow{StepName: "implement", Status: StepDone, ExpectedCost: 1})
	dear := f.add(StepRow{StepName: "review", Status: StepDone, ExpectedCost: 4})
	f.event("step-claimed", cheap, `{"expected_cost":2.5}`)
	f.event("step-claimed", cheap, `{}`)
	f.event("step-claimed", dear, `not json`)
	f.event("step-recorded", dear, `{"expected_cost":100}`)
	f.add(StepRow{StepName: "ballot", Kind: "vote", Status: StepPending, ExpectedCost: 3})
	f.add(StepRow{StepName: "ballot", Ordinal: 1, Kind: "vote", Status: StepSkipped, ExpectedCost: 10})
	f.add(StepRow{StepName: "ballot", Ordinal: 2, Kind: "vote", Status: StepSuperseded, ExpectedCost: 10})
	elsewhere := f.add(StepRow{RunID: f.otherRun, StepName: "ballot", Kind: "vote", Status: StepPending, ExpectedCost: 20})
	testsupport.Must(t, InsertEventTx(f.tx, NewEvent{
		AtMS: 1, Kind: "step-claimed", RunID: f.otherRun, StepID: elsewhere, Data: `{"expected_cost":50}`,
	}), "seeding another run's claim")

	floor, err = RunCostFloorTx(f.tx, f.run, "step-claimed", "vote")
	if err != nil || floor != 10.5 {
		t.Errorf("the floor = (%v, %v), want (10.5, nil): 2.5 carried + 1 and 4 declared + 3 open vote",
			floor, err)
	}
}
