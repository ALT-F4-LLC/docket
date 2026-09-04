package engine

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1284: `dispatch close` verifies every write-class step's own recorded
// commit reached the shared branch before it will close, replacing dotfiles'
// src/user/claude_code/workflows/integration-check.js — a script the
// conductor had to remember to launch and paste into the close report, which
// is exactly how a run once shipped a close whose shared branch never
// advanced, found 19 hours later.

// writeClassWorkflowSrc is one executor step in a class [limits] bounds
// (`write`), which is what makes it a WRITE-CLASS step per Scheduler's own
// reading (reap_ack.go's writeClassOf: "a class whose effective [limits] max
// is finite").
const writeClassWorkflowSrc = `
[pipeline]
name = "integration-check-fixture"
version = 1

[match]
kind = ["task"]

[limits]
write = { max = 1 }

[[step]]
name = "implement"
executor = "w"
class = "write"
emits = "change-summary"
after = []
`

// integrationFixture activates one issue against writeClassWorkflowSrc and
// completes its write-class step with a recorded commit — `head` (from the
// injected HeadFn) and `worktree` — so the step's own `issue.diff` artifact
// carries exactly what checkIntegration reads.
func integrationFixture(t *testing.T, head, worktree string) (*sql.DB, *Engine, int) {
	t.Helper()
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeClassWorkflowSrc), "integration-fixture.toml")
	issue := createIssue(t, conn, "integration fixture", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	e.HeadFn = func(string) string { return head }

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"),
		WorkDir: worktree, NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete implement: %v", err)

	return conn, e, run.ID
}

// TestCloseRefusesAnUnintegratedWriteStep is AC1's refusal half: a recorded
// commit that is neither an ancestor nor patch-equivalent blocks the close,
// naming the step, its sha, and its worktree.
func TestCloseRefusesAnUnintegratedWriteStep(t *testing.T) {
	conn, e, runID := integrationFixture(t, "deadbeef00", "/worktrees/wf-implement")
	e.IsAncestorFn = func(_, _ string) (bool, bool) { return false, true }
	e.PatchContainedFn = func(_, _ string) (bool, bool) { return false, true }

	_, err := e.CloseDispatch(conn, runID, true, "", nowMS)
	if err == nil {
		t.Fatal("want a refusal over the unintegrated write-step commit")
	}
	msg := err.Error()
	for _, want := range []string{"deadbeef00", "/worktrees/wf-implement"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not name %q", msg, want)
		}
	}
}

// TestCloseAcceptsAnAncestorIntegratedWriteStep is AC1's clean-close half:
// after "cherry-pick" (here, ancestry becomes true) it closes clean and the
// event's Integration reports "verified" with the checked sha.
func TestCloseAcceptsAnAncestorIntegratedWriteStep(t *testing.T) {
	conn, e, runID := integrationFixture(t, "cafefeed01", "/worktrees/wf-implement")
	e.IsAncestorFn = func(_, sha string) (bool, bool) { return sha == "cafefeed01", true }
	openDispatch(t, conn, runID, 0, nowMS)

	outcome, err := e.CloseDispatch(conn, runID, true, "", nowMS)
	testsupport.Must(t, err, "CloseDispatch: %v", err)

	if outcome.Integration == nil || outcome.Integration.Status != "verified" {
		t.Fatalf("Integration = %+v, want status verified", outcome.Integration)
	}
	if len(outcome.Integration.Checked) != 1 || outcome.Integration.Checked[0].SHA != "cafefeed01" ||
		outcome.Integration.Checked[0].How != "ancestor" {
		t.Errorf("Checked = %+v, want one ancestor row for cafefeed01", outcome.Integration.Checked)
	}
}

// TestCloseAcceptsAPatchEquivalentWriteStep is AC2 verbatim: a cherry-picked
// commit mints a NEW sha for identical content, so ancestry fails by
// construction — patch-equivalence is what must pass it.
func TestCloseAcceptsAPatchEquivalentWriteStep(t *testing.T) {
	conn, e, runID := integrationFixture(t, "beadfeed02", "/worktrees/wf-implement")
	e.IsAncestorFn = func(_, _ string) (bool, bool) { return false, true }
	e.PatchContainedFn = func(_, sha string) (bool, bool) { return sha == "beadfeed02", true }
	openDispatch(t, conn, runID, 0, nowMS)

	outcome, err := e.CloseDispatch(conn, runID, true, "", nowMS)
	testsupport.Must(t, err, "CloseDispatch: %v", err)

	if outcome.Integration == nil || outcome.Integration.Status != "verified" {
		t.Fatalf("Integration = %+v, want status verified", outcome.Integration)
	}
	if len(outcome.Integration.Checked) != 1 || outcome.Integration.Checked[0].How != "patch-equivalent" {
		t.Errorf("Checked = %+v, want one patch-equivalent row", outcome.Integration.Checked)
	}
}

