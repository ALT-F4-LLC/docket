package engine

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-116: abandonment NAMES the run's recorded worktrees. A relay's
// close-time sweep only covers worktrees its own session created, and an
// abandoned run never reaches a close — so abandonment stranded checkouts and
// worktree-wf_* branches with no surface reporting them (RUN-4..12 left
// debris in five repos, three holding never-integrated shas).

func TestAbandonNamesRecordedWorktrees(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	// Two steps recorded the same checkout, one another: the answer is the
	// DISTINCT set, ordered.
	_, err := conn.Exec(`UPDATE steps SET work_root = '/tmp/wt/wf_b' WHERE run_id = ?`, run.ID)
	testsupport.Must(t, err, "seeding work_root: %v", err)
	_, err = conn.Exec(
		`UPDATE steps SET work_root = '/tmp/wt/wf_a'
		  WHERE id = (SELECT MIN(id) FROM steps WHERE run_id = ?)`, run.ID)
	testsupport.Must(t, err, "seeding second work_root: %v", err)

	_, worktrees, err := MoveRun(conn, run.ID, "abandon", model.RunAbandoned,
		abandonFrom, "debris test", nowMS)
	testsupport.Must(t, err, "MoveRun: %v", err)

	want := []string{"/tmp/wt/wf_a", "/tmp/wt/wf_b"}
	if !reflect.DeepEqual(worktrees, want) {
		t.Errorf("worktrees = %v, want %v", worktrees, want)
	}

	// The fact survives in the run-abandoned event, where a later session
	// reads it after this response has scrolled away.
	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	event, ok := findEvent(t, page, EventRunAbandoned)
	if !ok {
		t.Fatal("no run-abandoned event")
	}
	var data struct {
		Worktrees []string `json:"worktrees"`
	}
	testsupport.Must(t, json.Unmarshal(event.Data, &data), "decoding data")
	if !reflect.DeepEqual(data.Worktrees, want) {
		t.Errorf("event worktrees = %v, want %v", data.Worktrees, want)
	}
}

// TestAbandonWithNoRecordedWorktreesNamesNone: a run whose steps never
// recorded a worktree abandons exactly as before — no list, and no
// `worktrees` key in the event.
func TestAbandonWithNoRecordedWorktreesNamesNone(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	_, worktrees, err := MoveRun(conn, run.ID, "abandon", model.RunAbandoned,
		abandonFrom, "no debris", nowMS)
	testsupport.Must(t, err, "MoveRun: %v", err)
	if len(worktrees) != 0 {
		t.Errorf("worktrees = %v, want none", worktrees)
	}

	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	event, ok := findEvent(t, page, EventRunAbandoned)
	if !ok {
		t.Fatal("no run-abandoned event")
	}
	var data map[string]any
	testsupport.Must(t, json.Unmarshal(event.Data, &data), "decoding data")
	if _, present := data["worktrees"]; present {
		t.Error("the event carries a worktrees key with nothing to name")
	}
}

// TestAbandonIssueNamesItsWorktreesOnly: the per-issue disposition names the
// stopped issue's recorded worktrees, not its siblings'.
func TestAbandonIssueNamesItsWorktreesOnly(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	doomed := createIssue(t, conn, "mis-routed work", "body", "task", nil)
	kept := createIssue(t, conn, "healthy work", "body", "task", nil)
	run := startRun(t, conn, doomed, kept)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	_, err = conn.Exec(
		`UPDATE steps SET work_root = '/tmp/wt/wf_doomed'
		  WHERE run_id = ? AND issue_id = ?`, run.ID, doomed)
	testsupport.Must(t, err, "seeding doomed work_root: %v", err)
	_, err = conn.Exec(
		`UPDATE steps SET work_root = '/tmp/wt/wf_kept'
		  WHERE run_id = ? AND issue_id = ?`, run.ID, kept)
	testsupport.Must(t, err, "seeding kept work_root: %v", err)

	outcome, err := AbandonIssueInRun(conn, run.ID, doomed, "wrong repository", nowMS)
	testsupport.Must(t, err, "AbandonIssueInRun: %v", err)
	if !reflect.DeepEqual(outcome.Worktrees, []string{"/tmp/wt/wf_doomed"}) {
		t.Errorf("worktrees = %v, want the doomed issue's only", outcome.Worktrees)
	}
}
