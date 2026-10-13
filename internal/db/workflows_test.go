package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func mustMigrated(t *testing.T) *sql.DB {
	t.Helper()
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = Migrate(db)
	testsupport.Must(t, err, "Migrate: %v", err)
	return db
}

func testWorkflow(name string, version int, body string) *model.Workflow {
	return &model.Workflow{
		Name:         name,
		Version:      version,
		Description:  "a test workflow",
		SourcePath:   "test.toml",
		SourceSHA256: sha(body),
		Body:         body,
		Parsed:       `{"pipeline":{"name":"` + name + `","version":1},"steps":[]}`,
	}
}

// sha mirrors what the register path stores in source_sha256. It is computed
// here rather than imported from internal/workflow so the storage tests do not
// depend on the grammar package; the hash's own stability is asserted there.
func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestInsertWorkflowRoundTrips(t *testing.T) {
	db := mustMigrated(t)
	wf := testWorkflow("w", 1, "body one")

	stored, created, err := InsertWorkflow(db, wf, 1000)
	testsupport.Must(t, err, "InsertWorkflow: %v", err)
	if !created {
		t.Error("first registration reports created = false")
	}

	got, err := GetWorkflow(db, 1, "w", 1)
	testsupport.Must(t, err, "GetWorkflow: %v", err)
	if got.Name != wf.Name || got.Version != wf.Version {
		t.Errorf("read back %s@%d, want %s@%d", got.Name, got.Version, wf.Name, wf.Version)
	}
	if got.Body != wf.Body {
		t.Errorf("body = %q, want %q", got.Body, wf.Body)
	}
	if got.Parsed != wf.Parsed {
		t.Errorf("parsed = %q, want %q", got.Parsed, wf.Parsed)
	}
	if got.SourceSHA256 != wf.SourceSHA256 {
		t.Errorf("source_sha256 = %q, want %q", got.SourceSHA256, wf.SourceSHA256)
	}
	if got.Description != wf.Description {
		t.Errorf("description = %q, want %q", got.Description, wf.Description)
	}
	if got.CreatedAtMS != 1000 {
		t.Errorf("created_at_ms = %d, want 1000", got.CreatedAtMS)
	}
	if got.RowVersion != 1 {
		t.Errorf("row_version = %d, want 1 on a fresh row", got.RowVersion)
	}
	if stored.ID == 0 {
		t.Error("the inserted row has no id")
	}
}

