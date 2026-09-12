package cli

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1056 at the CLI boundary. internal/engine/dkt1056_test.go proves the
// RESOLUTION — that the view's pair is the bundle's pair, and empty when there
// is no round record; what this file asserts is the WIRE SHAPE of `step show
// --json`, which is what a wave's pre-check actually reads: the two keys
// spelled exactly as the bundle spells them, beside the row, and ABSENT on a
// step with no target rather than present-and-empty.

// showTargetWorkflow is the shape a vote wave sees: a tree-holding implement
// and a judge that reads its issue.diff.
const showTargetWorkflow = `
[pipeline]
name = "cli-show-target"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "implement"
emits = "change-summary"

[[step]]
name = "review"
after = ["implement"]
holds_tree = false
executor = "judge"
emits = "findings"
inputs = ["issue.diff"]
`

// showTargetRun activates showTargetWorkflow over one issue and completes
// `implement` with the given recorded head and worktree — head "" and worktree
// "" is the no-round-record case. It returns the two step ids.
func showTargetRun(t *testing.T, conn *sql.DB, head, worktree string) (implementID, reviewID int) {
	t.Helper()
	now := model.NowMS()

	registerForRun(t, conn, showTargetWorkflow)
	issueID, err := db.CreateIssue(conn, &model.Issue{
		Title: "seat the panel on the judged tree", Description: "a body",
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating issue: %v", err)

	run, err := db.InsertRunWithContext(conn, 1, "", 0, now, db.RunContext{ExecRoot: t.TempDir()})
	testsupport.Must(t, err, "starting run: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueID), "adding issue to run: %v", nil)
	_, err = engine.Activate(conn, run.ID, engine.ActivateOptions{NowMS: now})
	testsupport.Must(t, err, "activate: %v", err)

	for instance, id := range map[string]*int{"implement@0": &implementID, "review@0": &reviewID} {
		err := conn.QueryRow(`SELECT id FROM steps WHERE instance = ?`, instance).Scan(id)
		testsupport.Must(t, err, "finding %s: %v", instance, err)
	}

	// Stubbed rather than shelled out: the subject here is the emitted shape,
	// and a real checkout would make the fixture a git test.
	e := engine.NewEngine()
	e.HeadFn = func(string) string { return head }
	e.DiffFn = func(_, _ string, _ []string) (string, error) { return "the diff", nil }

	claim, err := engine.ClaimStep(conn, implementID, engine.ClaimOptions{Owner: "w", NowMS: now})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = e.CompleteStep(conn, implementID, engine.CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"),
		WorkDir: worktree, NowMS: now,
	})
	testsupport.Must(t, err, "complete implement: %v", err)
	return implementID, reviewID
}

// showTargetEnvelope runs `step show --json` on one step and decodes the two
// fields under test, plus the step id so the row itself is proven still flat.
func showTargetEnvelope(t *testing.T, conn *sql.DB, stepID int) (step, sha, worktree, raw string) {
	t.Helper()
	w, buf := bufWriter(true)
	err := runStepShow(cmdWithDB(conn), []string{model.FormatStepID(stepID)}, w)
	testsupport.Must(t, err, "step show: %v\n%s", err, buf.String())

	var envelope struct {
		Data struct {
			Step           string `json:"step"`
			TargetSHA      string `json:"target_sha"`
			TargetWorktree string `json:"target_worktree"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	return envelope.Data.Step, envelope.Data.TargetSHA, envelope.Data.TargetWorktree, buf.String()
}

// TestStepShowJSONCarriesTheResolvedTarget: the wave's pre-check reads the
// judged tree off the gate's own row, without a bundle probe.
func TestStepShowJSONCarriesTheResolvedTarget(t *testing.T) {
	conn := newTestDB(t)
	_, reviewID := showTargetRun(t, conn, "cafe1234cafe1234", "/worktrees/issue-under-test")

	step, sha, worktree, raw := showTargetEnvelope(t, conn, reviewID)
	if step != model.FormatStepID(reviewID) {
		t.Errorf("data.step = %q; the row must still marshal flat:\n%s", step, raw)
	}
	if sha != "cafe1234cafe1234" {
		t.Errorf("data.target_sha = %q, want the recorded head:\n%s", sha, raw)
	}
	if worktree != "/worktrees/issue-under-test" {
		t.Errorf("data.target_worktree = %q, want the declared worktree:\n%s", worktree, raw)
	}

	// The same pair the bundle carries, from the surface that costs no probe.
	bundle, err := engine.ReadContext(conn, reviewID, model.NowMS())
	testsupport.Must(t, err, "ReadContext: %v", err)
	if bundle.TargetSHA != sha || bundle.TargetWorktree != worktree {
		t.Errorf("step show says (%q, %q), the bundle says (%q, %q)",
			sha, worktree, bundle.TargetSHA, bundle.TargetWorktree)
	}
}

// TestStepShowJSONOmitsTheTargetWithoutARoundRecord is the fabrication guard:
// a judge whose producer recorded no head and no worktree emits NEITHER key.
// Present-and-empty would read to the wave as "there is a target", and a
// stand-in sha would seat the panel on a tree nobody judged.
func TestStepShowJSONOmitsTheTargetWithoutARoundRecord(t *testing.T) {
	conn := newTestDB(t)
	_, reviewID := showTargetRun(t, conn, "", "")

	_, sha, worktree, raw := showTargetEnvelope(t, conn, reviewID)
	if sha != "" || worktree != "" {
		t.Errorf("step show invented a target (%q, %q):\n%s", sha, worktree, raw)
	}
	if strings.Contains(raw, "target_sha") || strings.Contains(raw, "target_worktree") {
		t.Errorf("the envelope carries a target key with no target to name:\n%s", raw)
	}
}

// TestStepShowJSONOmitsTheTargetForAStepThatConsumesNoDiff: `implement`
// declares no inputs, so its row is byte-for-byte what it always was.
func TestStepShowJSONOmitsTheTargetForAStepThatConsumesNoDiff(t *testing.T) {
	conn := newTestDB(t)
	implementID, _ := showTargetRun(t, conn, "cafe1234cafe1234", "/worktrees/issue-under-test")

	_, sha, worktree, raw := showTargetEnvelope(t, conn, implementID)
	if sha != "" || worktree != "" {
		t.Errorf("implement@0 reports a target (%q, %q); it consumes no "+
			"issue.diff:\n%s", sha, worktree, raw)
	}
	if strings.Contains(raw, "target_") {
		t.Errorf("a step consuming no issue.diff grew a target key:\n%s", raw)
	}
}

// TestStepShowHumanModeStatesTheTarget: the same fact on the operator's
// channel, and nothing at all where there is no target.
func TestStepShowHumanModeStatesTheTarget(t *testing.T) {
	conn := newTestDB(t)
	implementID, reviewID := showTargetRun(t, conn, "cafe1234cafe1234", "/worktrees/issue-under-test")

	w, buf := bufWriter(false)
	err := runStepShow(cmdWithDB(conn), []string{model.FormatStepID(reviewID)}, w)
	testsupport.Must(t, err, "step show: %v", err)
	if !strings.Contains(buf.String(), "cafe1234cafe1234") ||
		!strings.Contains(buf.String(), "/worktrees/issue-under-test") {
		t.Errorf("the human block does not state the target:\n%s", buf.String())
	}

	w, buf = bufWriter(false)
	err = runStepShow(cmdWithDB(conn), []string{model.FormatStepID(implementID)}, w)
	testsupport.Must(t, err, "step show: %v", err)
	if strings.Contains(buf.String(), "Target") {
		t.Errorf("a step with no target printed a target section:\n%s", buf.String())
	}
}
