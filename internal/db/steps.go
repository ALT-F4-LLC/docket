package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Step sentinels.
var (
	// ErrStepNotFound means no `steps` row matches. NOT_FOUND (exit 2).
	ErrStepNotFound = errors.New("step not found")

	// ErrSagaStageMoved means a saga stage's CAS guard matched zero rows:
	// another engine invocation advanced the saga first (TDD §6.8, "resume is
	// lazy and idempotent"). It is not a failure — the loser re-reads and
	// either finds the work done or advances the next stage — so no CLI verb
	// maps it to an exit code; the saga driver handles it internally.
	ErrSagaStageMoved = errors.New("saga stage already advanced")
)

// The nine PERSISTED step statuses (TDD §6.2). `ready` is the tenth member of
// the MACHINE and is deliberately absent here: it is computed at read time by
// the §6.3 predicate and never stored as intent, exactly as v6's effective
// lease status is. Both numbers are stated in the TDD because the enum's size
// and the machine's size are different questions.
const (
	StepPending      = "pending"
	StepClaimed      = "claimed"
	StepRunning      = "running"
	StepGated        = "gated"
	StepDone         = "done"
	StepWaitingHuman = "waiting-human"
	StepSkipped      = "skipped"
	StepSuperseded   = "superseded"
	StepFailedRouted = "failed-routed"

	// StepReady is the COMPUTED status a read verb renders when the §6.3
	// predicate holds. It is a rendering value, never a column value, and
	// TestReadyIsNeverPersisted asserts nothing writes it.
	StepReady = "ready"

	// StepStaged is the second COMPUTED status, rendered only on offer rows
	// (`next`, `dispatch open`): the step is NOT ready — its `after`
	// predecessors have not all recorded — but every unsatisfied predecessor
	// is itself in the same offer at a lower stage, so a dispatcher that runs
	// the offer's stages in order will find it claimable by the time its stage
	// begins. Like `ready` it is a rendering value, never a column value, and
	// never an answer `step show` gives (stagedness is a property of one
	// offer's membership, not of the step). A `staged` row is NOT claimable
	// yet: `claim` re-checks the predicate and refuses until the predecessors
	// actually record, which is what keeps a stage-skipping dispatcher safe.
	StepStaged = "staged"
)

// StepTerminal reports whether a status ends a step's life. A terminal step is
// never re-offered by `next`, never claimable, and never reaped.
func StepTerminal(status string) bool {
	switch status {
	case StepDone, StepSkipped, StepSuperseded, StepFailedRouted:
		return true
	}
	return false
}

// StepOffScheduler reports whether a status takes a step OUT OF THE SCHEDULER'S
// ANSWER — a strictly wider question than StepTerminal's (DKT-65).
//
// The two are different because a step's life can pause without ending. A
// `waiting-human` step is very much alive — an operator will approve, reject, or
// resolve it — but `next` does not offer it, so anything comparing a stored
// manifest against a fresh recomputation must expect it to be ABSENT rather
// than to have moved. `dispatch verify` used StepTerminal for that comparison
// and consequently reported
//
//	does not match its current rendering ... recomputed: (no row at this position)
//
// with exit 4 for a step that had recorded correctly and legitimately parked.
//
// THE ASK NAMED THREE STATUSES AND THE STEP MACHINE HAS ONE. `paused` is a RUN
// status and `held` is a SAGA STAGE (SagaHeld); neither is ever written to
// `steps.status`, whose persisted vocabulary is the nine constants above. A
// paused run removes its steps from the scheduler through their run, not
// through their own status, so this predicate covers what a step can actually
// say about itself and the run-level case stays the run's to answer.
//
// It is a SEPARATE predicate rather than a widening of StepTerminal because the
// callers that ask "is this step over?" — reap, claim, re-offer — must keep
// getting `false` for `waiting-human`: a step waiting on a person is one an
// operator is still expected to act on.
func StepOffScheduler(status string) bool {
	return StepTerminal(status) || status == StepWaitingHuman
}

// Saga stages (TDD §6.8). The value stored in `steps.saga_stage` is the RESUME
// POINT: the stage that has committed, so the next engine invocation knows
// which one to run next. NULL means "not in the saga" — either never entered or
// complete.
const (
	// SagaRecorded is stage 1's commit: the artifact is in, the token is
	// retired, status is `gated`. From here the saga is engine-owned and needs
	// no lease.
	SagaRecorded = "recorded"
	// SagaRouting is the last stage's resume point: gates are all recorded and
	// routing is what remains.
	SagaRouting = "routing"
	// SagaHeld is the stage an `aggregate` step enters when `hold_spread` trips
	// (payloads-thresholds §7.7 H8). Its artifact is recorded and its
	// `<step>-held` question is open; its routing is DEFERRED until an operator
	// resolves that question.
	//
	// It is a stage rather than a status because "gating the routing step" is a
	// DEFERRAL OF ROUTING, and the machinery for deferring a saga already
	// exists. The step's status stays `gated` — non-terminal — so every
	// downstream successor fails R3 and nothing proceeds: no new status, no
	// synthetic `after` edge, no second readiness rule.
	SagaHeld = "held"
	// SagaGatePrefix prefixes a per-gate resume point, `gate:<name>`.
	SagaGatePrefix = "gate:"
)

