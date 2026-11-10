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

const (
	repinHead       = "1111111111111111111111111111111111111111"
	repinWriterBase = "2222222222222222222222222222222222222222"
	repinRoundBase  = "3333333333333333333333333333333333333333"
	repinBranchBase = "4444444444444444444444444444444444444444"
	repinWorktree   = "/worktrees/wf-implement"
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

func pinRunCommit(t *testing.T, conn *sql.DB, runID int, sha string) {
	t.Helper()
	_, err := conn.Exec(`UPDATE runs SET commit_sha = ? WHERE id = ?`, sha, runID)
	testsupport.Must(t, err, "pin commit_sha: %v", err)
}

// doneWriter is a done implement@0 that handed back repinHead from
// repinWorktree. With writerPayload empty, its own completion records its
// issue.diff, cut from writerBase (the run's pinned commit standing in for a
// fork point the fixture cannot resolve; "" records no base). Otherwise the
// completion records no diff and writerPayload is recorded as the writer's
// round record, the shape appendRoundDelta writes at a loop re-entry.
func doneWriter(t *testing.T, writerBase, writerPayload string) (*sql.DB, *Engine, int, int) {
	t.Helper()
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeClassWorkflowSrc), "integration-fixture.toml")
	run := startRun(t, conn, createIssue(t, conn, "repin", "a body", "task", nil))
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	pinRunCommit(t, conn, run.ID, writerBase)

	e := testEngine()
	e.HeadFn = func(string) string { return repinHead }
	diffFromBase := func(_, base string, _ []string) (string, error) { return "diff from " + base, nil }
	e.DiffFn = diffFromBase
	if writerPayload != "" {
		e.DiffFn = nil
	}
	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	testsupport.Must(t, e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"),
		WorkDir: repinWorktree, NowMS: nowMS,
	}), "complete implement")
	if s := mustGetStep(t, conn, stepID); s.Status != store.StepDone {
		t.Fatalf("premise: implement@0 is %s, want done", s.Status)
	}
	if writerPayload != "" {
		insertStepIssueDiff(t, conn, run.ID, stepID, "the writer's round diff", writerPayload)
	}
	e.DiffFn = diffFromBase
	e.RangePatchFn = func(_, from, to string, _ []string) (string, error) {
		return "range patch " + from + ".." + to, nil
	}
	return conn, e, run.ID, stepID
}

// rewriteFromBranchBase records the issue.diff a later routing stage cuts for
// the same head once the step's base resolves to the whole-branch base, as a
// rerun-gates does after the worktree is swept: the diff stage's own body and
// payload, inserted without a supersession pointer as runRoutingStage does.
func rewriteFromBranchBase(t *testing.T, conn *sql.DB, e *Engine, runID, stepID int) int {
	t.Helper()
	pinRunCommit(t, conn, runID, repinBranchBase)
	body, payload, err := e.computeIssueDiff(conn, mustGetStep(t, conn, stepID), true)
	testsupport.Must(t, err, "computeIssueDiff: %v", err)
	if !strings.Contains(body, repinBranchBase) || payloadField(t, payload, "base") != repinBranchBase ||
		handBackHead(payload) != repinHead {
		t.Fatalf("premise: the rewrite is %q / %s, want a whole-branch body naming base %s at head %s",
			body, payload, repinBranchBase, repinHead)
	}
	return insertStepIssueDiff(t, conn, runID, stepID, body, payload)
}

func payloadField(t *testing.T, payload, key string) any {
	t.Helper()
	var fields map[string]any
	testsupport.Must(t, json.Unmarshal([]byte(payload), &fields), "payload %s", payload)
	return fields[key]
}

func issueDiffRepinnedEvent(t *testing.T, conn *sql.DB, runID int) map[string]any {
	t.Helper()
	var event map[string]any
	data := lastEventData(t, conn, runID, EventIssueDiffRepinned)
	testsupport.Must(t, json.Unmarshal([]byte(data), &event), "event data %s", data)
	return event
}

