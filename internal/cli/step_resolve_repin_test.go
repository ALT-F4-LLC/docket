package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1034 at the CLI boundary. internal/engine/dkt1034_test.go proves the
// re-pin itself — the superseding artifact, the event, the downstream
// binding — against resolveStep directly; what this file asserts is the VERB:
// that `step resolve --as override-pass --worktree C` reaches the engine with
// the checkout normalized, that the re-pin rides the JSON envelope as
// `issue_diff_repin` and the human success line as a sentence, and that the
// two surfaces the issue's acceptance criteria name — `step render` of a
// downstream review step and `dispatch open` — read the patched tree
// afterwards through the same code paths the verbs run.

// repinWorkflow is the incident's shape, small: a tree-holding implement whose
// gate parks it, and a review that reads its issue.diff.
const repinWorkflow = `
[pipeline]
name = "cli-repin"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "implement"
emits = "change-summary"
gates = ["lint"]
on_fail = "waiting-human"

[[step]]
name = "review"
after = ["implement"]
holds_tree = false
executor = "judge"
emits = "findings"
inputs = ["issue.diff"]
`

// failingGates is the self-hygiene gate that parked RUN-67's implement step:
// it fails whatever it measures, and spawns nothing.
type failingGates struct{}

func (failingGates) Run(_ context.Context, spec engine.GateSpec, _ engine.StepContext) (engine.GateResult, error) {
	return engine.GateResult{Gate: spec.Name, Exit: 1, Verdict: engine.VerdictFail}, nil
}

// repinGit runs git in dir with a fixed identity and returns its trimmed
// output.
func repinGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{
		"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false",
	}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	testsupport.Must(t, err, "git %v in %s: %v\n%s", args, dir, err, out)
	return strings.TrimSpace(string(out))
}

func repinWrite(t *testing.T, dir, rel, body string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	testsupport.Must(t, os.MkdirAll(filepath.Dir(path), 0o755), "mkdir: %v", nil)
	testsupport.Must(t, os.WriteFile(path, []byte(body), 0o644), "writing %s: %v", rel, nil)
}

// repinRun is the parked run: implement@0 recorded from its worktree and
// parked by the failing gate, then the conductor's patch committed on top in
// that worktree and both commits cherry-picked onto the shared branch.
type repinRun struct {
	runID, implementID, reviewID int
	execRoot, worktree           string
	executorSHA, patchSHA        string
}

func parkedRepinRun(t *testing.T, conn *sql.DB) *repinRun {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	now := model.NowMS()

	registerForRun(t, conn, repinWorkflow)
	issueID, err := db.CreateIssue(conn, &model.Issue{
		Title: "re-pin me", Description: "a body",
		Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating issue: %v", err)

	r := &repinRun{execRoot: t.TempDir()}
	repinGit(t, r.execRoot, "init", "-q")
	repinWrite(t, r.execRoot, "internal/work.txt", "original\n")
	repinGit(t, r.execRoot, "add", "-A")
	repinGit(t, r.execRoot, "commit", "-q", "-m", "base")

	run, err := db.InsertRunWithContext(conn, 1, "", 0, now, db.RunContext{ExecRoot: r.execRoot})
	testsupport.Must(t, err, "starting run: %v", err)
	r.runID = run.ID
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueID), "adding issue to run: %v", nil)
	_, err = engine.Activate(conn, run.ID, engine.ActivateOptions{NowMS: now})
	testsupport.Must(t, err, "activate: %v", err)
	for instance, id := range map[string]*int{"implement@0": &r.implementID, "review@0": &r.reviewID} {
		err := conn.QueryRow(`SELECT id FROM steps WHERE instance = ?`, instance).Scan(id)
		testsupport.Must(t, err, "finding %s: %v", instance, err)
	}

	r.worktree = filepath.Join(t.TempDir(), "wt")
	repinGit(t, r.execRoot, "worktree", "add", "-q", "-b", "executor", r.worktree)
	repinWrite(t, r.worktree, "internal/work.txt", "the executor's change\nlint defect\n")
	repinGit(t, r.worktree, "add", "-A")
	repinGit(t, r.worktree, "commit", "-q", "-m", "implement the issue")
	r.executorSHA = repinGit(t, r.worktree, "rev-parse", "HEAD")

	e := engine.NewEngine()
	e.Gates = failingGates{}
	claim, err := engine.ClaimStep(conn, r.implementID, engine.ClaimOptions{Owner: "w", NowMS: now})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, r.implementID, engine.CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"),
		WorkDir: r.worktree, NowMS: now,
	})
	testsupport.Must(t, err, "complete: %v", err)
	if got := stepStatusByInstance(t, conn, "implement@0"); got != db.StepWaitingHuman {
		t.Fatalf("premise: implement@0 = %q, want %q", got, db.StepWaitingHuman)
	}

	repinWrite(t, r.worktree, "internal/work.txt", "the executor's change\nlint fixed\n")
	repinGit(t, r.worktree, "add", "-A")
	repinGit(t, r.worktree, "commit", "-q", "-m", "conductor patch")
	r.patchSHA = repinGit(t, r.worktree, "rev-parse", "HEAD")
	repinGit(t, r.execRoot, "cherry-pick", "-x", r.executorSHA, r.patchSHA)
	return r
}

