package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestEventsScopeToTheProject pins the feed's three-way attribution (v12): a
// run-attributed event belongs to the run's project, a run-less event follows
// its issue, and an event attributable to neither — a trust change — is a
// store-level fact every scoped view carries. Without the scope predicate a
// shared store's feed interleaved every project's transitions.
func TestEventsScopeToTheProject(t *testing.T) {
	conn := mustDB(t)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}

	exec(`INSERT INTO projects (id, identity, name) VALUES (2, '/repo/two', 'two')`)
	exec(`INSERT INTO issues (id, project_id, title, created_at, updated_at) VALUES (1, 1, 'one', 't', 't')`)
	exec(`INSERT INTO issues (id, project_id, title, created_at, updated_at) VALUES (2, 2, 'two', 't', 't')`)
	exec(`INSERT INTO runs (id, project_id, request, status, budget, created_at_ms, updated_at_ms, row_version)
		VALUES (10, 1, 'r', 'active', 0, 0, 0, 1)`)
	exec(`INSERT INTO runs (id, project_id, request, status, budget, created_at_ms, updated_at_ms, row_version)
		VALUES (20, 2, 'r', 'active', 0, 0, 0, 1)`)

	// One event of each attribution class, per project, plus one store-level.
	exec(`INSERT INTO events (at_ms, kind, run_id, data) VALUES (1, 'run-activated', 10, '{}')`)
	exec(`INSERT INTO events (at_ms, kind, run_id, data) VALUES (2, 'run-activated', 20, '{}')`)
	exec(`INSERT INTO events (at_ms, kind, issue_id, data) VALUES (3, 'issue-claimed', 1, '{}')`)
	exec(`INSERT INTO events (at_ms, kind, issue_id, data) VALUES (4, 'issue-claimed', 2, '{}')`)
	exec(`INSERT INTO events (at_ms, kind, data) VALUES (5, 'trust-added', '{}')`)

	page, err := ListEvents(conn, EventQuery{ProjectID: 1})
	testsupport.Must(t, err, "ListEvents(project 1): %v", err)
	if len(page.Events) != 3 || page.Total != 3 {
		t.Fatalf("project 1 sees %d events (total %d), want 3: its run's, its issue's, and the store-level one\n%+v",
			len(page.Events), page.Total, page.Events)
	}
	for _, e := range page.Events {
		if e.Run == "RUN-20" || e.Issue == "DKT-2" {
			t.Errorf("project 1's feed carries a neighbor's event: %+v", e)
		}
	}

	// The unscoped feed still carries everything.
	all, err := ListEvents(conn, EventQuery{})
	testsupport.Must(t, err, "ListEvents(all): %v", err)
	if len(all.Events) != 5 {
		t.Errorf("the unscoped feed sees %d events, want all 5", len(all.Events))
	}
}

