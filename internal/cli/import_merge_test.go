package cli

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// Re-importing a project's own export with --merge used to fail on
// a foreign-key constraint. The id remap probed the whole store, so every id
// of the project's own rows looked like a collision: issues were reassigned
// fresh ids and duplicated, the label re-insert died silently on
// UNIQUE(project_id, name), and the first issue-label mapping then pointed at
// a label that was never written.

func tableCount(t *testing.T, conn *sql.DB, table string) int {
	t.Helper()
	var n int
	err := conn.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
	testsupport.Must(t, err, "counting %s: %v", table, err)
	return n
}

func labelNames(t *testing.T, conn *sql.DB, issueID int) []string {
	t.Helper()
	labels, err := db.GetIssueLabelObjects(conn, issueID)
	testsupport.Must(t, err, "GetIssueLabelObjects(%d): %v", issueID, err)
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l.Name)
	}
	return out
}

// seedLabeled is the export fixture: a labeled issue with a comment.
func seedLabeled(t *testing.T, conn *sql.DB) int {
	t.Helper()
	id := createIssue(t, conn, "labeled issue", model.StatusTodo, model.PriorityMedium)
	err := db.AddLabelsToIssue(conn, id, []string{"bug"}, "", "tester")
	testsupport.Must(t, err, "AddLabelsToIssue: %v", err)
	_, err = db.CreateComment(conn, &model.Comment{IssueID: id, Body: "a comment", Author: "tester"})
	testsupport.Must(t, err, "CreateComment: %v", err)
	return id
}

// TestImportMergeOwnExportSkipsEverything is the acceptance criterion: an
// unchanged project merging its own export writes nothing and reports every
// row as a duplicate.
func TestImportMergeOwnExportSkipsEverything(t *testing.T) {
	conn := newTestDB(t)
	issueID := seedLabeled(t, conn)
	export := buildExport(t, conn)

	before := map[string]int{}
	for _, table := range []string{"issues", "labels", "issue_labels", "comments", "activity_log"} {
		before[table] = tableCount(t, conn, table)
	}

	result, err := doImport(conn, export, false, db.DefaultProjectID)
	testsupport.Must(t, err, "merging the project's own export: %v", err)
	if result.Imported != 0 || result.Remapped != 0 {
		t.Errorf("merge of an unchanged project imported %d and remapped %d rows, want 0 and 0",
			result.Imported, result.Remapped)
	}
	if result.Skipped == 0 {
		t.Error("merge of an unchanged project skipped nothing")
	}
	for table, n := range before {
		if got := tableCount(t, conn, table); got != n {
			t.Errorf("%s has %d rows after the merge, want %d", table, got, n)
		}
	}
	if got := labelNames(t, conn, issueID); len(got) != 1 || got[0] != "bug" {
		t.Errorf("issue labels = %v, want [bug]", got)
	}
}

// TestImportMergeOwnExportAfterChanges is the sweep's shape: label and issue
// changes land between the export and the merge, and the merge still skips
// what it already has without disturbing the newer rows.
func TestImportMergeOwnExportAfterChanges(t *testing.T) {
	conn := newTestDB(t)
	first := seedLabeled(t, conn)
	export := buildExport(t, conn)

	second := createIssue(t, conn, "newer issue", model.StatusBacklog, model.PriorityLow)
	err := db.AddLabelsToIssue(conn, second, []string{"urgent", "bug"}, "", "tester")
	testsupport.Must(t, err, "AddLabelsToIssue: %v", err)
	issuesBefore := tableCount(t, conn, "issues")
	labelsBefore := tableCount(t, conn, "labels")

	result, err := doImport(conn, export, false, db.DefaultProjectID)
	testsupport.Must(t, err, "merging after changes: %v", err)
	if result.Imported != 0 || result.Remapped != 0 {
		t.Errorf("imported %d and remapped %d rows, want 0 and 0", result.Imported, result.Remapped)
	}
	if got := tableCount(t, conn, "issues"); got != issuesBefore {
		t.Errorf("issues = %d after the merge, want %d", got, issuesBefore)
	}
	if got := tableCount(t, conn, "labels"); got != labelsBefore {
		t.Errorf("labels = %d after the merge, want %d", got, labelsBefore)
	}
	if got := labelNames(t, conn, first); len(got) != 1 || got[0] != "bug" {
		t.Errorf("first issue's labels = %v, want [bug]", got)
	}
	if got := labelNames(t, conn, second); len(got) != 2 {
		t.Errorf("second issue's labels = %v, want both of its own", got)
	}
}