// Step is one `steps` row, read whole. It is a superset of StepRow — which
// activation writes — because phase 3 reads the lease, saga, and routing
// columns activation never fills.
type Step struct {
	ID           int
	RunID        int
	IssueID      int
	WorkflowID   int
	StepName     string
	Ordinal      int
	SiblingIndex *int
	Instance     string
	Kind         string
	Executor     string
	Class        string
	Status       string
	Attempt      int
	// AttemptBase is the attempt count the retry budget counts FROM (v16,
	// DKT-86/DKT-90). `attempt` is monotonic for the step's whole life — it is
	// the usage ledger's key half and §11.4's "claims made against this step,
	// ever" — so `step resolve --as retry` refreshes the budget by moving this
	// base to the current attempt instead of zeroing the counter. Exhaustion
	// compares Attempt-AttemptBase against MaxAttempts.
	AttemptBase int
	// FailedAttempts and ReapedClaims are the OUTCOME breakdown of the claims
	// Attempt counts (v23, DKT-490): how many ended in an explicit `step fail`,
	// and how many were reaped without one (lease expiry, `max_step_duration`,
	// `step reap`). Attempt alone cannot answer that — it spends one count per
	// claim whatever the ending — and a consumer that read it as "attempts
	// that failed" escalated on claims that never failed at all. A claim that
	// RECORDED counts in neither, and `step resolve --as retry` touches
	// neither. FailedAttempts+ReapedClaims never exceeds Attempt; the
	// remainder is live claims, recorded completions, and pre-v23 history
	// (the migration back-fills nothing — see migrateV22ToV23).
	FailedAttempts int
	ReapedClaims   int
	MaxAttempts    *int
	ExpectedCost   float64
	Owner          string
	TokenHash      string
	ExpiresMS      int64
	StartedMS      *int64
	ActivityMS     *int64
	SagaStage      string
	GateTrail      string
	Routing        string
	Metadata       string
	ContextBytes   int
	// Materialized reports a step the ENGINE minted rather than one the pinned
	// definition declares — the `<step>-held` human step a tripped `hold_spread`
	// creates (payloads-thresholds §7.7 H4). Its spec is synthesized from the
	// pinned bytes, so nothing unpinned enters a run; the column exists so a
	// reader can tell a declared question from a computed one.
	Materialized bool
	// UsageRecorded is the v10 column group 1 writes on every `--usage` and
	// group 2's discrepancy probe reads (§2.3, §5.8 D2). It is the LEDGER'S OWN
	// answer to "did this step report", written in the recording transaction, so
	// the probe needs no join per step.
	UsageRecorded bool
	// WorkRoot is the checkout the step's work happened in — recorded at
	// complete/record via --worktree (v12, G8), read by the diff stage so a
	// resumed saga diffs the tree the work touched, not the resumer's cwd.
	WorkRoot    string
	CreatedAtMS int64
	UpdatedAtMS int64
	RowVersion  int
}

// Ref renders the step's `STEP-N` display identity.
func (s *Step) Ref() string { return model.FormatStepID(s.ID) }

// Lease returns the step's lease, for the same effective-status computation
// issues use. A step and an issue carry the same lease columns by contract
// (TDD §2), so they share the model type as well as the SQL.
func (s *Step) Lease() *model.Lease {
	return &model.Lease{
		Owner: s.Owner, TokenHash: s.TokenHash,
		ExpiresMS: s.ExpiresMS, Attempt: s.Attempt,
	}
}

// InSaga reports whether the step is mid-saga — recorded but not yet routed.
// Such a step needs NO lease to advance: the token retired at stage 1, so any
// later engine invocation may resume it (§6.8).
func (s *Step) InSaga() bool { return s.SagaStage != "" }

const stepFullSelect = `
SELECT id, run_id, issue_id, workflow_id, step_name, ordinal, sibling_index, instance,
       kind, executor, class, status, attempt, attempt_base, failed_attempts,
       reaped_claims, max_attempts, expected_cost,
       owner, token_hash, expires_ms, started_ms, activity_ms, saga_stage,
       gate_trail, routing, metadata, context_bytes, materialized, usage_recorded,
       created_at_ms, updated_at_ms, row_version, work_root
  FROM steps`

// GetStep reads one step by id.
func GetStep(db *sql.DB, id int) (*Step, error) {
	return scanStep(db.QueryRow(stepFullSelect+` WHERE id = ?`, id))
}

// GetStepTx is GetStep inside a transaction — every saga stage's reader, so the
// stage's guard and its mutation see one consistent row.
func GetStepTx(tx *sql.Tx, id int) (*Step, error) {
	return scanStep(tx.QueryRow(stepFullSelect+` WHERE id = ?`, id))
}

// ListRunSteps reads every step of a run, ordered by (issue, id) — creation
// order within an issue, which is declaration order, which is what the topology
// goldens compare against (§8.3).
func ListRunSteps(db *sql.DB, runID int) ([]*Step, error) {
	return scanSteps(db.Query(stepFullSelect+` WHERE run_id = ? ORDER BY issue_id, id`, runID))
}

// IssueStepRuns reads the ids of every run holding a step for one issue, in
// run order. `step list --issue ISSUE-N` needs it because an issue's steps are
// not confined to one run — a re-activation mints a fresh round under a new
// run — and the caller who asks about an issue rarely knows which runs those
// are (DKT-244).
func IssueStepRuns(db *sql.DB, issueID int) ([]int, error) {
	rows, err := db.Query(
		`SELECT DISTINCT run_id FROM steps WHERE issue_id = ? ORDER BY run_id`, issueID)
	if err != nil {
		return nil, fmt.Errorf("listing an issue's runs: %w", err)
	}
	return scanRows(rows, "run ids", func(r *sql.Rows) (int, error) {
		var id int
		return id, r.Scan(&id)
	})
}

