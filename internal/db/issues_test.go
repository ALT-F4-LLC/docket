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

// createTestIssue is a helper that creates an issue with the given status and
// priority, sleeping 1ms between calls to guarantee distinct created_at values.
func createTestIssue(t *testing.T, conn *sql.DB, title string, status model.Status, priority model.Priority) int {
	t.Helper()
	issue := &model.Issue{
		Title:    title,
		Status:   status,
		Priority: priority,
		Kind:     model.IssueKindTask,
	}
	id, err := CreateIssue(conn, issue, nil, nil)
	testsupport.Must(t, err, "CreateIssue(%q): %v", title, err)
	// Small sleep to ensure distinct created_at timestamps for tiebreaking.
	time.Sleep(time.Millisecond)
	return id
}

func TestListIssuesDefaultSortOrder(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

	// Create issues in an order that is intentionally NOT the expected output
	// order, so the test can verify the sort is actually applied.
	//
	// Expected output order (status rank, then priority rank, then created_at DESC):
	//   1. in-progress / critical  (id=ipCrit)
	//   2. in-progress / medium    (id=ipMed)
	//   3. todo / high             (id=todoHigh)
	//   4. todo / low              (id=todoLow)
	//   5. backlog / high          (id=blogHigh)
	//   6. backlog / medium        (id=blogMed)

	// Create them out-of-order.
	blogMed := createTestIssue(t, db, "backlog-med", model.StatusBacklog, model.PriorityMedium)
	todoLow := createTestIssue(t, db, "todo-low", model.StatusTodo, model.PriorityLow)
	ipMed := createTestIssue(t, db, "ip-med", model.StatusInProgress, model.PriorityMedium)
	blogHigh := createTestIssue(t, db, "backlog-high", model.StatusBacklog, model.PriorityHigh)
	todoHigh := createTestIssue(t, db, "todo-high", model.StatusTodo, model.PriorityHigh)
	ipCrit := createTestIssue(t, db, "ip-crit", model.StatusInProgress, model.PriorityCritical)

	// Default sort: no Sort field set.
	issues, total, err := ListIssues(db, ListOptions{IncludeDone: false})
	testsupport.Must(t, err, "ListIssues: %v", err)
	if total != 6 {
		t.Fatalf("total = %d, want 6", total)
	}
	if len(issues) != 6 {
		t.Fatalf("len(issues) = %d, want 6", len(issues))
	}

	wantOrder := []int{ipCrit, ipMed, todoHigh, todoLow, blogHigh, blogMed}
	for i, want := range wantOrder {
		if issues[i].ID != want {
			// Build a readable description of what we got.
			var gotIDs []int
			for _, iss := range issues {
				gotIDs = append(gotIDs, iss.ID)
			}
			t.Fatalf("issues[%d].ID = %d, want %d\n  got order:  %v\n  want order: %v",
				i, issues[i].ID, want, gotIDs, wantOrder)
		}
	}
}

func TestListIssuesDefaultSortCreatedAtTiebreaker(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

	// Insert two issues with the same status and priority but explicit timestamps
	// so we can verify the created_at DESC tiebreaker. CreateIssue uses
	// time.RFC3339 (second precision), so we need timestamps at least 1 second apart.
	now := time.Now().UTC()
	older := &model.Issue{
		Title:    "older",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}
	olderID, err := CreateIssue(db, older, nil, nil)
	testsupport.Must(t, err, "CreateIssue(older): %v", err)

	// Manually update the created_at of the "older" issue to be clearly in the past.
	pastTime := now.Add(-10 * time.Second).Format(time.RFC3339)
	_, err = db.Exec("UPDATE issues SET created_at = ? WHERE id = ?", pastTime, olderID)
	testsupport.Must(t, err, "updating created_at: %v", err)

	newer := &model.Issue{
		Title:    "newer",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}
	newerID, err := CreateIssue(db, newer, nil, nil)
	testsupport.Must(t, err, "CreateIssue(newer): %v", err)

	issues, _, err := ListIssues(db, ListOptions{})
	testsupport.Must(t, err, "ListIssues: %v", err)
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2", len(issues))
	}
	if issues[0].ID != newerID || issues[1].ID != olderID {
		t.Errorf("expected newer (%d) before older (%d), got [%d, %d]",
			newerID, olderID, issues[0].ID, issues[1].ID)
	}
}

func TestListIssuesExplicitSortOverridesDefault(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

	createTestIssue(t, db, "ip-low", model.StatusInProgress, model.PriorityLow)
	createTestIssue(t, db, "todo-crit", model.StatusTodo, model.PriorityCritical)

	// Explicit sort by priority ascending should put "low" before "critical",
	// regardless of the default status-first ordering.
	issues, _, err := ListIssues(db, ListOptions{
		Sort:    "priority",
		SortDir: "asc",
	})
	testsupport.Must(t, err, "ListIssues: %v", err)
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2", len(issues))
	}
	// SQLite sorts "critical" < "low" alphabetically in ASC, so priority:asc
	// yields critical first, low second.
	if issues[0].Priority != model.PriorityCritical {
		t.Errorf("explicit sort priority:asc — issues[0].Priority = %q, want critical", issues[0].Priority)
	}
	if issues[1].Priority != model.PriorityLow {
		t.Errorf("explicit sort priority:asc — issues[1].Priority = %q, want low", issues[1].Priority)
	}
}

