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
	if err := postStepCommentTx(tx, step, artifactID, []byte("posted once")); err == nil {
		t.Error("re-posting the same artifact was accepted; it must refuse, not silently skip")
	}
	testsupport.Must(t, tx.Rollback(), "rollback")

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
		// An escape sequence rewrites a terminal, a bidi override reorders
		// text, and a body of only zero-width characters would pass the empty
		// check: none is printable prose.
		"terminal escape": {[]byte("ok\x1b[2K\rnothing happened")},
		"carriage return": {[]byte("line\rreplaced")},
		"bidi override":   {[]byte("approved ‮gnorw")},
		"zero width only": {[]byte("​​")},
		"c1 control":      {[]byte("next\u0085line")},
		"invalid utf-8":   {[]byte("bad \xff byte")},
		"line separator":  {[]byte("a\u2028b")},
		"hangul filler":   {[]byte("\u3164\u3164")},
		"private use":     {[]byte("a\ue000b")},
		"unassigned":      {[]byte("a\u0378b")},
		"variation sel":   {[]byte("a\ufe00b")},
		"lone emoji sel":  {[]byte("\ufe0f")},
		"emoji sel space": {[]byte("a \ufe0f")},
		"mongolian sel":   {[]byte("a\u180bb")},
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

// `issue comment add --idempotency-key` cannot reach the key a step's comment
// uses: it lives in its own scope, so squatting on the same string suppresses
// nothing.
func TestAnIssueCommentKeyCannotSuppressAStepComment(t *testing.T) {
	conn := mustDB(t)
	stepID, issueID, token := claimedImplement(t, conn)

	var next int
	testsupport.Must(t, conn.QueryRow(
		`SELECT COALESCE(MAX(id), 0) + 1 FROM artifacts`).Scan(&next), "next artifact id")
	for id := next; id < next+4; id++ {
		_, err := db.CreateCommentIdempotent(conn, &model.Comment{
			IssueID: issueID, Body: "squatter", Author: "someone",
		}, stepCommentKey(id))
		testsupport.Must(t, err, "seeding the key: %v", err)
	}

	testsupport.Must(t, testEngine().CompleteStep(conn, stepID, CompleteOptions{
		Token: token, Artifact: []byte("summary"), NowMS: nowMS,
		Comments: [][]byte{[]byte("the real comment")},
	}), "complete")
	got := commentsBy(t, conn, issueID, model.FormatStepID(stepID))
	if len(got) != 1 || got[0] != "the real comment" {
		t.Fatalf("step comments = %q; want the recorded body, unsuppressed", got)
	}
}

// A key already recorded in the step-comment scope still refuses the recording
// rather than letting it succeed without its comment.
func TestAKeyAlreadyInTheStepCommentScopeRefusesTheRecording(t *testing.T) {
	conn := mustDB(t)
	stepID, issueID, token := claimedImplement(t, conn)

	var next int
	testsupport.Must(t, conn.QueryRow(
		`SELECT COALESCE(MAX(id), 0) + 1 FROM artifacts`).Scan(&next), "next artifact id")
	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin")
	for id := next; id < next+4; id++ {
		testsupport.Must(t, db.RecordIdempotencyKeyTx(tx, db.ScopeStepComment, stepCommentKey(id), 1), "seeding")
	}
	testsupport.Must(t, tx.Commit(), "commit")

	err = testEngine().CompleteStep(conn, stepID, CompleteOptions{
		Token: token, Artifact: []byte("summary"), NowMS: nowMS,
		Comments: [][]byte{[]byte("the real comment")},
	})
	if err == nil {
		t.Fatal("the recording succeeded though its comment could not post")
	}
	if got := commentsBy(t, conn, issueID, model.FormatStepID(stepID)); len(got) != 0 {
		t.Errorf("step comments = %q after a refused recording; want none", got)
	}
	var artifacts int
	testsupport.Must(t, conn.QueryRow(
		`SELECT COUNT(*) FROM artifacts WHERE step_id = ? AND kind = ?`,
		stepID, IssueCommentArtifactKind).Scan(&artifacts), "counting")
	if artifacts != 0 {
		t.Errorf("%d issue-comment artifacts survived a refused recording", artifacts)
	}
}

// The control for the refusals above: ordinary prose in other scripts, accents,
// emoji, tabs, newlines and non-breaking spaces still posts.
func TestUnsafeCommentRuneAllowsOrdinaryProse(t *testing.T) {
	for _, s := range []string{
		"Approved: the change is safe.\n\n- tab\there",
		"café naïve résumé", "日本語のコメント", "👍 shipped", "a b", "é",
	} {
		if r, bad := unsafeCommentRune(s); bad {
			t.Errorf("unsafeCommentRune(%q) refused %U", s, r)
		}
	}
}