// ListRunStepsTx is ListRunSteps inside a transaction — the readiness
// predicate's reader, which must see one consistent snapshot of the run because
// R3 (predecessors done) and R4 (scope non-overlap) are questions about the
// same set of rows at the same instant.
func ListRunStepsTx(tx *sql.Tx, runID int) ([]*Step, error) {
	return scanSteps(tx.Query(stepFullSelect+` WHERE run_id = ? ORDER BY issue_id, id`, runID))
}

// ListIssueStepsTx reads every step of ONE issue in a run, inside a
// transaction — the `after_fired` cascade's reader (DKT-1085), which must see
// the rows the open routing transaction just terminalized and needs no other
// issue's steps to answer its question.
func ListIssueStepsTx(tx *sql.Tx, runID, issueID int) ([]*Step, error) {
	return scanSteps(tx.Query(
		stepFullSelect+` WHERE run_id = ? AND issue_id = ? ORDER BY id`, runID, issueID))
}

// ListActiveRunSteps reads the steps of every non-terminal run — `guard stop`'s
// reader (§6.12) and the scope-conflict check's, since a claimed step in ANOTHER
// active run excludes just as surely as one in this run.
func ListActiveRunSteps(db *sql.DB) ([]*Step, error) {
	return scanSteps(db.Query(
		stepFullSelect + `
		 WHERE run_id IN (SELECT id FROM runs WHERE status NOT IN ('done', 'abandoned'))
		 ORDER BY run_id, issue_id, id`))
}

func scanStep(row *sql.Row) (*Step, error) { return scanOneStep(row) }

func scanSteps(rows *sql.Rows, err error) ([]*Step, error) {
	if err != nil {
		return nil, fmt.Errorf("listing steps: %w", err)
	}
	return scanRows(rows, "steps", func(r *sql.Rows) (*Step, error) { return scanOneStep(r) })
}

// rowScannerFor abstracts *sql.Row and *sql.Rows so one column-order-sensitive
// scan serves both. The column list is long enough that two copies of it would
// drift on the next added column.
type rowScannerFor interface{ Scan(dest ...any) error }

func scanOneStep(s rowScannerFor) (*Step, error) {
	var (
		step      Step
		sibling   sql.NullInt64
		executor  sql.NullString
		class     sql.NullString
		maxAtt    sql.NullInt64
		owner     sql.NullString
		tokenHash sql.NullString
		expires   sql.NullInt64
		started   sql.NullInt64
		activity  sql.NullInt64
		saga      sql.NullString
		gateTrail sql.NullString
		routing   sql.NullString
		metadata  sql.NullString
		ctxBytes  sql.NullInt64
		mat       sql.NullInt64
		usageRec  sql.NullInt64
		workRoot  sql.NullString
	)
	err := s.Scan(
		&step.ID, &step.RunID, &step.IssueID, &step.WorkflowID, &step.StepName,
		&step.Ordinal, &sibling, &step.Instance, &step.Kind, &executor, &class,
		&step.Status, &step.Attempt, &step.AttemptBase, &step.FailedAttempts,
		&step.ReapedClaims, &maxAtt, &step.ExpectedCost,
		&owner, &tokenHash, &expires, &started, &activity, &saga,
		&gateTrail, &routing, &metadata, &ctxBytes, &mat, &usageRec,
		&step.CreatedAtMS, &step.UpdatedAtMS, &step.RowVersion, &workRoot,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrStepNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading step: %w", err)
	}

	if sibling.Valid {
		v := int(sibling.Int64)
		step.SiblingIndex = &v
	}
	if maxAtt.Valid {
		v := int(maxAtt.Int64)
		step.MaxAttempts = &v
	}
	if started.Valid {
		v := started.Int64
		step.StartedMS = &v
	}
	if activity.Valid {
		v := activity.Int64
		step.ActivityMS = &v
	}
	step.Executor, step.Class = executor.String, class.String
	step.Owner, step.TokenHash, step.ExpiresMS = owner.String, tokenHash.String, expires.Int64
	step.SagaStage, step.GateTrail, step.Routing = saga.String, gateTrail.String, routing.String
	step.Metadata = metadata.String
	step.ContextBytes = int(ctxBytes.Int64)
	step.Materialized = mat.Int64 != 0
	step.UsageRecorded = usageRec.Int64 != 0
	step.WorkRoot = workRoot.String
	return &step, nil
}

// ClaimStepTx takes a lease on a step inside the caller's transaction, through
// the SAME generalized implementation issues use (TDD §6.6).
//
// It is transaction-scoped rather than standalone because `step claim` must
// mint the token and assemble the §11.4 context bundle in ONE transaction — "an
// unclaimed executor has nothing, a claimed one has everything" (engine-core
// §8) — so the caller owns the transaction and this is one statement inside it.
func ClaimStepTx(
	tx *sql.Tx, id int, owner string, ttlMS, nowMS int64,
) (token string, lease *model.Lease, err error) {
	if owner == "" {
		return "", nil, fmt.Errorf("claiming step: owner must not be empty")
	}
	token, hash, err := model.MintToken()
	if err != nil {
		return "", nil, err
	}
	lease, err = leaseSteps.claimTx(tx, id, owner, hash, nowMS+ttlMS, nowMS)
	if err != nil {
		return "", nil, err
	}
	return token, lease, nil
}

