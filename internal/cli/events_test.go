package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// renderEventList's issue column (DKT-74): engine.Event.Issue was already on
// the wire (events_read.go) but the human table never printed it.
func TestRenderEventListIncludesIssueColumn(t *testing.T) {
	events := []engine.Event{
		{Seq: 1, AtMS: 1000, Kind: "step.claimed", Run: "RUN-1", Issue: "DKT-74", Step: "implement@0"},
	}

	out := renderEventList(events, false)

	if !strings.Contains(out, "DKT-74") {
		t.Fatalf("renderEventList output missing issue DKT-74: %q", out)
	}
}

// An event with no issue (e.g. a trust event) holds the column open with "-"
// rather than collapsing it, matching the run column's existing convention.
func TestRenderEventListHoldsIssueColumnOpenWhenAbsent(t *testing.T) {
	events := []engine.Event{
		{Seq: 1, AtMS: 1000, Kind: "trust.added"},
	}

	out := renderEventList(events, false)

	fields := strings.Fields(out)
	if len(fields) < 5 {
		t.Fatalf("expected at least 5 columns (seq at kind run issue), got %d: %q", len(fields), out)
	}
	if fields[4] != "-" {
		t.Fatalf("expected issue column held open with %q, got %q in %q", "-", fields[4], out)
	}
}

// DKT-862 — a gate-recorded line says whether the verdict routed anything.
//
// `gate-recorded ... detail=ac-commands exit=2 verdict=fail` was byte-identical
// whether the gate BLOCKED the step or was a `pre = true` advisory input to it.
// On RUN-61 three pre-gate failures sat in this feed beside the `step-routed`
// that contradicted them, with nothing on the line to reconcile the two.
//
// The marker rides as its own key in `data`, because eventDetail renders the
// payload as sorted `key=value` pairs and INTERPRETS NOTHING — the genericity
// line this feed holds with the report's metadata rollup.
func TestGateRecordedLineMarksTheAdvisoryVerdict(t *testing.T) {
	events := []engine.Event{{
		Seq: 11394, AtMS: 1000, Kind: engine.EventGateRecorded,
		Run: "RUN-61", Issue: "DKT-862", Step: "verify@1",
		Data: json.RawMessage(
			`{"detail":"ac-commands","exit":2,"verdict":"fail","pre":true}`),
	}, {
		Seq: 11400, AtMS: 1001, Kind: engine.EventGateRecorded,
		Run: "RUN-61", Issue: "DKT-862", Step: "implement@1",
		Data: json.RawMessage(`{"detail":"build","exit":2,"verdict":"fail"}`),
	}}

	lines := strings.Split(renderEventList(events, false), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected one line per event, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "pre=true") {
		t.Errorf("the pre-gate's gate-recorded line does not mark a verdict "+
			"that routed nothing:\n  %s", lines[0])
	}
	// The verdict and exit stay exactly where they were (DKT-63): the marker
	// qualifies the line, it does not rewrite the closed vocabulary a program
	// reads out of `--json`.
	for _, needle := range []string{"detail=ac-commands", "exit=2", "verdict=fail"} {
		if !strings.Contains(lines[0], needle) {
			t.Errorf("the marker cost the line %q:\n  %s", needle, lines[0])
		}
	}
	// And the blocking failure beside it stays unmarked, which is what makes
	// the marker readable as a difference rather than as decoration.
	if strings.Contains(lines[1], "pre=") {
		t.Errorf("a blocking gate's line carries the advisory marker:\n  %s",
			lines[1])
	}
}

// eventsListCmdWithDB is `events list`'s flag set on a pristine command, so each
// case parses its own flags rather than inheriting the previous case's values.
func eventsListCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().Int64("since", 0, "")
	cmd.Flags().String("run", "", "")
	cmd.Flags().Bool("all-projects", false, "")
	cmd.Flags().Int("tail", 0, "")
	cmd.Flags().Int("limit", 0, "")
	return cmd
}

// eventsListJSON is the verb's envelope, read the way the analysts in DKT-583
// read it: `ok`, and the page under `data`.
type eventsListJSON struct {
	OK   bool `json:"ok"`
	Data struct {
		Events []struct {
			Seq int64  `json:"seq"`
			Run string `json:"run"`
		} `json:"events"`
		Total int `json:"total"`
	} `json:"data"`
}

// eventsFor drives `events list --json` from one project's cwd.
func eventsFor(
	t *testing.T, conn *sql.DB, invoking int, runRef string, allProjects bool,
) eventsListJSON {
	t.Helper()
	cmd := eventsListCmdWithDB(conn)
	if err := cmd.Flags().Set("run", runRef); err != nil {
		t.Fatalf("setting --run=%q: %v", runRef, err)
	}
	if allProjects {
		if err := cmd.Flags().Set("all-projects", "true"); err != nil {
			t.Fatalf("setting --all-projects: %v", err)
		}
	}
	cmd.SetContext(context.WithValue(cmd.Context(), projectKey, invoking))

	w, buf := bufWriter(true)
	if err := runEventsList(cmd, w); err != nil {
		t.Fatalf("runEventsList(--run %q): %v", runRef, err)
	}

	var out eventsListJSON
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshalling the envelope: %v\n%s", err, buf.String())
	}
	return out
}

