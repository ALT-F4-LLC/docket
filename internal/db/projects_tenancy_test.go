package db

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// The tenancy group of the 2026-08-17 shadow post-mortem: DKT-60's colliding
// prefixes and DKT-59's unremovable rows. DKT-58's registration gate and
// DKT-61's event live in internal/cli, where the decision is made.

func tenancyDB(t *testing.T) *sql.DB {
	t.Helper()
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

// TestDerivedPrefixesDoNotCollide is DKT-60.
//
// Registration inserted a hardcoded 'DKT' for every project, so the
// auto-registered `claude-501` row rendered as DKT — the prefix docket.git
// already owned. The collision is display-only (the number is the global
// identity), but the prefix is the ONLY project discriminator any listing,
// event feed, or report carries, so every id in the store became ambiguous
// about its owner.
func TestDerivedPrefixesDoNotCollide(t *testing.T) {
	db := tenancyDB(t)

	// The first identity CLAIMS the unclaimed default row, which keeps its
	// seeded prefix — that is EnsureProject's ladder step 2 and is not what
	// this test is about.
	if _, err := EnsureProject(db, "/src/first.git", "first.git", 1); err != nil {
		t.Fatalf("claiming the default project: %v", err)
	}

	// Every subsequent registration derives and must not repeat.
	names := []string{
		"docket.git", "docket-two.git", "agentic-mcp-services",
		"artifacts.vorpal", "claude-501", "docket.git-again",
	}
	seen := map[string]string{}
	for i, name := range names {
		id, err := EnsureProject(db, "/src/"+name, name, int64(i+2))
		if err != nil {
			t.Fatalf("registering %s: %v", name, err)
		}
		p, err := GetProject(db, id)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if owner, taken := seen[p.Prefix]; taken {
			t.Errorf("%s took prefix %s, already held by %s — two projects "+
				"sharing a prefix makes every id in the store ambiguous",
				name, p.Prefix, owner)
		}
		seen[p.Prefix] = name
		if err := model.ValidateProjectPrefix(p.Prefix); err != nil {
			t.Errorf("%s got an invalid prefix %q: %v", name, p.Prefix, err)
		}
	}
}

// TestDerivePrefixReadsTheNameAloud pins the derivation rule itself: initials
// for a multi-word name, first three letters for a single word. The first-three
// rule alone would collapse whole families of sibling repositories onto the
// same three characters, which is the collision this is trying to avoid.
func TestDerivePrefixReadsTheNameAloud(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"docket.git", "DOC"},
		{"docket", "DOC"},
		{"agentic-mcp-services", "AMS"},
		{"artifacts.vorpal", "AV"},
		{"a", "A"},
		{"1234", ""},
		{"", ""},
	} {
		if got := DerivePrefix(tc.name); got != tc.want {
			t.Errorf("DerivePrefix(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestReservedPrefixIsNeverDerived: DOC, RUN, and STEP name other entities, and
// `docket.git` derives to DOC — so the very repo this tracker was built for is
// the case that proves availablePrefix consults the reserved list rather than
// only the taken one.
func TestReservedPrefixIsNeverDerived(t *testing.T) {
	db := tenancyDB(t)
	if _, err := EnsureProject(db, "/src/first.git", "first.git", 1); err != nil {
		t.Fatalf("claiming the default project: %v", err)
	}

	id, err := EnsureProject(db, "/src/docket.git", "docket.git", 2)
	if err != nil {
		t.Fatalf("registering docket.git: %v", err)
	}
	p, err := GetProject(db, id)
	if err != nil {
		t.Fatalf("reading the project: %v", err)
	}
	if p.Prefix == "DOC" {
		t.Error("registered with the reserved prefix DOC; `DOC-3` would then " +
			"mean both a document and an issue on one command line")
	}
	if err := model.ValidateProjectPrefix(p.Prefix); err != nil {
		t.Errorf("prefix %q is invalid: %v", p.Prefix, err)
	}
}

// TestPrefixHolderFindsTheConflict backs `project set-prefix`'s refusal.
func TestPrefixHolderFindsTheConflict(t *testing.T) {
	db := tenancyDB(t)
	first, err := EnsureProject(db, "/src/first.git", "first.git", 1)
	if err != nil {
		t.Fatalf("registering: %v", err)
	}
	second, err := EnsureProject(db, "/src/second.git", "second.git", 2)
	if err != nil {
		t.Fatalf("registering: %v", err)
	}
	if err := SetProjectPrefix(db, first, "AAA"); err != nil {
		t.Fatalf("setting the prefix: %v", err)
	}

	holder, err := PrefixHolder(db, "aaa", second)
	if err != nil {
		t.Fatalf("PrefixHolder: %v", err)
	}
	if holder != first {
		t.Errorf("PrefixHolder = %d, want %d — the comparison is "+
			"case-insensitive, since the prefix is stored uppercased", holder, first)
	}

	// A project never conflicts with ITSELF: re-setting a prefix to what it
	// already holds must not be an error.
	holder, err = PrefixHolder(db, "AAA", first)
	if err != nil {
		t.Fatalf("PrefixHolder: %v", err)
	}
	if holder != 0 {
		t.Errorf("a project conflicts with itself (holder %d); re-setting a "+
			"prefix to its current value must stay legal", holder)
	}
}

// TestDeleteProjectRefusesAnythingWithRows is DKT-59.
//
// The junk row minted by the cwd-registration defect could not be removed by
// any supported means, leaving a raw sqlite DELETE against a store shared by
// every repository on the machine as the operator's only remedy. The verb that
// removes it can remove ONLY junk: the refusals are what make it safe to
// expose at all.
func TestDeleteProjectRefusesAnythingWithRows(t *testing.T) {
	db := tenancyDB(t)
	if _, err := EnsureProject(db, "/src/first.git", "first.git", 1); err != nil {
		t.Fatalf("claiming the default project: %v", err)
	}
	junk, err := EnsureProject(db, "/tmp/claude-501", "claude-501", 2)
	if err != nil {
		t.Fatalf("registering the junk project: %v", err)
	}

	// Empty ⇒ removable. This is the whole point.
	if err := DeleteProject(db, junk); err != nil {
		t.Fatalf("an empty project refused deletion: %v", err)
	}
	if _, err := GetProject(db, junk); !errors.Is(err, ErrNotFound) {
		t.Errorf("the project survived its own deletion (err %v)", err)
	}

	// The default row is never removable: every pre-tenancy datum belongs to it.
	if err := DeleteProject(db, DefaultProjectID); !errors.Is(err, ErrProjectIsDefault) {
		t.Errorf("deleting the default project returned %v, want ErrProjectIsDefault", err)
	}

	// A project holding ANYTHING is refused, and the counts name what.
	occupied, err := EnsureProject(db, "/src/occupied.git", "occupied.git", 3)
	if err != nil {
		t.Fatalf("registering: %v", err)
	}
	issueID, err := CreateIssue(db, &model.Issue{
		Title: "held", Status: model.StatusBacklog, Kind: model.IssueKindTask,
		ProjectID: occupied,
	}, nil, nil)
	if err != nil {
		t.Fatalf("creating the issue: %v", err)
	}
	if err := DeleteProject(db, occupied); !errors.Is(err, ErrProjectInUse) {
		t.Fatalf("deleting an occupied project returned %v, want ErrProjectInUse", err)
	}
	counts, err := ProjectRefCounts(db, occupied)
	if err != nil {
		t.Fatalf("ProjectRefCounts: %v", err)
	}
	if counts["issues"] != 1 {
		t.Errorf("counts = %v, want one issue — the refusal names what is still "+
			"there so the operator does not have to go find it", counts)
	}

	// Emptied by re-homing, it becomes deletable — the documented way out.
	if _, err := MoveIssueProject(db, issueID, DefaultProjectID, "tester", 4); err != nil {
		t.Fatalf("re-homing the issue: %v", err)
	}
	if err := DeleteProject(db, occupied); err != nil {
		t.Errorf("an emptied project still refused deletion: %v", err)
	}
}

// TestProjectRefTablesCoverTheSchema is the fail-closed half of DKT-59's
// refusal, and the reason projectRefTables is a hand-written LIST.
//
// A project-scoped table added later and forgotten there would let its rows be
// orphaned silently by a delete that reported success. This enumerates the
// schema and asserts the list still covers it — the same discipline
// eventKinds' closed set uses.
func TestProjectRefTablesCoverTheSchema(t *testing.T) {
	db := tenancyDB(t)

	// The table names are collected COMPLETELY before any column is probed.
	// internal/db caps the pool at one connection, so probing inside the row
	// loop would deadlock against the cursor still holding it — the same
	// constraint actorCountsTx documents for the report.
	var tables []string
	func() {
		rows, err := db.Query(
			`SELECT name FROM sqlite_master
			  WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
			  ORDER BY name`)
		if err != nil {
			t.Fatalf("listing tables: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var table string
			if err := rows.Scan(&table); err != nil {
				t.Fatalf("reading a table name: %v", err)
			}
			tables = append(tables, table)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("listing tables: %v", err)
		}
	}()

	listed := map[string]bool{}
	for _, table := range projectRefTables {
		listed[table] = true
	}

	var missing []string
	for _, table := range tables {
		if table == "projects" {
			continue
		}
		has, err := hasColumnDB(db, table, "project_id")
		if err != nil {
			t.Fatalf("probing %s: %v", table, err)
		}
		if has && !listed[table] {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		t.Errorf("these tables carry project_id but are absent from "+
			"projectRefTables, so `project delete` would orphan their rows "+
			"while reporting success: %s", strings.Join(missing, ", "))
	}
}