// TestCloseCherryErrorCountsAsUnintegrated is AC1's explicit clause: a patch
// probe that could not even run is refused, never assumed equivalent.
func TestCloseCherryErrorCountsAsUnintegrated(t *testing.T) {
	conn, e, runID := integrationFixture(t, "0ddba11003", "/worktrees/wf-implement")
	e.IsAncestorFn = func(_, _ string) (bool, bool) { return false, true }
	e.PatchContainedFn = func(_, _ string) (bool, bool) { return false, false } // known=false: cherry itself failed

	_, err := e.CloseDispatch(conn, runID, true, "", nowMS)
	if err == nil {
		t.Fatal("want a refusal when the patch probe itself could not run")
	}
	if !strings.Contains(err.Error(), "cherry-error") {
		t.Errorf("refusal %q does not name the cherry-error cause", err.Error())
	}
}

// TestCloseSkipIntegrationCheckRecordsTheReason is AC3's override half:
// --skip-integration-check names a reason, the check never runs (an
// otherwise-unintegrated commit does not block), and the reason rides the
// close event.
func TestCloseSkipIntegrationCheckRecordsTheReason(t *testing.T) {
	conn, e, runID := integrationFixture(t, "5ca1ab1e04", "/worktrees/wf-implement")
	// Would refuse if the check ran at all.
	e.IsAncestorFn = func(_, _ string) (bool, bool) { return false, true }
	e.PatchContainedFn = func(_, _ string) (bool, bool) { return false, true }
	openDispatch(t, conn, runID, 0, nowMS)

	outcome, err := e.CloseDispatch(conn, runID, true, "verified by hand, ops incident 88", nowMS)
	testsupport.Must(t, err, "CloseDispatch with --skip-integration-check: %v", err)

	if outcome.Integration == nil || outcome.Integration.Status != "skipped" {
		t.Fatalf("Integration = %+v, want status skipped", outcome.Integration)
	}
	if outcome.Integration.Reason != "verified by hand, ops incident 88" {
		t.Errorf("Integration.Reason = %q, want the operator's reason", outcome.Integration.Reason)
	}
	if len(outcome.Integration.Checked) != 0 {
		t.Errorf("a skipped check must ask git nothing, got %+v", outcome.Integration.Checked)
	}

	// AC3: the close EVENT carries it too, not only the returned struct.
	events, err := ListEvents(conn, EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	found := false
	for _, ev := range events.Events {
		if ev.Kind != EventDispatchClosed {
			continue
		}
		var data struct {
			Integration struct {
				Status string `json:"status"`
				Reason string `json:"reason"`
			} `json:"integration"`
		}
		err := json.Unmarshal(ev.Data, &data)
		testsupport.Must(t, err, "decoding the close event: %v", err)
		if data.Integration.Status != "skipped" || data.Integration.Reason == "" {
			t.Errorf("close event integration = %+v, want skipped with the reason", data.Integration)
		}
		found = true
	}
	if !found {
		t.Fatal("no dispatch-closed event was recorded")
	}
}

// TestCloseNonWriteClassStepsAreNotCandidates: a step whose class carries no
// [limits] bound (unbounded — the author declared it safe to parallelize) is
// never a candidate, however its commit would resolve; the run report shows
// nothing was checked, not a false "verified".
func TestCloseNonWriteClassStepsAreNotCandidates(t *testing.T) {
	const src = `
[pipeline]
name = "unbounded-fixture"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "w"
emits = "change-summary"
after = []
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "unbounded-fixture.toml")
	issue := createIssue(t, conn, "unbounded", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	e.HeadFn = func(string) string { return "unboundedsha05" }
	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"),
		WorkDir: "/worktrees/unbounded", NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete implement: %v", err)

	// If this ran at all it would refuse — proving absence rather than mere
	// silence.
	e.IsAncestorFn = func(_, _ string) (bool, bool) { return false, true }
	e.PatchContainedFn = func(_, _ string) (bool, bool) { return false, true }
	openDispatch(t, conn, run.ID, 0, nowMS)

	outcome, err := e.CloseDispatch(conn, run.ID, true, "", nowMS)
	testsupport.Must(t, err, "CloseDispatch: %v", err)
	if outcome.Integration == nil || outcome.Integration.Status != "verified" || len(outcome.Integration.Checked) != 0 {
		t.Errorf("Integration = %+v, want verified with nothing checked (no write-class steps)", outcome.Integration)
	}
}
