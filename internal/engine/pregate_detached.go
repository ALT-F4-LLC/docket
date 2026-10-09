package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// Detached pre-gate runs (TDD docs/tdd/gates-trust.md §7.6.2 PG6).
//
// PG5 bounds the claim's whole pre-gate phase at 60s so `docket step claim`
// returns inside the executor's 120s tool timeout. A gate that needs longer —
// this repository's own ac-commands runs the full test suite for about three
// minutes — can therefore never hand the claim a complete result: every claim
// cut it off at the budget and recorded the cut, and a verify step whose
// evidence IS that gate's exit had nothing to verify with. Raising the budget
// would undo PG5; narrowing the gate would measure less. Neither is the fix.
//
// THE SHAPE. The sha a verify step's pre-gates measure first exists when a
// tree-holding step's routing stage records its `issue.diff` round record
// (appendRoundDelta's `head`), or when a resolution re-pins that record, or an
// integration annotation re-points it. Each of those commits calls
// startDetachedPreGates, which resolves every pending pre-gated step of the run
// to its target EXACTLY as the claim will (preGateTarget's own resolution) and
// launches `docket step pregate STEP-N --target SHA` detached — its own
// session, stdio on the null device, nothing for the launching verb to wait
// on. The child reconstructs the sha into a scratch tree (pregate_scratch.go:
// the same sidecar flock and dispatch sweep reclaim it if the child dies), runs
// each pre-gate through measurePreGate with NO claim deadline, and records the
// rows keyed by (step, target sha). The claim finds them
// (reusableDetachedPreGates) and serves them instead of spawning; a target
// that moved, a run still in flight, or a run that measured nothing leaves the
// claim on its PG5 path, unchanged. The claim never waits.
//
// WHY THE TARGET IS RECONSTRUCTED EVEN WHEN A WORKTREE STILL STANDS. The row
// is keyed by the sha, so the sha is what it measures: a live worktree may
// hold uncommitted work the sha does not, and integration sweeps that worktree
// at the wave's close — in the middle of a three-minute test run, when the
// run started at the writer's completion. A reconstruction is the sha, is
// nobody else's, and is swept by nothing but its own sidecar lock.
//
// CRASH SAFETY. Rows are written once, after the gate finished, in one
// transaction per gate: a child killed mid-run leaves no row at all, never a
// partial pass. Its scratch tree is reclaimed by the next dispatch open or
// close through the sidecar flock the kernel released. Its in-flight lock
// releases the same way, so the next completion in the run launches again.
//
// IDEMPOTENCE IS A FLOCK, NOT A PID. One run per (step, target) at a time: the
// child takes an exclusive, non-blocking flock on a lockfile named for both,
// in the store's lock directory beside tree.lock, and a second child finding
// it held exits without measuring. The launcher probes the same lock so a run
// in flight is not re-launched at every later completion; a probe that misses
// costs one child that exits at the lock.

// LaunchDetachedPreGates starts one detached pre-gate run for a step and a
// target sha, outside the calling process. nil means the mechanism is off and
// the engine runs every pre-gate inside the claim exactly as before.
//
// The CLI installs its launcher in the binary's real entry point only. A test
// binary must never find one installed: the launcher re-executes
// os.Executable(), which under `go test` is the test suite itself.
var LaunchDetachedPreGates func(stepID int, targetSHA string) error

// detachedPreGateLockDir resolves the directory the in-flight locks live in —
// the tree lock's own, so a local store keeps them in `.docket/` and the global
// store under its `locks/`. A variable so a test can point it at a temp
// directory; "" means no lock can be taken and no run may start.
var detachedPreGateLockDir = func() string {
	lock := repoPathsFrom(resolvePaths()).LockPath
	if lock == "" {
		return ""
	}
	return filepath.Dir(lock)
}

