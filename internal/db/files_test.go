package db

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func TestAttachFiles(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id := mustCreateIssue(t, db, "attach-test")

	// Attach two files.
	err = AttachFiles(db, id, []string{"main.go", "util.go"}, "alice")
	testsupport.Must(t, err, "AttachFiles: %v", err)

	files, err := GetIssueFiles(db, id)
	testsupport.Must(t, err, "GetIssueFiles: %v", err)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if files[0] != "main.go" || files[1] != "util.go" {
		t.Errorf("unexpected files: %v", files)
	}
}

func TestAttachFilesIdempotent(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id := mustCreateIssue(t, db, "idempotent-test")

	// Attach the same file twice in the same call.
	err = AttachFiles(db, id, []string{"main.go", "main.go"}, "alice")
	testsupport.Must(t, err, "AttachFiles: %v", err)

	files, err := GetIssueFiles(db, id)
	testsupport.Must(t, err, "GetIssueFiles: %v", err)
	if len(files) != 1 {
		t.Errorf("expected 1 file after duplicate attach, got %d", len(files))
	}

	// Attach again in a separate call.
	err = AttachFiles(db, id, []string{"main.go"}, "alice")
	testsupport.Must(t, err, "AttachFiles second call: %v", err)

	files, err = GetIssueFiles(db, id)
	testsupport.Must(t, err, "GetIssueFiles: %v", err)
	if len(files) != 1 {
		t.Errorf("expected 1 file after re-attach, got %d", len(files))
	}
}

func TestAttachFilesEmpty(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id := mustCreateIssue(t, db, "empty-test")

	// Attaching no files should be a no-op.
	err = AttachFiles(db, id, nil, "alice")
	testsupport.Must(t, err, "AttachFiles nil: %v", err)
	err = AttachFiles(db, id, []string{}, "alice")
	testsupport.Must(t, err, "AttachFiles empty: %v", err)
}

func TestDetachFiles(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id := mustCreateIssue(t, db, "detach-test")

	err = AttachFiles(db, id, []string{"a.go", "b.go", "c.go"}, "alice")
	testsupport.Must(t, err, "AttachFiles: %v", err)

	// Detach one file.
	err = DetachFiles(db, id, []string{"b.go"}, "alice")
	testsupport.Must(t, err, "DetachFiles: %v", err)

	files, err := GetIssueFiles(db, id)
	testsupport.Must(t, err, "GetIssueFiles: %v", err)
	if len(files) != 2 {
		t.Fatalf("expected 2 files after detach, got %d", len(files))
	}
	if files[0] != "a.go" || files[1] != "c.go" {
		t.Errorf("unexpected files after detach: %v", files)
	}
}

func TestDetachFilesNonExistent(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id := mustCreateIssue(t, db, "detach-nonexistent-test")

	// Detaching a file that was never attached should not error.
	err = DetachFiles(db, id, []string{"no-such-file.go"}, "alice")
	testsupport.Must(t, err, "DetachFiles non-existent: %v", err)
}

func TestDetachFilesEmpty(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id := mustCreateIssue(t, db, "detach-empty-test")

	err = DetachFiles(db, id, nil, "alice")
	testsupport.Must(t, err, "DetachFiles nil: %v", err)
	err = DetachFiles(db, id, []string{}, "alice")
	testsupport.Must(t, err, "DetachFiles empty: %v", err)
}

func TestGetIssueFilesEmpty(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id := mustCreateIssue(t, db, "no-files-test")

	files, err := GetIssueFiles(db, id)
	testsupport.Must(t, err, "GetIssueFiles: %v", err)
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d", len(files))
	}
}

func TestGetIssueFilesSorted(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id := mustCreateIssue(t, db, "sorted-test")

	// Attach in reverse order.
	err = AttachFiles(db, id, []string{"z.go", "a.go", "m.go"}, "alice")
	testsupport.Must(t, err, "AttachFiles: %v", err)

	files, err := GetIssueFiles(db, id)
	testsupport.Must(t, err, "GetIssueFiles: %v", err)
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(files))
	}
	if files[0] != "a.go" || files[1] != "m.go" || files[2] != "z.go" {
		t.Errorf("expected alphabetical order, got %v", files)
	}
}

func TestSetIssueFiles(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id := mustCreateIssue(t, db, "set-files-test")

	// Set initial files.
	err = SetIssueFiles(db, id, []string{"old.go", "keep.go"}, "alice")
	testsupport.Must(t, err, "SetIssueFiles initial: %v", err)

	files, err := GetIssueFiles(db, id)
	testsupport.Must(t, err, "GetIssueFiles: %v", err)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	// Replace with different files.
	err = SetIssueFiles(db, id, []string{"new.go", "keep.go"}, "bob")
	testsupport.Must(t, err, "SetIssueFiles replace: %v", err)

	files, err = GetIssueFiles(db, id)
	testsupport.Must(t, err, "GetIssueFiles after replace: %v", err)
	if len(files) != 2 {
		t.Fatalf("expected 2 files after replace, got %d", len(files))
	}
	if files[0] != "keep.go" || files[1] != "new.go" {
		t.Errorf("unexpected files after replace: %v", files)
	}
}

func TestSetIssueFilesDoesNotMutateInput(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id := mustCreateIssue(t, db, "no-mutate-test")

	input := []string{"z.go", "a.go", "m.go"}
	original := make([]string, len(input))
	copy(original, input)

	err = SetIssueFiles(db, id, input, "alice")
	testsupport.Must(t, err, "SetIssueFiles: %v", err)

	// Verify input slice was not reordered.
	for i := range input {
		if input[i] != original[i] {
			t.Errorf("input[%d] = %q, want %q (slice was mutated)", i, input[i], original[i])
		}
	}
}

