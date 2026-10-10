package engine

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1180: re-activation returned `issues_expanded: 0` for an issue whose only
// depends_on predecessor was `done`.
//
// RUN-76 bound nine issues: two with no predecessors, and a seven-issue
// depends_on chain rooted at one of those two. The root ran to `done`; the
// other phase-1 issue was abandoned mid-run (`step resolve --as abandon-issue`),
// which leaves the ISSUE at `todo` by design. Every re-activation after that
// expanded nothing, said nothing about why, and the chain could never start.
//
// The predecessor check ignored the issue it was asked about: it required
// EVERY issue in every earlier topological level to be `done`, so an unrelated
// sibling in any non-terminal status gated the whole next phase. Moving the
// sibling to `backlog` — the reporter's attempted workaround — changed nothing,
// because `backlog` is not `done` either. An issue's predecessors are its own
// depends_on edges (engine-spine §6.3 R2, and planner.FindReady's reading of
// the same graph), and nothing else.

// dependsOn seeds `source depends_on target`, the row `issue link` writes.
func dependsOn(t *testing.T, conn *sql.DB, source, target int) {
	t.Helper()
	_, err := conn.Exec(
		`INSERT INTO issue_relations (source_issue_id, target_issue_id, relation_type, created_at)
		 VALUES (?, ?, 'depends_on', '2026-08-02T00:00:00Z')`,
		source, target,
	)
	testsupport.Must(t, err, "seeding relation %d depends_on %d: %v", source, target, err)
}

// stepIDOf finds ONE issue's step by instance. stepIDByInstance's lowest-id
// pick is the wrong issue's step once two issues in a run share a topology.
func stepIDOf(t *testing.T, conn *sql.DB, runID, issueID int, instance string) int {
	t.Helper()
	var id int
	err := conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND issue_id = ? AND instance = ?`,
		runID, issueID, instance,
	).Scan(&id)
	testsupport.Must(t, err, "finding %s of issue %d: %v", instance, issueID, err)
	return id
}

// stepCountOf counts the steps a run holds for one issue.
func stepCountOf(t *testing.T, conn *sql.DB, runID, issueID int) int {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM steps WHERE run_id = ? AND issue_id = ?`, runID, issueID,
	).Scan(&n)
	testsupport.Must(t, err, "counting steps of issue %d: %v", issueID, err)
	return n
}

// abandonIssueFrom drops an issue out of a run the way RUN-76's operator did:
// its `implement` is parked `waiting-human` and then resolved `--as
// abandon-issue`, through the real resolve path — so the issue is left in
// exactly the state the routing leaves it in, not a hand-set stand-in.
func abandonIssueFrom(t *testing.T, conn *sql.DB, e *Engine, runID, issueID int) {
	t.Helper()
	id := stepIDOf(t, conn, runID, issueID, "implement@0")
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token: claim.Token, Artifact: []byte("  \n"),
		Gaps:  [][]byte{[]byte("# Cannot be done here\n\nResidue.")},
		NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)
	err = e.ResolveStep(conn, id, ResolveAbandonIssue, "not converging", nowMS+1)
	testsupport.Must(t, err, "resolve --as abandon-issue: %v", err)
}

