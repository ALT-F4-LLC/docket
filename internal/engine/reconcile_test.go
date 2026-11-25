package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestReparkRefreshesTheRunReason pins that the reason `run status` reports
// belongs to the run's current park. An operator pause and resume store their
// reasons; a later park the rollup decides was typed by nobody, so the earlier
// operator reason must not survive it as the current one.
func TestReparkRefreshesTheRunReason(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	_, _, err := MoveRun(conn, run.ID, "pause", model.RunWaitingHuman,
		[]model.RunStatus{model.RunActive}, "holding for the panel", nowMS+1)
	testsupport.Must(t, err, "pause: %v", err)
	_, _, err = MoveRun(conn, run.ID, "resume", model.RunActive,
		[]model.RunStatus{model.RunWaitingHuman}, "the panel answered", nowMS+2)
	testsupport.Must(t, err, "resume: %v", err)
	resumed, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if resumed.Status != model.RunActive || resumed.Reason != "the panel answered" {
		t.Fatalf("premise: run = (%s, %q) after the resume, want (%s, %q)",
			resumed.Status, resumed.Reason, model.RunActive, "the panel answered")
	}

	execSQL(t, conn,
		`UPDATE steps SET status = ? WHERE run_id = ? AND issue_id = ? AND instance = 'implement@0'`,
		string(db.StepWaitingHuman), run.ID, issue)
	rollup(t, conn, run.ID)

	parked, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if parked.Status != model.RunWaitingHuman {
		t.Fatalf("premise: run = %s after its only issue parked, want %s",
			parked.Status, model.RunWaitingHuman)
	}
	if parked.Reason != "" {
		t.Errorf("run reason = %q after the rollup parked the run, want none: "+
			"the operator's earlier reason does not describe this park", parked.Reason)
	}
}