// TestImportMergeMapsLabelsByName: a label deleted and recreated under a new
// id is still the label the export's mappings mean. The merge lands the
// mapping on the existing row rather than inserting a duplicate name that
// UNIQUE(project_id, name) would silently drop.
func TestImportMergeMapsLabelsByName(t *testing.T) {
	conn := newTestDB(t)
	first := seedLabeled(t, conn)
	export := buildExport(t, conn)

	old, err := db.GetLabelByName(conn, db.DefaultProjectID, "bug")
	testsupport.Must(t, err, "GetLabelByName: %v", err)
	_, err = db.DeleteLabel(conn, old.ID, "bug", "tester")
	testsupport.Must(t, err, "DeleteLabel: %v", err)

	other := createIssue(t, conn, "another issue", model.StatusTodo, model.PriorityMedium)
	err = db.AddLabelsToIssue(conn, other, []string{"bug"}, "", "tester")
	testsupport.Must(t, err, "AddLabelsToIssue: %v", err)
	recreated, err := db.GetLabelByName(conn, db.DefaultProjectID, "bug")
	testsupport.Must(t, err, "GetLabelByName: %v", err)
	if recreated.ID == old.ID {
		t.Fatalf("test premise broken: the recreated label reused id %d", old.ID)
	}

	result, err := doImport(conn, export, false, db.DefaultProjectID)
	testsupport.Must(t, err, "merging after a label was recreated: %v", err)
	if result.Remapped != 0 {
		t.Errorf("a name match counted as %d remapped rows, want 0", result.Remapped)
	}

	labels, err := db.GetIssueLabelObjects(conn, first)
	testsupport.Must(t, err, "GetIssueLabelObjects: %v", err)
	if len(labels) != 1 || labels[0].ID != recreated.ID {
		t.Errorf("first issue's labels = %+v, want the recreated bug label (id %d)", labels, recreated.ID)
	}
	var named int
	err = conn.QueryRow(`SELECT COUNT(*) FROM labels WHERE name = 'bug'`).Scan(&named)
	testsupport.Must(t, err, "counting bug labels: %v", err)
	if named != 1 {
		t.Errorf("%d labels named bug after the merge, want 1", named)
	}
}

// TestImportMergeSkipsDanglingMapping: a mapping whose issue is absent from
// both the export and the project is skipped, never a raw foreign-key
// failure that rolls back the whole import.
func TestImportMergeSkipsDanglingMapping(t *testing.T) {
	conn := newTestDB(t)
	seedLabeled(t, conn)
	export := buildExport(t, conn)

	bug, err := db.GetLabelByName(conn, db.DefaultProjectID, "bug")
	testsupport.Must(t, err, "GetLabelByName: %v", err)
	export.IssueLabelMappings = append(export.IssueLabelMappings,
		model.IssueLabelMapping{IssueID: 9999, LabelID: bug.ID})
	export.IssueFileMappings = append(export.IssueFileMappings,
		model.IssueFileMapping{IssueID: 9999, FilePath: "nowhere.go"})
	mappingsBefore := tableCount(t, conn, "issue_labels")

	result, err := doImport(conn, export, false, db.DefaultProjectID)
	testsupport.Must(t, err, "merging an export with a dangling mapping: %v", err)
	if result.Imported != 0 {
		t.Errorf("imported %d rows, want 0", result.Imported)
	}
	if got := tableCount(t, conn, "issue_labels"); got != mappingsBefore {
		t.Errorf("issue_labels = %d after the merge, want %d", got, mappingsBefore)
	}
}
