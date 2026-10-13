package engine

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// A retried write step supersedes the prior attempt's target.
//
// Attempt 1 recorded its commit in one worktree and parked on a failing gate.
// The operator resolved `--as retry`; attempt 2 landed the same patch on a
// fresh base in a fresh worktree and recorded done. The diff BODY of the two
// attempts is byte-identical — same patch — but the head and the worktree are
// not, and those are what every downstream packet renders as target_sha and
// target_worktree. Comparing bodies alone dropped attempt 2's record, so the
// review chain, verify-ac, and its vote all targeted attempt 1's commit and
// swept worktree and rejected work the judges found correct.

// newestIssueDiffTarget reads the head and worktree the issue's newest
// issue.diff payload names, and how many issue.diff records the issue holds.
func newestIssueDiffTarget(t *testing.T, conn *sql.DB, runID, issueID int) (head, worktree string, records int) {
	t.Helper()
	var payload string
	err := conn.QueryRow(
		`SELECT COALESCE(a.payload, '') FROM artifacts a JOIN steps s ON s.id = a.step_id
		  WHERE a.run_id = ? AND s.issue_id = ? AND a.kind = ?
		  ORDER BY a.id DESC LIMIT 1`,
		runID, issueID, ArtifactKindIssueDiff).Scan(&payload)
	testsupport.Must(t, err, "reading the newest issue.diff payload: %v", err)
	var record struct {
		Head     string `json:"head"`
		Worktree string `json:"worktree"`
	}
	err = json.Unmarshal([]byte(payload), &record)
	testsupport.Must(t, err, "unmarshal %q: %v", payload, err)
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM artifacts a JOIN steps s ON s.id = a.step_id
		  WHERE a.run_id = ? AND s.issue_id = ? AND a.kind = ?`,
		runID, issueID, ArtifactKindIssueDiff).Scan(&records)
	testsupport.Must(t, err, "counting issue.diff records: %v", err)
	return record.Head, record.Worktree, records
}

func TestRetryRecordsAFreshIssueDiffAtTheNewTarget(t *testing.T) {
	conn := mustDB(t)
	run, issueID := activatedRun(t, conn)
	// A pinned run commit, so the diff base resolves the way it does in a
	// real run and the body carries no base-resolution note naming the tree.
	// Without it the two attempts' bodies differ by their worktree path and
	// the guard under test never sees the identical bodies the defect needs.
	_, err := conn.Exec(`UPDATE runs SET commit_sha = ? WHERE id = ?`,
		"0395bc99a03fc757ebbaacd34be22d11a00f7379", run.ID)
	testsupport.Must(t, err, "pinning the run commit: %v", err)

	treeA, treeB := t.TempDir(), t.TempDir()
	heads := map[string]string{
		treeA: "872ed150872ed150872ed150872ed150872ed150",
		treeB: "1eb345b31eb345b31eb345b31eb345b31eb345b3",
	}
	gates := &scriptedGates{fail: true}
	e := testEngine()
	e.Gates = gates
	e.HeadFn = func(dir string) string { return heads[dir] }
	// The same patch either time: the body cannot tell the attempts apart.
	e.DiffFn = func(_, _ string, _ []string) (string, error) {
		return "diff --git a/f b/f\n+the change\n", nil
	}

	id := stepIDByInstance(t, conn, "implement@0")

	// Attempt 1, in tree A, parks on the failing gate.
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w1", NowMS: nowMS})
	testsupport.Must(t, err, "claim (attempt 1): %v", err)
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"),
		WorkDir: treeA, NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete (attempt 1): %v", err)
	if got := stepStatus(t, conn, "implement@0"); got != db.StepWaitingHuman {
		t.Fatalf("status = %q after a failing gate, want %q", got, db.StepWaitingHuman)
	}
	head, worktree, records := newestIssueDiffTarget(t, conn, run.ID, issueID)
	if records != 1 || head != heads[treeA] || worktree != treeA {
		t.Fatalf("attempt 1 recorded %d issue.diff(s) at %s in %s, want 1 at %s in %s",
			records, head, worktree, heads[treeA], treeA)
	}

	// The operator retries; attempt 2 lands the same patch in tree B.
	gates.fail = false
	err = e.ResolveStep(conn, id, ResolveRetry, "redo it on a fresh base", nowMS+1)
	testsupport.Must(t, err, "resolve --as retry: %v", err)
	claim, err = ClaimStep(conn, id, ClaimOptions{Owner: "w2", NowMS: nowMS + 2})
	testsupport.Must(t, err, "claim (attempt 2): %v", err)
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary, again"),
		WorkDir: treeB, NowMS: nowMS + 2,
	})
	testsupport.Must(t, err, "complete (attempt 2): %v", err)
	if got := stepStatus(t, conn, "implement@0"); got != db.StepDone {
		t.Fatalf("status = %q after the retry passed its gates, want %q", got, db.StepDone)
	}

	// THE ASSERTION: the retry recorded its own issue.diff, and the newest
	// one names the tree that recorded done, not the superseded attempt.
	head, worktree, records = newestIssueDiffTarget(t, conn, run.ID, issueID)
	if records != 2 {
		t.Errorf("the issue holds %d issue.diff record(s), want 2 — the retry's "+
			"same-content diff at a new head was dropped as byte-identical", records)
	}
	if head != heads[treeB] {
		t.Errorf("newest issue.diff head = %s, want %s — consumers would target "+
			"the prior attempt's commit", head, heads[treeB])
	}
	if worktree != treeB {
		t.Errorf("newest issue.diff worktree = %s, want %s", worktree, treeB)
	}
}
