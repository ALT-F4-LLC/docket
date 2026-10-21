package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-4279: a writer that records a candidate beside a gap it marked
// AC-Blocking parks for the operator, and the review that follows is not
// offered.

const partialGapSrc = `
[pipeline]
name = "partial-with-gap"
version = 1

[match]
kind = ["task"]

[[step]]
name = "work"
after = []
executor = "w"
emits = "out"

[[step]]
name = "review"
after = ["work"]
executor = "j"
emits = "findings"
`

// completeWorkWithGap records a non-empty candidate and one gap body.
func completeWorkWithGap(t *testing.T, conn *sql.DB, gap string) (runID, stepID int) {
	t.Helper()
	runID = activateSrc(t, conn, partialGapSrc, "partial-gap.toml")
	stepID = stepIDByInstance(t, conn, "work@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	var gapIssues []string
	err = testEngine().CompleteStep(conn, stepID, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("candidate 11995d2: AC2 and AC3 met"),
		Gaps:     [][]byte{[]byte(gap)}, GapIssues: &gapIssues, NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete with a candidate and a gap: %v", err)
	return runID, stepID
}

func TestACBlockingGapParksACandidateAndHoldsItsReviews(t *testing.T) {
	conn := mustDB(t)
	runID, stepID := completeWorkWithGap(t, conn,
		"LoadScheduler takes a *sql.Tx\nAC-Blocking: true\n\nAC1 and AC4 cannot be met here.")

	assertParkClass(t, conn, park{runID, stepID}, db.ParkClassPartialWithGap)

	manifest := openDispatch(t, conn, runID, 0, nowMS)
	for _, row := range manifest.Rows {
		if strings.HasPrefix(row.Instance, "review@") {
			t.Errorf("dispatch open offered %s behind a parked candidate", row.Instance)
		}
	}
}

func TestNonBlockingGapStillCompletesDone(t *testing.T) {
	for name, gap := range map[string]string{
		"no header":          "an unrelated finding\n\nworth a look",
		"declared false":     "an unrelated finding\nAC-Blocking: false\n\nworth a look",
		"key past the block": "an unrelated finding\n\nAC-Blocking: true",
	} {
		t.Run(name, func(t *testing.T) {
			conn := mustDB(t)
			_, stepID := completeWorkWithGap(t, conn, gap)
			var status, class string
			testsupport.Must(t, conn.QueryRow(
				`SELECT status, park_class FROM steps WHERE id = ?`, stepID).Scan(&status, &class),
				"reading the step")
			if status != db.StepDone || class != "" {
				t.Errorf("status=%q park_class=%q; want done with no park", status, class)
			}
		})
	}
}

// A retry after a partial-with-gap park re-does the work under the same step
// id, and the first attempt's artifacts stay. Its AC-blocking gap must not
// park the second attempt: only the gaps recorded beside the latest candidate
// speak for the completion being routed.
func TestARetryAfterAnACBlockingParkIsNotParkedByTheEarlierAttemptsGap(t *testing.T) {
	conn := mustDB(t)
	_, stepID := completeWorkWithGap(t, conn,
		"LoadScheduler takes a *sql.Tx\nAC-Blocking: true\n\nAC1 cannot be met here.")

	testsupport.Must(t, testEngine().ResolveStep(conn, stepID, ResolveRetry, "amended the scope", nowMS+1),
		"resolve --as retry: %v", nil)

	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS + 2})
	testsupport.Must(t, err, "re-claim: %v", err)
	testsupport.Must(t, testEngine().CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("candidate 2: AC1 met"), NowMS: nowMS + 3,
	}), "second completion: %v", nil)

	var status string
	testsupport.Must(t, conn.QueryRow(
		`SELECT status FROM steps WHERE id = ?`, stepID).Scan(&status), "reading the step")
	if status != db.StepDone {
		t.Errorf("status=%q; want done after a clean retry", status)
	}
}

// The retry's own AC-blocking gap still parks it.
func TestARetryThatRecordsItsOwnACBlockingGapParksAgain(t *testing.T) {
	conn := mustDB(t)
	_, stepID := completeWorkWithGap(t, conn,
		"first blocker\nAC-Blocking: true\n\nAC1 cannot be met here.")

	testsupport.Must(t, testEngine().ResolveStep(conn, stepID, ResolveRetry, "retry", nowMS+1),
		"resolve --as retry: %v", nil)

	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS + 2})
	testsupport.Must(t, err, "re-claim: %v", err)
	var gapIssues []string
	testsupport.Must(t, testEngine().CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("candidate 2"),
		Gaps:      [][]byte{[]byte("second blocker\nAC-Blocking: true\n\nAC4 cannot be met here.")},
		GapIssues: &gapIssues, NowMS: nowMS + 3,
	}), "second completion: %v", nil)

	var class string
	testsupport.Must(t, conn.QueryRow(
		`SELECT park_class FROM steps WHERE id = ?`, stepID).Scan(&class), "reading the step")
	if class != string(db.ParkClassPartialWithGap) {
		t.Errorf("park_class=%q; want %q", class, db.ParkClassPartialWithGap)
	}
}
