package db

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// newCASDB returns a migrated database with one issue at version 1.
func newCASDB(t *testing.T) (*sql.DB, int) {
	t.Helper()
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = Migrate(db)
	testsupport.Must(t, err, "Migrate: %v", err)
	id, err := CreateIssue(db, &model.Issue{
		Title:    "cas subject",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue: %v", err)
	return db, id
}

func TestNewIssueStartsAtVersionOne(t *testing.T) {
	db, id := newCASDB(t)

	v, err := GetVersion(db, "issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)
	if v != 1 {
		t.Errorf("new issue version = %d, want 1", v)
	}
}

func TestUpdateBumpsVersionWithoutPrecondition(t *testing.T) {
	db, id := newCASDB(t)

	for want := 2; want <= 4; want++ {
		err := UpdateIssue(db, id, map[string]any{"title": "t"}, "")
		testsupport.Must(t, err, "UpdateIssue: %v", err)
		got, err := GetVersion(db, "issues", id)
		testsupport.Must(t, err, "GetVersion: %v", err)
		if got != want {
			t.Errorf("version after update = %d, want %d", got, want)
		}
	}
}

func TestCASMatchingVersionSucceeds(t *testing.T) {
	db, id := newCASDB(t)

	one := 1
	err := UpdateIssueCAS(db, id, map[string]any{"status": "done"}, "", &one)
	testsupport.Must(t, err, "UpdateIssueCAS with correct version: %v", err)

	issue, err := GetIssue(db, id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if issue.Status != model.StatusDone {
		t.Errorf("status = %q, want done", issue.Status)
	}
	if issue.Version != 2 {
		t.Errorf("version = %d, want 2", issue.Version)
	}
}

func TestCASStaleVersionConflictsAndDoesNotWrite(t *testing.T) {
	db, id := newCASDB(t)

	// Advance to version 2.
	err := UpdateIssue(db, id, map[string]any{"title": "first"}, "")
	testsupport.Must(t, err, "UpdateIssue: %v", err)

	stale := 1
	err = UpdateIssueCAS(db, id, map[string]any{"title": "second"}, "", &stale)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("error = %v, want ErrVersionConflict", err)
	}

	// The refused write must not have applied — this is the whole point.
	issue, err := GetIssue(db, id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if issue.Title != "first" {
		t.Errorf("title = %q, want %q — conflicting write leaked through", issue.Title, "first")
	}
	if issue.Version != 2 {
		t.Errorf("version = %d, want 2 — conflict must not bump", issue.Version)
	}
}

// A CAS against a missing row is NOT_FOUND, not CONFLICT. Collapsing the two
// would report a deleted entity as a concurrent-write conflict.
func TestCASMissingRowIsNotFound(t *testing.T) {
	db, _ := newCASDB(t)

	one := 1
	err := UpdateIssueCAS(db, 999999, map[string]any{"title": "x"}, "", &one)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	if errors.Is(err, ErrVersionConflict) {
		t.Error("missing row reported as a version conflict")
	}
}

// Two writers read the same version; only one may win.
func TestCASConcurrentWritersOnlyOneWins(t *testing.T) {
	db, id := newCASDB(t)

	shared, err := GetVersion(db, "issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)

	err = UpdateIssueCAS(db, id, map[string]any{"title": "writer-A"}, "", &shared)
	testsupport.Must(t, err, "first writer: %v", err)
	err = UpdateIssueCAS(db, id, map[string]any{"title": "writer-B"}, "", &shared)
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("second writer error = %v, want ErrVersionConflict", err)
	}

	issue, err := GetIssue(db, id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if issue.Title != "writer-A" {
		t.Errorf("title = %q, want writer-A (lost update)", issue.Title)
	}
}

// --if-version on an empty update must still enforce the precondition.
func TestCASEmptyUpdateStillChecksPrecondition(t *testing.T) {
	db, id := newCASDB(t)

	err := UpdateIssue(db, id, map[string]any{"title": "moved"}, "")
	testsupport.Must(t, err, "UpdateIssue: %v", err)

	stale := 1
	if err := UpdateIssueCAS(db, id, nil, "", &stale); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("empty update with stale version = %v, want ErrVersionConflict", err)
	}

	current := 2
	if err := UpdateIssueCAS(db, id, nil, "", &current); err != nil {
		t.Errorf("empty update with current version: %v", err)
	}
}

func TestGetVersionRejectsUnversionedTable(t *testing.T) {
	db, _ := newCASDB(t)

	if _, err := GetVersion(db, "comments", 1); err == nil {
		t.Error("expected an error for a table with no version column")
	}
}

func TestAllVersionedTablesHaveVersionColumn(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = Migrate(db)
	testsupport.Must(t, err, "Migrate: %v", err)

	for _, table := range versionedTables {
		var n int
		err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'version'`, table,
		).Scan(&n)
		testsupport.Must(t, err, "inspecting %s: %v", table, err)
		if n != 1 {
			t.Errorf("%s has %d version columns, want 1", table, n)
		}
	}
}
