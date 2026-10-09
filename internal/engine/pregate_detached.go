package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/ALT-F4-LLC/docket/internal/config"
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
//
// THE SCHEDULER HOLDS THE STEP WHILE THE RUN IS IN FLIGHT. The claim never
// waits, so a claim that arrives while the child is still measuring runs the
// gate on its PG5 path and records the cut — the exact row this file exists
// to replace, and for a verify step whose evidence IS that gate's exit, no
// evidence at all. So a pending pre-gated step whose (step, target) lock is
// held right now is not ready (ready.go, CondPreGatePending): `next` and
// `dispatch open` do not offer it, the staged closure does not stage it, and a
// claim is refused rather than served a budget-cut row. The hold is the same
// flock, probed once per scheduler snapshot (loadDetachedPreGateHolds) for
// the step's exact resolved target, and it lifts the way the lock does: the
// child releases it when its rows are recorded, and the kernel releases it
// when the child dies. A lock nobody took — a child that never launched, or a
// lock directory that does not resolve — holds nothing. A live child is
// bounded by its entries' own timeouts, so the hold is too.

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
//
// Resolved ONCE per process. The resolution shells out to git (config.Resolve
// locates the worktree), and the readiness hold asks for the directory from
// inside a scheduler snapshot's transaction, on every scheduling verb that
// finds a pre-gated step at its turn; the store a process runs against does
// not change between those calls, so the first answer is every answer. The
// CLI primes it from the config it already resolved (PrimeDetachedPreGateLockDir),
// so under `docket` the first call never shells out at all — and never from
// inside a transaction, which under BEGIN IMMEDIATE holds the write lock.
var detachedPreGateLockDir = sync.OnceValue(func() string {
	return lockDirOf(resolvePaths())
})

// PrimeDetachedPreGateLockDir fixes the in-flight lock directory from a
// config the caller already resolved, so no scheduler snapshot pays a git
// resolution for it. The CLI calls it once at entry, after config.Resolve and
// before any verb opens a transaction. A nil config leaves the lazy default.
func PrimeDetachedPreGateLockDir(cfg *config.Config) {
	if cfg == nil {
		return
	}
	dir := lockDirOf(cfg)
	detachedPreGateLockDir = func() string { return dir }
}

// lockDirOf is the tree lock's directory for a resolved config, "" when the
// config resolves no tree lock.
func lockDirOf(cfg *config.Config) string {
	lock := repoPathsFrom(cfg).LockPath
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
	return detachablePreGateSteps(tx, sched, nil)
}

