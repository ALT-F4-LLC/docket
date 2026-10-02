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

// TestGapHeaderRanksTheFiledIssue is DKT-1082: drain-highs filed every
// high-severity cluster it drained at priority `none`, unranked and unlabelled
// — invisible to every priority-ordered planning pass, while the severity was
// already written into the body the step handed over. A gap body's leading
// header block now ranks the issue it materializes, and the body still records
// verbatim.
func TestGapHeaderRanksTheFiledIssue(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	// The shape drain-highs writes: title, the `Home:` convention line the
	// contract already puts on line two, then the severity.
	body := "The retry loop swallows the cancel\n" +
		"Home: THIS repository\n" +
		"Severity: high\n" +
		"Labels: review-drain, reliability\n" +
		"Kind: bug\n" +
		"\n" +
		"Cluster SYN-1, round 1.\n"

	var gapIssues []string
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token:     claim.Token,
		Artifact:  []byte("the change summary"),
		Gaps:      [][]byte{[]byte(body)},
		GapIssues: &gapIssues,
		NowMS:     nowMS,
	})
	testsupport.Must(t, err, "complete with a ranked gap: %v", err)

	if len(gapIssues) != 1 {
		t.Fatalf("gap issues = %v, want exactly one ref", gapIssues)
	}
	gapID, err := model.ParseID(gapIssues[0])
	testsupport.Must(t, err, "parsing %s: %v", gapIssues[0], err)

	issue, err := db.GetIssue(conn, gapID)
	testsupport.Must(t, err, "reading the gap issue: %v", err)
	if issue.Priority != model.PriorityHigh {
		t.Errorf("gap issue priority = %q, want %q — a drained high that lands "+
			"unranked is invisible to priority-ordered planning",
			issue.Priority, model.PriorityHigh)
	}
	if issue.Kind != model.IssueKindBug {
		t.Errorf("gap issue kind = %q, want %q", issue.Kind, model.IssueKindBug)
	}
	if issue.Title != "The retry loop swallows the cancel" {
		t.Errorf("gap issue title = %q; the header must not displace the title",
			issue.Title)
	}
	// The body is stored VERBATIM: the header is read, never stripped, so the
	// artifact and the issue keep saying the same thing.
	if issue.Description != body {
		t.Errorf("gap issue body = %q, want the gap verbatim %q",
			issue.Description, body)
	}

	labels, err := db.GetIssueLabels(conn, gapID)
	testsupport.Must(t, err, "reading labels: %v", err)
	for _, want := range []string{"reliability", "review-drain"} {
		if !slices.Contains(labels, want) {
			t.Errorf("gap issue labels = %v, missing %q", labels, want)
		}
	}
}

// TestGapWithoutHeaderMaterializesAsBefore pins the other half of DKT-1082:
// the header block is OPTIONAL, and a gap that declares nothing files exactly
// the row it always filed.
func TestGapWithoutHeaderMaterializesAsBefore(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	var gapIssues []string
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("the change summary"),
		Gaps: [][]byte{[]byte(
			"# The helper is duplicated in three packages\n\nSeverity: high\n")},
		GapIssues: &gapIssues,
		NowMS:     nowMS,
	})
	testsupport.Must(t, err, "complete with a headerless gap: %v", err)

	if len(gapIssues) != 1 {
		t.Fatalf("gap issues = %v, want exactly one ref", gapIssues)
	}
	gapID, err := model.ParseID(gapIssues[0])
	testsupport.Must(t, err, "parsing %s: %v", gapIssues[0], err)

	issue, err := db.GetIssue(conn, gapID)
	testsupport.Must(t, err, "reading the gap issue: %v", err)
	// The blank line after the title ENDS the header block: a `Severity:` line
	// in the prose below is body, not a declaration.
	if issue.Priority != model.PriorityNone {
		t.Errorf("gap issue priority = %q, want %q — a gap that declared no "+
			"header must materialize exactly as before", issue.Priority, model.PriorityNone)
	}
	if issue.Kind != model.IssueKindTask {
		t.Errorf("gap issue kind = %q, want %q", issue.Kind, model.IssueKindTask)
	}
	labels, err := db.GetIssueLabels(conn, gapID)
	testsupport.Must(t, err, "reading labels: %v", err)
	if len(labels) != 0 {
		t.Errorf("gap issue labels = %v, want none", labels)
	}
}

// TestParseGapHeader covers the mapping table and the block's boundaries
// directly, where the round trip through a completion cannot reach every case.
func TestParseGapHeader(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		priority model.Priority
		kind     model.IssueKind
		labels   []string
	}{
		{
			name:     "blocker maps to critical",
			body:     "title\nSeverity: blocker\n",
			priority: model.PriorityCritical,
		},
		{
			name:     "medium maps to medium",
			body:     "title\nSeverity: MEDIUM\n",
			priority: model.PriorityMedium,
		},
		{
			name: "low sits below every gate and ranks none",
			body: "title\nSeverity: low\n",
		},
		{
			name: "an unknown severity is ignored, not refused",
			body: "title\nSeverity: spicy\n",
		},
		{
			name:     "an explicit priority wins over the mapped severity",
			body:     "title\nSeverity: high\nPriority: low\n",
			priority: model.PriorityLow,
		},
		{
			name:     "and wins in either order",
			body:     "title\nPriority: low\nSeverity: high\n",
			priority: model.PriorityLow,
		},
		{
			name: "prose ends the block",
			body: "title\nFound while implementing.\nSeverity: high\n",
		},
		{
			name:     "an unknown key does not end the block",
			body:     "title\nHome: THIS repository\nSeverity: high\n",
			priority: model.PriorityHigh,
		},
		{
			name:   "labels split, trim and dedupe",
			body:   "title\nLabels: a , b,, a ,c\n",
			labels: []string{"a", "b", "c"},
		},
		{
			name: "an unknown kind is ignored",
			body: "title\nKind: catastrophe\n",
		},
		{
			name: "a header before the title is body, not a declaration",
			body: "Severity: high\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseGapHeader([]byte(tc.body))
			if got.Priority != tc.priority {
				t.Errorf("priority = %q, want %q", got.Priority, tc.priority)
			}
			if got.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", got.Kind, tc.kind)
			}
			if !slices.Equal(got.Labels, tc.labels) {
				t.Errorf("labels = %v, want %v", got.Labels, tc.labels)
			}
		})
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