func TestListIssuesDefaultSortWithDoneStatus(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

	createTestIssue(t, db, "done-high", model.StatusDone, model.PriorityHigh)
	todoMed := createTestIssue(t, db, "todo-med", model.StatusTodo, model.PriorityMedium)
	ipHigh := createTestIssue(t, db, "ip-high", model.StatusInProgress, model.PriorityHigh)

	// With IncludeDone, done issues should sort last.
	issues, _, err := ListIssues(db, ListOptions{IncludeDone: true})
	testsupport.Must(t, err, "ListIssues: %v", err)
	if len(issues) != 3 {
		t.Fatalf("len = %d, want 3", len(issues))
	}
	// in-progress first, then todo, then done.
	wantOrder := []int{ipHigh, todoMed}
	for i, want := range wantOrder {
		if issues[i].ID != want {
			t.Errorf("issues[%d].ID = %d, want %d", i, issues[i].ID, want)
		}
	}
	if issues[2].Status != model.StatusDone {
		t.Errorf("issues[2].Status = %q, want done", issues[2].Status)
	}
}

// createTestIssueWithParent creates a child issue with the specified parent ID.
func createTestIssueWithParent(t *testing.T, conn *sql.DB, title string, status model.Status, priority model.Priority, parentID int) int {
	t.Helper()
	issue := &model.Issue{
		Title:    title,
		Status:   status,
		Priority: priority,
		Kind:     model.IssueKindTask,
		ParentID: &parentID,
	}
	id, err := CreateIssue(conn, issue, nil, nil)
	testsupport.Must(t, err, "CreateIssue(%q): %v", title, err)
	time.Sleep(time.Millisecond)
	return id
}

func TestListIssues_ParentFetchingPattern(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

	// Create a parent issue that is in-progress (will be excluded by a "todo" filter).
	parentID := createTestIssue(t, db, "parent-epic", model.StatusInProgress, model.PriorityHigh)

	// Create child issues that are "todo" (will match the filter).
	child1ID := createTestIssueWithParent(t, db, "child-1", model.StatusTodo, model.PriorityHigh, parentID)
	child2ID := createTestIssueWithParent(t, db, "child-2", model.StatusTodo, model.PriorityMedium, parentID)
	child3ID := createTestIssueWithParent(t, db, "child-3", model.StatusTodo, model.PriorityLow, parentID)

	// Filter for "todo" status only -- parent (in-progress) should be excluded.
	issues, total, err := ListIssues(db, ListOptions{
		Statuses: []string{string(model.StatusTodo)},
	})
	testsupport.Must(t, err, "ListIssues: %v", err)
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(issues) != 3 {
		t.Fatalf("len(issues) = %d, want 3", len(issues))
	}

	// Verify only children are returned (parent should not be in the result).
	returnedIDs := make(map[int]bool)
	for _, iss := range issues {
		returnedIDs[iss.ID] = true
	}
	if returnedIDs[parentID] {
		t.Errorf("parent (ID=%d) should NOT be in filtered results, but it is", parentID)
	}
	for _, childID := range []int{child1ID, child2ID, child3ID} {
		if !returnedIDs[childID] {
			t.Errorf("child (ID=%d) should be in filtered results, but it is not", childID)
		}
	}

	// Verify all children reference the parent.
	for _, iss := range issues {
		if iss.ParentID == nil {
			t.Errorf("issue %d should have ParentID set, got nil", iss.ID)
		} else if *iss.ParentID != parentID {
			t.Errorf("issue %d ParentID = %d, want %d", iss.ID, *iss.ParentID, parentID)
		}
	}

	// Now fetch the parent via GetIssuesByIDs -- this is the pattern used by
	// the CLI to get parent headers for children whose parents were excluded by filters.
	parentMap, err := GetIssuesByIDs(db, []int{parentID})
	testsupport.Must(t, err, "GetIssuesByIDs: %v", err)
	if len(parentMap) != 1 {
		t.Fatalf("parentMap length = %d, want 1", len(parentMap))
	}

	parent, ok := parentMap[parentID]
	if !ok {
		t.Fatalf("parentMap missing parent ID %d", parentID)
	}
	if parent.Title != "parent-epic" {
		t.Errorf("parent.Title = %q, want %q", parent.Title, "parent-epic")
	}
	if parent.Status != model.StatusInProgress {
		t.Errorf("parent.Status = %q, want %q", parent.Status, model.StatusInProgress)
	}
}

// seedResolution sets an issue's resolution the way the engine does, outside
// the shared update path, so the update under test sees it as the old value.
func seedResolution(t *testing.T, conn *sql.DB, id int, resolution string) {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	testsupport.Must(t, SetIssueResolutionTx(tx, id, resolution), "SetIssueResolutionTx")
	testsupport.Must(t, tx.Commit(), "Commit")
}

// activityFor returns the issue's activity rows for one field.
func activityFor(t *testing.T, conn *sql.DB, id int, field string) []model.Activity {
	t.Helper()
	rows, err := GetActivity(conn, id, 0)
	testsupport.Must(t, err, "GetActivity: %v", err)
	var out []model.Activity
	for _, a := range rows {
		if a.FieldChanged == field {
			out = append(out, a)
		}
	}
	return out
}