// RefreshClaimLeaseTx recomputes a live claim's `expires_ms` from the caller's
// own time, guarded by the claim identity (TDD docs/tdd/gates-trust.md
// §7.6.1.1 LR1/LR2).
//
// WHY IT EXISTS: a claim with pre-gates runs subprocesses between the CAS and
// the response, and all of that wall time would otherwise be deducted from a
// lease the caller has not yet received. With a short TTL the caller can be
// handed an ALREADY-EXPIRED lease, so its first `step complete` fails on a
// lease it never had a chance to use — a livelock shaped like a too-short TTL
// but caused by docket's own pre-gate phase.
//
// IT IS A HEARTBEAT BY ANOTHER NAME, NOT A SECOND AUTHORIZATION. The guard is
// the token hash transaction A wrote: the claimant still holds the CAS, which
// is exactly the condition the lease model already sanctions for extending a
// live claim. It can only FAIL, never award a claim (LR4), so the single-winner
// property is untouched — a claim lost during phase 2 matches zero rows here
// and the caller gets CONFLICT in the ordinary way.
//
// It reports whether the refresh landed rather than erroring on a miss, because
// "the claim moved on" is an expected outcome the caller renders as CONFLICT,
// not a database failure.
func RefreshClaimLeaseTx(
	tx *sql.Tx, id int, tokenHash string, ttlMS, nowMS int64,
) (bool, error) {
	res, err := tx.Exec(
		`UPDATE steps SET expires_ms = ?, activity_ms = ?, updated_at_ms = ?
		  WHERE id = ? AND token_hash = ?`,
		nowMS+ttlMS, nowMS, nowMS, id, tokenHash)
	if err != nil {
		return false, fmt.Errorf("refreshing the claim lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("refreshing the claim lease: %w", err)
	}
	return n > 0, nil
}

// ClaimStep is the standalone claim, for tests and for any caller that needs
// only the lease. `step claim` uses ClaimStepTx.
func ClaimStep(db *sql.DB, id int, owner string, ttlMS, nowMS int64) (string, *model.Lease, error) {
	return leaseSteps.claim(db, id, owner, ttlMS, nowMS)
}

// GetStepLease reads a step's lease without writing anything (§6.3: reads never
// write).
func GetStepLease(db *sql.DB, id int) (*model.Lease, error) {
	return leaseSteps.getLease(db, id)
}

// HeartbeatStep extends a live lease held by token. attempt is untouched — a
// heartbeat is not a new claim.
func HeartbeatStep(db *sql.DB, id int, token string, ttlMS, nowMS int64) (*model.Lease, error) {
	return leaseSteps.heartbeat(db, id, token, ttlMS, nowMS)
}

// AuthorizeStepTx is §6.8 stage 0's holder check and §6.9's R1-R4 in one call:
// the token must hold a LIVE lease on the step. It is leaseSteps.authorize,
// exported under a name the saga reads naturally.
func AuthorizeStepTx(tx *sql.Tx, id int, token string, nowMS int64) (*model.Lease, error) {
	return leaseSteps.authorize(tx, id, token, nowMS)
}

// AuthorizeStepRead is AuthorizeStepTx's predicate over a READ, for the one
// caller that must refuse a non-holder before opening a write transaction:
// stage 0's payload validation (payloads-thresholds §4.8 C6). It shares the
// predicate rather than restating it, so the two cannot disagree, and it is
// ADVISORY — AuthorizeStepTx remains the authority.
func AuthorizeStepRead(db *sql.DB, id int, token string, nowMS int64) error {
	return leaseSteps.authorizeRead(db, id, token, nowMS)
}

// RetireStepTokenTx is §6.8 stage 1's hinge: the token retires when the
// artifact records.
//
// Retirement is the same state change as release — no owner, no hash, no
// expiry — so it is the shared clearLeaseTx rather than a second UPDATE that
// must agree with it. What differs is the AUTHORITY: release is the holder
// giving the lease back, retirement is the engine taking ownership of a saga
// that no longer needs one. After this commits, `complete` is AUTH_ERROR (R9)
// and any engine invocation may resume the saga.
func RetireStepTokenTx(tx *sql.Tx, id int) error {
	return leaseSteps.clearLeaseTx(tx, id)
}

// SetStepStatusTx moves a step's status inside the caller's transaction,
// bumping the CAS column and refreshing `updated_at_ms`.
//
// `activityMS` refreshes the saga's activity clock when non-zero — §6.8's
// "every stage commit refreshing the step's activity clock". Passing 0 leaves
// it alone, which is what a non-saga transition wants.
func SetStepStatusTx(tx *sql.Tx, id int, status string, nowMS, activityMS int64) error {
	query := `UPDATE steps SET status = ?, updated_at_ms = ?, row_version = row_version + 1`
	args := []any{status, nowMS}
	if activityMS > 0 {
		query += `, activity_ms = ?`
		args = append(args, activityMS)
	}
	query += ` WHERE id = ?`
	args = append(args, id)

	res, err := tx.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("setting step status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("setting step status: %w", err)
	}
	if n == 0 {
		return ErrStepNotFound
	}
	return nil
}

// StartStepTx stamps `started_ms` — the schedule-to-close clock
// `max_step_duration` is evaluated against (§6.3).
//
// It is set at CLAIM, not at first heartbeat, and only when NULL. That is what
// makes the bound schedule-to-close rather than activity-to-close: a runaway
// holder that heartbeats forever cannot push its own deadline out, because the
// deadline was fixed the moment it took the lease. A re-claim after expiry
// re-stamps it (the new holder gets its own full budget); the `IS NULL` guard
// is against the same holder's later stages, not against a new attempt.
func StartStepTx(tx *sql.Tx, id int, nowMS int64) error {
	_, err := tx.Exec(
		`UPDATE steps SET started_ms = ?, activity_ms = ? WHERE id = ? AND started_ms IS NULL`,
		nowMS, nowMS, id,
	)
	if err != nil {
		return fmt.Errorf("stamping step start: %w", err)
	}
	return nil
}

// ClearStepStartTx clears the schedule-to-close clock, so a step returned to
// the unclaimed pool starts a fresh budget on its next claim. Reaping and
// explicit failure both go through it.
func ClearStepStartTx(tx *sql.Tx, id int) error {
	_, err := tx.Exec(`UPDATE steps SET started_ms = NULL WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("clearing step start: %w", err)
	}
	return nil
}

// AdvanceSagaTx moves a step's saga stage under a CAS guard on the CURRENT
// stage — §6.8's "each stage's transaction is WHERE saga_stage = <expected>
// CAS-guarded, so two concurrent engine invocations resuming the same saga
// produce exactly one advance".
//
// The loser matches zero rows and gets ErrSagaStageMoved, which is not an error
// condition: it re-reads and either finds the saga finished or advances the
// stage that is now current. This is the whole of the concurrent-resume story,
// and it is one WHERE clause because a saga guarded by a read-then-write would
// have the window this exists to close.
//
// `from` is "" for the NULL stage (entering the saga) and `to` is "" to leave
// it (the saga completing).
func AdvanceSagaTx(tx *sql.Tx, id int, from, to string, nowMS int64) error {
	query := `UPDATE steps
	             SET saga_stage = ?, activity_ms = ?, updated_at_ms = ?,
	                 row_version = row_version + 1
	           WHERE id = ? AND `
	args := []any{nullable(to), nowMS, nowMS, id}
	if from == "" {
		query += `saga_stage IS NULL`
	} else {
		query += `saga_stage = ?`
		args = append(args, from)
	}

	res, err := tx.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("advancing saga: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("advancing saga: %w", err)
	}
	if n == 0 {
		return ErrSagaStageMoved
	}
	return nil
}

// SetStepGateTrailTx records the accumulated gate results (TDD §6.1's
// `gate_trail`, the recorded storage-location deviation: §11.4's `gate
// result` shape rides here until v8's `gate_results` table).
func SetStepGateTrailTx(tx *sql.Tx, id int, trail string, nowMS int64) error {
	_, err := tx.Exec(
		`UPDATE steps SET gate_trail = ?, activity_ms = ?, updated_at_ms = ?,
		        row_version = row_version + 1
		  WHERE id = ?`,
		nullable(trail), nowMS, nowMS, id,
	)
	if err != nil {
		return fmt.Errorf("recording gate trail: %w", err)
	}
	return nil
}

// SetStepRoutingTx records the routing a step resolved to, alongside its final
// status. They are ONE statement because they are one fact — the step ended
// this way, for this reason — and a status without its routing is a step whose
// disposition cannot be explained.
func SetStepRoutingTx(tx *sql.Tx, id int, routing, status string, nowMS int64) error {
	_, err := tx.Exec(
		`UPDATE steps SET routing = ?, status = ?, activity_ms = ?, updated_at_ms = ?,
		        row_version = row_version + 1
		  WHERE id = ?`,
		nullable(routing), status, nowMS, nowMS, id,
	)
	if err != nil {
		return fmt.Errorf("recording step routing: %w", err)
	}
	return nil
}

// SetStepMetadataTx records a step's opaque KV bag, already merged.
//
// The merge itself is engine-side, in Go (engine.mergeMetadata), so this stays
// a plain assignment and no JSON function dependency enters the schema layer.
// The bag is OPAQUE here as everywhere: this writes bytes it never parses.
//
// It follows SetStepRoutingTx's shape — same row_version bump, same
// updated_at_ms — because a metadata write is a step-row mutation like any
// other and CAS-guarded readers must see it move.
func SetStepMetadataTx(tx *sql.Tx, id int, metadata string, nowMS int64) error {
	_, err := tx.Exec(
		`UPDATE steps SET metadata = ?, updated_at_ms = ?,
		        row_version = row_version + 1
		  WHERE id = ?`,
		nullable(metadata), nowMS, id,
	)
	if err != nil {
		return fmt.Errorf("recording step metadata: %w", err)
	}
	return nil
}

// ReapStepTx returns an expired step to the unclaimed pool: lease cleared,
// status back to `pending`, `started_ms` cleared, `attempt` LEFT ALONE.
//
// attempt already counted this try — it incremented at claim — so incrementing
// again here would double-count a single death. §6.3's "returning them to ready
// with attempt++" is satisfied by the claim's own increment: the trail records
// one attempt per claim, for all time (claims-leases §5), which is exactly what
// §9 item 4's "attempt trail is complete" asks for.
//
// It is MECHANISM, not classification: `step fail`'s below-the-budget branch
// shares it to return a failed step to the pool, so the outcome counters
// (DKT-490) deliberately live outside it — each caller records what actually
// ended the claim, MarkStepClaimReapedTx on the reap paths and
// MarkStepAttemptFailedTx on the failure paths. Folding either bump in here
// would classify a failure as a reap at exactly the call site that knows
// better.
//
// This is one of the two places a lease write may happen (§6.3: "lazy reaping
// confined to next/claim"). No read verb reaches it.
func ReapStepTx(tx *sql.Tx, id int, nowMS int64) error {
	_, err := tx.Exec(
		`UPDATE steps
		    SET owner = NULL, token_hash = NULL, expires_ms = NULL, started_ms = NULL,
		        status = ?, updated_at_ms = ?, row_version = row_version + 1
		  WHERE id = ?`,
		StepPending, nowMS, id,
	)
	if err != nil {
		return fmt.Errorf("reaping step lease: %w", err)
	}
	return nil
}

// ReleaseStepLeaseTx returns a step to the UNCLAIMED pool without touching its
// status, its attempt, or its saga (DKT-259).
//
// It exists because `pending` and a live lease are a CONTRADICTION that the
// system had no way to express and every reader disagreed about.
// `claimPredicate` says a step is claimable only when `owner IS NULL OR owner
// = ” OR expires_ms <= now`, so a step returned to `pending` with its lease
// intact is a step the scheduler offers and no claimant can take. What CAN
// still happen is the worst case: the ORIGINAL holder's token is still valid,
// so it re-records without re-claiming — and `attempt` increments only at
// claim, so the second execution lands on the first one's attempt number.
//
// RUN-13 STEP-132 is what that costs. The step ran twice with two gate rounds
// and two artifact sets, `run report` said `attempts: 1`, and one execution's
// usage was permanently unrecordable because `usage_ledger`'s
// `UNIQUE(step_id, attempt, unit)` had already been taken by the other.
//
// It is deliberately NARROWER than ReapStepTx: no status write, no
// `started_ms` clear. A reap decides what the step becomes; this decides only
// that nobody holds it, and leaves the caller to say the rest. Two callers
// with different intentions sharing one helper is how a release grows a status
// write that surprises one of them.
func ReleaseStepLeaseTx(tx *sql.Tx, id int, nowMS int64) error {
	_, err := tx.Exec(
		`UPDATE steps
		    SET owner = NULL, token_hash = NULL, expires_ms = NULL,
		        updated_at_ms = ?, row_version = row_version + 1
		  WHERE id = ?`,
		nowMS, id,
	)
	if err != nil {
		return fmt.Errorf("releasing the step lease: %w", err)
	}
	return nil
}

// MarkStepAttemptFailedTx counts one claim ended by an explicit `step fail`
// into the step's outcome breakdown (v23, DKT-490).
//
// It exists because `attempt` spends one count per claim WHATEVER the ending,
// and a consumer that needed "how many attempts genuinely failed" had nothing
// to read but the event log — which is prunable, and whose instance labels
// repeat across a run's issues. Both `step fail` branches call it (the
// below-budget return to the pool and the exhausted routing): each is a claim
// whose holder measured its own work and recorded the failure.
//
// It is a COUNTER BUMP ONLY — the lease write it accompanies (ReapStepTx or
// RetireStepTokenTx) stays the caller's, per ReapStepTx's mechanism/
// classification split. It follows SetStepMetadataTx's shape — row_version
// bump, updated_at_ms — because a counter move is a step-row mutation
// CAS-guarded readers must see.
func MarkStepAttemptFailedTx(tx *sql.Tx, id int, nowMS int64) error {
	_, err := tx.Exec(
		`UPDATE steps SET failed_attempts = failed_attempts + 1, updated_at_ms = ?,
		        row_version = row_version + 1
		  WHERE id = ?`,
		nowMS, id,
	)
	if err != nil {
		return fmt.Errorf("counting the failed attempt: %w", err)
	}
	return nil
}

// MarkStepClaimReapedTx counts one claim reaped WITHOUT a recorded failure
// into the step's outcome breakdown (v23, DKT-490) — MarkStepAttemptFailedTx's
// other half, called on the reap paths only: the lazy expiry reap (`next`,
// `dispatch open`, `claim`) and the forced `step reap`.
//
// The distinction is the whole point. A reaped claim spent an `attempt`
// without anything failing — the holder went silent, or an operator asserted
// it dead — and an escalation policy that cannot tell that from a measured
// failure escalates on it, which is the DKT-490 misread. `step fail`'s return
// to the pool shares ReapStepTx's row reset but never this counter.
func MarkStepClaimReapedTx(tx *sql.Tx, id int, nowMS int64) error {
	_, err := tx.Exec(
		`UPDATE steps SET reaped_claims = reaped_claims + 1, updated_at_ms = ?,
		        row_version = row_version + 1
		  WHERE id = ?`,
		nowMS, id,
	)
	if err != nil {
		return fmt.Errorf("counting the reaped claim: %w", err)
	}
	return nil
}

// ResetStepRetryBudgetTx refreshes a step instance's retry budget — `step
// resolve --as retry` (§2: "retry = attempts reset") — by moving `attempt_base`
// to the current attempt. Exhaustion compares `attempt - attempt_base` against
// `max_attempts`, so the budget reads zero-spent from here on.
//
// `attempt` ITSELF IS NEVER RESET (DKT-86, DKT-90). It is the usage ledger's
// key half (`UNIQUE(step_id, attempt, unit)`) and §11.4's "claims made against
// this step, ever": zeroing it here made a retried step's next claim re-mint an
// attempt number the ledger had already recorded, and the re-execution's
// genuinely distinct usage became permanently unrecordable through `dispatch
// backfill-usage`. The retry budget and the ledger key are different facts, and
// the base column is what lets one column serve as neither's lie.
//
// This is still a DIFFERENT counter from the issue-level `attempt` v6 declared
// monotonic, on a different entity, and claims-leases §5 anticipated exactly
// this: the step's budget is live against `max_attempts`, the issue's trail is
// permanent. Both statements stay true because they are about different rows.
func ResetStepRetryBudgetTx(tx *sql.Tx, id int, nowMS int64) error {
	_, err := tx.Exec(
		`UPDATE steps SET attempt_base = attempt, updated_at_ms = ?,
		        row_version = row_version + 1
		  WHERE id = ?`,
		nowMS, id,
	)
	if err != nil {
		return fmt.Errorf("resetting the step retry budget: %w", err)
	}
	return nil
}

// ExemptStepAttemptFromBudgetTx exempts ONE spent attempt from a step's retry
// budget — the forced reap's classification (DKT-585) — by moving
// `attempt_base` forward by exactly one. Exhaustion compares
// `attempt - attempt_base` against `max_attempts`, so from here on the budget
// reads one fewer spent: the attempt still happened, it just does not count
// against the declared allowance.
//
// It is a NUDGE, not ResetStepRetryBudgetTx's reset. A forced reap asserts
// that ONE claim's holder is gone — a relay declaring a dead spawn is not an
// executor failure (RUN-30 STEP-755: a wave stopped by an accidental
// interrupt consumed the step's last attempt, leaving it one interrupt from
// `waiting-human` on a healthy charter) — and the exemption must be as narrow
// as the assertion: this one attempt, nothing else. `attempt_base = attempt`
// here would also forgive every EARLIER genuinely-failed attempt, silently
// granting a full fresh budget on a verb that never claimed to be a retry.
//
// `attempt` ITSELF IS UNTOUCHED, for exactly ResetStepRetryBudgetTx's reason
// (DKT-86, DKT-90): it is the usage ledger's key half (`UNIQUE(step_id,
// attempt, unit)`), and the dead attempt's usage stays back-fillable against
// its own attempt number after this runs — RUN-30's dead attempt had real
// measured usage back-filled, and decrementing the counter would have made
// that row collide with the successor's, the RUN-13 STEP-132 failure mode
// ReleaseStepLeaseTx documents.
//
// The `attempt_base < attempt` guard keeps the base at or below the counter.
// Without it, reaching this twice for one claim — or on a row whose base has
// already caught up — would push the base PAST the counter, and
// `attempt - attempt_base` would go negative: budget minted out of nothing.
func ExemptStepAttemptFromBudgetTx(tx *sql.Tx, id int, nowMS int64) error {
	_, err := tx.Exec(
		`UPDATE steps SET attempt_base = attempt_base + 1, updated_at_ms = ?,
		        row_version = row_version + 1
		  WHERE id = ? AND attempt_base < attempt`,
		nowMS, id,
	)
	if err != nil {
		return fmt.Errorf("exempting the reaped attempt from the retry budget: %w", err)
	}
	return nil
}

// Artifact is one `artifacts` row: what a step produced.
type Artifact struct {
	ID     int
	RunID  int
	StepID int
	Kind   string
	Body   string
	// Payload is the structured half, validated for SHAPE only at S3 (§6.8
	// stage 0); the schema register is S5's. It is JSON text or "".
	Payload string
	SHA256  string
	// Stub is 1 ONLY on artifacts the S3/S4 stub action runner produced, and it
	// is `omitempty` for the reason gate_results' own marker is: a result THIS
	// stage produces serializes with no `stub` key at all (§6.3 S4).
	//
	// NOTHING WRITTEN FROM NOW ON SETS IT. The column exists so a migrated
	// artifact stays distinguishable forever — the migration marks the rows
	// whose payload carries the old `{"stub":true,…}` wrapper and leaves their
	// BYTES alone, because rewriting them would destroy the evidence that a
	// computation did not run.
	Stub bool `json:"stub,omitempty"`
	// Supersedes names the artifact this one REVISES, or nil for an original
	// (v15, DKT-70).
	//
	// A held cluster's resolution records a new artifact rather than annotating
	// the old one (H13). It shares the original's `kind`; its body is
	// regenerated from the resolved payload and its `sha256` covers body and
	// payload both (DKT-112), so two links of the chain never share a content
	// address while their payloads differ.
	//
	// The pointer says which of the two is the record of a computation and
	// which is the record of a decision, WITHOUT rewriting either. A consumer
	// counting work counts `Supersedes == nil`.
	Supersedes  *int `json:"supersedes,omitempty"`
	CreatedAtMS int64
}

// ArtifactMaxBytes is §3's explicit cap. An artifact over it is a
// VALIDATION_ERROR naming the size and the cap — a refusal rather than a
// truncation, because a silently truncated artifact is one a downstream step
// consumes as if it were whole.
const ArtifactMaxBytes = 1 << 20

// InsertArtifactTx records one artifact inside the caller's transaction — §6.8
// stage 1, alongside the token's retirement, because "the token retires when
// the artifact records" is one commit or it is not the hinge it is specified
// to be.
func InsertArtifactTx(tx *sql.Tx, a Artifact, nowMS int64) (int, error) {
	res, err := tx.Exec(
		`INSERT INTO artifacts
		   (run_id, step_id, kind, body, payload, sha256, supersedes, created_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.RunID, nullableInt(a.StepID), a.Kind, a.Body, nullable(a.Payload), a.SHA256,
		nullableIntPtr(a.Supersedes), nowMS,
	)
	if err != nil {
		return 0, fmt.Errorf("recording artifact: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading artifact id: %w", err)
	}
	return int(id), nil
}

const artifactSelect = `
SELECT id, run_id, step_id, kind, body, payload, sha256, stub, supersedes, created_at_ms
  FROM artifacts`

// ListRunArtifacts reads a run's artifacts ordered by id — the tie-break
// §6.7's resolution order ends on, so the read order and the resolution order
// agree without a second sort.
func ListRunArtifacts(db *sql.DB, runID int) ([]*Artifact, error) {
	return scanArtifacts(db.Query(artifactSelect+` WHERE run_id = ? ORDER BY id`, runID))
}

// ListRunArtifactsTx is ListRunArtifacts inside a transaction — context
// assembly's reader, which must see the artifact set as of the claim it is
// part of.
func ListRunArtifactsTx(tx *sql.Tx, runID int) ([]*Artifact, error) {
	return scanArtifacts(tx.Query(artifactSelect+` WHERE run_id = ? ORDER BY id`, runID))
}

// ListStepArtifacts reads the artifacts ONE step produced, ordered by id.
//
// The run-scoped reader above answers "what does this run hold", which is the
// question context assembly and the run report ask. This answers "what did
// this step produce", which is the question an operator reading a finished
// step asks — and which previously had no answer short of opening
// .docket/issues.db with sqlite.
//
// `step_id` is nullable in the schema (a run-scoped artifact has none), so
// this deliberately matches on equality and never returns those: an artifact
// with no producing step is not this step's output.
func ListStepArtifacts(db *sql.DB, stepID int) ([]*Artifact, error) {
	return scanArtifacts(db.Query(artifactSelect+` WHERE step_id = ? ORDER BY id`, stepID))
}

// GetArtifactTx reads ONE artifact by id, inside a transaction — the reader
// for an id pinned at activation (DKT-547's `issue.linked` form), where the
// row may belong to another run entirely and the run-scoped listing above
// cannot reach it. ErrNotFound when no such row exists.
func GetArtifactTx(tx *sql.Tx, id int) (*Artifact, error) {
	out, err := scanArtifacts(tx.Query(artifactSelect+` WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out[0], nil
}

func scanArtifacts(rows *sql.Rows, err error) ([]*Artifact, error) {
	if err != nil {
		return nil, fmt.Errorf("listing artifacts: %w", err)
	}
	return scanRows(rows, "artifacts", func(r *sql.Rows) (*Artifact, error) {
		var (
			a          Artifact
			stepID     sql.NullInt64
			payload    sql.NullString
			stub       sql.NullInt64
			supersedes sql.NullInt64
		)
		if err := r.Scan(
			&a.ID, &a.RunID, &stepID, &a.Kind, &a.Body, &payload, &a.SHA256,
			&stub, &supersedes, &a.CreatedAtMS,
		); err != nil {
			return nil, fmt.Errorf("reading artifact: %w", err)
		}
		a.StepID = int(stepID.Int64)
		a.Payload = payload.String
		a.Stub = stub.Int64 != 0
		if supersedes.Valid {
			id := int(supersedes.Int64)
			a.Supersedes = &id
		}
		return &a, nil
	})
}

// InsertStepInputTx records that a step consumed an artifact at a declared
// position — the resolution §6.7 computed, MATERIALIZED.
//
// The record exists because resolution is a function of run state at ASSEMBLY
// time, and run state moves: a later ordinal's artifact would re-resolve the
// same input differently. Storing what was actually handed over is what makes
// the ledger answer "what did this step see" rather than "what would it see
// now" — and ListStepInputArtifactsTx is how a read-back asks it (DKT-1054).
func InsertStepInputTx(tx *sql.Tx, stepID, position, artifactID int) error {
	_, err := tx.Exec(
		`INSERT OR REPLACE INTO step_inputs (step_id, position, artifact_id)
		 VALUES (?, ?, ?)`,
		stepID, position, artifactID,
	)
	if err != nil {
		return fmt.Errorf("recording step input: %w", err)
	}
	return nil
}

// ClearStepInputsTx drops a step's recorded input bindings, so a re-claim can
// record the bindings of the attempt that is about to run in their place
// (DKT-1054).
//
// The table's key is (step, position, artifact), so without this a retried
// step's second claim would ADD its bindings beside the first attempt's rather
// than replace them, and a read-back would see the union of two attempts —
// neither of which any executor saw. The step's snapshot is the snapshot of
// its CURRENT attempt; a lapsed or retried attempt's bindings are history the
// event log keeps, not a second answer to "what did this step see".
func ClearStepInputsTx(tx *sql.Tx, stepID int) error {
	if _, err := tx.Exec(`DELETE FROM step_inputs WHERE step_id = ?`, stepID); err != nil {
		return fmt.Errorf("clearing step inputs: %w", err)
	}
	return nil
}

// ListStepInputArtifactsTx reads the artifacts a step's claim RECORDED as its
// inputs (InsertStepInputTx), ordered by id like every other artifact reader —
// the snapshot `step context` replays for a step that has been handed out
// (DKT-1054).
//
// It is the artifact rows themselves, not the (position, artifact) pairs,
// because the reader re-runs §6.7's resolution over exactly this set: the
// declared-position order, the engine-produced forms that bind no artifact
// (`issue.body`, an empty `issue.diff`, `gate-results`), and the within-input
// sort all come from the same code the claim ran, so a replayed bundle has
// the claim-time bundle's shape by construction rather than by a second
// rendering of it. Empty for a step whose claim bound no artifact at all.
func ListStepInputArtifactsTx(tx *sql.Tx, stepID int) ([]*Artifact, error) {
	return scanArtifacts(tx.Query(
		artifactSelect+` WHERE id IN (SELECT artifact_id FROM step_inputs WHERE step_id = ?)
		 ORDER BY id`, stepID))
}

// nullableInt maps 0 to SQL NULL, for optional foreign keys.
func nullableInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// nullableIntPtr maps an unset pointer to NULL. It is separate from
// nullableInt because the two encode DIFFERENT absences: a zero id means "no
// row" for a column that always has one when it means anything, while a nil
// pointer means "this artifact revises nothing" — a fact, not a missing value.
func nullableIntPtr(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}