// detachedTargetSHA is the one form a target takes: a full commit id, as the
// round records store them and the launcher passes them. An abbreviation would
// record a key no claim's resolved sha could ever equal.
var detachedTargetSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// Outcomes of one detached run, as DetachedPreGateRun reports them.
const (
	// DetachedPreGateMeasured: the gates ran and their rows are recorded.
	DetachedPreGateMeasured = "measured"
	// DetachedPreGateReused: a complete result for this step and target was
	// already recorded, so nothing ran.
	DetachedPreGateReused = "reused"
	// DetachedPreGateRunning: another run holds this step and target's lock.
	DetachedPreGateRunning = "running"
	// DetachedPreGateStale: the step no longer resolves this target, so a
	// measurement of it would serve no claim.
	DetachedPreGateStale = "stale"
	// DetachedPreGateClosed: the step is over, or measures its own worktree
	// and never a sha, so nothing it does could serve this result.
	DetachedPreGateClosed = "closed"
)

// DetachedPreGateRun is what one detached run did, as `step pregate` reports
// it.
type DetachedPreGateRun struct {
	Step      string `json:"step"`
	TargetSHA string `json:"target_sha"`
	Outcome   string `json:"outcome"`
	// Gates are the rows this run recorded, or the recorded rows it found
	// complete — empty for every other outcome.
	Gates []PreGateResult `json:"gates,omitempty"`
}

// detachedPreGateCandidate is one pending step whose pre-gates resolve a
// target sha right now.
type detachedPreGateCandidate struct {
	step  *db.Step
	sha   string
	gates []workflow.Gate
}

// startDetachedPreGates launches a detached run for every pending pre-gated
// step of the run whose target is known and neither measured nor in flight.
//
// Best-effort by construction: it runs after a committed completion and
// returns nothing, because housekeeping must not stand between a step and the
// completion it already has. A launch that fails leaves the claim on its PG5
// path, which is where it was before this file existed.
func (e *Engine) startDetachedPreGates(conn *sql.DB, runID int, nowMS int64) {
	if LaunchDetachedPreGates == nil {
		return
	}
	candidates, err := detachedPreGateCandidates(conn, runID, nowMS)
	if err != nil {
		return
	}
	for _, c := range candidates {
		rows, err := db.GateResultsForStep(conn, c.step.ID)
		if err != nil {
			continue
		}
		if detachedPreGatesComplete(rows, c.gates, c.sha) {
			continue
		}
		if detachedPreGateInFlight(detachedPreGateLockPath(c.step.ID, c.sha)) {
			continue
		}
		_ = LaunchDetachedPreGates(c.step.ID, c.sha)
	}
}

// detachedPreGateCandidates resolves, inside one read-only snapshot, every
// pending executor step of an active run whose pre-gates consume `issue.diff`
// and whose target sha the claim would resolve now.
//
// The resolution is the claim's own (resolvedTargetFor over the live
// artifacts, the half preGateTarget takes for a step with no worktree of its
// own), so the key a run records is the key the claim will look up. A step
// that already has a worktree of its own measures that tree and never a sha,
// so it is not a candidate; neither is a step that is no longer pending — a
// claim in flight runs its own phase, and a finished step consumes nothing.
func detachedPreGateCandidates(
	conn *sql.DB, runID int, nowMS int64,
) ([]detachedPreGateCandidate, error) {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return nil, err
	}
	if !declaresDetachablePreGates(defs) {
		// The common case: no pinned definition has a pre-gate over a diff,
		// so nothing is loaded and the completion pays nothing.
		return nil, nil
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	sched, err := LoadScheduler(tx, runID, defs, nowMS)
	if err != nil {
		return nil, err
	}
	if run := sched.Run(); run == nil || run.Status != model.RunActive {
		return nil, nil
	}

	var (
		artifacts []*db.Artifact
		out       []detachedPreGateCandidate
	)
	for _, step := range sched.Steps() {
		if step.Status != db.StepPending || !isExecutorStep(step) || step.WorkRoot != "" {
			continue
		}
		spec := materializedSpec(sched.defs[step.WorkflowID], step, sched.holdTally)
		if spec == nil || !consumesIssueDiff(spec) {
			continue
		}
		gates := preClaimGates(spec)
		if len(gates) == 0 {
			continue
		}
		if artifacts == nil {
			if artifacts, err = db.ListRunArtifactsTx(tx, runID); err != nil {
				return nil, err
			}
		}
		sha, _, err := resolvedTargetFor(tx, sched, step, spec, artifacts)
		if err != nil {
			return nil, err
		}
		if sha == "" {
			continue
		}
		out = append(out, detachedPreGateCandidate{step: step, sha: sha, gates: gates})
	}
	return out, nil
}

