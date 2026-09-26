package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// The mid-run scope widen, and why there is no verb that performs it (DKT-741).
//
// `docket issue edit --scope` writes `issues.scope_globs` and nothing else.
// That column is the LIVE scope, and exactly one consumer reads it live: the
// scheduler's mutual-exclusion check (§6.3 R4, loadIssueFacts), which asks
// "what does this issue touch NOW" so an operator's correction takes effect
// against collisions immediately.
//
// EVERY OTHER CONSUMER READS THE ACTIVATION SNAPSHOT, and does so by design:
//
//   - the context bundle's `issue.scope` — the scope a rendered packet's brief
//     carries — comes from `run_issues.issue_snapshot` (§5.1.1, §6.6's
//     five-source rule). Assembly reads NO live issue field at all, and
//     TestContextAssemblyReadsNoLiveState enforces that at the code level.
//   - the recorded `issue.diff` is computed over snapshotScope (§6.7.1 D1),
//     the same frozen blob, so a step's recorded diff cannot depend on an edit
//     made after the run was activated.
//
// This is §9 item 5's mid-run edit immunity — the same invariant that keeps a
// `description` edit out of an already-activated packet (DKT-725) — and it is
// what makes a packet reproducible from the ledger. So `issue edit --scope`
// still re-snapshots NOTHING: an edit that reached a live step by itself would
// let two steps of one run render two different issue scopes and record two
// diffs over two different path sets, which is precisely the drift the
// snapshot exists to prevent.
//
// The consequence an operator meets is that an AUTHORIZED widen — the panel
// rejected the work as out of scope and the operator agreed to widen it — is
// not executable against the running run BY THE EDIT ALONE. DKT-741 left the
// sanctioned path at abandon + re-plan; RUN-52 (VPL-434) then paid for that
// twice on an intact premise, and DKT-869 added the narrower disposition:
//
//	docket run refresh-scope RUN-N --issue DKT-M --reason "scope widened"
//
// which copies the declared column into ONE run-issue's snapshot as a second,
// explicit, refusable, event-logged act (scope_refresh.go states the four
// properties that keep it from being a hole in the freeze). Abandon + re-plan
// remains right where the run's PREMISE changed rather than one declaration:
//
//	docket run abandon RUN-N --issue DKT-M --reason "scope widened"
//	# then re-plan the issue into a new run
//
// ScopeEditFrozenForActiveRuns is what names both at the moment it matters.

