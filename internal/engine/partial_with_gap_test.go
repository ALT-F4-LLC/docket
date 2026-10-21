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