// declaresDetachablePreGates reports whether any pinned definition has a step
// a detached run could ever serve: a pre-gate on a step that consumes
// `issue.diff`.
func declaresDetachablePreGates(defs map[int]*workflow.Definition) bool {
	for _, def := range defs {
		if def == nil {
			continue
		}
		for _, spec := range def.Steps {
			if spec != nil && consumesIssueDiff(spec) && len(preClaimGates(spec)) > 0 {
				return true
			}
		}
	}
	return false
}

// RunDetachedPreGates is the detached child's whole work: hold the (step,
// target) lock, confirm the measurement is still wanted, reconstruct the
// target, run every pre-gate under its entry's own timeout, and record the
// rows keyed to the target. `step pregate` is its only production caller.
//
// Every outcome short of a measurement is a quiet decline rather than an
// error: a second child finding the lock held, a result already complete, a
// step that moved on, a target that moved. Only a target that cannot be
// reconstructed is an error, because a run that was asked for and could not
// measure should say so to whoever asked.
func (e *Engine) RunDetachedPreGates(
	conn *sql.DB, stepID int, targetSHA string, nowMS int64,
) (*DetachedPreGateRun, error) {
	targetSHA = strings.ToLower(strings.TrimSpace(targetSHA))
	if !detachedTargetSHA.MatchString(targetSHA) {
		return nil, validationErr(
			"--target must be a full commit id, got %q; the round record's `head` "+
				"is one", targetSHA)
	}

	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return nil, notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return nil, err
	}
	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return nil, err
	}
	tally, err := loadHoldTally(conn, step.RunID)
	if err != nil {
		return nil, err
	}
	spec := materializedSpec(defs[step.WorkflowID], step, tally)
	if spec == nil {
		return nil, validationErr("step %s: %q is not a step of its pinned workflow",
			step.Instance, step.StepName)
	}
	gates := preClaimGates(spec)
	if len(gates) == 0 {
		return nil, validationErr(
			"step %s declares no pre-gate; there is nothing to run ahead of its claim",
			step.Instance)
	}

	out := &DetachedPreGateRun{Step: model.FormatStepID(stepID), TargetSHA: targetSHA}

	lockPath := detachedPreGateLockPath(stepID, targetSHA)
	if lockPath == "" {
		return nil, conflictErr(
			"no lock directory could be resolved for this store, and a detached " +
				"pre-gate run needs one to stay single; nothing was measured")
	}
	lock, held, err := holdDetachedPreGateLock(lockPath)
	if err != nil {
		return nil, err
	}
	if held {
		out.Outcome = DetachedPreGateRunning
		return out, nil
	}
	defer releaseDetachedPreGateLock(lock, lockPath)

	// UNDER THE LOCK, the three reasons not to measure, in the order they
	// are cheap: a result already recorded by the run that held this lock
	// before us; a step nothing can serve — over, claimed, or measuring its
	// own worktree; a target the step no longer resolves, because a later
	// round or a re-pin moved it.
	rows, err := db.GateResultsForStep(conn, stepID)
	if err != nil {
		return nil, err
	}
	if detachedPreGatesComplete(rows, gates, targetSHA) {
		out.Outcome = DetachedPreGateReused
		for _, gate := range gates {
			recorded, _ := recordedDetachedPreGate(rows, gate.Name, targetSHA)
			out.Gates = append(out.Gates, preGateResultOfRecorded(recorded))
		}
		return out, nil
	}
	if step.Status != db.StepPending || step.WorkRoot != "" {
		out.Outcome = DetachedPreGateClosed
		return out, nil
	}
	declined, err := stepStillWantsDetachedPreGate(conn, step, defs, targetSHA, nowMS)
	if err != nil {
		return nil, err
	}
	if declined != "" {
		out.Outcome = declined
		return out, nil
	}

	// The sha itself, rebuilt from the object database (see the file comment
	// for why a standing worktree is not used instead). Nothing is recorded
	// when it cannot be: the claim's own phase decides what to say about an
	// unbindable target, in the words it already has for that.
	scratch := reconstructTarget(conn, step.RunID, targetSHA)
	if scratch.Dir == "" {
		return nil, conflictErr(
			"the target %.12s could not be reconstructed from the run's object "+
				"database; nothing was measured", targetSHA)
	}
	defer scratch.release()

	// THE RECORD IS GUARDED IN ITS OWN TRANSACTION. The check above ran before
	// a gate that may take minutes; by the time a row is ready the step may
	// have been claimed — its claim measured for itself, or served another
	// run's rows — or its target may have moved. A row recorded then would be
	// a measurement of a sha the step no longer asks about, sitting at the
	// highest ordinal where a re-minted claim's replay would find it. So the
	// same question is asked again inside the recording transaction, and a
	// `no` writes nothing and ends the run.
	var declinedAtRecord string
	guard := func(tx *sql.Tx) (bool, error) {
		why, err := detachedPreGateWanted(tx, step, defs, targetSHA, nowMS)
		if err != nil {
			return false, err
		}
		declinedAtRecord = why
		return why == "", nil
	}

	for _, gate := range gates {
		// A gate this (step, target) already holds a complete row for is
		// served, not re-measured: a relaunch after a child died between two
		// gates picks up where that child stopped, and the ledger keeps one
		// measurement per gate.
		if recorded, ok := recordedDetachedPreGate(rows, gate.Name, targetSHA); ok {
			out.Gates = append(out.Gates, preGateResultOfRecorded(recorded))
			continue
		}
		measured, err := measurePreGate(conn, e, step, gate, preGateMeasurement{
			workRoot: scratch.Dir, scratch: scratch, targetSHA: targetSHA,
			detached: true, record: guard,
		}, nowMS)
		if errors.Is(err, errPreGateRecordDeclined) {
			out.Gates = nil
			out.Outcome = declinedAtRecord
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		for _, r := range measured {
			out.Gates = append(out.Gates, preGateResultOf(r))
		}
	}
	out.Outcome = DetachedPreGateMeasured
	return out, nil
}

