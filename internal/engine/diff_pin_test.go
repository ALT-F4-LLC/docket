package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// issue.diff pinning (DKT-75, amending §6.7.1 D1).
//
// The diff used to be recomputed at the completion of EVERY executor step,
// against the live tree at that step's own record time. For read-shaped
// fanout siblings that made the input wrong in both directions: a diff taken
// after the change was committed came out empty, and one taken beside a
// sibling's in-flight probe carried the probe. The recompute now happens only
// at steps that HOLD THE TREE — the declaration scope exclusion already
// consults — and every non-holding consumer resolves to the artifact the last
// holding step recorded: the reviewed object, pinned.

const diffPinWorkflow = `
[pipeline]
name = "diffpin"
version = 1
[[step]]
name = "implement"
after = []
executor = "author"
emits = "change-summary"
[[step]]
name = "review"
holds_tree = false
after = ["implement"]
executor = "judge"
emits = "findings"
inputs = ["issue.diff"]
`

func TestDiffPinsToTheTreeHoldingStep(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(diffPinWorkflow), "diffpin.toml")
	issue := createIssue(t, conn, "pin the diff", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// The tree as it reads while the holding step finishes.
	e := testEngine()
	e.DiffFn = func(string, string, []string) (string, error) { return "the reviewed change", nil }
	claimAndComplete(t, conn, e, "implement@0", "summary", "")

	// The live tree MOVES — a sibling probe, a commit, anything — before the
	// non-holding step completes. Nothing downstream may see this.
	e.DiffFn = func(string, string, []string) (string, error) { return "the live tree moved", nil }

	claim, err := ClaimStep(conn, stepIDByInstance(t, conn, "review@0"),
		ClaimOptions{Owner: "judge", NowMS: nowMS})
	testsupport.Must(t, err, "claim review: %v", err)

	// The consumer's input is the HOLDING step's recorded diff.
	found := false
	for _, input := range claim.Context.Inputs {
		if input.Kind != ArtifactKindIssueDiff {
			continue
		}
		found = true
		if input.Body != "the reviewed change" {
			t.Errorf("review's issue.diff = %q, want the artifact implement "+
				"recorded, never a live re-read", input.Body)
		}
	}
	if !found {
		t.Fatal("review's context carries no issue.diff input")
	}

	// And the non-holding step's own completion records NO new diff artifact.
	err = e.CompleteStep(conn, stepIDByInstance(t, conn, "review@0"), CompleteOptions{
		Token: claim.Token, Artifact: []byte("findings"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete review: %v", err)

	var diffs int
	var bodies []string
	rows, err := conn.Query(
		`SELECT body FROM artifacts WHERE kind = ?`, ArtifactKindIssueDiff)
	testsupport.Must(t, err, "reading diff artifacts: %v", err)
	defer rows.Close()
	for rows.Next() {
		var body string
		testsupport.Must(t, rows.Scan(&body), "scanning")
		diffs++
		bodies = append(bodies, body)
	}
	testsupport.Must(t, rows.Err(), "reading diff artifacts")
	if diffs != 1 || !strings.Contains(bodies[0], "the reviewed change") {
		t.Errorf("diff artifacts = %d %v; only the tree-holding step records "+
			"one, and it carries the reviewed object", diffs, bodies)
	}
}

// TestDiffStillRecordsAtEveryHoldingStep: a definition that declares nothing
// keeps D1's original behavior — holds_tree defaults to true, so every
// executor step still records. Dormancy for existing repos.
func TestDiffStillRecordsAtEveryHoldingStep(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "summary", "")

	var diffs int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM artifacts WHERE kind = ? AND run_id = ?`,
		ArtifactKindIssueDiff, run.ID).Scan(&diffs)
	testsupport.Must(t, err, "counting diff artifacts: %v", err)
	if diffs != 1 {
		t.Errorf("diff artifacts after an undeclared holder completed = %d, "+
			"want 1: nil holds_tree still records", diffs)
	}
}
