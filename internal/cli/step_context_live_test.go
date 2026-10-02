package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-1054 at the CLI boundary. internal/engine/dkt1054_test.go proves the
// RESOLUTION — a claimed step reads back the bindings its claim recorded, a
// pending one reads live; what this file asserts is the verb: `step context`
// replays, and `--live` is the switch back to the current resolution.

// contextEnvelope runs `step context --json` (with or without --live) on one
// step and decodes the fields under test.
func contextEnvelope(t *testing.T, conn *sql.DB, stepID int, live bool) (targetSHA string, inputs []string) {
	t.Helper()
	cmd := cmdWithDB(conn)
	cmd.Flags().Bool("meta", false, "")
	cmd.Flags().Bool("live", live, "")
	w, buf := bufWriter(true)
	err := runStepContext(cmd, []string{model.FormatStepID(stepID)}, w)
	testsupport.Must(t, err, "step context: %v\n%s", err, buf.String())

	var envelope struct {
		Data struct {
			Context struct {
				TargetSHA string `json:"target_sha"`
				Inputs    []struct {
					Artifact     string `json:"artifact"`
					Kind         string `json:"kind"`
					ProducerStep string `json:"producer_step"`
				} `json:"inputs"`
			} `json:"context"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	for _, in := range envelope.Data.Context.Inputs {
		inputs = append(inputs, in.Kind+"/"+in.ProducerStep+"/"+in.Artifact)
	}
	return envelope.Data.Context.TargetSHA, inputs
}

// TestStepContextReplaysTheClaimedBindingsUnlessLive: the judge is seated on
// the executor's commit; the executor's diff is then superseded at the same
// ordinal (a re-pin's row). The verb reports what the judge was handed; --live
// reports the superseding record.
func TestStepContextReplaysTheClaimedBindingsUnlessLive(t *testing.T) {
	conn := newTestDB(t)
	implementID, reviewID := showTargetRun(t, conn, "cafe1234cafe1234", "/worktrees/issue-under-test")

	claim, err := engine.ClaimStep(conn, reviewID, engine.ClaimOptions{Owner: "judge", NowMS: model.NowMS()})
	testsupport.Must(t, err, "claim review: %v", err)
	handed := claim.Context.Inputs[0].Artifact

	var runID, prior int
	err = conn.QueryRow(`SELECT run_id FROM steps WHERE id = ?`, implementID).Scan(&runID)
	testsupport.Must(t, err, "reading the run: %v", err)
	err = conn.QueryRow(
		`SELECT MAX(id) FROM artifacts WHERE step_id = ? AND kind = ?`,
		implementID, engine.ArtifactKindIssueDiff).Scan(&prior)
	testsupport.Must(t, err, "reading the recorded diff: %v", err)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	superseding, err := db.InsertArtifactTx(tx, db.Artifact{
		RunID: runID, StepID: implementID, Kind: engine.ArtifactKindIssueDiff,
		Body: "the re-pinned diff", Payload: `{"head":"feed5678feed5678","worktree":"/worktrees/repinned"}`,
		SHA256: workflow.SHA256([]byte("the re-pinned diff")), Supersedes: &prior,
	}, model.NowMS())
	if err != nil {
		tx.Rollback()
		t.Fatalf("InsertArtifactTx: %v", err)
	}
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)

	sha, inputs := contextEnvelope(t, conn, reviewID, false)
	if sha != "cafe1234cafe1234" {
		t.Errorf("step context target_sha = %q, want the handed-over cafe1234cafe1234", sha)
	}
	if len(inputs) != 1 || inputs[0] != "issue.diff/implement@0/"+handed {
		t.Errorf("step context inputs = %v, want the handed-over [issue.diff/implement@0/%s]", inputs, handed)
	}

	liveSHA, liveInputs := contextEnvelope(t, conn, reviewID, true)
	if liveSHA != "feed5678feed5678" {
		t.Errorf("step context --live target_sha = %q, want the superseding feed5678feed5678", liveSHA)
	}
	want := fmt.Sprintf("issue.diff/implement@0/ARTIFACT-%d", superseding)
	if len(liveInputs) != 1 || liveInputs[0] != want {
		t.Errorf("step context --live inputs = %v, want [%s]", liveInputs, want)
	}
}