func assertResolutionRow(t *testing.T, rows []model.Activity, oldVal, newVal string) {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("resolution activity rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].OldValue != oldVal || rows[0].NewValue != newVal {
		t.Errorf("resolution row = %q -> %q, want %q -> %q", rows[0].OldValue, rows[0].NewValue, oldVal, newVal)
	}
}

func TestUpdateIssue_ImplicitDoneClearRecordsResolutionActivity(t *testing.T) {
	conn := mustOpen(t)
	testsupport.Must(t, Initialize(conn), "Initialize")
	id := createTestIssue(t, conn, "abandoned-todo", model.StatusTodo, model.PriorityMedium)
	seedResolution(t, conn, id, IssueResolutionAbandoned)

	err := UpdateIssue(conn, id, map[string]interface{}{"status": string(model.StatusDone)}, "tester")
	testsupport.Must(t, err, "UpdateIssue: %v", err)

	assertResolutionRow(t, activityFor(t, conn, id, "resolution"), IssueResolutionAbandoned, "")
}

func TestUpdateIssue_ReopenClearRecordsResolutionActivity(t *testing.T) {
	conn := mustOpen(t)
	testsupport.Must(t, Initialize(conn), "Initialize")
	id := createTestIssue(t, conn, "abandoned-done", model.StatusDone, model.PriorityMedium)
	seedResolution(t, conn, id, IssueResolutionAbandoned)

	err := UpdateIssue(conn, id, map[string]interface{}{
		"status":     string(model.StatusBacklog),
		"resolution": "",
	}, "tester")
	testsupport.Must(t, err, "UpdateIssue: %v", err)

	assertResolutionRow(t, activityFor(t, conn, id, "resolution"), IssueResolutionAbandoned, "")
}

func TestUpdateIssue_DoneWithEmptyResolutionRecordsNoResolutionActivity(t *testing.T) {
	conn := mustOpen(t)
	testsupport.Must(t, Initialize(conn), "Initialize")
	id := createTestIssue(t, conn, "plain-todo", model.StatusTodo, model.PriorityMedium)

	err := UpdateIssue(conn, id, map[string]interface{}{"status": string(model.StatusDone)}, "tester")
	testsupport.Must(t, err, "UpdateIssue: %v", err)

	if rows := activityFor(t, conn, id, "status"); len(rows) != 1 {
		t.Fatalf("status activity rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows := activityFor(t, conn, id, "resolution"); len(rows) != 0 {
		t.Errorf("resolution activity rows = %d, want 0: %+v", len(rows), rows)
	}
}

func TestDoneTransitionClearsResolution(t *testing.T) {
	// Every row starts from an issue resolved IssueResolutionAbandoned; the
	// rule decides whether the status write clears it or leaves it standing.
	cases := []struct {
		name               string
		prior, target      model.Status
		explicitResolution bool
		wantClear          bool
	}{
		{"todo moving to done clears the abandoned resolution", model.StatusTodo, model.StatusDone, false, true},
		{"done re-asserting done keeps the abandoned resolution", model.StatusDone, model.StatusDone, false, false},
		{"todo moving to done with an explicit resolution keeps the explicit value", model.StatusTodo, model.StatusDone, true, false},
		{"todo moving to in-progress keeps the abandoned resolution", model.StatusTodo, model.StatusInProgress, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DoneTransitionClearsResolution(c.prior, c.target, c.explicitResolution); got != c.wantClear {
				t.Errorf("DoneTransitionClearsResolution(%q, %q, %v) = %v, want %v",
					c.prior, c.target, c.explicitResolution, got, c.wantClear)
			}
		})
	}
}

// issueQueryStore is a migrated store with one workflow row, which the steps
// the engine-query tests seed all name.
type issueQueryStore struct {
	db   *sql.DB
	wfID int
}

func newIssueQueryStore(t *testing.T) issueQueryStore {
	t.Helper()
	db := newIdemDB(t)
	res, err := db.Exec(
		`INSERT INTO workflows (name, version, source_sha256, body, parsed, created_at_ms, row_version)
		 VALUES ('wf', 1, 'sha', 'body', '{}', 1000, 1)`)
	testsupport.Must(t, err, "seeding a workflow: %v", err)
	wfID, _ := res.LastInsertId()
	return issueQueryStore{db: db, wfID: int(wfID)}
}

