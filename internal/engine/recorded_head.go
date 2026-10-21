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
	// Base is the head's fork point from the shared checkout, the
	// DOCKET_GATE_BASE a gate is handed.
	Base string `json:"base,omitempty"`
	// Worktree is the path the head was read from. A head is honored only for
	// the worktree the step still records, so an operator re-pin to another
	// checkout is never measured at the stale sha.
	Worktree string `json:"worktree"`
	// Live marks a head whose WHOLE work a checkout of it would not reproduce:
	// the worktree held uncommitted or untracked state, or no resolvable fork
	// point, or a commit the run's checkout cannot check out. The gates and the
	// diff then read the live worktree, as they always did, but REFUSE to stand
	// for it once its HEAD is no longer Head, so a worktree that moves on after
	// the record cannot make gates that passed one commit name another.
	//
	// The pin is HEAD only. Uncommitted content edited after the record, at the
	// same HEAD, is not bound: only a gate run in a checkout of a snapshot of
	// the content could bind it, and close never verifies uncommitted content
	// either, so the residual is stated rather than half-closed.
	Live bool `json:"live,omitempty"`
}

// captureRecordedHead reads the head of the worktree a step records from. The
// zero value means there is nothing to record: no head resolves, or the
// worktree is the shared checkout (a step may record it; it keeps the live-tree
// behavior, because its head legitimately moves as work is
// integrated.
func (e *Engine) captureRecordedHead(conn *sql.DB, runID int, worktree string) (recordedHead, bool) {
	if worktree == "" || e.HeadFn == nil || worktree == runExecRoot(conn, runID) {
		return recordedHead{}, false
	}
	head := e.HeadFn(worktree)
	if head == "" {
		return recordedHead{}, false
	}
	// The fork point is taken from the head just read, not from a second read of
	// the worktree's HEAD, so base is always an ancestor of head.
	base := ""
	if shared := sharedCheckoutHead(runExecRoot(conn, runID)); shared != "" {
		base = mergeBase(worktree, head, shared)
	}
	// A checkout of the head stands for the writer's WHOLE work when its fork
	// point resolves, the run's checkout holds the commit, and nothing is left
	// uncommitted. That includes a writer that committed nothing and left its
	// worktree clean (head == base): a checkout of base IS that worktree. When
	// anything is uncommitted the live worktree stays the tree, the only one
	// that holds it, and the record only pins its head.
	live := base == "" || !worktreeClean(worktree) ||
		!commitInRepo(runExecRoot(conn, runID), head)
	return recordedHead{Head: head, Base: base, Worktree: worktree, Live: live}, true
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

// liveHeadMoved reports whether a Live record's worktree no longer stands at
// the recorded head (or cannot be read at all).
func (e *Engine) liveHeadMoved(rec recordedHead, worktree string) bool {
	return e.HeadFn == nil || e.HeadFn(worktree) != rec.Head
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

// liveHeadMovedReason words the skipped row a moved worktree records.
func liveHeadMovedReason(rec recordedHead) string {
	return "the worktree no longer stands at the commit " + rec.Head[:min(12, len(rec.Head))] +
		" this step recorded, so a gate cannot say which commit it measured"
}

// commitInRepo reports whether the commit is in dir's object database: a
// worktree of a different repository records a head the run's checkout cannot
// check out, and its record then pins the head without replacing the tree.
func commitInRepo(dir, sha string) bool {
	return osexec.Command("git", "-c", "core.fsmonitor=false", "-C", dir,
		"cat-file", "-e", sha+"^{commit}").Run() == nil
}
