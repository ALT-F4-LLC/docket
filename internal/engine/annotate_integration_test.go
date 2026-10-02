package engine

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The verified integration annotation: `step annotate --integrated-sha` names
// the commit the shared branch carries for a write-class step whose landed
// content diverged from its recorded commit. The engine verifies ancestry, re-
// records the step's issue.diff from that commit's patch, and the close then
// accepts the step with a `resolved` verdict — no --skip-integration-check,
// and every downstream packet binds to the resolved head.

const (
	resolvedSHA = "0123456789abcdef0123456789abcdef01234567"
	strangerSHA = "fedcba9876543210fedcba9876543210fedcba98"
)

// TestAnnotateIntegrationResolvesCloseAndRecord: after the annotation the
// close verifies clean with How "resolved", the step's newest issue.diff names
// the resolved head with the commit's own patch and supersedes the stale
// record, and the step row carries integrated_sha.
func TestAnnotateIntegrationResolvesCloseAndRecord(t *testing.T) {
	conn, e, runID := integrationFixture(t, "deadbeef00", "/worktrees/wf-implement")
	e.IsAncestorFn = func(_, sha string) (bool, bool) { return sha == resolvedSHA, true }
	e.PatchContainedFn = func(_, _ string) (bool, bool) { return false, true }
	e.CommitPatchFn = func(_, sha string, scope []string) (string, error) {
		return "resolved patch of " + sha + " over " + jsonString(scope), nil
	}
	stepID := stepIDByInstance(t, conn, "implement@0")

	// Premise: as recorded, the close refuses — a hand-resolved cherry-pick is
	// neither an ancestor nor patch-equivalent.
	if _, err := e.CloseDispatch(conn, runID, true, "", nowMS); err == nil {
		t.Fatal("premise: the close accepted the unintegrated recorded commit")
	}

	ann, err := e.AnnotateIntegration(conn, stepID, resolvedSHA, `{"note":"resolved by hand"}`, nowMS+1)
	testsupport.Must(t, err, "annotate --integrated-sha: %v", err)
	if ann.IntegratedSHA != resolvedSHA || ann.ResolvedFrom != "deadbeef00" {
		t.Errorf("annotation = %+v, want integrated %s resolved from deadbeef00", ann, resolvedSHA)
	}
	if ann.Repin == nil || ann.Repin.Unchanged || ann.Repin.Artifact == "" || ann.Repin.Supersedes == "" {
		t.Fatalf("repin = %+v, want a superseding artifact recorded", ann.Repin)
	}

	// The record: newest issue.diff names the resolved head, carries the
	// commit's patch, and marks what it resolved from.
	latest, err := stepLatestIssueDiff(conn, stepID)
	testsupport.Must(t, err, "stepLatestIssueDiff: %v", err)
	if latest == nil || "ARTIFACT-"+itoa(latest.ID) != ann.Repin.Artifact {
		t.Fatalf("newest issue.diff = %+v, want %s", latest, ann.Repin.Artifact)
	}
	if got := handBackHead(latest.Payload); got != resolvedSHA {
		t.Errorf("newest issue.diff head = %q, want %s", got, resolvedSHA)
	}
	if !strings.Contains(latest.Body, "resolved patch of "+resolvedSHA) {
		t.Errorf("newest issue.diff body = %q, want the commit's own patch", latest.Body)
	}
	var record struct {
		ResolvedFrom string `json:"resolved_from"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(latest.Payload), &record), "payload: %v", latest.Payload)
	if record.ResolvedFrom != "deadbeef00" {
		t.Errorf("payload resolved_from = %q, want deadbeef00", record.ResolvedFrom)
	}

	// The row: integrated_sha rides the step's metadata beside the caller's own.
	step := mustGetStep(t, conn, stepID)
	var meta map[string]any
	testsupport.Must(t, json.Unmarshal([]byte(step.Metadata), &meta), "metadata: %v", step.Metadata)
	if meta[MetadataKeyIntegratedSHA] != resolvedSHA || meta["note"] != "resolved by hand" {
		t.Errorf("metadata = %v, want integrated_sha and the caller's note", meta)
	}

	// The close: verified, and the verdict says HOW the head came to be the
	// branch's.
	openDispatch(t, conn, runID, 0, nowMS+2)
	outcome, err := e.CloseDispatch(conn, runID, true, "", nowMS+2)
	testsupport.Must(t, err, "CloseDispatch after the annotation: %v", err)
	if outcome.Integration == nil || outcome.Integration.Status != "verified" {
		t.Fatalf("Integration = %+v, want status verified", outcome.Integration)
	}
	if len(outcome.Integration.Checked) != 1 || outcome.Integration.Checked[0].SHA != resolvedSHA ||
		outcome.Integration.Checked[0].How != "resolved" {
		t.Errorf("Checked = %+v, want one resolved row for %s", outcome.Integration.Checked, resolvedSHA)
	}

	// Both facts are event-logged: the re-record with both shas, and the
	// annotation verbatim.
	if n := eventKindCount(t, conn, runID, EventIssueDiffRepinned); n != 1 {
		t.Errorf("%d issue-diff-repinned events, want 1", n)
	}
	if n := eventKindCount(t, conn, runID, EventStepAnnotated); n != 1 {
		t.Errorf("%d step-annotated events, want 1", n)
	}
}

func mustGetStep(t *testing.T, conn *sql.DB, stepID int) *db.Step {
	t.Helper()
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep %d: %v", stepID, err)
	return step
}

// TestAnnotateIntegrationRefusesAnUnverifiedSHA: the engine records only an
// integration it can verify. A sha the shared branch does not carry, an
// unanswerable ancestry question, and an engine with no probe wired all refuse
// BEFORE anything is written.
func TestAnnotateIntegrationRefusesAnUnverifiedSHA(t *testing.T) {
	conn, e, _ := integrationFixture(t, "deadbeef00", "/worktrees/wf-implement")
	stepID := stepIDByInstance(t, conn, "implement@0")
	before, err := stepLatestIssueDiff(conn, stepID)
	testsupport.Must(t, err, "stepLatestIssueDiff: %v", err)

	cases := []struct {
		name     string
		ancestry func(string, string) (bool, bool)
		want     string
	}{
		{"not an ancestor", func(_, _ string) (bool, bool) { return false, true }, "not an ancestor"},
		{"unanswerable", func(_, _ string) (bool, bool) { return false, false }, "could not establish"},
		{"no probe", nil, "no ancestry probe"},
	}
	for _, tc := range cases {
		e.IsAncestorFn = tc.ancestry
		_, err := e.AnnotateIntegration(conn, stepID, strangerSHA, "", nowMS+1)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a refusal naming %q", tc.name, err, tc.want)
		}
	}

	after, err := stepLatestIssueDiff(conn, stepID)
	testsupport.Must(t, err, "stepLatestIssueDiff: %v", err)
	if after.ID != before.ID {
		t.Errorf("a refused annotation re-recorded the issue.diff (%d -> %d)", before.ID, after.ID)
	}
	if step := mustGetStep(t, conn, stepID); strings.Contains(step.Metadata, MetadataKeyIntegratedSHA) {
		t.Errorf("a refused annotation wrote metadata: %s", step.Metadata)
	}
}

// TestAnnotateIntegrationValidatesTheSHA: the one accepted form is a full
// commit id — the recorded heads are full ids and the close keys on them.
func TestAnnotateIntegrationValidatesTheSHA(t *testing.T) {
	conn, e, _ := integrationFixture(t, "deadbeef00", "/worktrees/wf-implement")
	e.IsAncestorFn = func(_, _ string) (bool, bool) { return true, true }
	stepID := stepIDByInstance(t, conn, "implement@0")
	for _, bad := range []string{"", "deadbeef", "HEAD", strings.Repeat("g", 40)} {
		if _, err := e.AnnotateIntegration(conn, stepID, bad, "", nowMS); err == nil ||
			!strings.Contains(err.Error(), "full 40-hex") {
			t.Errorf("sha %q: err = %v, want the full-sha validation", bad, err)
		}
	}
}

// TestAnnotateIntegrationRefusesALiveStep: a live step's record lands under
// its holder's token; a side channel into it would bypass that authorization.
func TestAnnotateIntegrationRefusesALiveStep(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeClassWorkflowSrc), "integration-fixture.toml")
	issue := createIssue(t, conn, "integration fixture", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()
	e.IsAncestorFn = func(_, _ string) (bool, bool) { return true, true }
	stepID := stepIDByInstance(t, conn, "implement@0")
	claimInstance(t, conn, "implement@0", nowMS)

	_, err = e.AnnotateIntegration(conn, stepID, resolvedSHA, "", nowMS)
	if err == nil || !strings.Contains(err.Error(), "finished step") {
		t.Errorf("err = %v, want a refusal on the live step", err)
	}
}