// twoProjectRuns is DKT-583's shape: a machine-global store with two projects,
// one run each, and events on both.
func twoProjectRuns(t *testing.T, conn *sql.DB) (here, there int, hereRun, thereRun string) {
	t.Helper()
	here, err := db.EnsureProject(conn, "/src/here.git", "here.git", 1)
	testsupport.Must(t, err, "registering the local project: %v", err)
	there, err = db.EnsureProject(conn, "/src/there.git", "there.git", 2)
	testsupport.Must(t, err, "registering the other project: %v", err)
	if here == there {
		t.Fatalf("the fixture needs two distinct projects, got %d twice", here)
	}

	mine, err := db.InsertRun(conn, here, "here", 0, 1)
	testsupport.Must(t, err, "starting the local run: %v", err)
	theirs, err := db.InsertRun(conn, there, "there", 0, 2)
	testsupport.Must(t, err, "starting the other run: %v", err)

	event := func(runID int, kind string) {
		t.Helper()
		_, err := conn.Exec(
			`INSERT INTO events (at_ms, kind, run_id, data) VALUES (?, ?, ?, '{}')`,
			1, kind, runID)
		testsupport.Must(t, err, "writing an event: %v", err)
	}
	event(mine.ID, "run-activated")
	event(theirs.ID, "run-activated")
	event(theirs.ID, "step-claimed")

	return here, there, model.FormatRunID(mine.ID), model.FormatRunID(theirs.ID)
}

// TestEventsListRunInAnotherProjectIsNotAnEmptySuccess is DKT-583, verbatim.
//
// From one project's cwd, `events list --run RUN-N --json` for a run owned by
// ANOTHER project answered `{"ok":true,"data":{"events":null,"total":0}}` while
// `--all-projects` returned hundreds of rows for the same run. `run report`,
// `step show`, and `step artifact` all answer cross-project from that same cwd,
// so the empty page read as fact rather than as a scoping accident — four
// analysts recorded "this run has no events".
//
// THE INVARIANT IS THE ABSENCE OF THE FALSE POSITIVE: a real run must never be
// answered with a successful empty feed.
func TestEventsListRunInAnotherProjectIsNotAnEmptySuccess(t *testing.T) {
	conn := newTestDB(t)
	here, _, _, thereRun := twoProjectRuns(t, conn)

	out := eventsFor(t, conn, here, thereRun, false)

	if !out.OK {
		t.Fatalf("`events list --run %s` failed outright: %+v", thereRun, out)
	}
	if out.Data.Total == 0 || len(out.Data.Events) == 0 {
		t.Fatalf("`events list --run %s` from another project answered ok with an "+
			"empty feed — DKT-583's exact defect: %+v", thereRun, out.Data)
	}
	if len(out.Data.Events) != 2 || out.Data.Total != 2 {
		t.Fatalf("answered %d events (total %d), want the run's 2",
			len(out.Data.Events), out.Data.Total)
	}
	for _, e := range out.Data.Events {
		if e.Run != thereRun {
			t.Errorf("the feed carries an event from another run: %+v", e)
		}
	}
}

// A run inside the invoking project is unchanged: the scope it never needed is
// gone, and the answer is the same one it always gave.
func TestEventsListRunInTheInvokingProjectIsUnchanged(t *testing.T) {
	conn := newTestDB(t)
	here, _, hereRun, _ := twoProjectRuns(t, conn)

	out := eventsFor(t, conn, here, hereRun, false)

	if len(out.Data.Events) != 1 || out.Data.Total != 1 {
		t.Fatalf("the invoking project's own run answered %d events (total %d), want 1",
			len(out.Data.Events), out.Data.Total)
	}
	if out.Data.Events[0].Run != hereRun {
		t.Errorf("answered %q, want %q", out.Data.Events[0].Run, hereRun)
	}
}

// --all-projects with --run still answers the same run: the workaround the
// analysts found keeps working.
func TestEventsListAllProjectsWithRunStillAnswers(t *testing.T) {
	conn := newTestDB(t)
	here, _, _, thereRun := twoProjectRuns(t, conn)

	out := eventsFor(t, conn, here, thereRun, true)

	if len(out.Data.Events) != 2 || out.Data.Total != 2 {
		t.Fatalf("--all-projects --run %s answered %d events (total %d), want 2",
			thereRun, len(out.Data.Events), out.Data.Total)
	}
}

// The run-less feed is STILL scoped to the invoking project: dropping the scope
// predicate under --run must not drop it everywhere.
func TestEventsListWithoutRunStaysScopedToTheInvokingProject(t *testing.T) {
	conn := newTestDB(t)
	here, _, hereRun, _ := twoProjectRuns(t, conn)

	out := eventsFor(t, conn, here, "", false)

	if len(out.Data.Events) != 1 {
		t.Fatalf("the unfiltered feed answered %d events, want only this project's 1: %+v",
			len(out.Data.Events), out.Data.Events)
	}
	if out.Data.Events[0].Run != hereRun {
		t.Errorf("the scoped feed carries %q, want this project's %q",
			out.Data.Events[0].Run, hereRun)
	}
}
