package engine

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
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

	_, err := e.CloseDispatch(conn, runID, true, IntegrationSkip{}, nowMS)
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

	outcome, err := e.CloseDispatch(conn, runID, true, IntegrationSkip{}, nowMS)
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

	outcome, err := e.CloseDispatch(conn, runID, true, IntegrationSkip{}, nowMS)
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

	_, err := e.CloseDispatch(conn, runID, true, IntegrationSkip{}, nowMS)
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

	outcome, err := e.CloseDispatch(conn, runID, true,
		IntegrationSkip{Reason: "verified by hand, ops incident 88", Token: testConductorToken}, nowMS)
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
	outcome, err := e.CloseDispatch(conn, runID, true, IntegrationSkip{}, nowMS)
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

	_, err = e.CloseDispatch(conn, runID, true, IntegrationSkip{}, nowMS+1)
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
	outcome, err = e.CloseDispatch(conn, runID, true, IntegrationSkip{}, nowMS+1)
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
	_, err := e.CloseDispatch(conn, runID, true,
		IntegrationSkip{Reason: "hand-integrated per operator ruling", Token: testConductorToken}, nowMS)
	testsupport.Must(t, err, "skipped close: %v", err)

	openDispatch(t, conn, runID, 0, nowMS+1)
	outcome, err := e.CloseDispatch(conn, runID, true, IntegrationSkip{}, nowMS+1)
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

	outcome, err := e.CloseDispatch(conn, run.ID, true, IntegrationSkip{}, nowMS)
	testsupport.Must(t, err, "CloseDispatch: %v", err)
	if outcome.Integration == nil || outcome.Integration.Status != "verified" || len(outcome.Integration.Checked) != 0 {
		t.Errorf("Integration = %+v, want verified with nothing checked (no write-class steps)", outcome.Integration)
	}
}

