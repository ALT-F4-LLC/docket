package engine

import (
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestActivationFreezesIssueSize pins that a step row carries the issue's Size
// as frozen at activation: a resize after activation must not change how an
// already-scheduled step routes, on the `step show` row or the dispatch row.
func TestActivationFreezesIssueSize(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issue, err := db.CreateIssue(conn, &model.Issue{
		Title: "size me", Description: "body", Status: model.StatusBacklog,
		Priority: model.PriorityNone, Kind: model.IssueKind("task"),
		Size: model.SizeSmall,
	}, nil, nil)
	testsupport.Must(t, err, "creating issue: %v", err)
	run := startRun(t, conn, issue)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// The resize lands AFTER the freeze.
	err = db.UpdateIssue(conn, issue,
		map[string]interface{}{"size": string(model.SizeBounded)}, "tester")
	testsupport.Must(t, err, "resizing: %v", err)

	ttls, err := loadTTLConfig(conn, run.ID)
	testsupport.Must(t, err, "loadTTLConfig: %v", err)
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		row, err := StepRowFor(sched, stepNamed(t, sched, "implement@0"), ttls)
		testsupport.Must(t, err, "StepRowFor(implement@0): %v", err)
		if row.Size != string(model.SizeSmall) {
			t.Errorf("step row size = %q, want the FROZEN %q; a live read "+
				"would have said %q", row.Size, model.SizeSmall, model.SizeBounded)
		}
	})

	m, err := testEngine().OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.Rows) == 0 {
		t.Fatal("dispatch manifest carries no rows")
	}
	for _, row := range m.Rows {
		if row.Size != string(model.SizeSmall) {
			t.Errorf("%s: manifest row size = %q, want the FROZEN %q",
				row.Step, row.Size, model.SizeSmall)
		}
	}
}

// TestLegacySnapshotSizeUnset pins the pre-size snapshot: a blob with no `size`
// key renders an empty Size, and the row's JSON carries no size key at all, so
// a row from such a run serializes exactly as it did before the field existed.
func TestLegacySnapshotSizeUnset(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	legacy := `{"title":"do the thing","kind":"task","labels":[],"scope":[],"files":[]}`
	_, err := conn.Exec(
		`UPDATE run_issues SET issue_snapshot = ? WHERE run_id = ? AND issue_id = ?`,
		legacy, run.ID, issue)
	testsupport.Must(t, err, "seeding legacy snapshot: %v", err)

	ttls, err := loadTTLConfig(conn, run.ID)
	testsupport.Must(t, err, "loadTTLConfig: %v", err)
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		row, err := StepRowFor(sched, stepNamed(t, sched, "implement@0"), ttls)
		testsupport.Must(t, err, "StepRowFor(implement@0): %v", err)
		if row.Size != "" {
			t.Errorf("legacy snapshot row size = %q, want empty", row.Size)
		}

		out, err := json.Marshal(row)
		testsupport.Must(t, err, "marshaling row: %v", err)
		var keys map[string]json.RawMessage
		testsupport.Must(t, json.Unmarshal(out, &keys), "decoding row: %s", out)
		if _, ok := keys["size"]; ok {
			t.Errorf("legacy snapshot row JSON carries a size key: %s", out)
		}
	})
}