// TestRepinIssueDiffRangeRerecordsARewrittenDoneStepOverItsOwnRange: a done
// step whose newest issue.diff was re-cut from the whole-branch base is
// re-recorded over the range its writer recorded for that head, round_base
// (else base) to head, as an artifact superseding the rewrite, and the event
// logs the head and the range's start. A second re-pin finds nothing to do.
func TestRepinIssueDiffRangeRerecordsARewrittenDoneStepOverItsOwnRange(t *testing.T) {
	for _, tc := range []struct {
		name, writerBase, writerPayload, wantFrom string
	}{
		{"base without round_base", repinWriterBase, "", repinWriterBase},
		{"round_base", "", `{"worktree":"` + repinWorktree + `","head":"` + repinHead +
			`","base":"` + repinWriterBase + `","round_base":"` + repinRoundBase + `"}`, repinRoundBase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, e, runID, stepID := doneWriter(t, tc.writerBase, tc.writerPayload)
			rewritten := rewriteFromBranchBase(t, conn, e, runID, stepID)

			repin, err := e.RepinIssueDiffRange(conn, stepID, testBy, testConductorToken, nowMS+2)
			testsupport.Must(t, err, "RepinIssueDiffRange: %v", err)

			latest, err := stepLatestIssueDiff(conn, stepID)
			testsupport.Must(t, err, "stepLatestIssueDiff: %v", err)
			if want := "range patch " + tc.wantFrom + ".." + repinHead; latest.Body != want {
				t.Errorf("re-recorded body = %q, want %q", latest.Body, want)
			}
			if latest.Supersedes == nil || *latest.Supersedes != rewritten {
				t.Errorf("new issue.diff supersedes %v, want the rewritten ARTIFACT-%d", latest.Supersedes, rewritten)
			}
			for key, want := range map[string]any{
				"head": repinHead, "base": repinWriterBase, "range_from": tc.wantFrom, "worktree": repinWorktree,
			} {
				if got := payloadField(t, latest.Payload, key); got != want {
					t.Errorf("re-recorded payload %s = %v, want %s (payload %s)", key, got, want, latest.Payload)
				}
			}
			if repin.Artifact != "ARTIFACT-"+itoa(latest.ID) || repin.Supersedes != "ARTIFACT-"+itoa(rewritten) {
				t.Errorf("repin = %+v, want ARTIFACT-%d superseding ARTIFACT-%d", repin, latest.ID, rewritten)
			}

			event := issueDiffRepinnedEvent(t, conn, runID)
			for key, want := range map[string]string{
				"range_from": tc.wantFrom, "from_sha": repinHead, "to_sha": repinHead,
				"artifact": repin.Artifact, "supersedes": repin.Supersedes,
				"actor": testBy.Actor, "resolution": ResolutionOwnRange,
			} {
				if event[key] != want {
					t.Errorf("event %s = %v, want %s (event %v)", key, event[key], want, event)
				}
			}

			again, err := e.RepinIssueDiffRange(conn, stepID, testBy, testConductorToken, nowMS+3)
			testsupport.Must(t, err, "second RepinIssueDiffRange: %v", err)
			if !again.Unchanged || again.Artifact != "" {
				t.Errorf("second re-pin = %+v, want Unchanged with nothing recorded", again)
			}
			if n := eventKindCount(t, conn, runID, EventIssueDiffRepinned); n != 1 {
				t.Errorf("%d issue-diff-repinned events after an unchanged re-pin, want 1", n)
			}
			if newest, _ := stepLatestIssueDiff(conn, stepID); newest.ID != latest.ID {
				t.Errorf("unchanged re-pin recorded ARTIFACT-%d, want none", newest.ID)
			}
		})
	}
}