// TestInsertWorkflowIsIdempotent: re-registering IDENTICAL bytes returns the
// original row and inserts nothing.
func TestInsertWorkflowIsIdempotent(t *testing.T) {
	db := mustMigrated(t)
	wf := testWorkflow("w", 1, "body one")

	first, _, err := InsertWorkflow(db, wf, 1000)
	testsupport.Must(t, err, "first InsertWorkflow: %v", err)

	// A later timestamp, to prove the replay returns the ORIGINAL row rather
	// than overwriting it: created_at_ms must not move.
	second, created, err := InsertWorkflow(db, testWorkflow("w", 1, "body one"), 2000)
	testsupport.Must(t, err, "replay InsertWorkflow: %v", err)
	if created {
		t.Error("re-registering identical bytes reports created = true")
	}
	if second.ID != first.ID {
		t.Errorf("replay returned id %d, want the original %d", second.ID, first.ID)
	}
	if second.CreatedAtMS != 1000 {
		t.Errorf("replay moved created_at_ms to %d", second.CreatedAtMS)
	}
	// A re-register must not bump the CAS column, or --if-version would fail
	// for a caller that changed nothing.
	if second.RowVersion != 1 {
		t.Errorf("replay bumped row_version to %d", second.RowVersion)
	}

	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM workflows`).Scan(&n)
	testsupport.Must(t, err, "counting workflows: %v", err)
	if n != 1 {
		t.Errorf("two registrations of identical bytes produced %d rows", n)
	}
}

// TestInsertWorkflowConflictsOnDifferingBytes: a registered name@version is
// frozen. Version pinning is worth nothing if the pinned bytes can be swapped
// underneath a run.
func TestInsertWorkflowConflictsOnDifferingBytes(t *testing.T) {
	db := mustMigrated(t)

	_, _, err := InsertWorkflow(db, testWorkflow("w", 1, "body one"), 1000)
	testsupport.Must(t, err, "first InsertWorkflow: %v", err)

	_, _, err = InsertWorkflow(db, testWorkflow("w", 1, "body TWO"), 2000)
	if err == nil {
		t.Fatal("re-registering different bytes at the same name@version succeeded")
	}
	if !errors.Is(err, ErrWorkflowConflict) {
		t.Errorf("error is %v, want ErrWorkflowConflict", err)
	}
	// The refusal must be distinguishable from not-found: they map to
	// different exit codes (4 vs 2).
	if errors.Is(err, ErrWorkflowNotFound) {
		t.Error("a conflict is reported as not-found")
	}
	// Both hashes are named, so an operator can tell which file is which.
	oldSHA, newSHA := sha("body one"), sha("body TWO")
	msg := err.Error()
	if !strings.Contains(msg, oldSHA) || !strings.Contains(msg, newSHA) {
		t.Errorf("conflict %q does not name both hashes", msg)
	}

	// And the stored row is untouched.
	got, err := GetWorkflow(db, 1, "w", 1)
	testsupport.Must(t, err, "GetWorkflow: %v", err)
	if got.Body != "body one" {
		t.Errorf("the refused registration overwrote the body: %q", got.Body)
	}
}

// TestInsertWorkflowAllowsANewVersion: freezing name@version does not freeze
// the name — a new version is an ordinary registration.
func TestInsertWorkflowAllowsANewVersion(t *testing.T) {
	db := mustMigrated(t)

	_, _, err := InsertWorkflow(db, testWorkflow("w", 1, "one"), 1000)
	testsupport.Must(t, err, "v1: %v", err)
	if _, created, err := InsertWorkflow(db, testWorkflow("w", 2, "two"), 2000); err != nil {
		t.Fatalf("v2: %v", err)
	} else if !created {
		t.Error("a new version reports created = false")
	}
}

// TestGetWorkflowWithoutVersionSelectsTheHighest is what
// `workflow show NAME` means.
func TestGetWorkflowSelectsHighestVersion(t *testing.T) {
	db := mustMigrated(t)

	// Registered out of order, so the query cannot pass by taking the last
	// inserted row.
	for _, v := range []int{2, 5, 1} {
		_, _, err := InsertWorkflow(db, testWorkflow("w", v, "body"+string(rune('a'+v))), int64(v))
		testsupport.Must(t, err, "registering v%d: %v", v, err)
	}

	got, err := GetWorkflow(db, 1, "w", 0)
	testsupport.Must(t, err, "GetWorkflow: %v", err)
	if got.Version != 5 {
		t.Errorf("unversioned lookup returned v%d, want the highest (v5)", got.Version)
	}

	exact, err := GetWorkflow(db, 1, "w", 2)
	testsupport.Must(t, err, "GetWorkflow(v2): %v", err)
	if exact.Version != 2 {
		t.Errorf("exact lookup returned v%d, want v2", exact.Version)
	}
}

// TestGetWorkflowSkipsDeprecatedVersions (DKT-616): the unversioned lookup is
// what `workflow show NAME` displays, and it must name the version a new run
// would bind — so a deprecated top version is skipped, an explicit @version
// still reaches it, and a name with nothing left to bind is not found.
func TestGetWorkflowSkipsDeprecatedVersions(t *testing.T) {
	db := mustMigrated(t)

	for _, v := range []int{1, 2} {
		_, _, err := InsertWorkflow(db, testWorkflow("w", v, "body"+string(rune('a'+v))), int64(v))
		testsupport.Must(t, err, "registering v%d: %v", v, err)
	}
	_, err := DeprecateWorkflow(db, 1, "w", 2, 1000)
	testsupport.Must(t, err, "deprecating w@2: %v", err)

	got, err := GetWorkflow(db, 1, "w", 0)
	testsupport.Must(t, err, "GetWorkflow: %v", err)
	if got.Version != 1 {
		t.Errorf("unversioned lookup returned v%d with v2 deprecated, want v1", got.Version)
	}

	exact, err := GetWorkflow(db, 1, "w", 2)
	testsupport.Must(t, err, "GetWorkflow(v2): %v", err)
	if !exact.Deprecated() {
		t.Errorf("explicit lookup of the deprecated v2 returned a row not marked deprecated: %+v", exact)
	}

	_, err = DeprecateWorkflow(db, 1, "w", 1, 1000)
	testsupport.Must(t, err, "deprecating w@1: %v", err)
	if _, err := GetWorkflow(db, 1, "w", 0); !errors.Is(err, ErrWorkflowNotFound) {
		t.Errorf("unversioned lookup with every version deprecated returned %v, want ErrWorkflowNotFound", err)
	}
}

func TestGetWorkflowNotFound(t *testing.T) {
	db := mustMigrated(t)

	if _, err := GetWorkflow(db, 1, "ghost", 0); !errors.Is(err, ErrWorkflowNotFound) {
		t.Errorf("unregistered name returned %v, want ErrWorkflowNotFound", err)
	}
	_, _, err := InsertWorkflow(db, testWorkflow("w", 1, "body"), 1000)
	testsupport.Must(t, err, "InsertWorkflow: %v", err)
	if _, err := GetWorkflow(db, 1, "w", 9); !errors.Is(err, ErrWorkflowNotFound) {
		t.Errorf("unregistered version returned %v, want ErrWorkflowNotFound", err)
	}
}

// TestListWorkflowsReportsTruePreLimitTotal is the Collection contract: total
// must ignore the limit, or truncation cannot be computed honestly.
func TestListWorkflowsReportsTruePreLimitTotal(t *testing.T) {
	db := mustMigrated(t)

	for i := 1; i <= 5; i++ {
		wf := testWorkflow("w", i, "body"+string(rune('a'+i)))
		_, _, err := InsertWorkflow(db, wf, int64(i))
		testsupport.Must(t, err, "registering v%d: %v", i, err)
	}

	items, total, err := ListWorkflows(db, WorkflowListOptions{Limit: 2})
	testsupport.Must(t, err, "ListWorkflows: %v", err)
	if len(items) != 2 {
		t.Errorf("returned %d items under --limit 2", len(items))
	}
	if total != 5 {
		t.Errorf("total = %d, want the true pre-limit count 5", total)
	}
}

func TestListWorkflowsFiltersByName(t *testing.T) {
	db := mustMigrated(t)

	_, _, err := InsertWorkflow(db, testWorkflow("alpha", 1, "a"), 1)
	testsupport.Must(t, err, "alpha: %v", err)
	_, _, err = InsertWorkflow(db, testWorkflow("beta", 1, "b"), 2)
	testsupport.Must(t, err, "beta: %v", err)

	items, total, err := ListWorkflows(db, WorkflowListOptions{Name: "alpha"})
	testsupport.Must(t, err, "ListWorkflows: %v", err)
	if total != 1 || len(items) != 1 || items[0].Name != "alpha" {
		t.Errorf("name filter returned %d/%d items: %+v", len(items), total, items)
	}
}

// TestListWorkflowsOrdersDeterministically: the list must not depend on
// insertion order or on SQLite's rowid, since QA golden-diffs its output.
func TestListWorkflowsOrdersDeterministically(t *testing.T) {
	db := mustMigrated(t)

	for _, wf := range []*model.Workflow{
		testWorkflow("zeta", 1, "z1"),
		testWorkflow("alpha", 2, "a2"),
		testWorkflow("alpha", 1, "a1"),
	} {
		_, _, err := InsertWorkflow(db, wf, 1)
		testsupport.Must(t, err, "registering %s: %v", wf.Ref(), err)
	}

	items, _, err := ListWorkflows(db, WorkflowListOptions{})
	testsupport.Must(t, err, "ListWorkflows: %v", err)
	want := []string{"alpha@2", "alpha@1", "zeta@1"}
	if len(items) != len(want) {
		t.Fatalf("got %d items, want %d", len(items), len(want))
	}
	for i, ref := range want {
		if items[i].Ref() != ref {
			t.Errorf("item %d is %s, want %s", i, items[i].Ref(), ref)
		}
	}
}

// TestListWorkflowsExcludeDeprecatedDropsRetiredVersionsFromRowsAndTotal:
// ExcludeDeprecated is a WHERE clause, not a post-filter, so the total a
// caller sees is already the visible population's — the same guarantee
// --orphans gives its own filtered total.
func TestListWorkflowsExcludeDeprecatedDropsRetiredVersionsFromRowsAndTotal(t *testing.T) {
	db := mustMigrated(t)

	_, _, err := InsertWorkflow(db, testWorkflow("w", 1, "a"), 1)
	testsupport.Must(t, err, "w@1: %v", err)
	_, _, err = InsertWorkflow(db, testWorkflow("w", 2, "b"), 2)
	testsupport.Must(t, err, "w@2: %v", err)
	_, err = DeprecateWorkflow(db, 1, "w", 1, 1000)
	testsupport.Must(t, err, "deprecating w@1: %v", err)

	items, total, err := ListWorkflows(db, WorkflowListOptions{ExcludeDeprecated: true})
	testsupport.Must(t, err, "ListWorkflows: %v", err)
	if total != 1 || len(items) != 1 || items[0].Ref() != "w@2" {
		t.Errorf("ExcludeDeprecated returned %d/%d items %+v, want only w@2",
			len(items), total, items)
	}

	// The zero value keeps every existing caller's behavior: both versions.
	items, total, err = ListWorkflows(db, WorkflowListOptions{})
	testsupport.Must(t, err, "ListWorkflows: %v", err)
	if total != 2 || len(items) != 2 {
		t.Errorf("the zero value filtered rows it was never asked to: %d/%d %+v",
			len(items), total, items)
	}
}

// TestWorkflowsTableIsDormantUntilRegistered is the phase-1 dormancy claim at
// the storage layer.
func TestWorkflowsTableIsDormantUntilRegistered(t *testing.T) {
	db := mustMigrated(t)

	items, total, err := ListWorkflows(db, WorkflowListOptions{})
	testsupport.Must(t, err, "ListWorkflows: %v", err)
	if total != 0 || len(items) != 0 {
		t.Errorf("a repo that never registered a workflow reports %d/%d", len(items), total)
	}
}