func TestHydrateFiles(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id1 := mustCreateIssue(t, db, "hydrate-1")
	id2 := mustCreateIssue(t, db, "hydrate-2")
	id3 := mustCreateIssue(t, db, "hydrate-3")

	err = AttachFiles(db, id1, []string{"a.go", "b.go"}, "alice")
	testsupport.Must(t, err, "AttachFiles id1: %v", err)
	err = AttachFiles(db, id2, []string{"c.go"}, "alice")
	testsupport.Must(t, err, "AttachFiles id2: %v", err)
	// id3 has no files.

	issues := []*model.Issue{
		{ID: id1},
		{ID: id2},
		{ID: id3},
	}

	err = HydrateFiles(db, issues)
	testsupport.Must(t, err, "HydrateFiles: %v", err)

	if len(issues[0].Files) != 2 {
		t.Errorf("issue 1: expected 2 files, got %d", len(issues[0].Files))
	}
	if len(issues[1].Files) != 1 {
		t.Errorf("issue 2: expected 1 file, got %d", len(issues[1].Files))
	}
	if len(issues[2].Files) != 0 {
		t.Errorf("issue 3: expected 0 files, got %d", len(issues[2].Files))
	}
}

func TestHydrateFilesEmpty(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	// No issues to hydrate should be a no-op.
	err = HydrateFiles(db, nil)
	testsupport.Must(t, err, "HydrateFiles nil: %v", err)
	err = HydrateFiles(db, []*model.Issue{})
	testsupport.Must(t, err, "HydrateFiles empty: %v", err)
}

func TestListAllIssueFileMappings(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id1 := mustCreateIssue(t, db, "mapping-1")
	id2 := mustCreateIssue(t, db, "mapping-2")

	err = AttachFiles(db, id1, []string{"x.go", "y.go"}, "alice")
	testsupport.Must(t, err, "AttachFiles id1: %v", err)
	err = AttachFiles(db, id2, []string{"z.go"}, "bob")
	testsupport.Must(t, err, "AttachFiles id2: %v", err)

	mappings, err := ListAllIssueFileMappings(db, 0)
	testsupport.Must(t, err, "ListAllIssueFileMappings: %v", err)
	if len(mappings) != 3 {
		t.Fatalf("expected 3 mappings, got %d", len(mappings))
	}

	// Verify both issue IDs are present.
	issueIDs := make(map[int]bool)
	for _, m := range mappings {
		issueIDs[m.IssueID] = true
	}
	if !issueIDs[id1] || !issueIDs[id2] {
		t.Errorf("expected mappings for issues %d and %d, got %v", id1, id2, issueIDs)
	}
}

func TestInsertIssueFileMapping(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id := mustCreateIssue(t, db, "insert-mapping-test")

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)

	inserted, err := InsertIssueFileMapping(tx, id, "test.go")
	testsupport.Must(t, err, "InsertIssueFileMapping: %v", err)
	if !inserted {
		t.Error("expected inserted=true for new mapping")
	}

	// Duplicate should return false.
	inserted, err = InsertIssueFileMapping(tx, id, "test.go")
	testsupport.Must(t, err, "InsertIssueFileMapping duplicate: %v", err)
	if inserted {
		t.Error("expected inserted=false for duplicate mapping")
	}

	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	files, err := GetIssueFiles(db, id)
	testsupport.Must(t, err, "GetIssueFiles: %v", err)
	if len(files) != 1 {
		t.Errorf("expected 1 file, got %d", len(files))
	}
}

func TestCreateIssueWithFiles(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id, err := CreateIssue(db, &model.Issue{
		Title: "with-files", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, []string{"main.go", "lib.go"})
	testsupport.Must(t, err, "CreateIssue: %v", err)

	files, err := GetIssueFiles(db, id)
	testsupport.Must(t, err, "GetIssueFiles: %v", err)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if files[0] != "lib.go" || files[1] != "main.go" {
		t.Errorf("unexpected files: %v", files)
	}
}

func TestCreateIssueDoesNotMutateFileInput(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	input := []string{"z.go", "a.go"}
	original := make([]string, len(input))
	copy(original, input)

	if _, err := CreateIssue(db, &model.Issue{
		Title: "no-mutate", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, input); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	for i := range input {
		if input[i] != original[i] {
			t.Errorf("input[%d] = %q, want %q (slice was mutated)", i, input[i], original[i])
		}
	}
}

func TestAttachFilesRecordsActivity(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id := mustCreateIssue(t, db, "activity-test")

	err = AttachFiles(db, id, []string{"main.go"}, "alice")
	testsupport.Must(t, err, "AttachFiles: %v", err)

	// Verify activity was recorded.
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM activity_log WHERE issue_id = ? AND field_changed = 'files'`,
		id,
	).Scan(&count); err != nil {
		t.Fatalf("counting activity: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 files activity entry, got %d", count)
	}
}

func TestDetachFilesRecordsActivity(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	id := mustCreateIssue(t, db, "detach-activity-test")

	err = AttachFiles(db, id, []string{"main.go"}, "alice")
	testsupport.Must(t, err, "AttachFiles: %v", err)
	err = DetachFiles(db, id, []string{"main.go"}, "bob")
	testsupport.Must(t, err, "DetachFiles: %v", err)

	// Should have 2 file activity entries: one for attach, one for detach.
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM activity_log WHERE issue_id = ? AND field_changed = 'files'`,
		id,
	).Scan(&count); err != nil {
		t.Fatalf("counting activity: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 files activity entries, got %d", count)
	}
}