// TestRepinIssueDiffRangeKeepsAnIntegrationAnnotationsRange: an annotation
// moved the head to the integrated sha and named its range in range_from, and
// a later record re-cut that head from the whole-branch base. The re-pin
// re-records the annotation's range and carries its writer chain forward.
func TestRepinIssueDiffRangeKeepsAnIntegrationAnnotationsRange(t *testing.T) {
	const (
		integrated = "5555555555555555555555555555555555555555"
		rangeFrom  = "6666666666666666666666666666666666666666"
	)
	conn, e, runID, stepID := doneWriter(t, repinWriterBase, "")
	// The payload AnnotateIntegration records (annotate_integration.go).
	insertStepIssueDiff(t, conn, runID, stepID, "range patch "+rangeFrom+".."+integrated,
		`{"head":"`+integrated+`","resolved_from":"`+repinHead+`","writer_head":"`+repinHead+
			`","writer_base":"`+repinWriterBase+`","range_from":"`+rangeFrom+`"}`)
	rewritten := insertStepIssueDiff(t, conn, runID, stepID, "diff from "+repinBranchBase,
		`{"head":"`+integrated+`","base":"`+repinBranchBase+`"}`)

	_, err := e.RepinIssueDiffRange(conn, stepID, testBy, testConductorToken, nowMS+2)
	testsupport.Must(t, err, "RepinIssueDiffRange: %v", err)

	latest, err := stepLatestIssueDiff(conn, stepID)
	testsupport.Must(t, err, "stepLatestIssueDiff: %v", err)
	if want := "range patch " + rangeFrom + ".." + integrated; latest.Body != want {
		t.Errorf("re-recorded body = %q, want %q", latest.Body, want)
	}
	if latest.Supersedes == nil || *latest.Supersedes != rewritten {
		t.Errorf("new issue.diff supersedes %v, want ARTIFACT-%d", latest.Supersedes, rewritten)
	}
	for key, want := range map[string]any{
		"head": integrated, "range_from": rangeFrom, "resolved_from": repinHead,
		"writer_head": repinHead, "writer_base": repinWriterBase, "base": nil,
	} {
		if got := payloadField(t, latest.Payload, key); got != want {
			t.Errorf("re-recorded payload %s = %v, want %v (payload %s)", key, got, want, latest.Payload)
		}
	}
}

// TestRepinIssueDiffRangeRefusals: each refusal records nothing, and a step
// whose own record of its head names no bound is refused rather than re-cut
// from a later record's whole-branch base.
func TestRepinIssueDiffRangeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, writerBase, writerPayload, token, want string
		by                                           Attribution
		rewrite, noRangePatch                        bool
	}{
		{name: "wrong conductor token", writerBase: repinWriterBase, token: "not-the-token", by: testBy, rewrite: true,
			want: "conductor capability"},
		{name: "no attribution", writerBase: repinWriterBase, token: testConductorToken, rewrite: true,
			want: "unattributed"},
		{name: "own record names no base", token: testConductorToken, by: testBy, rewrite: true, want: "names no"},
		{name: "head is not a commit id", token: testConductorToken, by: testBy,
			writerPayload: `{"head":"--output=x","base":"` + repinWriterBase + `"}`, want: "commit ids"},
		{name: "no range patch wired", writerBase: repinWriterBase, token: testConductorToken, by: testBy,
			rewrite: true, noRangePatch: true, want: "no range patch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, e, runID, stepID := doneWriter(t, tc.writerBase, tc.writerPayload)
			if tc.writerBase == "" && tc.writerPayload == "" {
				first, err := stepLatestIssueDiff(conn, stepID)
				testsupport.Must(t, err, "stepLatestIssueDiff: %v", err)
				if handBackHead(first.Payload) != repinHead || payloadField(t, first.Payload, "base") != nil {
					t.Fatalf("premise: writer record %s, want head %s and no base", first.Payload, repinHead)
				}
			}
			if tc.rewrite {
				rewriteFromBranchBase(t, conn, e, runID, stepID)
			}
			if tc.noRangePatch {
				e.RangePatchFn = nil
			}
			before, err := stepLatestIssueDiff(conn, stepID)
			testsupport.Must(t, err, "stepLatestIssueDiff: %v", err)

			_, err = e.RepinIssueDiffRange(conn, stepID, tc.by, tc.token, nowMS+2)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RepinIssueDiffRange = %v, want a refusal containing %q", err, tc.want)
			}
			if after, _ := stepLatestIssueDiff(conn, stepID); after.ID != before.ID {
				t.Errorf("a refusal recorded ARTIFACT-%d", after.ID)
			}
			if n := eventKindCount(t, conn, runID, EventIssueDiffRepinned); n != 0 {
				t.Errorf("%d issue-diff-repinned events after a refusal, want none", n)
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