// TestResolveWorktreeRepinRidesTheJSONEnvelope: the re-pin the verb performed
// is stated beside the row, and the two downstream surfaces the acceptance
// criteria name read the patched tree afterwards.
func TestResolveWorktreeRepinRidesTheJSONEnvelope(t *testing.T) {
	conn := newTestDB(t)
	r := parkedRepinRun(t, conn)

	cmd := resolveCmdWithDB(conn)
	testsupport.Must(t, cmd.Flags().Set("as", "override-pass"), "set --as: %v", nil)
	testsupport.Must(t, cmd.Flags().Set("worktree", r.worktree), "set --worktree: %v", nil)
	w, buf := bufWriter(true)
	err := runStepResolve(cmd, []string{model.FormatStepID(r.implementID)}, w)
	testsupport.Must(t, err, "step resolve: %v\n%s", err, buf.String())

	var envelope struct {
		Data struct {
			Step   string `json:"step"`
			Status string `json:"status"`
			Repin  *struct {
				Worktree   string `json:"worktree"`
				FromSHA    string `json:"from_sha"`
				ToSHA      string `json:"to_sha"`
				Artifact   string `json:"artifact"`
				Supersedes string `json:"supersedes"`
			} `json:"issue_diff_repin"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if envelope.Data.Step != model.FormatStepID(r.implementID) || envelope.Data.Status != db.StepDone {
		t.Errorf("data = %+v, want %s done", envelope.Data, model.FormatStepID(r.implementID))
	}
	repin := envelope.Data.Repin
	if repin == nil {
		t.Fatalf("the envelope carries no issue_diff_repin:\n%s", buf.String())
	}
	if repin.FromSHA != r.executorSHA || repin.ToSHA != r.patchSHA {
		t.Errorf("issue_diff_repin = %s -> %s, want %s -> %s",
			repin.FromSHA, repin.ToSHA, r.executorSHA, r.patchSHA)
	}
	if repin.Worktree != r.worktree {
		t.Errorf("issue_diff_repin.worktree = %q, want %q", repin.Worktree, r.worktree)
	}
	if !strings.HasPrefix(repin.Artifact, "ARTIFACT-") || !strings.HasPrefix(repin.Supersedes, "ARTIFACT-") {
		t.Errorf("issue_diff_repin names artifact %q superseding %q; both must be recorded",
			repin.Artifact, repin.Supersedes)
	}

	// `step render` of the downstream review step shows the patched diff and
	// names the patch commit as its target.
	rendered, err := engine.RenderStep(conn, r.reviewID, "", model.NowMS())
	testsupport.Must(t, err, "render review@0: %v", err)
	if !strings.Contains(rendered.Packet, "target_sha: "+r.patchSHA) {
		t.Errorf("review@0's packet does not target the patch commit:\n%s", rendered.Packet)
	}
	if !strings.Contains(rendered.Packet, "+lint fixed") || strings.Contains(rendered.Packet, "lint defect") {
		t.Errorf("review@0's packet does not render the patched diff:\n%s", rendered.Packet)
	}

	// `dispatch open` offers the review row and flags nothing stale: the
	// shared branch carries the patch by cherry-pick, which the real probes
	// acquit.
	m, err := engine.NewEngine().OpenDispatch(conn, r.runID, 0, nil, model.NowMS())
	testsupport.Must(t, err, "dispatch open: %v", err)
	offered := false
	for _, row := range m.Rows {
		if row.Instance == "review@0" {
			offered = true
		}
	}
	if !offered {
		t.Errorf("dispatch open does not offer review@0: %+v", m.Rows)
	}
	if len(m.StaleTargets) != 0 {
		t.Errorf("dispatch open flagged stale targets after the re-pin: %+v", m.StaleTargets)
	}

	var events int
	err = conn.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`,
		engine.EventIssueDiffRepinned).Scan(&events)
	testsupport.Must(t, err, "counting events: %v", err)
	if events != 1 {
		t.Errorf("issue-diff-repinned events = %d, want 1", events)
	}
}

// TestResolveWorktreeRepinStatesItselfInHumanMode: the success line says what
// moved, with both shas and the artifact chain.
func TestResolveWorktreeRepinStatesItselfInHumanMode(t *testing.T) {
	conn := newTestDB(t)
	r := parkedRepinRun(t, conn)

	cmd := resolveCmdWithDB(conn)
	testsupport.Must(t, cmd.Flags().Set("as", "override-pass"), "set --as: %v", nil)
	testsupport.Must(t, cmd.Flags().Set("worktree", r.worktree), "set --worktree: %v", nil)
	w, buf := bufWriter(false)
	err := runStepResolve(cmd, []string{model.FormatStepID(r.implementID)}, w)
	testsupport.Must(t, err, "step resolve: %v\n%s", err, buf.String())

	out := buf.String()
	for _, want := range []string{
		"Resolved " + model.FormatStepID(r.implementID) + " (done)",
		"issue.diff re-pinned " + r.executorSHA[:12] + " -> " + r.patchSHA[:12],
		"supersedes ARTIFACT-",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
}

// TestResolveWorktreeRefusesOutsideItsResolutions: the flag rides only the
// resolutions that keep the step's record as the reviewed object, and the
// refusal names them; nothing mutates.
func TestResolveWorktreeRefusesOutsideItsResolutions(t *testing.T) {
	conn := newTestDB(t)
	r := parkedRepinRun(t, conn)

	cmd := resolveCmdWithDB(conn)
	testsupport.Must(t, cmd.Flags().Set("as", "skip"), "set --as: %v", nil)
	testsupport.Must(t, cmd.Flags().Set("worktree", r.worktree), "set --worktree: %v", nil)
	w, buf := bufWriter(true)
	err := runStepResolve(cmd, []string{model.FormatStepID(r.implementID)}, w)
	if err == nil {
		t.Fatalf("--worktree with --as skip succeeded:\n%s", buf.String())
	}
	if !strings.Contains(err.Error(), "override-pass") || !strings.Contains(err.Error(), "rerun-gates") {
		t.Errorf("the refusal does not name the resolutions the flag rides: %v", err)
	}
	if got := stepStatusByInstance(t, conn, "implement@0"); got != db.StepWaitingHuman {
		t.Errorf("implement@0 = %q after a refused resolution, want it still parked", got)
	}
}