// stepStillWantsDetachedPreGate is detachedPreGateWanted read from a fresh
// snapshot of its own: the pre-measurement check.
func stepStillWantsDetachedPreGate(
	conn *sql.DB, step *db.Step, defs map[int]*workflow.Definition,
	targetSHA string, nowMS int64,
) (string, error) {
	tx, err := conn.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	return detachedPreGateWanted(tx, step, defs, targetSHA, nowMS)
}

// detachedPreGateWanted reports whether a measurement of targetSHA can still
// serve the step's claim, inside the caller's transaction: the step is still
// pending and unclaimed, measures no worktree of its own, and its claim would
// resolve this exact sha right now (preGateTarget's own answer). It returns ""
// when so, and otherwise the outcome that names why not — the same answer
// before the measurement and at the moment of recording.
func detachedPreGateWanted(
	tx *sql.Tx, step *db.Step, defs map[int]*workflow.Definition,
	targetSHA string, nowMS int64,
) (string, error) {
	sched, err := LoadScheduler(tx, step.RunID, defs, nowMS)
	if err != nil {
		return "", err
	}
	fresh := sched.stepByID[step.ID]
	if fresh == nil || fresh.Status != db.StepPending || fresh.WorkRoot != "" {
		return DetachedPreGateClosed, nil
	}
	spec := materializedSpec(sched.defs[fresh.WorkflowID], fresh, sched.holdTally)
	if spec == nil {
		return DetachedPreGateClosed, nil
	}
	sha, _, err := preGateTarget(tx, sched, fresh, spec)
	if err != nil {
		return "", err
	}
	if sha != targetSHA {
		return DetachedPreGateStale, nil
	}
	return "", nil
}

