package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestForeignHoldersStayWithinTheProject is G6's scheduler half: scope globs
// are literal path prefixes with no repo qualifier, so the foreign-holder set
// must be bounded by the project or `internal/**` in one repository would
// exclude `internal/**` in every other repository sharing the store.
func TestForeignHoldersStayWithinTheProject(t *testing.T) {
	conn := mustDB(t)

	mustExecSQL := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}

	mustExecSQL(`INSERT INTO projects (id, identity, name) VALUES (2, '/repo/two', 'two')`)
	mustExecSQL(`INSERT INTO workflows (id, project_id, name, version, source_sha256, body, parsed, created_at_ms, row_version)
		VALUES (1, 1, 'wf', 1, 'x', 'b', '{}', 0, 1)`)
	mustExecSQL(`INSERT INTO issues (id, project_id, title, created_at, updated_at) VALUES (1, 1, 'one', 't', 't')`)
	mustExecSQL(`INSERT INTO issues (id, project_id, title, created_at, updated_at) VALUES (2, 2, 'two', 't', 't')`)
	mustExecSQL(`INSERT INTO runs (id, project_id, request, status, budget, created_at_ms, updated_at_ms, row_version)
		VALUES (10, 1, 'r10', 'active', 0, 0, 0, 1)`)
	mustExecSQL(`INSERT INTO runs (id, project_id, request, status, budget, created_at_ms, updated_at_ms, row_version)
		VALUES (20, 1, 'r20', 'active', 0, 0, 0, 1)`)
	mustExecSQL(`INSERT INTO runs (id, project_id, request, status, budget, created_at_ms, updated_at_ms, row_version)
		VALUES (30, 2, 'r30', 'active', 0, 0, 0, 1)`)
	// One claimed step in a SIBLING run of project 1, one in project 2.
	mustExecSQL(`INSERT INTO steps (id, run_id, issue_id, workflow_id, step_name, ordinal, sibling_index,
			instance, kind, status, attempt, max_attempts, created_at_ms, updated_at_ms, row_version)
		VALUES (100, 20, 1, 1, 'implement', 0, 0, 'implement@0', 'human', 'claimed', 1, 3, 0, 0, 1)`)
	mustExecSQL(`INSERT INTO steps (id, run_id, issue_id, workflow_id, step_name, ordinal, sibling_index,
			instance, kind, status, attempt, max_attempts, created_at_ms, updated_at_ms, row_version)
		VALUES (200, 30, 2, 1, 'implement', 0, 0, 'implement@0', 'human', 'claimed', 1, 3, 0, 0, 1)`)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	defer tx.Rollback()

	holders, err := foreignHoldingStepsTx(tx, 1, 10)
	testsupport.Must(t, err, "foreignHoldingStepsTx: %v", err)

	if len(holders) != 1 {
		t.Fatalf("project 1's run sees %d foreign holders, want exactly its sibling's 1", len(holders))
	}
	if holders[0].ID != 100 {
		t.Errorf("foreign holder = step %d, want the same-project step 100 — a "+
			"neighbor project's claimed step must not hold scope here", holders[0].ID)
	}
}
