package engine

import (
	"database/sql"
	"encoding/json"
	osexec "os/exec"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// RecordedHeadArtifactKind is the auxiliary kind a worktree-recorded step
// carries beside its declared emit: the commit its writer left in the
// worktree, captured at record time (DKT-4250). Like GapArtifactKind it is
// packaging vocabulary and needs no workflow declaration.
//
// It exists because the commit is otherwise named only by the diff stage, and
// that stage runs LAST in the saga, after the completion gates. By then the
// writer's worktree may already be gone, so the one moment the head can be
// read from the worktree is the record itself.
const RecordedHeadArtifactKind = "recorded-head"

// recordedHead is a recorded-head artifact's payload.
type recordedHead struct {
	// Head is the writer's HEAD in the worktree at record time.
	Head string `json:"head"`
	// Base is the worktree's fork point, the DOCKET_GATE_BASE a gate is handed.
	Base string `json:"base,omitempty"`
	// Worktree is the path the head was read from. A head is honored only for
	// the worktree the step still records, so an operator re-pin to another
	// checkout is never measured at the stale sha.
	Worktree string `json:"worktree"`
}

// captureRecordedHead reads the head of the worktree a step records from. The
// zero value means there is nothing to record: no head resolves, or the
// worktree holds no commit of its own (head is its fork point), in which case
// the writer's work is uncommitted state that only the worktree itself holds.
func (e *Engine) captureRecordedHead(conn *sql.DB, runID int, worktree string) (recordedHead, bool) {
	if worktree == "" || e.HeadFn == nil {
		return recordedHead{}, false
	}
	head := e.HeadFn(worktree)
	if head == "" {
		return recordedHead{}, false
	}
	// A recorded head stands for the writer's WHOLE work, so it is taken only
	// when a checkout of it is that work: the worktree is not the shared
	// checkout, its fork point resolves, the head is a commit of its own, and
	// nothing is left uncommitted. Otherwise the live worktree stays the tree,
	// which is the only one that holds the uncommitted part.
	base := gateBaseSHA(conn, runID, worktree)
	if base == "" || head == base || !worktreeClean(worktree) {
		return recordedHead{}, false
	}
	return recordedHead{Head: head, Base: base, Worktree: worktree}, true
}

// recordedHeadOf is the head recorded for the step's CURRENT worktree.
func recordedHeadOf(conn *sql.DB, step *db.Step) (recordedHead, bool) {
	var payload sql.NullString
	err := conn.QueryRow(
		`SELECT payload FROM artifacts WHERE step_id = ? AND kind = ?
		  ORDER BY id DESC LIMIT 1`,
		step.ID, RecordedHeadArtifactKind).Scan(&payload)
	if err != nil {
		return recordedHead{}, false
	}
	var rec recordedHead
	if json.Unmarshal([]byte(payload.String), &rec) != nil ||
		rec.Head == "" || rec.Worktree != step.WorkRoot {
		return recordedHead{}, false
	}
	return rec, true
}

// worktreeClean reports whether dir has no tracked change and no untracked
// file. Anything it cannot read counts as not clean.
func worktreeClean(dir string) bool {
	// No fsmonitor hook and no index lock: this reads a tree the executor owns.
	out, err := osexec.Command("git", "-c", "core.fsmonitor=false", "-C", dir,
		"--no-optional-locks", "status", "--porcelain",
		"--untracked-files=all").Output()
	return err == nil && len(strings.TrimSpace(string(out))) == 0
}

// insertRecordedHeadTx records the commit a step's worktree holds, or, when
// none reproduces it, a cleared record (an empty head) that supersedes any
// earlier one: recordedHeadOf reads the newest row and honors only a head.
func insertRecordedHeadTx(
	tx *sql.Tx, step *db.Step, rec recordedHead, have bool, worktree string, nowMS int64,
) error {
	if !have {
		rec = recordedHead{Worktree: worktree}
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = db.InsertArtifactTx(tx, db.Artifact{
		RunID: step.RunID, StepID: step.ID, Kind: RecordedHeadArtifactKind,
		Body: rec.Head, Payload: string(payload),
		SHA256: workflow.SHA256([]byte(rec.Head)),
	}, nowMS)
	return err
}