// rebasedBaseRepo builds DKT-3288's shape in a real repository: the step's
// worktree forks at base B; the shared branch is then rebased so B is
// rewritten with different content; the step's own commit S is integrated
// onto it — verbatim, or with one hunk edited when edited is true. It returns
// the shared checkout, S, and B.
func rebasedBaseRepo(t *testing.T, edited bool) (execRoot, step, base string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		testsupport.Must(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644),
			"writing %s: %v", name, nil)
	}
	gitRun(t, dir, "init", "-q", "-b", "main")
	write("a.txt", "a\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "A")
	root := gitRun(t, dir, "rev-parse", "HEAD")
	write("b.txt", "one\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "B")
	base = gitRun(t, dir, "rev-parse", "HEAD")

	gitRun(t, dir, "checkout", "-q", "-b", "work")
	write("s.txt", "line 1\nline 2\nline 3\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "S")
	step = gitRun(t, dir, "rev-parse", "HEAD")

	// The shared branch is rebased: B is rewritten with different content.
	gitRun(t, dir, "checkout", "-q", "main")
	gitRun(t, dir, "reset", "-q", "--hard", root)
	write("b.txt", "one, rewritten\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "B rewritten")

	if edited {
		write("s.txt", "line 1\nline 2 edited during the pick\nline 3\n")
		gitRun(t, dir, "add", ".")
		gitRun(t, dir, "commit", "-q", "-m", "S, hand-resolved")
	} else {
		gitRun(t, dir, "cherry-pick", step)
	}
	return dir, step, base
}

// TestCloseJudgesOnlyTheStepsOwnCommitsAfterABaseRewrite is DKT-3288
// criteria 1 and 3: a verbatim pick of the step's own commit is
// patch-equivalent although the shared branch rewrote an inherited base
// commit, and that rewritten commit is named on the verdict as an advisory.
func TestCloseJudgesOnlyTheStepsOwnCommitsAfterABaseRewrite(t *testing.T) {
	execRoot, step, base := rebasedBaseRepo(t, false)
	checked, unintegrated := NewEngine().checkIntegration(execRoot, []integrationCandidate{{
		step: "STEP-1", instance: "implement@0", sha: step, base: base,
	}})
	if len(unintegrated) != 0 || len(checked) != 1 || checked[0].How != "patch-equivalent" {
		t.Fatalf("checked=%+v unintegrated=%+v; want one patch-equivalent verdict", checked, unintegrated)
	}
	if got := checked[0].BaseDiverged; len(got) != 1 || got[0] != base {
		t.Errorf("base_diverged = %v, want the rewritten base commit [%s]", got, base)
	}
}

// TestCloseStillRefusesAnEditedPickAfterABaseRewrite is DKT-3288 criterion 2:
// with the same rewrite, a pick whose hunk was edited is unintegrated.
func TestCloseStillRefusesAnEditedPickAfterABaseRewrite(t *testing.T) {
	execRoot, step, base := rebasedBaseRepo(t, true)
	checked, unintegrated := NewEngine().checkIntegration(execRoot, []integrationCandidate{{
		step: "STEP-1", instance: "implement@0", sha: step, base: base,
	}})
	if len(checked) != 0 || len(unintegrated) != 1 || unintegrated[0].How != "unintegrated" {
		t.Fatalf("checked=%+v unintegrated=%+v; want one unintegrated verdict", checked, unintegrated)
	}
}

// TestIssueDiffRecordCarriesTheFixedBase pins the record half of DKT-3288:
// the base close bounds the probe with is written beside the head when the
// step records, and never for a live base.
func TestIssueDiffRecordCarriesTheFixedBase(t *testing.T) {
	e := testEngine()
	e.HeadFn = func(string) string { return "head-sha" }
	var body string
	payload := e.appendRoundDelta(nil, &db.Step{WorkRoot: "/w"}, "/w", "/x", "base-sha", false, &body)
	var record map[string]string
	testsupport.Must(t, json.Unmarshal([]byte(payload), &record), "decoding %q: %v", payload, nil)
	if record["base"] != "base-sha" || record["head"] != "head-sha" {
		t.Errorf("record = %v, want head and the fixed base", record)
	}
	payload = e.appendRoundDelta(nil, &db.Step{WorkRoot: "/w"}, "/w", "/x", "live-sha", true, &body)
	if strings.Contains(payload, "live-sha") {
		t.Errorf("a live base was recorded: %s", payload)
	}
}

// TestWriterDiffBaseIsItsClaimHead is DKT-3300: a write-class step recorded in
// the shared checkout diffs from the HEAD its first claim recorded, not from
// the run's older pinned commit, so a commit integrated between the two is not
// rendered as this step's work; the record carries the base it used.
func TestWriterDiffBaseIsItsClaimHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		testsupport.Must(t, os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644),
			"writing %s: %v", name, nil)
	}
	gitRun(t, repo, "init", "-q", "-b", "main")
	write("seed.txt", "seed\n")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "run start")
	pinned := gitRun(t, repo, "rev-parse", "HEAD")

	conn := mustDB(t)
	registerSource(t, conn, []byte(writeClassWorkflowSrc), "integration-fixture.toml")
	issue := createIssue(t, conn, "writer base", "a body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "writer base run", 0, nowMS,
		db.RunContext{ExecRoot: repo, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue: %v", err)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// A sibling's work lands on the shared branch before this writer claims.
	write("sibling.txt", "A SIBLING'S INTEGRATED WORK\n")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "sibling")
	claimHead := gitRun(t, repo, "rev-parse", "HEAD")

	e := testEngine()
	e.DiffFn = GitDiff
	e.HeadFn = sharedCheckoutHead
	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	write("work.txt", "THE WRITER'S OWN CHANGE\n")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "writer")
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)

	var body, payload string
	err = conn.QueryRow(`SELECT body, payload FROM artifacts WHERE step_id = ? AND kind = ?`,
		stepID, ArtifactKindIssueDiff).Scan(&body, &payload)
	testsupport.Must(t, err, "reading issue.diff: %v", err)
	if !strings.Contains(body, "THE WRITER'S OWN CHANGE") {
		t.Errorf("issue.diff lacks the writer's change:\n%s", body)
	}
	if strings.Contains(body, "SIBLING") {
		t.Errorf("issue.diff carries the intervening sibling commit:\n%s", body)
	}
	var record map[string]string
	testsupport.Must(t, json.Unmarshal([]byte(payload), &record), "decoding %q: %v", payload, nil)
	if record["base"] != claimHead {
		t.Errorf("record base = %q, want the claim head %q", record["base"], claimHead)
	}
}

// gatedWriteWorkflowSrc is a write-class step whose failing gate parks it,
// followed by a non-write step that can park the issue after the writer is
// done — the two places an operator rules a writer's work out of the run.
const gatedWriteWorkflowSrc = `
[pipeline]
name = "gated-write-fixture"
version = 1

[match]
kind = ["task"]

[limits]
write = { max = 2 }

[[step]]
name = "implement"
executor = "w"
class = "write"
emits = "change-summary"
gates = ["build"]
on_fail = "waiting-human"
after = []

[[step]]
name = "review"
executor = "r"
emits = "report"
gates = ["build"]
on_fail = "waiting-human"
after = ["implement"]
`

