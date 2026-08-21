package db

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Project migration (DKT-27). Gaps recorded by `step complete --gap-file`
// land in the run's own project unconditionally — cwd is the record's only
// routing — so work surfaced for another repository needs a verb that
// re-homes it. Before this one existed the operator re-filed each stranded
// issue by hand from the owning checkout.

func migrationStore(t *testing.T) (conn *sql.DB, one, two int) {
	t.Helper()
	c := mustOpen(t)
	if err := Initialize(c); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(c); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	p1, err := EnsureProject(c, "/repo/one", "one", 1)
	if err != nil {
		t.Fatalf("EnsureProject(one): %v", err)
	}
	p2, err := EnsureProject(c, "/repo/two", "two", 2)
	if err != nil {
		t.Fatalf("EnsureProject(two): %v", err)
	}
	return c, p1, p2
}

func TestMoveIssueProjectMigratesSubtreeAndLabels(t *testing.T) {
	conn, one, two := migrationStore(t)

	root, err := CreateIssue(conn, &model.Issue{
		ProjectID: one, Title: "mis-filed gap",
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, []string{"engine"}, nil)
	if err != nil {
		t.Fatalf("CreateIssue(root): %v", err)
	}
	child, err := CreateIssue(conn, &model.Issue{
		ProjectID: one, Title: "sub-task", ParentID: &root,
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue(child): %v", err)
	}

	ids, err := MoveIssueProject(conn, root, two, "tester", 1000)
	if err != nil {
		t.Fatalf("MoveIssueProject: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("migrated %v, want the root and its sub-issue", ids)
	}

	// Both rows re-homed.
	for _, id := range []int{root, child} {
		issue, err := GetIssue(conn, id)
		if err != nil {
			t.Fatalf("GetIssue(%d): %v", id, err)
		}
		if issue.ProjectID != two {
			t.Errorf("issue %d project = %d, want %d", id, issue.ProjectID, two)
		}
	}

	// The label re-mapped BY NAME: the issue's label row now belongs to the
	// target project, so the target's `issue list -l engine` finds it.
	var labelProject int
	err = conn.QueryRow(
		`SELECT l.project_id FROM labels l
		  JOIN issue_labels il ON il.label_id = l.id
		 WHERE il.issue_id = ?`, root).Scan(&labelProject)
	if err != nil {
		t.Fatalf("reading the re-mapped label: %v", err)
	}
	if labelProject != two {
		t.Errorf("label project = %d, want %d", labelProject, two)
	}

	// And the trail says what happened, identity to identity.
	var oldV, newV string
	err = conn.QueryRow(
		`SELECT old_value, new_value FROM activity_log
		  WHERE issue_id = ? AND field_changed = 'project'`, root).Scan(&oldV, &newV)
	if err != nil {
		t.Fatalf("reading the activity row: %v", err)
	}
	if oldV != "/repo/one" || newV != "/repo/two" {
		t.Errorf("activity = %q -> %q, want /repo/one -> /repo/two", oldV, newV)
	}
}

func TestMoveIssueProjectRefusals(t *testing.T) {
	conn, one, two := migrationStore(t)

	root, err := CreateIssue(conn, &model.Issue{
		ProjectID: one, Title: "root",
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue(root): %v", err)
	}
	child, err := CreateIssue(conn, &model.Issue{
		ProjectID: one, Title: "child", ParentID: &root,
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue(child): %v", err)
	}

	// A sub-issue migrates with its root, never alone.
	if _, err := MoveIssueProject(conn, child, two, "tester", 1000); !errors.Is(err, ErrIssueHasParent) {
		t.Errorf("migrating a sub-issue = %v, want ErrIssueHasParent", err)
	}

	// A missing issue is a miss, not a silent no-op.
	if _, err := MoveIssueProject(conn, 99999, two, "tester", 1000); !errors.Is(err, ErrNotFound) {
		t.Errorf("migrating a missing issue = %v, want ErrNotFound", err)
	}

	// An issue a run holds stays put: its snapshots and steps are
	// project-scoped bookkeeping.
	if _, err := conn.Exec(
		`INSERT INTO runs (project_id, status, created_at_ms, updated_at_ms)
		 VALUES (?, 'planning', 0, 0)`, one); err != nil {
		t.Fatalf("inserting a run: %v", err)
	}
	var runID int
	if err := conn.QueryRow(`SELECT id FROM runs ORDER BY id DESC LIMIT 1`).Scan(&runID); err != nil {
		t.Fatalf("reading the run id: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO run_issues (run_id, issue_id) VALUES (?, ?)`, runID, root); err != nil {
		t.Fatalf("attaching the issue to the run: %v", err)
	}
	if _, err := MoveIssueProject(conn, root, two, "tester", 1000); !errors.Is(err, ErrIssueInRun) {
		t.Errorf("migrating a run-held issue = %v, want ErrIssueInRun", err)
	}

	// Nothing moved on the refusals.
	issue, err := GetIssue(conn, child)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue.ProjectID != one {
		t.Errorf("child project = %d after refusals, want %d", issue.ProjectID, one)
	}
}

func TestMoveIssueProjectSameProjectIsANoOp(t *testing.T) {
	conn, one, _ := migrationStore(t)

	id, err := CreateIssue(conn, &model.Issue{
		ProjectID: one, Title: "already home",
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	before, err := GetIssue(conn, id)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	ids, err := MoveIssueProject(conn, id, one, "tester", 1000)
	if err != nil {
		t.Fatalf("MoveIssueProject: %v", err)
	}
	if len(ids) != 1 || ids[0] != id {
		t.Errorf("ids = %v, want [%d]", ids, id)
	}
	after, err := GetIssue(conn, id)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if after.Version != before.Version {
		t.Errorf("version bumped %d -> %d on a no-op", before.Version, after.Version)
	}
}
