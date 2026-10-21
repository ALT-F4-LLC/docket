package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-4272: a step records an issue comment the engine posts, so an executor
// that cannot run `docket issue comment add` still meets an AC that asks for one.

// commentsBy returns the bodies of an issue's comments authored by author.
func commentsBy(t *testing.T, conn *sql.DB, issueID int, author string) []string {
	t.Helper()
	rows, err := conn.Query(
		`SELECT body FROM comments WHERE issue_id = ? AND author = ? ORDER BY id`,
		issueID, author)
	testsupport.Must(t, err, "reading comments: %v", err)
	defer rows.Close()
	var bodies []string
	for rows.Next() {
		var body string
		testsupport.Must(t, rows.Scan(&body), "scanning")
		bodies = append(bodies, body)
	}
	testsupport.Must(t, rows.Err(), "reading comments")
	return bodies
}

func claimedImplement(t *testing.T, conn *sql.DB) (stepID, issueID int, token string) {
	t.Helper()
	_, issueID = activatedRun(t, conn)
	stepID = stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	return stepID, issueID, claim.Token
}

func TestRecordedIssueCommentPostsExactlyOnceOnTheStepsIssue(t *testing.T) {
	conn := mustDB(t)
	stepID, issueID, token := claimedImplement(t, conn)
	other := createIssue(t, conn, "an unrelated issue", "body", "task", nil)

	testsupport.Must(t, testEngine().CompleteStep(conn, stepID, CompleteOptions{
		Token: token, Artifact: []byte("summary"), NowMS: nowMS,
		Comments: [][]byte{[]byte("AC3: the helper is covered at internal/x_test.go")},
	}), "complete with a comment")

	author := model.FormatStepID(stepID)
	got := commentsBy(t, conn, issueID, author)
	if len(got) != 1 || got[0] != "AC3: the helper is covered at internal/x_test.go" {
		t.Fatalf("comments by %s on the step's issue = %q; want exactly the recorded body", author, got)
	}
	if n := len(commentsBy(t, conn, other, author)); n != 0 {
		t.Errorf("an unrelated issue received %d step comments", n)
	}
	var artifacts int
	testsupport.Must(t, conn.QueryRow(
		`SELECT COUNT(*) FROM artifacts WHERE step_id = ? AND kind = ?`,
		stepID, IssueCommentArtifactKind).Scan(&artifacts), "counting artifacts")
	if artifacts != 1 {
		t.Errorf("%d %s artifacts recorded; want 1", artifacts, IssueCommentArtifactKind)
	}
}

func TestRecordingTheSameArtifactAgainPostsNothing(t *testing.T) {
	conn := mustDB(t)
	stepID, issueID, token := claimedImplement(t, conn)
	e := testEngine()
	opts := CompleteOptions{
		Token: token, Artifact: []byte("summary"), NowMS: nowMS,
		Comments: [][]byte{[]byte("posted once")},
	}
	testsupport.Must(t, e.CompleteStep(conn, stepID, opts), "complete")

	// A second recording of the step is refused: its lease retired at the first.
	if err := e.CompleteStep(conn, stepID, opts); err == nil {
		t.Fatal("a second recording of the step was accepted")
	}

	// And the posting itself is idempotent per artifact: the same artifact
	// presented again, as a resumed saga would, adds no comment.
	var artifactID int
	testsupport.Must(t, conn.QueryRow(
		`SELECT id FROM artifacts WHERE step_id = ? AND kind = ?`,
		stepID, IssueCommentArtifactKind).Scan(&artifactID), "finding the artifact")
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "reading the step")
	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin")
	testsupport.Must(t, postStepCommentTx(tx, step, artifactID, []byte("posted once")), "re-post")
	testsupport.Must(t, tx.Commit(), "commit")

	if got := commentsBy(t, conn, issueID, model.FormatStepID(stepID)); len(got) != 1 {
		t.Fatalf("%d comments after re-posting the same artifact; want 1", len(got))
	}
}

func TestUnusableCommentsRefuseAndWriteNothing(t *testing.T) {
	big := strings.Repeat("x", StepCommentMaxBytes+1)
	five := [][]byte{[]byte("1"), []byte("2"), []byte("3"), []byte("4"), []byte("5")}
	for name, comments := range map[string][][]byte{
		"empty":    {[]byte("  \n")},
		"too big":  {[]byte(big)},
		"too many": five,
	} {
		t.Run(name, func(t *testing.T) {
			conn := mustDB(t)
			stepID, issueID, token := claimedImplement(t, conn)
			err := testEngine().CompleteStep(conn, stepID, CompleteOptions{
				Token: token, Artifact: []byte("summary"), NowMS: nowMS, Comments: comments,
			})
			if err == nil {
				t.Fatal("an unusable comment set was accepted")
			}
			if got := commentsBy(t, conn, issueID, model.FormatStepID(stepID)); len(got) != 0 {
				t.Errorf("a refused recording posted %d comments", len(got))
			}
			// The refusal spent nothing: the same lease still records.
			testsupport.Must(t, testEngine().CompleteStep(conn, stepID, CompleteOptions{
				Token: token, Artifact: []byte("summary"), NowMS: nowMS,
			}), "recording after a refused one")
		})
	}
}
