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
	// A skipped check asks git nothing, but records what the reason vouched
	// for — the record a later close of this run honors (DKT-1787).
	if len(outcome.Integration.Checked) != 1 || outcome.Integration.Checked[0].How != "skipped" ||
		outcome.Integration.Checked[0].SHA != "5ca1ab1e04" {
		t.Errorf("Checked = %+v, want the one candidate recorded as skipped", outcome.Integration.Checked)
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

// twoWriteStepsWorkflowSrc is writeClassWorkflowSrc with a second write-class
// step after the first, so a commit can land BETWEEN two closes of one run.
const twoWriteStepsWorkflowSrc = `
[pipeline]
name = "prior-integration-fixture"
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

[[step]]
name = "amend"
executor = "w"
class = "write"
emits = "amendment"
after = ["implement"]
`

// priorIntegrationFixture activates one issue against twoWriteStepsWorkflowSrc
// and completes implement@0 with the writer sha `first`. The engine's HeadFn
// reads *head, so the caller can move it before completing amend@0.
func priorIntegrationFixture(t *testing.T, first string) (*sql.DB, *Engine, int, *string) {
	t.Helper()
	conn := mustDB(t)
	registerSource(t, conn, []byte(twoWriteStepsWorkflowSrc), "prior-integration-fixture.toml")
	issue := createIssue(t, conn, "prior integration fixture", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	head := first
	e := testEngine()
	e.HeadFn = func(string) string { return head }
	completeWriteStep(t, conn, e, "implement@0", "/worktrees/wf-implement")
	return conn, e, run.ID, &head
}

// completeWriteStep claims and completes one write-class step from a worktree.
func completeWriteStep(t *testing.T, conn *sql.DB, e *Engine, instance, worktree string) {
	t.Helper()
	stepID := stepIDByInstance(t, conn, instance)
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim %s: %v", instance, err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the summary"), WorkDir: worktree, NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete %s: %v", instance, err)
}

// TestDispatchCloseHonorsPriorIntegration is DKT-1787 (RUN-90's shape): a
// write-class commit a prior close accepted — here as patch-equivalent, the
// way a hand-resolved cherry-pick reads at the moment it is verified — is not
// re-asked of git by a later close, where it would fail both probes; and a
// commit recorded AFTER that close is still asked, and still refused.
func TestDispatchCloseHonorsPriorIntegration(t *testing.T) {
	conn, e, runID, head := priorIntegrationFixture(t, "154e3be7ed65")
	e.IsAncestorFn = func(_, _ string) (bool, bool) { return false, true }
	e.PatchContainedFn = func(_, sha string) (bool, bool) { return sha == "154e3be7ed65", true }
	first := openDispatch(t, conn, runID, 0, nowMS)
	outcome, err := e.CloseDispatch(conn, runID, true, "", nowMS)
	testsupport.Must(t, err, "first close: %v", err)
	if outcome.Integration.Status != "verified" || len(outcome.Integration.Checked) != 1 {
		t.Fatalf("first close Integration = %+v, want one verified row", outcome.Integration)
	}

	// Between the closes: the hand-resolved integration lands (git can no
	// longer match the writer sha), and a second write step records a new
	// commit the prior close never saw.
	e.PatchContainedFn = func(_, _ string) (bool, bool) { return false, true }
	*head = "d0e9747b8acd"
	completeWriteStep(t, conn, e, "amend@0", "/worktrees/wf-amend")
	second := openDispatch(t, conn, runID, 0, nowMS+1)

	_, err = e.CloseDispatch(conn, runID, true, "", nowMS+1)
	if err == nil {
		t.Fatal("want a refusal over amend@0's unintegrated commit")
	}
	if !strings.Contains(err.Error(), "d0e9747b8acd") {
		t.Errorf("refusal %q does not name amend@0's sha", err.Error())
	}
	if strings.Contains(err.Error(), "154e3be7ed65") {
		t.Errorf("refusal %q re-flags implement@0's commit, which %s already accepted",
			err.Error(), first.Dispatch)
	}

	// Integrate the second commit; the close accepts both — one honored from
	// the prior close, one asked of git — and says which was which.
	e.PatchContainedFn = func(_, sha string) (bool, bool) { return sha == "d0e9747b8acd", true }
	outcome, err = e.CloseDispatch(conn, runID, true, "", nowMS+1)
	testsupport.Must(t, err, "second close: %v", err)
	if outcome.Dispatch != second.Dispatch {
		t.Errorf("closed %s, want %s", outcome.Dispatch, second.Dispatch)
	}
	rows := map[string]CheckedIntegration{}
	for _, c := range outcome.Integration.Checked {
		rows[c.SHA] = c
	}
	if got := rows["154e3be7ed65"]; got.Prior != first.Dispatch || got.How != "patch-equivalent" {
		t.Errorf("implement@0's row = %+v, want How patch-equivalent honored from %s",
			got, first.Dispatch)
	}
	if got := rows["d0e9747b8acd"]; got.Prior != "" || got.How != "patch-equivalent" {
		t.Errorf("amend@0's row = %+v, want a fresh patch-equivalent verdict", got)
	}
}

// TestDispatchCloseHonorsASkippedPriorClose: --skip-integration-check's record
// carries the same authority for the next close as a verified one, and the
// honored row says the acceptance was a skip, not a git verdict.
func TestDispatchCloseHonorsASkippedPriorClose(t *testing.T) {
	conn, e, runID, _ := priorIntegrationFixture(t, "5ca1ab1e04")
	e.IsAncestorFn = func(_, _ string) (bool, bool) { return false, true }
	e.PatchContainedFn = func(_, _ string) (bool, bool) { return false, true }
	first := openDispatch(t, conn, runID, 0, nowMS)
	_, err := e.CloseDispatch(conn, runID, true, "hand-integrated per operator ruling", nowMS)
	testsupport.Must(t, err, "skipped close: %v", err)

	openDispatch(t, conn, runID, 0, nowMS+1)
	outcome, err := e.CloseDispatch(conn, runID, true, "", nowMS+1)
	testsupport.Must(t, err, "close after a skipped close: %v", err)
	if outcome.Integration.Status != "verified" || len(outcome.Integration.Checked) != 1 {
		t.Fatalf("Integration = %+v, want verified with the one honored row", outcome.Integration)
	}
	got := outcome.Integration.Checked[0]
	if got.How != "skipped" || got.Prior != first.Dispatch {
		t.Errorf("honored row = %+v, want How skipped from %s", got, first.Dispatch)
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