// ScopeEditFrozenForActiveRuns names the live runs an `issue edit --scope` on
// issueID did NOT reach, and the abandon + re-plan path that would (DKT-741).
//
// It fires only when the widen is actually invisible somewhere: the issue is
// bound into a NON-TERMINAL run that has already been activated (so a snapshot
// exists), that run still holds at least one NON-TERMINAL step for the issue
// (so a packet will still be rendered from the stale snapshot), and the live
// scope genuinely DIFFERS from what that run froze. A re-declaration of the
// same globs, an edit to an unactivated `planning` run, and an issue whose
// steps have all recorded are all silent — none of them has anything to
// discover.
//
// It reports rather than refuses. The write to `issues.scope_globs` is real
// and does take effect for scheduling, so the operator is told what landed and
// what did not, not stopped. Advisory-only also means it is safe to call after
// the edit commits, which is where `issue edit` calls it: the answer is a
// property of the run's frozen snapshot, which the edit did not touch.
//
// Every error is swallowed to nil, matching OverridePassSkipsInterposedTargets:
// an advisory that cannot be computed must never fail the verb that already
// succeeded.
func ScopeEditFrozenForActiveRuns(conn *sql.DB, issueID int) []string {
	// The live column FIRST, before the cursor below is open. The engine's
	// pool is single-connection (§4.8's SQLite writer discipline), so a second
	// query issued while rows are still being walked waits forever for a
	// connection that the walk itself holds.
	live, err := liveIssueScope(conn, issueID)
	if err != nil {
		return nil
	}

	rows, err := conn.Query(
		`SELECT ri.run_id, ri.issue_snapshot, COUNT(s.id)
		   FROM run_issues ri
		   JOIN runs r ON r.id = ri.run_id
		   LEFT JOIN steps s
		     ON s.run_id = ri.run_id AND s.issue_id = ri.issue_id
		    AND s.status NOT IN (`+placeholders(len(terminalStepStatuses))+`)
		  WHERE ri.issue_id = ?
		    AND ri.issue_snapshot IS NOT NULL AND ri.issue_snapshot != ''
		    AND r.status NOT IN (?, ?)
		  GROUP BY ri.run_id
		 HAVING COUNT(s.id) > 0
		  ORDER BY ri.run_id`,
		append(
			terminalStepStatusArgs(),
			issueID, string(model.RunDone), string(model.RunAbandoned),
		)...,
	)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	var warnings []string
	for rows.Next() {
		var (
			runID     int
			snapshot  string
			liveSteps int
		)
		if err := rows.Scan(&runID, &snapshot, &liveSteps); err != nil {
			return nil
		}
		var frozen struct {
			Scope []string `json:"scope"`
		}
		if err := json.Unmarshal([]byte(snapshot), &frozen); err != nil {
			continue
		}
		if sameScope(frozen.Scope, live) {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s is bound into %s, which froze its scope at activation as %s "+
				"(§5.1.1) — the %d step(s) still live there will render that "+
				"frozen scope in their packets and record their diffs over it, "+
				"NOT the %s you just wrote. The live column does take effect "+
				"for scheduling's mutual-exclusion check, and for nothing else. "+
				"To make the widened scope real for this run's remaining work, "+
				"refresh its snapshot — "+
				"`docket run refresh-scope %s --issue %s --reason \"scope widened\"` "+
				"(DKT-869; refuses while any of the issue's steps is claimed, "+
				"running or gated, or while a dispatch is open). If the run's "+
				"PREMISE changed rather than one declaration, the older "+
				"disposition still applies: take the issue out of the run — "+
				"`docket run abandon %s --issue %s --reason \"scope widened\"` "+
				"— and re-plan it into a new run, whose activation snapshots "+
				"the scope afresh",
			model.FormatID(issueID), model.FormatRunID(runID),
			renderScope(frozen.Scope), liveSteps, renderScope(live),
			model.FormatRunID(runID), model.FormatID(issueID),
			model.FormatRunID(runID), model.FormatID(issueID),
		))
	}
	if rows.Err() != nil {
		return nil
	}
	return warnings
}

// terminalStepStatuses is db.StepTerminal's membership, as the SQL above needs
// it. It is derived from the constants rather than spelled as literals so a
// tenth status added to the machine cannot go stale here — TestStepTerminal
// StatusesMatchesStepTerminal pins the two together.
var terminalStepStatuses = []string{
	db.StepDone, db.StepSkipped, db.StepSuperseded, db.StepFailedRouted,
}

func terminalStepStatusArgs() []any {
	args := make([]any, 0, len(terminalStepStatuses)+3)
	for _, s := range terminalStepStatuses {
		args = append(args, s)
	}
	return args
}

// placeholders renders `?, ?, …` for an IN clause of n values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// liveIssueScope reads `issues.scope_globs` — the column `--scope` writes.
func liveIssueScope(conn *sql.DB, issueID int) ([]string, error) {
	var stored sql.NullString
	if err := conn.QueryRow(
		`SELECT scope_globs FROM issues WHERE id = ?`, issueID,
	).Scan(&stored); err != nil {
		return nil, err
	}
	return decodeScope(stored.String)
}

// sameScope compares two scope declarations IN ORDER, because the order is the
// author's and the snapshot echoes it back verbatim (issue_scope.go). Two
// declarations of the same globs in a different order are still a real edit to
// what a re-activation would freeze, so treating them as equal would hide one.
func sameScope(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// renderScope names a scope declaration for a human. It distinguishes the
// undeclared case from the declared-empty one, which issue_scope.go keeps
// apart on purpose and which a bare `[]` would collapse.
func renderScope(globs []string) string {
	if len(globs) == 0 {
		return "no declared scope"
	}
	return "[" + strings.Join(globs, ", ") + "]"
}
