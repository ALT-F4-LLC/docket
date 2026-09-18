package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-741: `docket issue edit --scope` does not reach a step that already
// exists, and no verb refreshes it. Observed on RUN-52/VPL-434 (2026-08-24):
// the panel rejected the work 3/3 twice on the same out-of-scope migration
// blocker, the operator AUTHORIZED a widen, the conductor ran the edit — and
// `docket step render STEP-2459` still showed the old two-path scope, so the
// authorized widen was unexecutable and the run paid a
// `run abandon --issue` plus a full re-plan to get a fresh snapshot.
//
// The freeze is CORRECT and stays (§5.1.1, §6.6, §9 item 5): both the packet's
// `context.issue.scope` and the recorded `issue.diff` scope read the
// activation snapshot, and re-snapshotting for a live step would let two steps
// of one run render two different scopes and record diffs over two different
// path sets. What was missing is that nothing SAID so at the moment an
// operator spends a widen on a run that cannot receive it.
//
// So: the snapshot semantics are pinned here as behavior, and
// ScopeEditFrozenForActiveRuns is the disclosure.

// scopedIssueInRun activates a one-issue run over the parks fixture with the
// given declared scope, and returns the run, the issue, and its `flaky@0` step.
func scopedIssueInRun(t *testing.T, conn *sql.DB, scope string) (int, int, int) {
	t.Helper()
	registerSource(t, conn, []byte(parkingWorkflow), "parks.toml")
	issue := createIssue(t, conn, "widen me", "body", "task", nil)
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, issue, scope), "declaring scope")
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	var stepID int
	err = conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND issue_id = ? AND instance = 'flaky@0'`,
		run.ID, issue).Scan(&stepID)
	testsupport.Must(t, err, "finding the step: %v", err)
	return run.ID, issue, stepID
}

// TestScopeEditDoesNotReachAnActivatedPacket is the semantics, asserted rather
// than assumed: the widen lands on `issues.scope_globs`, and the already-
// created step's RENDERED PACKET still carries the scope activation froze.
//
// This is the acceptance path DKT-741 offered first — "an engine verb makes a
// widened scope visible to an existing unclaimed step's packet" — recorded as
// a deliberate NON-goal. A test that pins the freeze is what stops a later
// reading of the issue from adding the refresh verb by accident.
func TestScopeEditDoesNotReachAnActivatedPacket(t *testing.T) {
	conn := mustDB(t)
	_, issue, stepID := scopedIssueInRun(t, conn, `["cli/src/command/start.rs"]`)

	before, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "rendering before the widen: %v", err)
	if !strings.Contains(before.Packet, "cli/src/command/start.rs") {
		t.Fatalf("premise: the packet does not carry the declared scope:\n%s",
			before.Packet)
	}

	// The authorized widen, exactly as the conductor ran it.
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, issue,
		`["cli/src/command/start.rs","script/install.sh","makefile"]`),
		"widening scope")

	after, err := RenderStep(conn, stepID, "", nowMS)
	testsupport.Must(t, err, "rendering after the widen: %v", err)
	if after.Packet != before.Packet {
		t.Errorf("the packet changed after a mid-run scope edit; §9 item 5's "+
			"edit immunity requires it not to:\nbefore:\n%s\nafter:\n%s",
			before.Packet, after.Packet)
	}
	for _, added := range []string{"script/install.sh", "makefile"} {
		if strings.Contains(after.Packet, added) {
			t.Errorf("the packet carries %q, which was added after activation:\n%s",
				added, after.Packet)
		}
	}

	// The diff scope reads the same frozen blob, so the two never disagree —
	// a packet that said one thing while the recorded diff covered another
	// would be worse than either answer alone.
	frozen, err := snapshotScope(conn, runIDOfStep(t, conn, stepID), issue)
	testsupport.Must(t, err, "reading the snapshot scope: %v", err)
	if len(frozen) != 1 || frozen[0] != "cli/src/command/start.rs" {
		t.Errorf("snapshotScope = %v after the widen, want the frozen "+
			"single-path scope", frozen)
	}
}

// TestScopeEditWarnsAndNamesTheAbandonPath is DKT-741's remedy: the operator
// who spends a widen on a live run is TOLD, in the same breath, that it did
// not reach the run and what would.
func TestScopeEditWarnsAndNamesTheAbandonPath(t *testing.T) {
	conn := mustDB(t)
	runID, issue, _ := scopedIssueInRun(t, conn, `["cli/src/command/start.rs"]`)

	if got := ScopeEditFrozenForActiveRuns(conn, issue); len(got) != 0 {
		t.Fatalf("premise: warned before any edit: %v", got)
	}

	testsupport.Must(t, db.SetIssueScopeGlobs(conn, issue,
		`["cli/src/command/start.rs","script/install.sh"]`), "widening scope")

	warnings := ScopeEditFrozenForActiveRuns(conn, issue)
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(warnings), warnings)
	}
	w := warnings[0]

	// It must name the run, the issue, BOTH scopes, and the sanctioned verb —
	// a warning that only says "this did not work" leaves the operator where
	// RUN-52's conductor already was.
	for _, want := range []string{
		model.FormatRunID(runID),
		model.FormatID(issue),
		"cli/src/command/start.rs",
		"script/install.sh",
		"run abandon " + model.FormatRunID(runID) + " --issue " + model.FormatID(issue),
		"re-plan",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("the warning does not name %q:\n%s", want, w)
		}
	}
}

// TestScopeEditWarningStaysSilentWhenThereIsNothingToDiscover pins the other
// half. An advisory that fires on edits it has nothing to say about is one an
// operator learns to ignore, and this one has to survive being read on the
// day it matters.
func TestScopeEditWarningStaysSilentWhenThereIsNothingToDiscover(t *testing.T) {
	t.Run("a re-declaration of the same globs", func(t *testing.T) {
		conn := mustDB(t)
		_, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)
		testsupport.Must(t, db.SetIssueScopeGlobs(conn, issue, `["internal/a/**"]`),
			"re-declaring")
		if got := ScopeEditFrozenForActiveRuns(conn, issue); len(got) != 0 {
			t.Errorf("warned on an edit that changed nothing: %v", got)
		}
	})

	t.Run("a run that never activated", func(t *testing.T) {
		conn := mustDB(t)
		registerSource(t, conn, []byte(parkingWorkflow), "parks.toml")
		issue := createIssue(t, conn, "not yet", "body", "task", nil)
		testsupport.Must(t, db.SetIssueScopeGlobs(conn, issue, `["internal/a/**"]`),
			"declaring")
		startRun(t, conn, issue)
		testsupport.Must(t, db.SetIssueScopeGlobs(conn, issue, `["internal/b/**"]`),
			"widening")
		if got := ScopeEditFrozenForActiveRuns(conn, issue); len(got) != 0 {
			t.Errorf("warned on a planning run, which has frozen nothing yet "+
				"and will snapshot the new scope at activation: %v", got)
		}
	})

	t.Run("a terminal run", func(t *testing.T) {
		conn := mustDB(t)
		runID, issue, _ := scopedIssueInRun(t, conn, `["internal/a/**"]`)
		mustExec(t, conn, `UPDATE runs SET status = ? WHERE id = ?`,
			string(model.RunAbandoned), runID)
		testsupport.Must(t, db.SetIssueScopeGlobs(conn, issue, `["internal/b/**"]`),
			"widening")
		if got := ScopeEditFrozenForActiveRuns(conn, issue); len(got) != 0 {
			t.Errorf("warned about an abandoned run: %v", got)
		}
	})

	t.Run("a live run whose steps have all recorded", func(t *testing.T) {
		conn := mustDB(t)
		_, issue, stepID := scopedIssueInRun(t, conn, `["internal/a/**"]`)
		mustExec(t, conn, `UPDATE steps SET status = ? WHERE id = ?`,
			db.StepDone, stepID)
		testsupport.Must(t, db.SetIssueScopeGlobs(conn, issue, `["internal/b/**"]`),
			"widening")
		if got := ScopeEditFrozenForActiveRuns(conn, issue); len(got) != 0 {
			t.Errorf("warned when no step will ever render again: %v", got)
		}
	})

	t.Run("an issue in no run at all", func(t *testing.T) {
		conn := mustDB(t)
		issue := createIssue(t, conn, "loose", "body", "task", nil)
		testsupport.Must(t, db.SetIssueScopeGlobs(conn, issue, `["internal/a/**"]`),
			"declaring")
		if got := ScopeEditFrozenForActiveRuns(conn, issue); len(got) != 0 {
			t.Errorf("warned on an unbound issue: %v", got)
		}
	})
}

// TestScopeEditWarningFiresForAParkedStep is the case DKT-741 was actually
// filed from: the step is `waiting-human`, which is NOT terminal — an operator
// will resolve it and its packet will render again, from the stale snapshot.
// StepTerminal is the right membership here and StepOffScheduler is not.
func TestScopeEditWarningFiresForAParkedStep(t *testing.T) {
	conn := mustDB(t)
	_, issue, stepID := scopedIssueInRun(t, conn, `["internal/a/**"]`)
	mustExec(t, conn, `UPDATE steps SET status = ? WHERE id = ?`,
		db.StepWaitingHuman, stepID)
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, issue, `["internal/b/**"]`),
		"widening")

	if got := ScopeEditFrozenForActiveRuns(conn, issue); len(got) != 1 {
		t.Fatalf("got %d warnings for a parked step, want 1: %v", len(got), got)
	}
}

// TestTerminalStepStatusesMatchesStepTerminal keeps the SQL list and the
// predicate from drifting: a tenth status added to the machine must land in
// both or in neither.
func TestTerminalStepStatusesMatchesStepTerminal(t *testing.T) {
	all := []string{
		db.StepPending, db.StepClaimed, db.StepRunning, db.StepGated,
		db.StepDone, db.StepWaitingHuman, db.StepSkipped, db.StepSuperseded,
		db.StepFailedRouted,
	}
	listed := make(map[string]bool, len(terminalStepStatuses))
	for _, s := range terminalStepStatuses {
		listed[s] = true
	}
	for _, s := range all {
		if listed[s] != db.StepTerminal(s) {
			t.Errorf("%q: terminalStepStatuses says %v, db.StepTerminal says %v",
				s, listed[s], db.StepTerminal(s))
		}
	}
	if len(terminalStepStatuses) != len(listed) {
		t.Errorf("terminalStepStatuses has a duplicate: %v", terminalStepStatuses)
	}
}

func mustExec(t *testing.T, conn *sql.DB, query string, args ...any) {
	t.Helper()
	_, err := conn.Exec(query, args...)
	testsupport.Must(t, err, "exec %q: %v", query, err)
}

func runIDOfStep(t *testing.T, conn *sql.DB, stepID int) int {
	t.Helper()
	var runID int
	err := conn.QueryRow(`SELECT run_id FROM steps WHERE id = ?`, stepID).Scan(&runID)
	testsupport.Must(t, err, "reading the step's run: %v", err)
	return runID
}
