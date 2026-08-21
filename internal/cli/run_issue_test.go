package cli

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `docket run issue add|remove` — DKT-53: the issue set is mutable while the
// run is in `planning`, so approving one more issue mid-planning no longer
// costs an abandon-and-retype cycle.

func planningRun(t *testing.T, conn *sql.DB, issueIDs ...int) *model.Run {
	t.Helper()
	run, err := db.InsertRun(conn, 1, "request", 0, model.NowMS())
	testsupport.Must(t, err, "InsertRun: %v", err)
	for _, id := range issueIDs {
		testsupport.Must(t, db.AddRunIssue(conn, run.ID, id),
			"AddRunIssue: attach %d", id)
	}
	return run
}

func attachedIssues(t *testing.T, conn *sql.DB, runID int) []int {
	t.Helper()
	rows, err := db.ListRunIssues(conn, runID)
	testsupport.Must(t, err, "ListRunIssues: %v", err)
	ids := make([]int, 0, len(rows))
	for _, ri := range rows {
		ids = append(ids, ri.IssueID)
	}
	return ids
}

func TestRunIssueAddAndRemoveInPlanning(t *testing.T) {
	conn := newTestDB(t)
	a := createIssue(t, conn, "first", model.StatusBacklog, model.PriorityNone)
	b := createIssue(t, conn, "second", model.StatusBacklog, model.PriorityNone)
	run := planningRun(t, conn, a)

	cmd := cmdWithDB(conn)
	w, _ := bufWriter(true)
	err := runRunIssueAdd(cmd, []string{run.Ref(), model.FormatID(b)}, w)
	testsupport.Must(t, err, "run issue add: %v", err)
	if got := attachedIssues(t, conn, run.ID); len(got) != 2 {
		t.Fatalf("attached = %v, want both issues", got)
	}

	w, _ = bufWriter(true)
	err = runRunIssueRemove(cmd, []string{run.Ref(), model.FormatID(a)}, w)
	testsupport.Must(t, err, "run issue remove: %v", err)
	if got := attachedIssues(t, conn, run.ID); len(got) != 1 || got[0] != b {
		t.Fatalf("attached = %v, want only the second issue", got)
	}
}

func TestRunIssueAddRefusesParkedAndTerminalRuns(t *testing.T) {
	for _, status := range []model.RunStatus{
		model.RunWaitingHuman, model.RunDone, model.RunAbandoned,
	} {
		t.Run(string(status), func(t *testing.T) {
			conn := newTestDB(t)
			a := createIssue(t, conn, "x", model.StatusBacklog, model.PriorityNone)
			run := planningRun(t, conn)
			testsupport.Must(t,
				db.SetRunStatus(conn, run.ID, status, "r", model.NowMS()),
				"SetRunStatus")

			cmd := cmdWithDB(conn)
			w, _ := bufWriter(true)
			err := runRunIssueAdd(cmd, []string{run.Ref(), model.FormatID(a)}, w)
			if err == nil {
				t.Fatalf("add succeeded on a %s run", status)
			}
			var cmdError *CmdError
			if !asCmdError(err, &cmdError) || cmdError.Code != output.ErrConflict {
				t.Errorf("error = %v, want CONFLICT", err)
			}
		})
	}
}

func TestRunIssueRemoveRefusesOutsidePlanning(t *testing.T) {
	conn := newTestDB(t)
	a := createIssue(t, conn, "x", model.StatusBacklog, model.PriorityNone)
	run := planningRun(t, conn, a)
	testsupport.Must(t,
		db.SetRunStatus(conn, run.ID, model.RunActive, "", model.NowMS()),
		"SetRunStatus")

	cmd := cmdWithDB(conn)
	w, _ := bufWriter(true)
	err := runRunIssueRemove(cmd, []string{run.Ref(), model.FormatID(a)}, w)
	if err == nil {
		t.Fatal("remove succeeded on an active run; the bound set must be immutable")
	}
	var cmdError *CmdError
	if !asCmdError(err, &cmdError) || cmdError.Code != output.ErrConflict {
		t.Errorf("error = %v, want CONFLICT", err)
	}
	if !strings.Contains(err.Error(), "planning") {
		t.Errorf("refusal does not say when removal is legal: %v", err)
	}
}

// TestRunIssueRemoveIsAllOrNothing: `remove A TYPO` must not detach A and then
// refuse — the membership check runs over the whole set first.
func TestRunIssueRemoveIsAllOrNothing(t *testing.T) {
	conn := newTestDB(t)
	a := createIssue(t, conn, "x", model.StatusBacklog, model.PriorityNone)
	run := planningRun(t, conn, a)

	cmd := cmdWithDB(conn)
	w, _ := bufWriter(true)
	err := runRunIssueRemove(cmd, []string{run.Ref(), model.FormatID(a), "DKT-99"}, w)
	if err == nil {
		t.Fatal("remove with an unattached id succeeded")
	}
	var cmdError *CmdError
	if !asCmdError(err, &cmdError) || cmdError.Code != output.ErrNotFound {
		t.Errorf("error = %v, want NOT_FOUND", err)
	}
	if got := attachedIssues(t, conn, run.ID); len(got) != 1 {
		t.Errorf("attached = %v; a refused remove detached something", got)
	}
}

// TestRunIssueAddRefusesCrossProjectIssue pins DKT-21 at the add edge: issue
// ids are store-wide, so another project's issue parses from any cwd, but a
// run's issues live in the run's own project — activation binds them against
// that project's workflow registry and books their snapshots, steps, and gaps
// there. The whole set validates before anything is written, so a good id in
// the same add must not be attached on the way to the refusal.
func TestRunIssueAddRefusesCrossProjectIssue(t *testing.T) {
	conn := newTestDB(t)
	other := otherProject(t, conn)
	local := createIssue(t, conn, "local", model.StatusBacklog, model.PriorityNone)
	foreign := createIssueInProject(t, conn, other, "foreign")
	run := planningRun(t, conn)

	cmd := cmdWithDB(conn)
	w, _ := bufWriter(true)
	err := runRunIssueAdd(cmd,
		[]string{run.Ref(), model.FormatID(local), model.FormatID(foreign)}, w)
	if err == nil {
		t.Fatal("run issue add attached a cross-project issue")
	}
	if got := codeOf(t, err); got != output.ErrValidation {
		t.Errorf("code = %s, want %s", got, output.ErrValidation)
	}
	if got := attachedIssues(t, conn, run.ID); len(got) != 0 {
		t.Errorf("attached = %v, want none (all-or-nothing)", got)
	}
}