// TestRunFilterAnswersAcrossProjects is DKT-583.
//
// `events list --run RUN-N` from a neighbouring project's cwd returned
// `{"ok":true,"events":null,"total":0}` for runs with hundreds of events: the
// invoking project's scope was anded onto the run filter, and a run owned by
// another project matched neither arm. Four analysts read that empty page as
// "this run recorded nothing".
//
// A SUCCESSFUL EMPTY FEED IS THE FAILURE. The run clause is already a project
// scope — narrower than any project's — so the second one can only subtract
// rows it has no business subtracting.
func TestRunFilterAnswersAcrossProjects(t *testing.T) {
	conn := mustDB(t)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}

	exec(`INSERT INTO projects (id, identity, name) VALUES (2, '/repo/two', 'two')`)
	exec(`INSERT INTO runs (id, project_id, request, status, budget, created_at_ms, updated_at_ms, row_version)
		VALUES (10, 1, 'r', 'active', 0, 0, 0, 1)`)
	exec(`INSERT INTO runs (id, project_id, request, status, budget, created_at_ms, updated_at_ms, row_version)
		VALUES (20, 2, 'r', 'active', 0, 0, 0, 1)`)
	exec(`INSERT INTO events (at_ms, kind, run_id, data) VALUES (1, 'run-activated', 10, '{}')`)
	exec(`INSERT INTO events (at_ms, kind, run_id, data) VALUES (2, 'run-activated', 20, '{}')`)
	exec(`INSERT INTO events (at_ms, kind, run_id, data) VALUES (3, 'step-claimed', 20, '{}')`)

	// The verbatim bug: project 1's cwd, project 2's run.
	foreign, err := ListEvents(conn, EventQuery{RunID: 20, ProjectID: 1})
	testsupport.Must(t, err, "ListEvents(run 20 from project 1): %v", err)
	if len(foreign.Events) != 2 || foreign.Total != 2 {
		t.Fatalf("a run in another project answered %d events (total %d), want its 2 — "+
			"a successful empty page is DKT-583's exact defect\n%+v",
			len(foreign.Events), foreign.Total, foreign.Events)
	}
	for _, e := range foreign.Events {
		if e.Run != "RUN-20" {
			t.Errorf("the run filter leaked a neighbour's event: %+v", e)
		}
	}

	// A run INSIDE the invoking project is unchanged.
	local, err := ListEvents(conn, EventQuery{RunID: 10, ProjectID: 1})
	testsupport.Must(t, err, "ListEvents(run 10 from project 1): %v", err)
	if len(local.Events) != 1 || local.Total != 1 {
		t.Errorf("the invoking project's own run answered %d events (total %d), want 1",
			len(local.Events), local.Total)
	}

	// The unscoped (--all-projects) form still answers the same run.
	all, err := ListEvents(conn, EventQuery{RunID: 20})
	testsupport.Must(t, err, "ListEvents(run 20, all projects): %v", err)
	if len(all.Events) != 2 {
		t.Errorf("--all-projects --run answered %d events, want 2", len(all.Events))
	}

	// And a project-scoped feed with NO run filter is still scoped: dropping the
	// predicate under --run must not drop it everywhere.
	scoped, err := ListEvents(conn, EventQuery{ProjectID: 1})
	testsupport.Must(t, err, "ListEvents(project 1): %v", err)
	if len(scoped.Events) != 1 {
		t.Errorf("the run-less project feed answered %d events, want only project 1's 1 — "+
			"the scope predicate must survive for every query that names no run\n%+v",
			len(scoped.Events), scoped.Events)
	}
}

// TestScopedFeedSurvivesACorruptPayload guards DKT-68's filter against the one
// way it could take the whole verb down.
//
// SQLite's `json_extract` does not return NULL for malformed input — it ABORTS
// THE QUERY with "malformed JSON". Without the json_valid guard, one corrupt
// `data` cell anywhere in the table makes every project-scoped `events list`
// fail outright, and the operator loses the entire feed over a single bad row.
//
// The writer normalizes `data` to a JSON object, so this is unreachable through
// product code; a hand-edited row reaches it, which is exactly the case
// eventDetail already decided should still render rather than break the read.
func TestScopedFeedSurvivesACorruptPayload(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	// A store-level row (no run, no issue) whose payload is not JSON — the
	// shape only a hand edit produces, and the shape that breaks json_extract.
	execSQL(t, conn,
		`INSERT INTO events (at_ms, kind, data) VALUES (?, ?, ?)`,
		nowMS, EventTrustAdded, "not json at all")

	page, err := ListEvents(conn, EventQuery{ProjectID: db.DefaultProjectID})
	if err != nil {
		t.Fatalf("a scoped feed failed over one corrupt row: %v\n"+
			"one bad cell must not cost the operator the whole feed", err)
	}

	// The corrupt row names no repository, so it is unattributable and stays
	// visible everywhere — the same answer every other unattributable
	// store-level event gets.
	var sawCorrupt bool
	for _, e := range page.Events {
		if e.Kind == EventTrustAdded {
			sawCorrupt = true
		}
	}
	if !sawCorrupt {
		t.Error("the corrupt row was filtered out; a payload that cannot be " +
			"parsed names no repository, and an unattributable store-level " +
			"event belongs in every scoped view")
	}
}