// reusableDetachedPreGates finds, per gate, the one complete detached result
// for this step and this exact target: the row the claim serves instead of
// spawning (§7.6.2 PG6). Nothing is read for a step with no target sha —
// such a step measures a tree, never a key.
func reusableDetachedPreGates(
	conn *sql.DB, stepID int, gates []workflow.Gate, targetSHA string,
) (map[string]db.GateResultRow, error) {
	if targetSHA == "" {
		return nil, nil
	}
	rows, err := db.GateResultsForStep(conn, stepID)
	if err != nil {
		return nil, err
	}
	served := make(map[string]db.GateResultRow)
	for _, gate := range gates {
		if recorded, ok := recordedDetachedPreGate(rows, gate.Name, targetSHA); ok {
			served[gate.Name] = recorded
		}
	}
	return served, nil
}

// recordedDetachedPreGate selects ONE row for a gate keyed to a target: the
// latest COMPLETE measurement — a process ran and exited — and reports
// whether there is one. A run that recorded only `skipped` or `unmatched` rows
// measured nothing, and the claim measures for itself; a stale unmatched row
// beside a later pass is not served alongside it, because one gate is one
// result in the bundle.
func recordedDetachedPreGate(
	rows []db.GateResultRow, gate, targetSHA string,
) (db.GateResultRow, bool) {
	var (
		latest db.GateResultRow
		found  bool
	)
	if targetSHA == "" {
		return latest, false
	}
	for _, r := range rows {
		if !r.Pre || r.Gate != gate || r.TargetSHA != targetSHA || r.Exit == nil {
			continue
		}
		if !found || r.Ordinal >= latest.Ordinal {
			latest, found = r, true
		}
	}
	return latest, found
}

// detachedPreGatesComplete reports whether every declared pre-gate has a
// complete detached result for the target.
func detachedPreGatesComplete(rows []db.GateResultRow, gates []workflow.Gate, targetSHA string) bool {
	for _, gate := range gates {
		if _, ok := recordedDetachedPreGate(rows, gate.Name, targetSHA); !ok {
			return false
		}
	}
	return true
}

// detachedNote is the sentence a row measured ahead of the claim carries,
// beside the reconstruction note: it names the target the row is keyed to and
// says which bound applied, so a reader is not left wondering why this row
// shows no claim-time bound.
func detachedNote(sha string) string {
	return fmt.Sprintf(
		"measured ahead of the claim by a detached pre-gate run against %.12s, "+
			"under the trust entry's own timeout", sha)
}

// detachedPreGateLockPath names the in-flight lockfile for one (step, target),
// or "" when no lock directory resolves.
func detachedPreGateLockPath(stepID int, targetSHA string) string {
	dir := detachedPreGateLockDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, fmt.Sprintf(
		"pregate-%s-%.12s.lock", model.FormatStepID(stepID), targetSHA))
}

// holdDetachedPreGateLock takes the in-flight lock, or reports that another
// run holds it. The open is the tree lock's (openTreeLockFile): O_NOFOLLOW, a
// regular file or a refusal, the directory created when missing — the lock
// lives in repo-shippable space for a local store, so §7.4 L7's discipline
// applies to it as well.
func holdDetachedPreGateLock(path string) (lock *os.File, held bool, err error) {
	file, err := openTreeLockFile(path)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("locking %s: %w", path, err)
	}
	return file, false, nil
}

// releaseDetachedPreGateLock closes the lock and removes its file, in that
// order: a prober that opens the file in between takes the lock on a run that
// is over, re-reads the ledger, and finds the rows this run recorded first.
func releaseDetachedPreGateLock(lock *os.File, path string) {
	lock.Close()
	_ = os.Remove(path)
}

// detachedPreGateInFlight reports whether a run holds the (step, target) lock
// right now. No path means nowhere to lock, which is read as "do not launch":
// a child could not keep itself single either.
func detachedPreGateInFlight(path string) bool {
	if path == "" {
		return true
	}
	lock, live := probeLiveLock(path)
	if lock != nil {
		lock.Close()
	}
	return live
}
