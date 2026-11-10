package engine

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// insertStepIssueDiff records an issue.diff on the step as a later stage
// would, returning the artifact id.
func insertStepIssueDiff(t *testing.T, conn *sql.DB, runID, stepID int, body, payload string) int {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	defer tx.Rollback()
	id, err := store.TxOn(tx).InsertArtifactTx(store.Artifact{
		RunID: runID, StepID: stepID, Kind: ArtifactKindIssueDiff,
		Body: body, Payload: payload, SHA256: workflow.SHA256([]byte(body)),
	}, nowMS+1)
	testsupport.Must(t, err, "insert issue.diff: %v", err)
	testsupport.Must(t, tx.Commit(), "commit: %v", err)
	return id
}

// TestRepinIssueDiffRangeRerecordsADoneStepsOwnRange: a done step whose
// newest issue.diff body spans the whole branch is re-recorded over its own
// range, round_base (else base) to head, as a superseding artifact, and the
// event names the range's start and the head.
func TestRepinIssueDiffRangeRerecordsADoneStepsOwnRange(t *testing.T) {
	const (
		head       = "1111111111111111111111111111111111111111"
		base       = "2222222222222222222222222222222222222222"
		roundBase  = "3333333333333333333333333333333333333333"
		branchBase = "4444444444444444444444444444444444444444"
	)
	for _, tc := range []struct {
		name, payload, wantFrom string
	}{
		{"round_base", `{"head":"` + head + `","base":"` + base + `","round_base":"` + roundBase + `"}`, roundBase},
		{"base without round_base", `{"head":"` + head + `","base":"` + base + `"}`, base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, e, runID := integrationFixture(t, head, "/worktrees/wf-implement")
			e.RangePatchFn = func(_, from, to string, _ []string) (string, error) {
				return "range patch " + from + ".." + to, nil
			}
			stepID := stepIDByInstance(t, conn, "implement@0")
			if s := mustGetStep(t, conn, stepID); s.Status != store.StepDone {
				t.Fatalf("premise: implement@0 is %s, want done", s.Status)
			}
			rewritten := insertStepIssueDiff(t, conn, runID, stepID,
				"whole-branch diff from "+branchBase, tc.payload)

			repin, err := e.RepinIssueDiffRange(conn, stepID, testBy, testConductorToken, nowMS+2)
			testsupport.Must(t, err, "RepinIssueDiffRange: %v", err)

			latest, err := stepLatestIssueDiff(conn, stepID)
			testsupport.Must(t, err, "stepLatestIssueDiff: %v", err)
			if want := "range patch " + tc.wantFrom + ".." + head; latest.Body != want {
				t.Errorf("re-recorded body = %q, want %q", latest.Body, want)
			}
			if latest.Supersedes == nil || *latest.Supersedes != rewritten {
				t.Errorf("new issue.diff supersedes %v, want the rewritten ARTIFACT-%d", latest.Supersedes, rewritten)
			}
			if got := handBackHead(latest.Payload); got != head {
				t.Errorf("re-recorded head = %q, want %s", got, head)
			}
			if repin.Artifact != "ARTIFACT-"+itoa(latest.ID) || repin.Supersedes != "ARTIFACT-"+itoa(rewritten) {
				t.Errorf("repin = %+v, want ARTIFACT-%d superseding ARTIFACT-%d", repin, latest.ID, rewritten)
			}

			var event map[string]any
			data := lastEventData(t, conn, runID, EventIssueDiffRepinned)
			testsupport.Must(t, json.Unmarshal([]byte(data), &event), "event data %s", data)
			for key, want := range map[string]string{
				"range_from": tc.wantFrom, "from_sha": head, "to_sha": head,
				"actor": testBy.Actor, "resolution": ResolutionOwnRange,
			} {
				if event[key] != want {
					t.Errorf("event %s = %v, want %s (data %s)", key, event[key], want, data)
				}
			}
		})
	}
}

// TestRepinIssueDiffRangeRefusesALiveStep: a step that is not done is
// re-pinned through `step resolve --worktree`, so nothing is recorded here.
func TestRepinIssueDiffRangeRefusesALiveStep(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeClassWorkflowSrc), "integration-fixture.toml")
	run := startRun(t, conn, createIssue(t, conn, "live", "a body", "task", nil))
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()
	e.RangePatchFn = func(_, _, _ string, _ []string) (string, error) { return "patch", nil }

	_, err = e.RepinIssueDiffRange(conn, stepIDByInstance(t, conn, "implement@0"), testBy, testConductorToken, nowMS)
	if err == nil || !strings.Contains(err.Error(), "not done") {
		t.Fatalf("re-pin of a pending step = %v, want a refusal naming its status", err)
	}
	if n := eventKindCount(t, conn, run.ID, EventIssueDiffRepinned); n != 0 {
		t.Errorf("%d issue-diff-repinned events after a refusal, want none", n)
	}
}
