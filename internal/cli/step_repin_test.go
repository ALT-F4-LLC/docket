package cli

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// rewrittenDoneStep is a done implement@0 whose newest issue.diff was re-cut
// for the writer's head from a whole-branch base, as a rerun-gates does once
// the worktree is swept. It returns the run, the rewrite's id, and the base
// the rewrite names, which only its payload carries.
func rewrittenDoneStep(t *testing.T, conn *sql.DB) (r *repinRun, rewriteID int, branchBase string) {
	t.Helper()
	r = parkedRepinRun(t, conn)
	resolve := resolveCmdWithDB(conn)
	testsupport.Must(t, resolve.Flags().Set("as", engine.ResolveOverridePass), "set --as: %v", nil)
	w, buf := bufWriter(true)
	err := runStepResolve(resolve, []string{model.FormatStepID(r.implementID)}, w)
	testsupport.Must(t, err, "step resolve: %v\n%s", err, buf.String())
	if got := stepStatusByInstance(t, conn, "implement@0"); got != db.StepDone {
		t.Fatalf("premise: implement@0 = %q, want done", got)
	}

	branchBase = strings.Repeat("4", 40)
	payload, err := json.Marshal(map[string]string{
		"worktree": r.worktree, "head": r.executorSHA, "base": branchBase,
	})
	testsupport.Must(t, err, "payload: %v", err)
	body := "the whole-branch re-cut\n"
	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	defer tx.Rollback()
	rewriteID, err = store.TxOn(tx).InsertArtifactTx(store.Artifact{
		RunID: r.runID, StepID: r.implementID, Kind: engine.ArtifactKindIssueDiff,
		Body: body, Payload: string(payload), SHA256: workflow.SHA256([]byte(body)),
	}, model.NowMS())
	testsupport.Must(t, err, "insert the rewrite: %v", err)
	testsupport.Must(t, tx.Commit(), "commit: %v", nil)
	return r, rewriteID, branchBase
}

// currentIssueDiff is the step's newest issue.diff record, the one every
// resolver binds a downstream packet to: its id, what it supersedes, and its
// body.
func currentIssueDiff(t *testing.T, conn *sql.DB, stepID int) (id int, supersedes sql.NullInt64, body string) {
	t.Helper()
	err := conn.QueryRow(`SELECT id, supersedes, body FROM artifacts
		WHERE step_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`,
		stepID, engine.ArtifactKindIssueDiff).Scan(&id, &supersedes, &body)
	testsupport.Must(t, err, "reading the current issue.diff: %v", err)
	return id, supersedes, body
}

// TestStepRepinRerecordsADoneStepOverItsOwnRange: `step repin` re-records the
// rewritten record over the range the writer recorded for its head, as the
// one current issue.diff superseding the rewrite, and names the old and new
// range in both renderings.
func TestStepRepinRerecordsADoneStepOverItsOwnRange(t *testing.T) {
	conn := newTestDB(t)
	r, rewriteID, branchBase := rewrittenDoneStep(t, conn)
	writerBase := repinGit(t, r.worktree, "rev-parse", "HEAD~2")

	w, buf := bufWriter(true)
	err := runStepRepin(cmdWithDB(conn), []string{model.FormatStepID(r.implementID)}, w)
	testsupport.Must(t, err, "step repin: %v\n%s", err, buf.String())

	var envelope struct {
		Data struct {
			Step  string `json:"step"`
			Repin *struct {
				FromBase   string `json:"from_base"`
				FromSHA    string `json:"from_sha"`
				ToBase     string `json:"to_base"`
				ToSHA      string `json:"to_sha"`
				Artifact   string `json:"artifact"`
				Supersedes string `json:"supersedes"`
			} `json:"issue_diff_repin"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal(buf.Bytes(), &envelope), "unmarshal: %v\n%s", nil, buf.String())
	repin := envelope.Data.Repin
	if repin == nil {
		t.Fatalf("the envelope carries no issue_diff_repin:\n%s", buf.String())
	}
	if repin.FromBase != branchBase || repin.FromSHA != r.executorSHA ||
		repin.ToBase != writerBase || repin.ToSHA != r.executorSHA {
		t.Errorf("issue_diff_repin range = %s..%s -> %s..%s, want %s..%s -> %s..%s",
			repin.FromBase, repin.FromSHA, repin.ToBase, repin.ToSHA,
			branchBase, r.executorSHA, writerBase, r.executorSHA)
	}

	id, supersedes, body := currentIssueDiff(t, conn, r.implementID)
	if repin.Artifact != model.FormatArtifactID(id) {
		t.Errorf("the current issue.diff is ARTIFACT-%d, want the re-pin %s", id, repin.Artifact)
	}
	if !supersedes.Valid || int(supersedes.Int64) != rewriteID ||
		repin.Supersedes != model.FormatArtifactID(rewriteID) {
		t.Errorf("the current issue.diff supersedes %v (reported %s), want the rewrite ARTIFACT-%d",
			supersedes, repin.Supersedes, rewriteID)
	}
	if !strings.Contains(body, "+lint defect") || strings.Contains(body, "whole-branch") {
		t.Errorf("the current issue.diff is not the writer's own range:\n%s", body)
	}
}

// TestStepRepinStatesBothRangesInHumanMode: the success line names the range
// the rewrite pinned and the range the re-pin pinned.
func TestStepRepinStatesBothRangesInHumanMode(t *testing.T) {
	conn := newTestDB(t)
	r, _, branchBase := rewrittenDoneStep(t, conn)
	writerBase := repinGit(t, r.worktree, "rev-parse", "HEAD~2")

	w, buf := bufWriter(false)
	err := runStepRepin(cmdWithDB(conn), []string{model.FormatStepID(r.implementID)}, w)
	testsupport.Must(t, err, "step repin: %v\n%s", err, buf.String())

	out := buf.String()
	for _, want := range []string{
		"Re-pinned " + model.FormatStepID(r.implementID) + " (done)",
		branchBase[:12] + ".." + r.executorSHA[:12] + " -> " + writerBase[:12] + ".." + r.executorSHA[:12],
		"supersedes ARTIFACT-",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
}