func (s issueQueryStore) issue(t *testing.T, title string, status model.Status) int {
	t.Helper()
	id, err := CreateIssue(s.db, &model.Issue{
		Title: title, Status: status, Priority: model.PriorityMedium, Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue(%q): %v", title, err)
	return id
}

// step seeds one step of issueID in runID with the given status.
func (s issueQueryStore) step(t *testing.T, runID, issueID int, instance, status string) int {
	t.Helper()
	tx, err := s.db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	err = InsertStepTx(tx, StepRow{
		RunID: runID, IssueID: issueID, WorkflowID: s.wfID,
		StepName: "implement", Instance: instance, Kind: "executor",
		Executor: "worker", Class: "worker", Status: StepPending,
	}, 1000)
	testsupport.Must(t, err, "InsertStepTx: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit")

	var id int
	err = s.db.QueryRow(`SELECT id FROM steps WHERE run_id = ? AND issue_id = ? AND instance = ?`,
		runID, issueID, instance).Scan(&id)
	testsupport.Must(t, err, "reading the step id: %v", err)
	mustExec(t, s.db, `UPDATE steps SET status = ? WHERE id = ?`, status, id)
	return id
}

// artifact seeds one artifact; stepID 0 stores a NULL step.
func (s issueQueryStore) artifact(t *testing.T, runID, stepID int, kind, body, payload string) int {
	t.Helper()
	tx, err := s.db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	id, err := InsertArtifactTx(tx, Artifact{
		RunID: runID, StepID: stepID, Kind: kind, Body: body, Payload: payload, SHA256: "sha",
	}, 1000)
	testsupport.Must(t, err, "InsertArtifactTx: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit")
	return id
}

func (s issueQueryStore) relate(t *testing.T, source, target int, rt model.RelationType) {
	t.Helper()
	_, err := CreateRelation(s.db, &model.Relation{SourceIssueID: source, TargetIssueID: target, RelationType: rt})
	testsupport.Must(t, err, "CreateRelation(%d %s %d): %v", source, rt, target, err)
}

func TestIssueQueriesActivationIssueTx(t *testing.T) {
	s := newIssueQueryStore(t)
	id, err := CreateIssue(s.db, &model.Issue{
		Title: "bind me", Description: "the body", Status: model.StatusBacklog,
		Priority: model.PriorityHigh, Kind: model.IssueKindBug, Size: model.SizeBounded,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue: %v", err)
	noDescription := s.issue(t, "no description", model.StatusTodo)
	mustExec(t, s.db, `UPDATE issues SET description = NULL WHERE id = ?`, noDescription)
	tx := beginEventTx(t, s.db)

	got, err := ActivationIssueTx(tx, id)
	testsupport.Must(t, err, "ActivationIssueTx: %v", err)
	want := model.Issue{
		ID: id, Title: "bind me", Description: "the body", Status: model.StatusBacklog,
		Priority: model.PriorityHigh, Kind: model.IssueKindBug, Size: model.SizeBounded, Version: 1,
	}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("ActivationIssueTx = %+v, want %+v", *got, want)
	}

	bare, err := ActivationIssueTx(tx, noDescription)
	testsupport.Must(t, err, "ActivationIssueTx(NULL description): %v", err)
	if bare.Description != "" || bare.Title != "no description" {
		t.Errorf("NULL description read as %+v, want title %q and an empty description", bare, "no description")
	}

	if _, err := ActivationIssueTx(tx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing issue: err = %v, want ErrNotFound", err)
	}
}

func TestIssueQueriesIssueLabelsTx(t *testing.T) {
	s := newIssueQueryStore(t)
	id, err := CreateIssue(s.db, &model.Issue{
		Title: "labelled", Status: model.StatusTodo, Priority: model.PriorityMedium, Kind: model.IssueKindTask,
	}, []string{"zeta", "alpha", "mid"}, nil)
	testsupport.Must(t, err, "CreateIssue: %v", err)
	other, err := CreateIssue(s.db, &model.Issue{
		Title: "other", Status: model.StatusTodo, Priority: model.PriorityMedium, Kind: model.IssueKindTask,
	}, []string{"elsewhere"}, nil)
	testsupport.Must(t, err, "CreateIssue: %v", err)
	unlabelled := s.issue(t, "unlabelled", model.StatusTodo)
	tx := beginEventTx(t, s.db)

	got, err := IssueLabelsTx(tx, id)
	testsupport.Must(t, err, "IssueLabelsTx: %v", err)
	if want := []string{"alpha", "mid", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("IssueLabelsTx = %v, want %v", got, want)
	}
	got, err = IssueLabelsTx(tx, other)
	testsupport.Must(t, err, "IssueLabelsTx(other): %v", err)
	if want := []string{"elsewhere"}; !reflect.DeepEqual(got, want) {
		t.Errorf("IssueLabelsTx(other) = %v, want %v", got, want)
	}

	for _, missing := range []int{unlabelled, 999} {
		got, err := IssueLabelsTx(tx, missing)
		testsupport.Must(t, err, "IssueLabelsTx(%d): %v", missing, err)
		if len(got) != 0 {
			t.Errorf("IssueLabelsTx(%d) = %v, want no labels", missing, got)
		}
	}
}

func TestIssueQueriesWorkGraphRelationsTx(t *testing.T) {
	s := newIssueQueryStore(t)
	a := s.issue(t, "a", model.StatusTodo)
	b := s.issue(t, "b", model.StatusTodo)
	c := s.issue(t, "c", model.StatusTodo)
	outside := s.issue(t, "outside", model.StatusTodo)
	s.relate(t, a, b, model.RelationDependsOn)
	s.relate(t, c, a, model.RelationBlocks)
	s.relate(t, b, c, model.RelationRelatesTo)
	s.relate(t, a, outside, model.RelationDependsOn)
	tx := beginEventTx(t, s.db)

	got, err := WorkGraphRelationsTx(tx, []int{a, b, c})
	testsupport.Must(t, err, "WorkGraphRelationsTx: %v", err)
	want := []model.Relation{
		{SourceIssueID: a, TargetIssueID: b, RelationType: model.RelationDependsOn},
		{SourceIssueID: c, TargetIssueID: a, RelationType: model.RelationBlocks},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("WorkGraphRelationsTx = %+v, want %+v", got, want)
	}

	for name, ids := range map[string][]int{"no issues": nil, "unrelated issues": {b, outside}} {
		got, err := WorkGraphRelationsTx(tx, ids)
		testsupport.Must(t, err, "%s: %v", name, err)
		if len(got) != 0 {
			t.Errorf("%s: WorkGraphRelationsTx = %+v, want none", name, got)
		}
	}
}

// statusActivityTx reads issueID's status-change activity rows inside tx as
// "old->new@created_at by changed_by" strings, oldest first.
func statusActivityTx(t *testing.T, tx *sql.Tx, issueID int) []string {
	t.Helper()
	rows, err := tx.Query(
		`SELECT old_value, new_value, changed_by, created_at FROM activity_log
		  WHERE issue_id = ? AND field_changed = 'status' ORDER BY id`, issueID)
	testsupport.Must(t, err, "reading activity: %v", err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var oldVal, newVal, by, at string
		testsupport.Must(t, rows.Scan(&oldVal, &newVal, &by, &at), "scanning activity")
		out = append(out, oldVal+"->"+newVal+"@"+at+" by "+by)
	}
	testsupport.Must(t, rows.Err(), "iterating activity")
	return out
}

func TestIssueQueriesMoveIssueStatusTx(t *testing.T) {
	s := newIssueQueryStore(t)
	backlog := s.issue(t, "backlog", model.StatusBacklog)
	started := s.issue(t, "started", model.StatusInProgress)
	tx := beginEventTx(t, s.db)
	const nowMS = int64(1_700_000_000_000)
	now := time.UnixMilli(nowMS).UTC().Format(time.RFC3339)

	moved, err := MoveIssueStatusTx(tx, backlog, model.StatusBacklog, model.StatusTodo, nowMS)
	testsupport.Must(t, err, "MoveIssueStatusTx: %v", err)
	if !moved {
		t.Fatal("a backlog issue did not move to todo")
	}
	var (
		status    model.Status
		updatedAt string
		version   int
	)
	err = tx.QueryRow(`SELECT status, updated_at, version FROM issues WHERE id = ?`, backlog).
		Scan(&status, &updatedAt, &version)
	testsupport.Must(t, err, "reading the moved issue: %v", err)
	if status != model.StatusTodo || updatedAt != now || version != 2 {
		t.Errorf("moved issue = (%s, %s, v%d), want (todo, %s, v2)", status, updatedAt, version, now)
	}
	if got, want := statusActivityTx(t, tx, backlog), []string{"backlog->todo@" + now + " by "}; !reflect.DeepEqual(got, want) {
		t.Errorf("activity = %v, want %v", got, want)
	}

	// The from-status guard: an issue already elsewhere, and a missing one,
	// move nothing and leave no trail.
	for name, id := range map[string]int{"wrong status": started, "missing issue": 999} {
		moved, err := MoveIssueStatusTx(tx, id, model.StatusBacklog, model.StatusTodo, nowMS)
		testsupport.Must(t, err, "%s: %v", name, err)
		if moved {
			t.Errorf("%s: moved = true, want false", name)
		}
		if got := statusActivityTx(t, tx, id); len(got) != 0 {
			t.Errorf("%s: activity = %v, want none", name, got)
		}
	}
	err = tx.QueryRow(`SELECT status, version FROM issues WHERE id = ?`, started).Scan(&status, &version)
	testsupport.Must(t, err, "reading the unmoved issue: %v", err)
	if status != model.StatusInProgress || version != 1 {
		t.Errorf("unmoved issue = (%s, v%d), want (in-progress, v1)", status, version)
	}
}

func TestIssueQueriesSetRunActiveTx(t *testing.T) {
	s := newIssueQueryStore(t)
	run := seedEventRun(t, s.db, model.RunPlanning, 1)
	tx := beginEventTx(t, s.db)
	readRun := func() (string, int64, sql.NullInt64, int) {
		var (
			status    string
			updatedMS int64
			activated sql.NullInt64
			version   int
		)
		err := tx.QueryRow(`SELECT status, updated_at_ms, activated_at_ms, row_version FROM runs WHERE id = ?`, run).
			Scan(&status, &updatedMS, &activated, &version)
		testsupport.Must(t, err, "reading the run: %v", err)
		return status, updatedMS, activated, version
	}

	testsupport.Must(t, SetRunActiveTx(tx, run, false, 5000), "SetRunActiveTx(first)")
	status, updatedMS, activated, version := readRun()
	if status != string(model.RunActive) || updatedMS != 5000 || activated.Int64 != 5000 || version != 2 {
		t.Errorf("first activation = (%s, %d, %v, v%d), want (active, 5000, 5000, v2)",
			status, updatedMS, activated, version)
	}

	testsupport.Must(t, SetRunActiveTx(tx, run, true, 9000), "SetRunActiveTx(re-activation)")
	status, updatedMS, activated, version = readRun()
	if status != string(model.RunActive) || updatedMS != 9000 || activated.Int64 != 5000 || version != 3 {
		t.Errorf("re-activation = (%s, %d, %v, v%d), want (active, 9000, 5000, v3)",
			status, updatedMS, activated, version)
	}

	if err := SetRunActiveTx(tx, 999, false, 5000); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("missing run: err = %v, want ErrRunNotFound", err)
	}
}

func TestIssueQueriesIssueSchedulingFactsTx(t *testing.T) {
	s := newIssueQueryStore(t)
	scoped := s.issue(t, "scoped", model.StatusTodo)
	mustExec(t, s.db, `UPDATE issues SET priority = ?, scope_globs = ? WHERE id = ?`,
		string(model.PriorityCritical), `["internal/**"]`, scoped)
	unscoped := s.issue(t, "unscoped", model.StatusTodo)
	tx := beginEventTx(t, s.db)

	priority, scope, err := IssueSchedulingFactsTx(tx, scoped)
	testsupport.Must(t, err, "IssueSchedulingFactsTx: %v", err)
	if priority != model.PriorityCritical || scope != `["internal/**"]` {
		t.Errorf("scoped facts = (%s, %q), want (critical, %q)", priority, scope, `["internal/**"]`)
	}
	priority, scope, err = IssueSchedulingFactsTx(tx, unscoped)
	testsupport.Must(t, err, "IssueSchedulingFactsTx(unscoped): %v", err)
	if priority != model.PriorityMedium || scope != "" {
		t.Errorf("unscoped facts = (%s, %q), want (medium, \"\")", priority, scope)
	}

	if _, _, err := IssueSchedulingFactsTx(tx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing issue: err = %v, want ErrNotFound", err)
	}
}

func TestIssueQueriesIssueDependenciesDoneTx(t *testing.T) {
	s := newIssueQueryStore(t)
	done := s.issue(t, "done", model.StatusDone)
	open := s.issue(t, "open", model.StatusTodo)
	blocked := s.issue(t, "waits on open", model.StatusTodo)
	ready := s.issue(t, "waits on done", model.StatusTodo)
	unrelated := s.issue(t, "relates to open", model.StatusTodo)
	s.relate(t, blocked, done, model.RelationDependsOn)
	s.relate(t, blocked, open, model.RelationDependsOn)
	s.relate(t, ready, done, model.RelationDependsOn)
	s.relate(t, unrelated, open, model.RelationRelatesTo)
	s.relate(t, open, ready, model.RelationBlocks)
	tx := beginEventTx(t, s.db)

	cases := []struct {
		name string
		id   int
		want bool
	}{
		{"one predecessor still open", blocked, false},
		{"every predecessor done", ready, true},
		{"only a non-dependency relation to an open issue", unrelated, true},
		{"no predecessors", done, true},
		{"missing issue", 999, true},
	}
	for _, c := range cases {
		got, err := IssueDependenciesDoneTx(tx, c.id)
		testsupport.Must(t, err, "%s: %v", c.name, err)
		if got != c.want {
			t.Errorf("%s: IssueDependenciesDoneTx = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIssueQueriesForeignHoldingStepsTx(t *testing.T) {
	s := newIssueQueryStore(t)
	here := seedEventRun(t, s.db, model.RunActive, 1)
	other := seedEventRun(t, s.db, model.RunActive, 1)
	finished := seedEventRun(t, s.db, model.RunDone, 1)
	abandoned := seedEventRun(t, s.db, model.RunAbandoned, 1)
	// The first identity claims the unclaimed default project 1 in place.
	_, err := EnsureProject(s.db, "this-repo", "this", 1000)
	testsupport.Must(t, err, "EnsureProject(this): %v", err)
	otherProjectID, err := EnsureProject(s.db, "other-repo", "other", 1000)
	testsupport.Must(t, err, "EnsureProject(other): %v", err)
	otherProject := seedEventRun(t, s.db, model.RunActive, otherProjectID)
	issue := s.issue(t, "subject", model.StatusInProgress)

	s.step(t, here, issue, "own@0", StepClaimed)
	claimed := s.step(t, other, issue, "claimed@0", StepClaimed)
	running := s.step(t, other, issue, "running@0", StepRunning)
	s.step(t, other, issue, "pending@0", StepPending)
	s.step(t, other, issue, "gated@0", StepGated)
	s.step(t, finished, issue, "finished@0", StepClaimed)
	s.step(t, abandoned, issue, "abandoned@0", StepRunning)
	s.step(t, otherProject, issue, "elsewhere@0", StepClaimed)
	tx := beginEventTx(t, s.db)

	type holder struct {
		ID, IssueID int
		Status      string
	}
	holders := func(steps []*Step) []holder {
		var out []holder
		for _, st := range steps {
			out = append(out, holder{st.ID, st.IssueID, st.Status})
		}
		return out
	}

	got, err := ForeignHoldingStepsTx(tx, 1, here)
	testsupport.Must(t, err, "ForeignHoldingStepsTx: %v", err)
	if want := []holder{{claimed, issue, StepClaimed}, {running, issue, StepRunning}}; !reflect.DeepEqual(holders(got), want) {
		t.Errorf("ForeignHoldingStepsTx = %+v, want %+v", holders(got), want)
	}

	got, err = ForeignHoldingStepsTx(tx, otherProjectID, otherProject)
	testsupport.Must(t, err, "ForeignHoldingStepsTx(other project): %v", err)
	if len(got) != 0 {
		t.Errorf("the other project's only run sees %+v held elsewhere, want none", holders(got))
	}
}

func TestIssueQueriesRelationTargetsTx(t *testing.T) {
	s := newIssueQueryStore(t)
	consumer := s.issue(t, "consumer", model.StatusTodo)
	specA := s.issue(t, "spec a", model.StatusDone)
	specB := s.issue(t, "spec b", model.StatusDone)
	pointsBack := s.issue(t, "points back", model.StatusTodo)
	s.relate(t, consumer, specB, model.RelationDependsOn)
	s.relate(t, consumer, specA, model.RelationDependsOn)
	s.relate(t, consumer, pointsBack, model.RelationRelatesTo)
	s.relate(t, pointsBack, consumer, model.RelationBlocks)
	tx := beginEventTx(t, s.db)

	got, err := RelationTargetsTx(tx, consumer, model.RelationDependsOn)
	testsupport.Must(t, err, "RelationTargetsTx: %v", err)
	if want := []int{specA, specB}; !reflect.DeepEqual(got, want) {
		t.Errorf("RelationTargetsTx(depends_on) = %v, want %v", got, want)
	}

	for name, args := range map[string]struct {
		id int
		rt model.RelationType
	}{
		"no relation of the type": {consumer, model.RelationBlocks},
		"only the target side":    {specA, model.RelationDependsOn},
		"missing issue":           {999, model.RelationDependsOn},
	} {
		got, err := RelationTargetsTx(tx, args.id, args.rt)
		testsupport.Must(t, err, "%s: %v", name, err)
		if len(got) != 0 {
			t.Errorf("%s: RelationTargetsTx = %v, want none", name, got)
		}
	}
}

func TestIssueQueriesRelationSourcesTx(t *testing.T) {
	s := newIssueQueryStore(t)
	spec := s.issue(t, "spec", model.StatusDone)
	consumerB := s.issue(t, "consumer b", model.StatusTodo)
	consumerA := s.issue(t, "consumer a", model.StatusTodo)
	s.relate(t, consumerB, spec, model.RelationDependsOn)
	s.relate(t, consumerA, spec, model.RelationDependsOn)
	s.relate(t, spec, consumerA, model.RelationRelatesTo)
	tx := beginEventTx(t, s.db)

	got, err := RelationSourcesTx(tx, spec, model.RelationDependsOn)
	testsupport.Must(t, err, "RelationSourcesTx: %v", err)
	if want := []int{consumerB, consumerA}; !reflect.DeepEqual(got, want) {
		t.Errorf("RelationSourcesTx(depends_on) = %v, want %v", got, want)
	}

	for name, args := range map[string]struct {
		id int
		rt model.RelationType
	}{
		"no relation of the type": {spec, model.RelationBlocks},
		"only the source side":    {consumerA, model.RelationDependsOn},
		"missing issue":           {999, model.RelationDependsOn},
	} {
		got, err := RelationSourcesTx(tx, args.id, args.rt)
		testsupport.Must(t, err, "%s: %v", name, err)
		if len(got) != 0 {
			t.Errorf("%s: RelationSourcesTx = %v, want none", name, got)
		}
	}
}

func TestIssueQueriesLatestRecordedArtifactIDTx(t *testing.T) {
	s := newIssueQueryStore(t)
	runA := seedEventRun(t, s.db, model.RunDone, 1)
	runB := seedEventRun(t, s.db, model.RunActive, 1)
	spec := s.issue(t, "spec", model.StatusDone)
	other := s.issue(t, "other", model.StatusDone)
	unrecorded := s.issue(t, "unrecorded", model.StatusTodo)

	doneStep := s.step(t, runA, spec, "spec@0", StepDone)
	supersededStep := s.step(t, runB, spec, "spec@0", StepSuperseded)
	liveStep := s.step(t, runB, spec, "spec@1", StepRunning)
	otherStep := s.step(t, runB, other, "spec@0", StepDone)
	gatedStep := s.step(t, runB, unrecorded, "spec@0", StepGated)

	s.artifact(t, runA, doneStep, "ux-spec", "first", "")
	newest := s.artifact(t, runB, supersededStep, "ux-spec", "revised", "")
	s.artifact(t, runB, liveStep, "ux-spec", "unrecorded", "")
	s.artifact(t, runB, doneStep, "change-summary", "other kind", "")
	s.artifact(t, runB, otherStep, "ux-spec", "other issue", "")
	s.artifact(t, runB, gatedStep, "ux-spec", "gated", "")
	tx := beginEventTx(t, s.db)

	id, found, err := LatestRecordedArtifactIDTx(tx, spec, "ux-spec")
	testsupport.Must(t, err, "LatestRecordedArtifactIDTx: %v", err)
	if !found || id != newest {
		t.Errorf("LatestRecordedArtifactIDTx = (%d, %v), want (%d, true)", id, found, newest)
	}

	for name, args := range map[string]struct {
		issue int
		kind  string
	}{
		"no recorded step":        {unrecorded, "ux-spec"},
		"no artifact of the kind": {spec, "threat-model"},
		"missing issue":           {999, "ux-spec"},
	} {
		id, found, err := LatestRecordedArtifactIDTx(tx, args.issue, args.kind)
		testsupport.Must(t, err, "%s: %v", name, err)
		if found || id != 0 {
			t.Errorf("%s: LatestRecordedArtifactIDTx = (%d, %v), want (0, false)", name, id, found)
		}
	}
}

func TestIssueQueriesPinnedArtifactTx(t *testing.T) {
	s := newIssueQueryStore(t)
	run := seedEventRun(t, s.db, model.RunActive, 1)
	spec := s.issue(t, "spec", model.StatusDone)
	step := s.step(t, run, spec, "spec@0", StepDone)
	produced := s.artifact(t, run, step, "ux-spec", "the spec", `{"ok":true}`)
	orphan := s.artifact(t, run, 0, "note", "no producer", "")
	tx := beginEventTx(t, s.db)

	got, err := PinnedArtifactTx(tx, produced)
	testsupport.Must(t, err, "PinnedArtifactTx: %v", err)
	want := PinnedArtifact{
		Kind: "ux-spec", Body: "the spec", Payload: `{"ok":true}`,
		ProducerIssueID: spec, ProducerInstance: "spec@0",
	}
	if got != want {
		t.Errorf("PinnedArtifactTx = %+v, want %+v", got, want)
	}

	got, err = PinnedArtifactTx(tx, orphan)
	testsupport.Must(t, err, "PinnedArtifactTx(no step): %v", err)
	if want := (PinnedArtifact{Kind: "note", Body: "no producer"}); got != want {
		t.Errorf("PinnedArtifactTx(no step) = %+v, want %+v", got, want)
	}

	if _, err := PinnedArtifactTx(tx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing artifact: err = %v, want ErrNotFound", err)
	}
}

func TestIssueQueriesIssueDescription(t *testing.T) {
	s := newIssueQueryStore(t)
	id, err := CreateIssue(s.db, &model.Issue{
		Title: "described", Description: "the live body", Status: model.StatusTodo,
		Priority: model.PriorityMedium, Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue: %v", err)
	bare := s.issue(t, "bare", model.StatusTodo)
	mustExec(t, s.db, `UPDATE issues SET description = NULL WHERE id = ?`, bare)

	got, err := IssueDescription(s.db, id)
	testsupport.Must(t, err, "IssueDescription: %v", err)
	if got != "the live body" {
		t.Errorf("IssueDescription = %q, want %q", got, "the live body")
	}
	got, err = IssueDescription(s.db, bare)
	testsupport.Must(t, err, "IssueDescription(NULL): %v", err)
	if got != "" {
		t.Errorf("IssueDescription(NULL) = %q, want \"\"", got)
	}

	if _, err := IssueDescription(s.db, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing issue: err = %v, want ErrNotFound", err)
	}
}

func TestIssueQueriesFrozenIssueBindings(t *testing.T) {
	s := newIssueQueryStore(t)
	issue := s.issue(t, "subject", model.StatusInProgress)
	otherIssue := s.issue(t, "other", model.StatusInProgress)
	unbound := s.issue(t, "unbound", model.StatusTodo)
	live := seedEventRun(t, s.db, model.RunActive, 1)
	parked := seedEventRun(t, s.db, model.RunWaitingHuman, 1)
	allRecorded := seedEventRun(t, s.db, model.RunActive, 1)
	unactivated := seedEventRun(t, s.db, model.RunPlanning, 1)
	finished := seedEventRun(t, s.db, model.RunDone, 1)
	abandoned := seedEventRun(t, s.db, model.RunAbandoned, 1)
	for _, run := range []int{live, parked, allRecorded, unactivated, finished, abandoned} {
		testsupport.Must(t, AddRunIssue(s.db, run, issue), "AddRunIssue")
	}
	testsupport.Must(t, AddRunIssue(s.db, live, otherIssue), "AddRunIssue(other)")
	snapshot := func(run, issueID int, issueSnap string, body any) {
		mustExec(t, s.db,
			`UPDATE run_issues SET issue_snapshot = ?, body_snapshot = ? WHERE run_id = ? AND issue_id = ?`,
			issueSnap, body, run, issueID)
	}
	snapshot(live, issue, `{"scope":["a/**"]}`, "frozen body")
	snapshot(parked, issue, `{"scope":[]}`, nil)
	snapshot(allRecorded, issue, `{}`, "x")
	snapshot(finished, issue, `{}`, "x")
	snapshot(abandoned, issue, `{}`, "x")
	snapshot(live, otherIssue, `{}`, "other body")

	s.step(t, live, issue, "implement@0", StepDone)
	s.step(t, live, issue, "review@0", StepPending)
	s.step(t, live, issue, "verify@0", StepWaitingHuman)
	s.step(t, live, otherIssue, "implement@0", StepPending)
	s.step(t, parked, issue, "implement@0", StepClaimed)
	s.step(t, allRecorded, issue, "done@0", StepDone)
	s.step(t, allRecorded, issue, "skipped@0", StepSkipped)
	s.step(t, allRecorded, issue, "superseded@0", StepSuperseded)
	s.step(t, allRecorded, issue, "routed@0", StepFailedRouted)
	s.step(t, unactivated, issue, "implement@0", StepPending)
	s.step(t, finished, issue, "implement@0", StepPending)
	s.step(t, abandoned, issue, "implement@0", StepPending)

	got, err := FrozenIssueBindings(s.db, issue)
	testsupport.Must(t, err, "FrozenIssueBindings: %v", err)
	want := []FrozenIssueBinding{
		{RunID: live, IssueSnapshot: `{"scope":["a/**"]}`, BodySnapshot: "frozen body", LiveSteps: 2},
		{RunID: parked, IssueSnapshot: `{"scope":[]}`, BodySnapshot: "", LiveSteps: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FrozenIssueBindings = %+v, want %+v", got, want)
	}

	for name, id := range map[string]int{"unbound issue": unbound, "missing issue": 999} {
		got, err := FrozenIssueBindings(s.db, id)
		testsupport.Must(t, err, "%s: %v", name, err)
		if len(got) != 0 {
			t.Errorf("%s: FrozenIssueBindings = %+v, want none", name, got)
		}
	}
}
