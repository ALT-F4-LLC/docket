package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The gap channel (DKT-72).
//
// Every rendered brief promised executors that a gap artifact was a success
// condition, and no workflow could emit one: artifact kinds came only from
// `emits`, nothing declared `gap`, and across every run zero gap artifacts
// existed while agents narrated their out-of-scope findings into bodies
// nothing re-reads. The channel is now always open on `step complete`.

// TestGapRecordsArtifactAndIssue is the headline: one --gap-file records a
// `gap` artifact beside the declared emit AND materializes a related backlog
// issue, atomically.
func TestGapRecordsArtifactAndIssue(t *testing.T) {
	conn := mustDB(t)
	_, issueID := activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	var gapIssues []string
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("the change summary"),
		Gaps: [][]byte{[]byte(
			"# The helper is duplicated in three packages\n\nFound while " +
				"implementing; out of this issue's scope.")},
		GapIssues: &gapIssues,
		NowMS:     nowMS,
	})
	testsupport.Must(t, err, "complete with a gap: %v", err)

	// The auxiliary artifact, beside the declared emit.
	var kinds []string
	rows, err := conn.Query(
		`SELECT kind FROM artifacts WHERE step_id = ? ORDER BY id`, stepID)
	testsupport.Must(t, err, "reading artifacts: %v", err)
	defer rows.Close()
	for rows.Next() {
		var kind string
		testsupport.Must(t, rows.Scan(&kind), "scanning")
		kinds = append(kinds, kind)
	}
	testsupport.Must(t, rows.Err(), "reading artifacts")
	// The saga records the declared emit and its own bookkeeping artifacts;
	// the gap joins them rather than replacing anything.
	if !slices.Contains(kinds, GapArtifactKind) {
		t.Fatalf("artifact kinds = %v, missing %q", kinds, GapArtifactKind)
	}
	if kinds[0] != "change-summary" {
		t.Fatalf("artifact kinds = %v; the declared emit must still record first", kinds)
	}

	// The materialized issue: backlog, titled from the gap's first line, and
	// related to the step's own issue so the next planning pass finds it.
	if len(gapIssues) != 1 {
		t.Fatalf("gap issues = %v, want exactly one ref", gapIssues)
	}
	gapID, err := model.ParseID(gapIssues[0])
	testsupport.Must(t, err, "parsing %s: %v", gapIssues[0], err)

	issue, err := db.GetIssue(conn, gapID)
	testsupport.Must(t, err, "reading the gap issue: %v", err)
	if issue.Status != model.StatusBacklog {
		t.Errorf("gap issue status = %s, want backlog", issue.Status)
	}
	if issue.Title != "The helper is duplicated in three packages" {
		t.Errorf("gap issue title = %q; the first non-empty line, heading "+
			"markers stripped, is the title", issue.Title)
	}
	if !strings.Contains(issue.Description, "out of this issue's scope") {
		t.Errorf("gap issue body dropped the content: %q", issue.Description)
	}

	relations, err := db.GetIssueRelations(conn, gapID)
	testsupport.Must(t, err, "reading relations: %v", err)
	related := false
	for _, rel := range relations {
		if rel.RelationType == model.RelationRelatesTo &&
			(rel.SourceIssueID == issueID || rel.TargetIssueID == issueID) {
			related = true
		}
	}
	if !related {
		t.Errorf("gap issue %s is not related to the step's issue; unrelated "+
			"residue is what the next planning pass misses", gapIssues[0])
	}
}

// TestGapOnlyCompletionParksForTheOperator is DKT-25: a completion whose only
// product is gap artifacts must not feed the issue's remaining pipeline. The
// measured failure: an unimplementable step recorded gap-only, its gates ran
// on the unchanged tree and passed, the step routed `pass`, and the full
// four-judge review fanout was scheduled over an empty change — followed by a
// fix-loop whose fix step was equally unimplementable.
func TestGapOnlyCompletionParksForTheOperator(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	var gapIssues []string
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("   \n"),
		Gaps: [][]byte{[]byte(
			"# The fix belongs in another repository\n\nNothing here to change.")},
		GapIssues: &gapIssues,
		NowMS:     nowMS,
	})
	testsupport.Must(t, err, "complete gap-only: %v", err)

	// The step is PARKED, not done: waiting-human, with the reason recorded.
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepWaitingHuman {
		t.Errorf("status = %q, want %q — a gap-only completion routed into the pipeline",
			step.Status, db.StepWaitingHuman)
	}
	if !strings.Contains(step.Routing, "gap") {
		t.Errorf("routing = %q; the recorded reason should say the product was gaps",
			step.Routing)
	}

	// The residue still lands: artifact and issue exactly as any gap records.
	if len(gapIssues) != 1 {
		t.Fatalf("gap issues = %v, want exactly one ref", gapIssues)
	}

	// And nothing downstream is ready — the park stops the lineage at its
	// source instead of feeding judges an empty change.
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		for _, s := range sched.Steps() {
			if s.StepName == "review" {
				if ready, _ := sched.Ready(s); ready {
					t.Errorf("%s is ready after a gap-only implement", s.Instance)
				}
			}
		}
	})
}

// TestGapBesideContentStillRoutes: the park is for gap-ONLY completions. A
// step that recorded real content beside its gaps routes exactly as before —
// gaps are residue, not a verdict on the work they rode along with.
func TestGapBesideContentStillRoutes(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("the change summary"),
		Gaps:     [][]byte{[]byte("# Out-of-scope residue\n\nDetails.")},
		NowMS:    nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)

	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepDone {
		t.Errorf("status = %q, want %q — content beside gaps must route normally",
			step.Status, db.StepDone)
	}
}

// TestGapRefusals: an empty gap records nothing anyone can pick up, and a
// refusal spends nothing — the step stays claimable-complete afterwards.
func TestGapRefusals(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("summary"),
		Gaps:     [][]byte{[]byte("   \n  ")},
		NowMS:    nowMS,
	})
	if err == nil {
		t.Fatal("an empty gap was accepted")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}

	// Nothing was written: the same token still completes.
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete after the refusal: %v", err)
}