// gatedWriteRun activates one run over gatedWriteWorkflowSrc holding
// issueCount issues. Every git probe answers "not integrated", so any
// candidate the check collects refuses the close. *head is the sha the next
// completion records.
func gatedWriteRun(t *testing.T, issueCount int) (*sql.DB, *Engine, *exitGates, int, []int, *string) {
	t.Helper()
	conn := mustDB(t)
	registerSource(t, conn, []byte(gatedWriteWorkflowSrc), "gated-write-fixture.toml")
	issues := make([]int, issueCount)
	for i := range issues {
		issues[i] = createIssue(t, conn, "gated write fixture", "a body", "task", nil)
	}
	run := startRun(t, conn, issues...)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	head := ""
	gates := &exitGates{}
	e := testEngine()
	e.Gates = gates
	e.HeadFn = func(string) string { return head }
	e.IsAncestorFn = func(_, _ string) (bool, bool) { return false, true }
	e.PatchContainedFn = func(_, _ string) (bool, bool) { return false, true }
	return conn, e, gates, run.ID, issues, &head
}

// issueStepID resolves an instance within one issue of one run.
func issueStepID(t *testing.T, conn *sql.DB, runID, issueID int, instance string) int {
	t.Helper()
	var id int
	err := conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND issue_id = ? AND instance = ?`,
		runID, issueID, instance,
	).Scan(&id)
	testsupport.Must(t, err, "finding %s of issue %d: %v", instance, issueID, err)
	return id
}

// completeAs claims and completes one step, leaving it wherever its gate
// routes it, and returns the status it landed in.
func completeAs(t *testing.T, conn *sql.DB, e *Engine, stepID int) string {
	t.Helper()
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim step %d: %v", stepID, err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the summary"),
		WorkDir: "/worktrees/wf-gated", NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete step %d: %v", stepID, err)
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	return step.Status
}

// requireRecordedHead fails unless the step's own issue.diff names sha, so an
// ignored candidate is proven ignored rather than never recorded.
func requireRecordedHead(t *testing.T, conn *sql.DB, stepID int, sha string) {
	t.Helper()
	var payload string
	err := conn.QueryRow(
		`SELECT payload FROM artifacts WHERE step_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`,
		stepID, ArtifactKindIssueDiff).Scan(&payload)
	testsupport.Must(t, err, "reading step %d's issue.diff: %v", stepID, err)
	if !strings.Contains(payload, sha) {
		t.Fatalf("premise: step %d's issue.diff payload %q does not record %s", stepID, payload, sha)
	}
}

// requireVerifiedWithout closes the run with no skip reason and requires a
// verified close that neither refused on nor checked sha.
func requireVerifiedWithout(t *testing.T, conn *sql.DB, e *Engine, runID int, sha string) {
	t.Helper()
	openDispatch(t, conn, runID, 0, nowMS+2)
	outcome, err := e.CloseDispatch(conn, runID, true, IntegrationSkip{}, nowMS+2)
	testsupport.Must(t, err, "CloseDispatch without --skip-integration-check: %v", err)
	if outcome.Integration == nil || outcome.Integration.Status != "verified" {
		t.Fatalf("Integration = %+v, want status verified", outcome.Integration)
	}
	for _, c := range outcome.Integration.Checked {
		if c.SHA == sha {
			t.Errorf("Checked = %+v, want no row for %s", outcome.Integration.Checked, sha)
		}
	}
}

// TestCloseIgnoresAnAbandonedIssuesWriter: once the run abandons an issue, its
// writers' commits are not integration candidates, whether the writer itself
// was routed abandon-issue or finished before a later step abandoned the issue.
func TestCloseIgnoresAnAbandonedIssuesWriter(t *testing.T) {
	t.Run("writer routed abandon-issue", func(t *testing.T) {
		conn, e, gates, runID, issues, head := gatedWriteRun(t, 1)
		implement := issueStepID(t, conn, runID, issues[0], "implement@0")
		*head, gates.fail, gates.exit = "abad0000aa01", true, 1
		if got := completeAs(t, conn, e, implement); got != db.StepWaitingHuman {
			t.Fatalf("premise: implement@0 = %q, want %q", got, db.StepWaitingHuman)
		}
		requireRecordedHead(t, conn, implement, "abad0000aa01")
		testsupport.Must(t, e.ResolveStep(conn, implement, ResolveAbandonIssue,
			"the approach was wrong", nowMS+1), "resolve --as abandon-issue")

		requireVerifiedWithout(t, conn, e, runID, "abad0000aa01")
	})

	t.Run("writer done, a later step routed abandon-issue", func(t *testing.T) {
		conn, e, gates, runID, issues, head := gatedWriteRun(t, 1)
		implement := issueStepID(t, conn, runID, issues[0], "implement@0")
		*head = "abad0000bb02"
		if got := completeAs(t, conn, e, implement); got != db.StepDone {
			t.Fatalf("premise: implement@0 = %q, want %q", got, db.StepDone)
		}
		requireRecordedHead(t, conn, implement, "abad0000bb02")
		review := issueStepID(t, conn, runID, issues[0], "review@0")
		gates.fail, gates.exit = true, 1
		if got := completeAs(t, conn, e, review); got != db.StepWaitingHuman {
			t.Fatalf("premise: review@0 = %q, want %q", got, db.StepWaitingHuman)
		}
		testsupport.Must(t, e.ResolveStep(conn, review, ResolveAbandonIssue,
			"the review reproduced no fix", nowMS+1), "resolve --as abandon-issue")

		requireVerifiedWithout(t, conn, e, runID, "abad0000bb02")
	})
}

// TestCloseIgnoresASkipResolvedWriter: a writer the operator resolved --as
// skip took its work out of the run, so its commit is not a candidate.
func TestCloseIgnoresASkipResolvedWriter(t *testing.T) {
	conn, e, gates, runID, issues, head := gatedWriteRun(t, 1)
	implement := issueStepID(t, conn, runID, issues[0], "implement@0")
	*head, gates.fail, gates.exit = "5c1b0000cc03", true, 1
	if got := completeAs(t, conn, e, implement); got != db.StepWaitingHuman {
		t.Fatalf("premise: implement@0 = %q, want %q", got, db.StepWaitingHuman)
	}
	requireRecordedHead(t, conn, implement, "5c1b0000cc03")
	testsupport.Must(t, e.ResolveStep(conn, implement, ResolveSkip,
		"superseded by another issue's change", nowMS+1), "resolve --as skip")
	step, err := db.GetStep(conn, implement)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepSkipped {
		t.Fatalf("premise: implement@0 = %q after resolve --as skip, want %q", step.Status, db.StepSkipped)
	}

	requireVerifiedWithout(t, conn, e, runID, "5c1b0000cc03")
}

// TestCloseStillRefusesAWriterBesideAnAbandonedIssue: abandoning one issue
// excludes only that issue's writers; another issue's unintegrated commit in
// the same run still refuses the close, named in the refusal.
func TestCloseStillRefusesAWriterBesideAnAbandonedIssue(t *testing.T) {
	conn, e, gates, runID, issues, head := gatedWriteRun(t, 2)

	abandoned := issueStepID(t, conn, runID, issues[0], "implement@0")
	*head, gates.fail, gates.exit = "abad0000dd04", true, 1
	if got := completeAs(t, conn, e, abandoned); got != db.StepWaitingHuman {
		t.Fatalf("premise: abandoned implement@0 = %q, want %q", got, db.StepWaitingHuman)
	}
	testsupport.Must(t, e.ResolveStep(conn, abandoned, ResolveAbandonIssue,
		"the approach was wrong", nowMS+1), "resolve --as abandon-issue")

	live := issueStepID(t, conn, runID, issues[1], "implement@0")
	*head, gates.fail = "11fe0000ee05", false
	if got := completeAs(t, conn, e, live); got != db.StepDone {
		t.Fatalf("premise: live implement@0 = %q, want %q", got, db.StepDone)
	}

	openDispatch(t, conn, runID, 0, nowMS+2)
	_, err := e.CloseDispatch(conn, runID, true, IntegrationSkip{}, nowMS+2)
	if err == nil {
		t.Fatal("want a refusal over the live issue's unintegrated commit")
	}
	msg := err.Error()
	for _, want := range []string{model.FormatStepID(live), "11fe0000ee05"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not name %q", msg, want)
		}
	}
	for _, unwanted := range []string{model.FormatStepID(abandoned), "abad0000dd04"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("refusal %q names the abandoned writer's %q", msg, unwanted)
		}
	}
}