// TestReactivationExpandsADependencySatisfiedIssueBesideAnAbandonedSibling is
// RUN-76's shape, minus five links of chain: a root, an unrelated phase-1
// sibling, and a chain hanging off the root. The sibling is abandoned, the root
// completes, and re-activation must expand the root's successor — the
// sibling's status is not the successor's business.
func TestReactivationExpandsADependencySatisfiedIssueBesideAnAbandonedSibling(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	e := testEngine()

	root := createIssue(t, conn, "root", "body", "task", nil)
	sibling := createIssue(t, conn, "unrelated phase-1 sibling", "body", "task", nil)
	next := createIssue(t, conn, "next in the chain", "body", "task", nil)
	tail := createIssue(t, conn, "tail of the chain", "body", "task", nil)
	dependsOn(t, conn, next, root)
	dependsOn(t, conn, tail, next)

	run := startRun(t, conn, root, sibling, next, tail)
	first, err := activate(conn, run.ID)
	testsupport.Must(t, err, "first activate: %v", err)
	if first.IssuesExpanded != 2 {
		t.Fatalf("first activation expanded %d issues, want 2 (root and sibling)",
			first.IssuesExpanded)
	}

	abandonIssueFrom(t, conn, e, run.ID, sibling)
	abandoned, err := db.GetIssue(conn, sibling)
	testsupport.Must(t, err, "reading the abandoned issue: %v", err)
	if abandoned.Status == model.StatusDone {
		t.Fatalf("abandon-issue left the sibling `done`; the shape under test " +
			"is a NON-terminal sibling")
	}
	if abandoned.Resolution != model.ResolutionAbandoned {
		t.Fatalf("sibling resolution = %q, want %q", abandoned.Resolution,
			model.ResolutionAbandoned)
	}
	siblingSteps := stepCountOf(t, conn, run.ID, sibling)

	// The root completes — the chain's only real predecessor is satisfied.
	err = db.UpdateIssue(conn, root, map[string]any{
		"status": string(model.StatusDone),
	}, "")
	testsupport.Must(t, err, "completing the root: %v", err)

	// The reporter's exact probe, first: `run activate --dry-run`.
	dry, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS + 2, DryRun: true})
	testsupport.Must(t, err, "dry-run re-activate: %v", err)
	if dry.IssuesExpanded != 1 || dry.StepsCreated == 0 {
		t.Errorf("dry run projected %d issue(s) / %d step(s), want 1 issue (the "+
			"root's successor) with its steps: sibling status %q gated a phase "+
			"it has no edge into", dry.IssuesExpanded, dry.StepsCreated,
			abandoned.Status)
	}

	re, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS + 3})
	testsupport.Must(t, err, "re-activate: %v", err)
	if re.IssuesExpanded != 1 {
		t.Fatalf("re-activation expanded %d issue(s), want 1 (the root's successor)",
			re.IssuesExpanded)
	}
	if !runIssueOf(t, conn, run.ID, next).Expanded() {
		t.Error("the root's successor was not expanded although its only " +
			"predecessor is done")
	}
	if n := stepCountOf(t, conn, run.ID, next); n == 0 {
		t.Error("the root's successor has no steps after re-activation")
	}
	if runIssueOf(t, conn, run.ID, tail).Expanded() {
		t.Error("the chain's tail expanded although its predecessor has not " +
			"started; later phases expand as THEIR predecessors finish")
	}
	if n := stepCountOf(t, conn, run.ID, sibling); n != siblingSteps {
		t.Errorf("the abandoned sibling holds %d steps after re-activation, "+
			"had %d; an expanded issue is never expanded again", n, siblingSteps)
	}
}

// TestExpansionIgnoresAnUnrelatedSiblingsStatus is the same rule without the
// resolve machinery: whatever status an unrelated phase-1 issue is in — the
// `todo` abandon-issue leaves, the `backlog` the reporter moved it to, a live
// `in-progress` — a successor whose own predecessor is `done` expands.
func TestExpansionIgnoresAnUnrelatedSiblingsStatus(t *testing.T) {
	for _, status := range []model.Status{
		model.StatusTodo, model.StatusBacklog, model.StatusInProgress, model.StatusReview,
	} {
		t.Run(string(status), func(t *testing.T) {
			conn := mustDB(t)
			registerFixture(t, conn)

			root := createIssue(t, conn, "root", "body", "task", nil)
			sibling := createIssue(t, conn, "sibling", "body", "task", nil)
			next := createIssue(t, conn, "next", "body", "task", nil)
			dependsOn(t, conn, next, root)

			run := startRun(t, conn, root, sibling, next)
			_, err := activate(conn, run.ID)
			testsupport.Must(t, err, "first activate: %v", err)

			err = db.UpdateIssue(conn, root, map[string]any{
				"status": string(model.StatusDone),
			}, "")
			testsupport.Must(t, err, "completing the root: %v", err)
			err = db.UpdateIssue(conn, sibling, map[string]any{
				"status": string(status),
			}, "")
			testsupport.Must(t, err, "moving the sibling: %v", err)

			re, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS + 2})
			testsupport.Must(t, err, "re-activate: %v", err)
			if re.IssuesExpanded != 1 {
				t.Errorf("re-activation expanded %d issue(s) with the sibling %s, "+
					"want 1: the sibling has no edge into the successor",
					re.IssuesExpanded, status)
			}
			if !runIssueOf(t, conn, run.ID, next).Expanded() {
				t.Error("the successor was not expanded")
			}
		})
	}
}