// detachablePreGateSteps is detachedPreGateCandidates' body over an
// already-loaded snapshot: every pending executor step of the run that
// measures no worktree of its own, consumes `issue.diff`, declares a pre-gate,
// and resolves a target sha right now — narrowed to the steps `admit` accepts
// when one is given. The launcher and the readiness hold both read it, so the
// key a run records, the key a claim looks up, and the key the hold probes are
// one resolution (resolvedTargetFor over the live artifacts).
func detachablePreGateSteps(
	tx *sql.Tx, sched *Scheduler, admit func(*db.Step) bool,
) ([]detachedPreGateCandidate, error) {
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
		if admit != nil && !admit(step) {
			continue
		}
		if artifacts == nil {
			var err error
			if artifacts, err = db.ListRunArtifactsTx(tx, step.RunID); err != nil {
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

// loadDetachedPreGateHolds fills the scheduler's preGateHolds: the pending
// pre-gated steps whose (step, resolved target) detached run holds its
// in-flight lock at this instant (the file comment's "the scheduler holds the
// step"). Called by LoadScheduler, inside the snapshot's transaction, because
// resolving a target needs one and Ready has none.
//
// DORMANT by the same two guards the launcher uses: a run that is not active
// offers nothing anyway, and a run whose pinned definitions declare no
// pre-gate over `issue.diff` can have no detached run to wait on, so neither
// pays a resolution or a probe. Candidates are narrowed to steps whose R3
// holds (predecessorsDone, side-effect free): a step still behind its
// predecessors is where the lock is held for most of a run's life, and it
// would report CondPredecessors whatever the lock says, so resolving its
// target would buy nothing. The hold clause itself sits after every clause of
// Ready but the budget, so the hold applies only to a step whose place in the
// run admits it whatever this narrowing admits.
//
// THE PROBE NEVER BLOCKS, NEVER CREATES, AND KEEPS NOTHING: detachedPreGateHeld
// opens the lockfile without O_CREATE, tries the flock non-blocking, and a
// lock it acquires — a child that died with the file in place — is closed
// again here. A missing lockfile, or no lock directory at all, is no hold.
func (s *Scheduler) loadDetachedPreGateHolds(tx *sql.Tx) error {
	return s.probeDetachedPreGateHolds(tx, s.predecessorsDone)
}

// refreshDetachedPreGateHold re-probes ONE step's hold after a lazy reap
// returned it to `pending` inside this snapshot (claim.go, reapOneTx). The
// holds were loaded with the snapshot, when the step was still claimed and so
// not a candidate; a readiness pass over the reflected reap would otherwise
// offer — or admit the claim of — a step whose detached run is in flight,
// which is the one state the hold exists to refuse. Same guards, same
// resolution, same probe as the load; only the admission narrows to the step.
func (s *Scheduler) refreshDetachedPreGateHold(tx *sql.Tx, step *db.Step) error {
	delete(s.preGateHolds, step.ID)
	return s.probeDetachedPreGateHolds(tx, func(c *db.Step) bool { return c.ID == step.ID })
}

// preGateHeld reports whether the hold is the ONE condition keeping a step out
// of the ready set over this snapshot: Ready answers CondPreGatePending, so
// the step is pending, placed, routed, unconflicted, within headroom, and
// waiting only on its detached run. `dispatch verify` reads a stored row in
// this state as matched rather than missing (verifyDispatchTx): the hold
// defers the wave's claim without changing what the manifest offered.
func preGateHeld(s *Scheduler, stepID int) bool {
	step := s.stepByID[stepID]
	if step == nil {
		return false
	}
	ok, cond := s.Ready(step)
	return !ok && cond == CondPreGatePending
}

// probeDetachedPreGateHolds is the body loadDetachedPreGateHolds and
// refreshDetachedPreGateHold share: resolve the admitted candidates' targets,
// probe each (step, target) lock, and record the ones held.
func (s *Scheduler) probeDetachedPreGateHolds(tx *sql.Tx, admit func(*db.Step) bool) error {
	if s.run == nil || s.run.Status != model.RunActive || !declaresDetachablePreGates(s.defs) {
		return nil
	}
	candidates, err := detachablePreGateSteps(tx, s, admit)
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if !detachedPreGateHeld(detachedPreGateLockPath(c.step.ID, c.sha)) {
			continue
		}
		if s.preGateHolds == nil {
			s.preGateHolds = make(map[int]string)
		}
		s.preGateHolds[c.step.ID] = c.sha
	}
	return nil
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
	return path == "" || detachedPreGateHeld(path)
}

// detachedPreGateHeld is the readiness hold's reading of the same lock: a run
// holds it, or nothing does. ONLY A FLOCK ANOTHER HOLDER REFUSES (EWOULDBLOCK)
// IS A HOLD. No path is nothing — no lock directory means no child ever took
// a lock there, so there is no run to wait on — and so is a lockfile that is
// missing, that cannot be opened, whose flock fails for any reason other than
// a holder, or whose flock this probe can take (closed again at once; the
// probe keeps nothing). Errors read as "not held" deliberately, the opposite
// of probeLiveLock's sweeper reading: a hold is a refusal to offer, and a
// probe that cannot ask the kernel knows of no run to wait on. On a filesystem
// where flock fails, a child creates the lockfile, fails its own flock, and
// exits without removing it; a probe that read every error as "held" would
// then hold the step — and suppress every relaunch — forever. The one reading
// that differs from the launcher's is the empty path, because the two answer
// different questions: "may I launch?" fails closed to not launching, "may I
// offer?" fails closed to offering, which is the step's behavior before this
// hold existed. On an error the launcher therefore relaunches, and the child
// exits at its own flock failure: one wasted child per completion, never a
// permanent hold.
func detachedPreGateHeld(path string) bool {
	if path == "" {
		return false
	}
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	return errors.Is(err, syscall.EWOULDBLOCK)
}
