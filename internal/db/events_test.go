package db

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// eventLog is a migrated store holding a known event log:
//
//	seq 1  run A  at 100  step-claimed
//	seq 2  run A  at 200  gate-started
//	seq 3  run A  at 300  step-claimed
//	seq 4  run B  at 400  run-activated
//	seq 5  none   at 500  trust-added
//
// Run A is active and run B is done.
type eventLog struct {
	db         *sql.DB
	runA, runB int
}

func seedEventLog(t *testing.T) eventLog {
	t.Helper()
	db := newIdemDB(t)
	runA := seedEventRun(t, db, model.RunActive, 1)
	runB := seedEventRun(t, db, model.RunDone, 1)
	seedEvent(t, db, 100, "step-claimed", runA, `{}`)
	seedEvent(t, db, 200, "gate-started", runA, `{}`)
	seedEvent(t, db, 300, "step-claimed", runA, `{}`)
	seedEvent(t, db, 400, "run-activated", runB, `{"head_commit":"b"}`)
	seedEvent(t, db, 500, "trust-added", 0, `{"repo":""}`)
	return eventLog{db: db, runA: runA, runB: runB}
}

func seedEventRun(t *testing.T, db *sql.DB, status model.RunStatus, projectID int) int {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO runs (request, status, project_id, created_at_ms, updated_at_ms)
		 VALUES ('', ?, ?, 1, 1)`, string(status), projectID)
	testsupport.Must(t, err, "seeding a run: %v", err)
	id, _ := res.LastInsertId()
	return int(id)
}

// seedEvent inserts one event; runID 0 stores a NULL run.
func seedEvent(t *testing.T, db *sql.DB, atMS int64, kind string, runID int, data string) {
	t.Helper()
	var run any
	if runID != 0 {
		run = runID
	}
	mustExec(t, db, `INSERT INTO events (at_ms, kind, run_id, data) VALUES (?, ?, ?, ?)`,
		atMS, kind, run, data)
}

// beginEventTx opens a transaction rolled back at cleanup. The pool has one
// connection, so seed through db before calling it.
func beginEventTx(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	t.Cleanup(func() { tx.Rollback() })
	return tx
}

func TestEventQueriesCountPrunableEvents(t *testing.T) {
	log := seedEventLog(t)
	tx := beginEventTx(t, log.db)

	cases := []struct {
		name string
		sel  EventPruneSelection
		want int
	}{
		{"below a seq", EventPruneSelection{BeforeSeq: 4}, 3},
		{"below a seq in one run", EventPruneSelection{BeforeSeq: 6, RunID: log.runB}, 1},
		{"one run", EventPruneSelection{RunID: log.runA}, 3},
		{"clamped by retention", EventPruneSelection{BeforeSeq: 6, BeforeAtMS: 250}, 2},
		{"missing run", EventPruneSelection{RunID: 999}, 0},
	}
	for _, c := range cases {
		got, err := CountPrunableEventsTx(tx, c.sel)
		testsupport.Must(t, err, "%s: %v", c.name, err)
		if got != c.want {
			t.Errorf("%s: count = %d, want %d", c.name, got, c.want)
		}
	}

	if _, err := CountPrunableEventsTx(tx, EventPruneSelection{}); err == nil {
		t.Error("an unbounded selection was counted, want a refusal")
	}
}

func TestEventQueriesDeletePrunableEvents(t *testing.T) {
	log := seedEventLog(t)
	tx := beginEventTx(t, log.db)

	if _, err := DeletePrunableEventsTx(tx, EventPruneSelection{}); err == nil {
		t.Error("an unbounded selection was deleted, want a refusal")
	}
	n, err := DeletePrunableEventsTx(tx, EventPruneSelection{RunID: 999})
	testsupport.Must(t, err, "deleting a missing run's events: %v", err)
	if n != 0 {
		t.Errorf("deleted %d events of a missing run, want 0", n)
	}

	n, err = DeletePrunableEventsTx(tx, EventPruneSelection{RunID: log.runA})
	testsupport.Must(t, err, "deleting run A's events: %v", err)
	if n != 3 {
		t.Errorf("deleted %d events, want 3", n)
	}
	left, err := CountEventsTx(tx, EventFilter{})
	testsupport.Must(t, err, "counting survivors: %v", err)
	if left != 2 {
		t.Errorf("%d events survive, want 2 (run B's and the store-level one)", left)
	}
}

func TestEventQueriesPrunableEventRuns(t *testing.T) {
	log := seedEventLog(t)
	tx := beginEventTx(t, log.db)

	got, err := PrunableEventRunsTx(tx, EventPruneSelection{BeforeSeq: 6})
	testsupport.Must(t, err, "PrunableEventRunsTx: %v", err)
	want := []EventRunStatus{
		{RunID: log.runA, Status: model.RunActive},
		{RunID: log.runB, Status: model.RunDone},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("runs = %+v, want %+v", got, want)
	}

	got, err = PrunableEventRunsTx(tx, EventPruneSelection{BeforeSeq: 6, BeforeAtMS: 350})
	testsupport.Must(t, err, "PrunableEventRunsTx clamped: %v", err)
	if want := []EventRunStatus{{RunID: log.runA, Status: model.RunActive}}; !reflect.DeepEqual(got, want) {
		t.Errorf("clamped runs = %+v, want %+v", got, want)
	}

	got, err = PrunableEventRunsTx(tx, EventPruneSelection{RunID: 999})
	testsupport.Must(t, err, "PrunableEventRunsTx missing run: %v", err)
	if len(got) != 0 {
		t.Errorf("a missing run's selection touched runs %+v, want none", got)
	}
}

func TestEventQueriesCountEventsHeldByRetention(t *testing.T) {
	log := seedEventLog(t)
	tx := beginEventTx(t, log.db)

	cases := []struct {
		name   string
		sel    EventPruneSelection
		cutoff int64
		want   int
	}{
		{"run A from 200", EventPruneSelection{BeforeSeq: 6, RunID: log.runA}, 200, 2},
		{"everything below seq 5 from 350", EventPruneSelection{BeforeSeq: 5}, 350, 1},
		{"nothing that recent", EventPruneSelection{RunID: log.runA}, 1000, 0},
		{"missing run", EventPruneSelection{RunID: 999}, 0, 0},
	}
	for _, c := range cases {
		got, err := CountEventsHeldByRetentionTx(tx, c.sel, c.cutoff)
		testsupport.Must(t, err, "%s: %v", c.name, err)
		if got != c.want {
			t.Errorf("%s: held = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestEventQueriesRaiseEventLowWatermark(t *testing.T) {
	log := seedEventLog(t)
	tx := beginEventTx(t, log.db)

	watermark := func() int64 {
		t.Helper()
		w, err := EventLowWatermarkTx(tx)
		testsupport.Must(t, err, "EventLowWatermarkTx: %v", err)
		return w
	}

	_, err := DeletePrunableEventsTx(tx, EventPruneSelection{BeforeSeq: 3})
	testsupport.Must(t, err, "pruning below seq 3: %v", err)
	testsupport.Must(t, RaiseEventLowWatermarkTx(tx), "raising")
	if got := watermark(); got != 2 {
		t.Errorf("watermark after pruning seq 1-2 = %d, want 2", got)
	}

	_, err = tx.Exec(`UPDATE event_low_watermark SET seq = 10 WHERE id = 1`)
	testsupport.Must(t, err, "setting the watermark: %v", err)
	testsupport.Must(t, RaiseEventLowWatermarkTx(tx), "raising")
	if got := watermark(); got != 10 {
		t.Errorf("watermark = %d, want 10: a raise must never lower it", got)
	}

	_, err = tx.Exec(`DELETE FROM events`)
	testsupport.Must(t, err, "emptying the log: %v", err)
	testsupport.Must(t, RaiseEventLowWatermarkTx(tx), "raising")
	if got := watermark(); got != 10 {
		t.Errorf("watermark over an empty log = %d, want 10 unchanged", got)
	}
}

func TestEventQueriesEventLowWatermark(t *testing.T) {
	db := newIdemDB(t)
	tx := beginEventTx(t, db)

	got, err := EventLowWatermarkTx(tx)
	testsupport.Must(t, err, "fresh store: %v", err)
	if got != 0 {
		t.Errorf("fresh watermark = %d, want 0", got)
	}

	_, err = tx.Exec(`UPDATE event_low_watermark SET seq = 7 WHERE id = 1`)
	testsupport.Must(t, err, "setting the watermark: %v", err)
	got, err = EventLowWatermarkTx(tx)
	testsupport.Must(t, err, "set watermark: %v", err)
	if got != 7 {
		t.Errorf("watermark = %d, want 7", got)
	}

	_, err = tx.Exec(`DELETE FROM event_low_watermark`)
	testsupport.Must(t, err, "removing the watermark row: %v", err)
	_, err = EventLowWatermarkTx(tx)
	if !errors.Is(err, ErrNoEventLowWatermark) {
		t.Errorf("missing row err = %v, want ErrNoEventLowWatermark", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Errorf("missing row err = %v exposes sql.ErrNoRows", err)
	}
}

// projectEventLog adds a second project and events attributed to it through
// each arm of the project scope.
type projectEventLog struct {
	eventLog
	other, otherRun, otherIssue int
}

func seedProjectEventLog(t *testing.T) projectEventLog {
	t.Helper()
	log := seedEventLog(t)
	// Inserted directly: EnsureProject would claim the unregistered default
	// project rather than add a second one.
	res, err := log.db.Exec(
		`INSERT INTO projects (identity, name, prefix) VALUES ('/repo/other', 'other', 'OTH')`)
	testsupport.Must(t, err, "seeding a project: %v", err)
	otherID, _ := res.LastInsertId()
	other := int(otherID)
	otherRun := seedEventRun(t, log.db, model.RunActive, other)
	res, err = log.db.Exec(
		`INSERT INTO issues (title, status, priority, kind, project_id, created_at, updated_at)
		 VALUES ('other', 'todo', 'none', 'task', ?, 1, 1)`, other)
	testsupport.Must(t, err, "seeding an issue: %v", err)
	issue, _ := res.LastInsertId()

	seedEvent(t, log.db, 600, "run-activated", otherRun, `{}`) // seq 6
	mustExec(t, log.db, `INSERT INTO events (at_ms, kind, issue_id, data)
		VALUES (700, 'issue-body-refreshed', ?, '{}')`, issue) // seq 7
	seedEvent(t, log.db, 800, "trust-added", 0, `{"repo":"/repo/other"}`)        // seq 8
	seedEvent(t, log.db, 900, "project-registered", 0, `{"identity":"/repo/x"}`) // seq 9
	return projectEventLog{eventLog: log, other: other, otherRun: otherRun, otherIssue: int(issue)}
}

func TestEventQueriesCountEvents(t *testing.T) {
	log := seedProjectEventLog(t)
	tx := beginEventTx(t, log.db)

	cases := []struct {
		name string
		f    EventFilter
		want int
	}{
		{"everything", EventFilter{}, 9},
		{"after a cursor", EventFilter{Since: 3}, 6},
		{"one run", EventFilter{RunID: log.runA}, 3},
		{"one kind", EventFilter{Kind: "step-claimed"}, 2},
		// The other project owns its run's event, its issue's event, and the
		// trust grant naming its repository; the global grant (seq 5) is
		// everyone's. The default project's runs and the grant naming
		// /repo/x are not its.
		{"one project", EventFilter{ProjectID: log.other}, 4},
		{"the default project", EventFilter{ProjectID: 1}, 5},
		{"a run outside the project", EventFilter{ProjectID: log.other, RunID: log.runA}, 3},
		{"missing run", EventFilter{RunID: 999}, 0},
	}
	for _, c := range cases {
		got, err := CountEventsTx(tx, c.f)
		testsupport.Must(t, err, "%s: %v", c.name, err)
		if got != c.want {
			t.Errorf("%s: count = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestEventQueriesListEvents(t *testing.T) {
	log := seedProjectEventLog(t)
	runID, stepID := seedRunAndStep(t, log.db)
	mustExec(t, log.db, `INSERT INTO events (at_ms, kind, run_id, step_id, data)
		VALUES (1000, 'step-claimed', ?, ?, '{"k":1}')`, runID, stepID) // seq 10
	tx := beginEventTx(t, log.db)

	seqs := func(rows []EventRow) []int64 {
		var out []int64
		for _, r := range rows {
			out = append(out, r.Seq)
		}
		return out
	}

	all, err := ListEventsTx(tx, EventFilter{}, 0, 0)
	testsupport.Must(t, err, "listing every event: %v", err)
	if got, want := seqs(all), []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}; !reflect.DeepEqual(got, want) {
		t.Fatalf("seqs = %v, want %v", got, want)
	}
	if got, want := all[6], (EventRow{
		Seq: 7, AtMS: 700, Kind: "issue-body-refreshed", IssueID: log.otherIssue,
		IssuePrefix: "OTH", ProjectName: "other", Data: "{}",
	}); got != want {
		t.Errorf("issue event = %+v, want %+v", got, want)
	}
	if got, want := all[9], (EventRow{
		Seq: 10, AtMS: 1000, Kind: "step-claimed", RunID: runID, StepID: stepID,
		Instance: "implement@0", ProjectName: "default", Data: `{"k":1}`,
	}); got != want {
		t.Errorf("step event = %+v, want %+v", got, want)
	}
	if got, want := all[4], (EventRow{
		Seq: 5, AtMS: 500, Kind: "trust-added", Data: `{"repo":""}`,
	}); got != want {
		t.Errorf("store-level event = %+v, want %+v", got, want)
	}

	page, err := ListEventsTx(tx, EventFilter{Since: 2}, 3, 0)
	testsupport.Must(t, err, "limit: %v", err)
	if got, want := seqs(page), []int64{3, 4, 5}; !reflect.DeepEqual(got, want) {
		t.Errorf("limit 3 after seq 2 = %v, want %v", got, want)
	}

	page, err = ListEventsTx(tx, EventFilter{RunID: log.runA}, 0, 2)
	testsupport.Must(t, err, "tail: %v", err)
	if got, want := seqs(page), []int64{2, 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("tail 2 of run A = %v, want %v (newest two, ascending)", got, want)
	}

	page, err = ListEventsTx(tx, EventFilter{RunID: 999}, 0, 0)
	testsupport.Must(t, err, "missing run: %v", err)
	if len(page) != 0 {
		t.Errorf("a missing run listed %v, want nothing", seqs(page))
	}
}

func TestEventQueriesEventKindCounts(t *testing.T) {
	log := seedEventLog(t)
	tx := beginEventTx(t, log.db)

	got, err := EventKindCountsTx(tx, log.runA)
	testsupport.Must(t, err, "run A: %v", err)
	if want := map[string]int{"step-claimed": 2, "gate-started": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("run A counts = %v, want %v", got, want)
	}

	got, err = EventKindCountsTx(tx, 999)
	testsupport.Must(t, err, "missing run: %v", err)
	if len(got) != 0 {
		t.Errorf("a missing run counted %v, want nothing", got)
	}
}

func TestEventQueriesLatestEventData(t *testing.T) {
	log := seedEventLog(t)
	seedEvent(t, log.db, 450, "run-activated", log.runB, `{"head_commit":"c"}`)
	seedEvent(t, log.db, 460, "step-claimed", log.runB, `{}`)
	tx := beginEventTx(t, log.db)

	data, found, err := LatestEventDataTx(tx, log.runB, "run-activated")
	testsupport.Must(t, err, "run B: %v", err)
	if !found || data != `{"head_commit":"c"}` {
		t.Errorf("latest run-activated = (%q, %v), want the newer payload", data, found)
	}

	data, found, err = LatestEventDataTx(tx, log.runA, "run-activated")
	if err != nil || found || data != "" {
		t.Errorf("a run with no such event = (%q, %v, %v), want (\"\", false, nil)", data, found, err)
	}
}

func TestEventQueriesStepIDByInstance(t *testing.T) {
	db := newIdemDB(t)
	runID, stepID := seedRunAndStep(t, db)
	var issueID, workflowID int
	testsupport.Must(t, db.QueryRow(`SELECT issue_id, workflow_id FROM steps WHERE id = ?`, stepID).
		Scan(&issueID, &workflowID), "reading the seeded step")
	// A second issue in the same run with the same instance: only the issue
	// tells the two steps apart.
	res, err := db.Exec(`INSERT INTO issues (title, status, priority, kind, created_at, updated_at)
		VALUES ('second', 'todo', 'none', 'task', 1, 1)`)
	testsupport.Must(t, err, "seeding an issue: %v", err)
	otherIssue, _ := res.LastInsertId()
	res, err = db.Exec(`INSERT INTO steps (run_id, issue_id, workflow_id, step_name, ordinal,
		instance, kind, status, attempt, created_at_ms, updated_at_ms, row_version)
		VALUES (?, ?, ?, 'implement', 0, 'implement@0', 'executor', 'pending', 0, 1, 1, 1)`,
		runID, otherIssue, workflowID)
	testsupport.Must(t, err, "seeding a step: %v", err)
	otherStep, _ := res.LastInsertId()
	tx := beginEventTx(t, db)

	cases := []struct {
		name      string
		runID     int
		issueID   int
		instance  string
		wantID    int
		wantFound bool
	}{
		{"the first issue's step", runID, issueID, "implement@0", stepID, true},
		{"the second issue's step", runID, int(otherIssue), "implement@0", int(otherStep), true},
		{"no issue narrowing", runID, 0, "implement@0", stepID, true},
		{"an instance with no row", runID, issueID, "review@0", 0, false},
		{"an issue with no such step", runID, 999, "implement@0", 0, false},
		{"a missing run", 999, issueID, "implement@0", 0, false},
	}
	for _, c := range cases {
		id, found, err := StepIDByInstanceTx(tx, c.runID, c.issueID, c.instance)
		testsupport.Must(t, err, "%s: %v", c.name, err)
		if id != c.wantID || found != c.wantFound {
			t.Errorf("%s: (%d, %v), want (%d, %v)", c.name, id, found, c.wantID, c.wantFound)
		}
	}
}

func TestEventQueriesInsertEvent(t *testing.T) {
	db := newIdemDB(t)
	runID, stepID := seedRunAndStep(t, db)
	var issueID int
	testsupport.Must(t, db.QueryRow(`SELECT issue_id FROM steps WHERE id = ?`, stepID).Scan(&issueID),
		"reading the seeded step")
	tx := beginEventTx(t, db)

	testsupport.Must(t, InsertEventTx(tx, NewEvent{
		AtMS: 42, Kind: "step-claimed", RunID: runID, StepID: stepID, IssueID: issueID,
		Data: `{"instance":"implement@0"}`,
	}), "inserting a step event")
	testsupport.Must(t, InsertEventTx(tx, NewEvent{AtMS: 43, Kind: "trust-added", Data: `{}`}),
		"inserting a store-level event")

	rows, err := tx.Query(`SELECT seq, at_ms, kind, run_id, step_id, issue_id, data FROM events ORDER BY seq`)
	testsupport.Must(t, err, "reading events: %v", err)
	type stored struct {
		seq, atMS        int64
		kind, data       string
		run, step, issue sql.NullInt64
	}
	var got []stored
	for rows.Next() {
		var s stored
		testsupport.Must(t, rows.Scan(&s.seq, &s.atMS, &s.kind, &s.run, &s.step, &s.issue, &s.data),
			"scanning an event")
		got = append(got, s)
	}
	testsupport.Must(t, rows.Err(), "reading events")
	id := func(n int) sql.NullInt64 { return sql.NullInt64{Int64: int64(n), Valid: true} }
	want := []stored{
		{seq: 1, atMS: 42, kind: "step-claimed", data: `{"instance":"implement@0"}`,
			run: id(runID), step: id(stepID), issue: id(issueID)},
		{seq: 2, atMS: 43, kind: "trust-added", data: `{}`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stored events = %+v, want %+v (zero ids stored as NULL)", got, want)
	}
}

func TestEventQueriesLastEventAtMS(t *testing.T) {
	empty := newIdemDB(t)
	tx := beginEventTx(t, empty)
	atMS, found, err := LastEventAtMSTx(tx)
	if err != nil || found || atMS != 0 {
		t.Errorf("an empty log = (%d, %v, %v), want (0, false, nil)", atMS, found, err)
	}

	log := seedEventLog(t)
	// The newest event by seq carries an older stamp than seq 5: the read
	// follows seq, not the largest at_ms.
	seedEvent(t, log.db, 50, "step-claimed", log.runA, `{}`)
	tx = beginEventTx(t, log.db)
	atMS, found, err = LastEventAtMSTx(tx)
	testsupport.Must(t, err, "LastEventAtMSTx: %v", err)
	if !found || atMS != 50 {
		t.Errorf("last at_ms = (%d, %v), want (50, true): the newest seq's stamp", atMS, found)
	}
}

func TestEventQueriesMaxEventSeq(t *testing.T) {
	empty := newIdemDB(t)
	tx := beginEventTx(t, empty)
	seq, found, err := MaxEventSeqTx(tx)
	if err != nil || found || seq != 0 {
		t.Errorf("an empty log = (%d, %v, %v), want (0, false, nil)", seq, found, err)
	}

	log := seedEventLog(t)
	tx = beginEventTx(t, log.db)
	seq, found, err = MaxEventSeqTx(tx)
	testsupport.Must(t, err, "MaxEventSeqTx: %v", err)
	if !found || seq != 5 {
		t.Errorf("max seq = (%d, %v), want (5, true)", seq, found)
	}
}

func TestEventQueriesConductorChainDeferral(t *testing.T) {
	db := newIdemDB(t)
	runID, stepID := seedRunAndStep(t, db)
	tx := beginEventTx(t, db)

	insert := func(kind string, step int, data string) int64 {
		t.Helper()
		testsupport.Must(t, InsertEventTx(tx, NewEvent{AtMS: 1, Kind: kind, RunID: runID, StepID: step, Data: data}),
			"seeding a %s event", kind)
		var seq int64
		testsupport.Must(t, tx.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&seq), "reading the seeded seq")
		return seq
	}
	query := ChainDeferralQuery{
		RunID: runID, StepID: stepID, DeferredKind: "step-deferred",
		Cause: "chain", DispatchClosedKind: "dispatch-closed",
	}
	read := func(name string, q ChainDeferralQuery, wantSeq int64, wantData string, wantFound bool) {
		t.Helper()
		seq, data, found, err := ConductorChainDeferralTx(tx, q)
		testsupport.Must(t, err, "%s: %v", name, err)
		if seq != wantSeq || data != wantData || found != wantFound {
			t.Errorf("%s: (%d, %q, %v), want (%d, %q, %v)",
				name, seq, data, found, wantSeq, wantData, wantFound)
		}
	}

	read("no deferral filed", query, 0, "", false)

	insert("step-deferred", stepID, `{"cause":"chain","source":"engine"}`)
	insert("step-deferred", stepID, `{"cause":"budget"}`)
	insert("step-claimed", stepID, `{"cause":"chain"}`)
	read("only engine, budget, and other-kind events", query, 0, "", false)

	early := `{"cause":"chain","reason":"early"}`
	earlySeq := insert("step-deferred", stepID, early)
	insert("step-deferred", stepID, `{"cause":"chain","reason":"second"}`)
	read("no dispatch closed", query, earlySeq, early, true)

	// An open dispatch, and a close event naming a dispatch that is not the
	// run's latest closed one, leave the window where it was.
	_, err := tx.Exec(`INSERT INTO dispatches (run_id, status, opened_seq, expires_ms, created_at_ms)
		VALUES (?, 'open', 0, 1, 1)`, runID)
	testsupport.Must(t, err, "seeding an open dispatch: %v", err)
	insert("dispatch-closed", 0, `{"dispatch":"DISPATCH-999"}`)
	read("a close of an unknown dispatch", query, earlySeq, early, true)

	res, err := tx.Exec(`INSERT INTO dispatches (run_id, status, opened_seq, expires_ms, closed_at_ms, created_at_ms)
		VALUES (?, ?, 0, 1, 10, 1)`, runID, DispatchClosed)
	testsupport.Must(t, err, "seeding a closed dispatch: %v", err)
	dispatchID, _ := res.LastInsertId()
	insert("dispatch-closed", 0, fmt.Sprintf(`{"dispatch":"DISPATCH-%d"}`, dispatchID))
	read("every deferral predates the close", query, 0, "", false)

	late := `{"cause":"chain","reason":"late"}`
	insert("step-deferred", stepID, `{"cause":"chain","source":"engine"}`)
	lateSeq := insert("step-deferred", stepID, late)
	insert("step-deferred", stepID, `{"cause":"chain","reason":"later"}`)
	read("after the close", query, lateSeq, late, true)

	other := query
	other.StepID = stepID + 100
	read("another step", other, 0, "", false)
	other = query
	other.RunID = runID + 100
	read("another run", other, 0, "", false)
}
