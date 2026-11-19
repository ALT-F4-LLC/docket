package db_test

import (
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// versionContract pins issue versioning and the compare-and-swap update.
var versionContract = []contractCase{
	{"NewIssueStartsAtVersionOne", contractNewIssueStartsAtVersionOne},
	{"UpdateBumpsVersionWithoutPrecondition", contractUpdateBumpsVersionWithoutPrecondition},
	{"CASMatchingVersionSucceeds", contractCASMatchingVersionSucceeds},
	{"CASStaleVersionConflictsAndDoesNotWrite", contractCASStaleVersionConflictsAndDoesNotWrite},
	{"CASMissingRowIsNotFound", contractCASMissingRowIsNotFound},
	{"CASConcurrentWritersOnlyOneWins", contractCASConcurrentWritersOnlyOneWins},
	{"CASEmptyUpdateStillChecksPrecondition", contractCASEmptyUpdateStillChecksPrecondition},
	{"GetVersionRejectsUnversionedTable", contractGetVersionRejectsUnversionedTable},
}

func contractNewIssueStartsAtVersionOne(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "cas subject")

	v, err := st.GetVersion("issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)
	if v != 1 {
		t.Errorf("new issue version = %d, want 1", v)
	}
}

func contractUpdateBumpsVersionWithoutPrecondition(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "cas subject")

	for want := 2; want <= 4; want++ {
		err := st.UpdateIssueCAS(id, map[string]any{"title": "t"}, "", nil)
		testsupport.Must(t, err, "UpdateIssueCAS: %v", err)
		got, err := st.GetVersion("issues", id)
		testsupport.Must(t, err, "GetVersion: %v", err)
		if got != want {
			t.Errorf("version after update = %d, want %d", got, want)
		}
	}
}

func contractCASMatchingVersionSucceeds(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "cas subject")

	one := 1
	err := st.UpdateIssueCAS(id, map[string]any{"status": "done"}, "", &one)
	testsupport.Must(t, err, "UpdateIssueCAS with correct version: %v", err)

	issue, err := st.GetIssue(id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if issue.Status != model.StatusDone {
		t.Errorf("status = %q, want done", issue.Status)
	}
	if issue.Version != 2 {
		t.Errorf("version = %d, want 2", issue.Version)
	}
}

func contractCASStaleVersionConflictsAndDoesNotWrite(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "cas subject")

	// Advance to version 2.
	err := st.UpdateIssueCAS(id, map[string]any{"title": "first"}, "", nil)
	testsupport.Must(t, err, "UpdateIssueCAS: %v", err)

	stale := 1
	err = st.UpdateIssueCAS(id, map[string]any{"title": "second"}, "", &stale)
	if !errors.Is(err, db.ErrVersionConflict) {
		t.Fatalf("error = %v, want ErrVersionConflict", err)
	}

	// The refused write must not have applied — this is the whole point.
	issue, err := st.GetIssue(id)
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
func contractCASMissingRowIsNotFound(t *testing.T, st store.Store) {
	createContractIssue(t, st, "cas subject")

	one := 1
	err := st.UpdateIssueCAS(999999, map[string]any{"title": "x"}, "", &one)
	if !errors.Is(err, db.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	if errors.Is(err, db.ErrVersionConflict) {
		t.Error("missing row reported as a version conflict")
	}
}

// Two writers read the same version; only one may win.
func contractCASConcurrentWritersOnlyOneWins(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "cas subject")

	shared, err := st.GetVersion("issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)

	err = st.UpdateIssueCAS(id, map[string]any{"title": "writer-A"}, "", &shared)
	testsupport.Must(t, err, "first writer: %v", err)
	err = st.UpdateIssueCAS(id, map[string]any{"title": "writer-B"}, "", &shared)
	if !errors.Is(err, db.ErrVersionConflict) {
		t.Fatalf("second writer error = %v, want ErrVersionConflict", err)
	}

	issue, err := st.GetIssue(id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if issue.Title != "writer-A" {
		t.Errorf("title = %q, want writer-A (lost update)", issue.Title)
	}
}

// --if-version on an empty update must still enforce the precondition.
func contractCASEmptyUpdateStillChecksPrecondition(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "cas subject")

	err := st.UpdateIssueCAS(id, map[string]any{"title": "moved"}, "", nil)
	testsupport.Must(t, err, "UpdateIssueCAS: %v", err)

	stale := 1
	if err := st.UpdateIssueCAS(id, nil, "", &stale); !errors.Is(err, db.ErrVersionConflict) {
		t.Errorf("empty update with stale version = %v, want ErrVersionConflict", err)
	}

	current := 2
	if err := st.UpdateIssueCAS(id, nil, "", &current); err != nil {
		t.Errorf("empty update with current version: %v", err)
	}
}

// The table name is spliced into SQL, so only allowlisted names may reach the
// query. "issues AS i" names an existing row's version and would read it if
// the allowlist were bypassed, so its rejection pins the allowlist itself.
func contractGetVersionRejectsUnversionedTable(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "cas subject")

	if _, err := st.GetVersion("comments", 1); err == nil {
		t.Error("expected an error for a table with no version column")
	}
	if v, err := st.GetVersion("issues AS i", id); err == nil {
		t.Errorf("GetVersion(%q) = %d, want an error for a name outside the allowlist", "issues AS i", v)
	}
}