// TestActivateNamesWhatItLeftBlocked is the diagnostic half. RUN-76's operator
// had `issues_expanded: 0` and nothing else to go on; every activation must
// name what it left unexpanded and which predecessor holds it.
//
// A first activation over root -> next -> tail, beside a sibling, expands root
// and sibling and reports next waiting on root — at the `todo` this very
// activation promotes it to, not the `backlog` it found — and tail waiting on
// next, still `backlog` (an unexpanded issue is not promoted). The dry run
// says the same. Once the chain is through, the roster is absent, not empty.
func TestActivateNamesWhatItLeftBlocked(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	root := createIssue(t, conn, "root", "body", "task", nil)
	sibling := createIssue(t, conn, "sibling", "body", "task", nil)
	next := createIssue(t, conn, "next", "body", "task", nil)
	tail := createIssue(t, conn, "tail", "body", "task", nil)
	dependsOn(t, conn, next, root)
	dependsOn(t, conn, tail, next)
	run := startRun(t, conn, root, sibling, next, tail)

	want := []BlockedIssue{
		{IssueID: model.FormatID(next), BlockedBy: []BlockingIssue{
			{IssueID: model.FormatID(root), Status: string(model.StatusTodo)},
		}},
		{IssueID: model.FormatID(tail), BlockedBy: []BlockingIssue{
			{IssueID: model.FormatID(next), Status: string(model.StatusBacklog)},
		}},
	}

	dry, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS, DryRun: true})
	testsupport.Must(t, err, "dry run: %v", err)
	if !reflect.DeepEqual(dry.BlockedIssues, want) {
		t.Errorf("dry-run blocked roster = %+v, want %+v", dry.BlockedIssues, want)
	}

	first, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	if !reflect.DeepEqual(first.BlockedIssues, want) {
		t.Errorf("blocked roster = %+v, want %+v", first.BlockedIssues, want)
	}

	err = db.UpdateIssue(conn, root, map[string]any{
		"status": string(model.StatusDone),
	}, "")
	testsupport.Must(t, err, "completing the root: %v", err)
	re, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS + 2})
	testsupport.Must(t, err, "re-activate: %v", err)
	wantRe := []BlockedIssue{
		{IssueID: model.FormatID(tail), BlockedBy: []BlockingIssue{
			{IssueID: model.FormatID(next), Status: string(model.StatusTodo)},
		}},
	}
	if !reflect.DeepEqual(re.BlockedIssues, wantRe) {
		t.Errorf("re-activation blocked roster = %+v, want %+v", re.BlockedIssues, wantRe)
	}

	err = db.UpdateIssue(conn, next, map[string]any{
		"status": string(model.StatusDone),
	}, "")
	testsupport.Must(t, err, "completing next: %v", err)
	last, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS + 3})
	testsupport.Must(t, err, "final re-activate: %v", err)
	if last.IssuesExpanded != 1 || !runIssueOf(t, conn, run.ID, tail).Expanded() {
		t.Errorf("final re-activation expanded %d issue(s), want 1 (the tail)",
			last.IssuesExpanded)
	}
	if last.BlockedIssues != nil {
		t.Errorf("blocked roster = %+v after the chain expanded, want none",
			last.BlockedIssues)
	}
}

// TestExpansionStillWaitsOnTheIssuesOwnPredecessor is the other edge of the
// rule: relaxing the sibling check must not relax the real one. A successor
// whose OWN predecessor is not done — including one abandoned in this run —
// stays unexpanded.
func TestExpansionStillWaitsOnTheIssuesOwnPredecessor(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	e := testEngine()

	root := createIssue(t, conn, "root", "body", "task", nil)
	other := createIssue(t, conn, "other root", "body", "task", nil)
	next := createIssue(t, conn, "next", "body", "task", nil)
	dependsOn(t, conn, next, root)

	run := startRun(t, conn, root, other, next)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "first activate: %v", err)

	// The successor's OWN predecessor is abandoned; the unrelated root is done.
	abandonIssueFrom(t, conn, e, run.ID, root)
	err = db.UpdateIssue(conn, other, map[string]any{
		"status": string(model.StatusDone),
	}, "")
	testsupport.Must(t, err, "completing the other root: %v", err)

	re, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS + 2})
	testsupport.Must(t, err, "re-activate: %v", err)
	if re.IssuesExpanded != 0 {
		t.Errorf("re-activation expanded %d issue(s), want 0: the successor's own "+
			"predecessor was abandoned, not finished", re.IssuesExpanded)
	}
	if runIssueOf(t, conn, run.ID, next).Expanded() {
		t.Error("a successor expanded over an abandoned predecessor")
	}
}
