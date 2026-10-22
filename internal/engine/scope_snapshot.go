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
// scheduler's mutual-exclusion check (R4, loadIssueFacts), which asks
// "what does this issue touch NOW" so an operator's correction takes effect
// against collisions immediately.
//
// EVERY OTHER CONSUMER READS THE ACTIVATION SNAPSHOT, and does so by design:
//
//   - the context bundle's `issue.scope` — the scope a rendered packet's brief
//     carries — comes from `run_issues.issue_snapshot` (the five-source
//     rule). Assembly reads NO live issue field at all, and
//     TestContextAssemblyReadsNoLiveState enforces that at the code level.
//   - the recorded `issue.diff` is computed over snapshotScope,
//     the same frozen blob, so a step's recorded diff cannot depend on an edit
//     made after the run was activated.
//
// This is mid-run edit immunity — the same invariant that keeps a
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
	live, err := liveIssueScope(conn, issueID)
	if err != nil {
		return nil
	}
	bindings, err := frozenBindingsWithLiveSteps(conn, issueID)
	if err != nil {
		return nil
	}

	var warnings []string
	for _, b := range bindings {
		runID, liveSteps := b.runID, b.liveSteps
		var frozen struct {
			Scope []string `json:"scope"`
		}
		if err := json.Unmarshal([]byte(b.issueSnapshot), &frozen); err != nil {
			continue
		}
		if sameScope(frozen.Scope, live) {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s is bound into %s, which froze its scope at activation as %s "+
				"— the %d step(s) still live there will render that "+
				"frozen scope in their packets and record their diffs over it, "+
				"NOT the %s you just wrote. The live column does take effect "+
				"for scheduling's mutual-exclusion check, and for nothing else. "+
				"To make the widened scope real for this run's remaining work, "+
				"refresh its snapshot — "+
				"`docket run refresh-scope %s --issue %s --reason \"scope widened\"` "+
				"(DKT-869; refuses while any of the issue's steps is claimed, "+
				"running or gated). If the run's "+
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
	return warnings
}

// DescriptionEditFrozenForActiveRuns is ScopeEditFrozenForActiveRuns for an
// `issue edit --description`: it names each live run that froze a different
// description for issueID and still holds a non-terminal step that will render
// it, and points at `run refresh-body`, the verb that makes the amendment
// reach that run's remaining steps.
//
// The description has no live reader at all — a packet's REQUEST and
// issue.body both render `run_issues.body_snapshot` — so, unlike the scope
// advisory, there is no half of the edit that already took effect to report.
// It is advisory-only and swallows every error to nil for the same reasons.
func DescriptionEditFrozenForActiveRuns(conn *sql.DB, issueID int) []string {
	var description sql.NullString
	if err := conn.QueryRow(
		`SELECT description FROM issues WHERE id = ?`, issueID,
	).Scan(&description); err != nil {
		return nil
	}
	bindings, err := frozenBindingsWithLiveSteps(conn, issueID)
	if err != nil {
		return nil
	}

	var warnings []string
	for _, b := range bindings {
		if b.bodySnapshot == description.String {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s is bound into %s, which froze its description at activation "+
				"— the %d step(s) still live there will render that "+
				"frozen description as their packets' REQUEST and issue.body, "+
				"NOT the description you just wrote. To make the amendment "+
				"reach this run's remaining work, refresh its snapshot — "+
				"`docket run refresh-body %s --issue %s --reason \"description amended\"` "+
				"(refuses while any of the issue's steps is claimed, running "+
				"or gated)",
			model.FormatID(issueID), model.FormatRunID(b.runID), b.liveSteps,
			model.FormatRunID(b.runID), model.FormatID(issueID),
		))
	}
	return warnings
}

// frozenBinding is one run's activation snapshot of an issue, in a run where
// that snapshot will still be rendered.
type frozenBinding struct {
	runID         int
	issueSnapshot string
	bodySnapshot  string
	liveSteps     int
}

// frozenBindingsWithLiveSteps returns, in run order, every binding of issueID
// into a non-terminal run that has activated it (so a snapshot exists) and
// still holds at least one non-terminal step for it (so a packet will still be
// rendered from that snapshot). This is the "live steps in a live run" both
// mid-run edit advisories fire on.
//
// The rows are drained before returning, so a caller may query again: the
// engine's pool is single-connection (the SQLite writer discipline), and a
// query issued while this cursor was open would wait forever for the
// connection the walk holds.
func frozenBindingsWithLiveSteps(conn *sql.DB, issueID int) ([]frozenBinding, error) {
	rows, err := conn.Query(
		`SELECT ri.run_id, ri.issue_snapshot, COALESCE(ri.body_snapshot, ''),
		        COUNT(s.id)
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
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var bindings []frozenBinding
	for rows.Next() {
		var b frozenBinding
		if err := rows.Scan(&b.runID, &b.issueSnapshot, &b.bodySnapshot, &b.liveSteps); err != nil {
			return nil, err
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
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
