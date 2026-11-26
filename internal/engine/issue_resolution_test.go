package engine

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestAbandonIssueInRunRecordsTheResolution is DKT-245 at the operator-verb
// edge.
//
// `abandon-issue` leaves the issue's STATUS alone on purpose — the run
// stopping work is a statement about the run, and forcing a terminal status
// would take the operator's triage decision away. The consequence, before the
// resolution column, was that an issue whose fix step had already completed
// stayed `done` with nothing on the row to contradict it: `issue show --json`
// carried no resolution key, and the tracker rendered "✔ done" for a fix the
// run's own review had reproduced as not fixing anything.
func TestAbandonIssueInRunRecordsTheResolution(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	doomed := createIssue(t, conn, "the flake", "body", "bug", nil)
	run := startRun(t, conn, doomed)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// The state the bug was found in: the fix landed, so the issue reads
	// `done`, and only then did review reproduce the failure.
	execSQL(t, conn, `UPDATE issues SET status = ? WHERE id = ?`,
		string(model.StatusDone), doomed)

	before, err := db.GetIssue(conn, doomed)
	testsupport.Must(t, err, "GetIssue before: %v", err)
	if before.Resolution != "" {
		t.Fatalf("an untouched issue already carries resolution %q", before.Resolution)
	}

	_, err = AbandonIssueInRun(conn, run.ID, doomed, "does not fix the flake", nowMS)
	testsupport.Must(t, err, "AbandonIssueInRun: %v", err)

	after, err := db.GetIssue(conn, doomed)
	testsupport.Must(t, err, "GetIssue after: %v", err)
	if after.Resolution != db.IssueResolutionAbandoned {
		t.Errorf("resolution = %q, want %q — with nothing on the row, the only "+
			"trace of the abandonment is an event, and every tracker surface "+
			"goes on calling this issue finished (DKT-245)",
			after.Resolution, db.IssueResolutionAbandoned)
	}

	// The status is still NOT forced: the resolution is an additional fact
	// beside it, never a replacement. Overwriting the status here would be the
	// very decision the routing declines to make for the operator.
	if after.Status != model.StatusDone {
		t.Errorf("status = %q, want it left at %q — recording the resolution "+
			"must not start forcing the status the routing deliberately leaves "+
			"to triage", after.Status, model.StatusDone)
	}
}

func firstStepOfIssue(t *testing.T, conn *sql.DB, runID, issueID int) int {
	t.Helper()
	var id int
	err := conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND issue_id = ? ORDER BY id LIMIT 1`,
		runID, issueID).Scan(&id)
	testsupport.Must(t, err, "finding a step of the issue: %v", err)
	return id
}
