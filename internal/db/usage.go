package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// The usage ledger and the budget columns v10 added
// (docs/tdd/runs-dispatch.md §3.1, §4).
//
// EVERY NUMBER IN HERE IS OPAQUE. `unit` is whatever the caller wrote —
// `tokens`, `seconds`, `pages`, `sheets` — and core never enumerates units,
// never has a default unit, and never converts between them. The sums below are
// PER-UNIT for exactly that reason: a sum across units would be core deciding
// they are commensurable, which is an opinion about what they mean
// (docs/design/genericity.md).

// UsageRow is one `usage_ledger` row: what a step reported, in one unit, on one
// attempt.
type UsageRow struct {
	RunID    int
	StepID   int
	Attempt  int
	Unit     string
	Quantity float64
	// Source is engine-core §7's "source recorded". Core writes
	// UsageSourceReported and nothing else at this stage; the column exists so a
	// harness back-filling from its own journal can record its own source later
	// without a migration.
	Source string
}

// UsageSourceReported is the only source core writes at completion: a claimant
// said so. A back-fill supplies its own (engine.UsageSourceBackfilled).
const UsageSourceReported = "reported"

// InsertVoteUsageTx records one unit's quantity against one SEAT's cast in the
// vote_usage ledger, source explicit (v17, DKT-115): the cast-time writer
// passes UsageSourceReported, the vote-scoped back-fill its own source. The
// (vote_id, unit) key firing maps to ErrUsageAlreadyRecorded so each caller
// can phrase the refusal for whoever hit it — the same split the step ledger's
// writer makes.
func InsertVoteUsageTx(
	tx *sql.Tx, voteID int64, unit string, quantity float64, source string, nowMS int64,
) error {
	if source == "" {
		source = UsageSourceReported
	}
	_, err := tx.Exec(
		`INSERT INTO vote_usage (vote_id, unit, quantity, source, created_at_ms)
		 VALUES (?, ?, ?, ?, ?)`,
		voteID, unit, quantity, source, nowMS)
	if err != nil {
		if isUniqueOrPKConflict(err) {
			return fmt.Errorf("unit %q: %w", unit, ErrUsageAlreadyRecorded)
		}
		return fmt.Errorf("recording vote usage %q: %w", unit, err)
	}
	return nil
}

// ErrUsageAlreadyRecorded is the (step_id, attempt, unit) key firing — this
// unit was already recorded for this attempt.
//
// It is a SENTINEL rather than a raw constraint error so a caller can phrase
// the refusal for whoever hit it: the same violation means "the claimant
// already reported this" at completion and "you are back-filling twice" at
// back-fill, and the two want different sentences.
var ErrUsageAlreadyRecorded = errors.New("usage already recorded for this step, attempt, and unit")

// InsertUsageRowTx records one unit's quantity for one attempt of one step.
//
// The unique key is (step_id, attempt, unit), so a reaped-and-reclaimed step's
// SECOND attempt records beside the first rather than overwriting it — which is
// what makes "retries re-accrue" true on the reported side as well as on the
// floor side. A second `complete` for the SAME attempt is impossible (the saga's
// stage-0 CAS), so the key is a belt-and-braces assertion rather than an upsert:
// if it ever fires, something upstream broke a guarantee and silently merging
// the rows would hide it.
func InsertUsageRowTx(tx *sql.Tx, row UsageRow, nowMS int64) error {
	source := row.Source
	if source == "" {
		source = UsageSourceReported
	}
	_, err := tx.Exec(
		`INSERT INTO usage_ledger
		   (run_id, step_id, attempt, unit, quantity, source, created_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		row.RunID, row.StepID, row.Attempt, row.Unit, row.Quantity, source, nowMS,
	)
	if err != nil {
		if isUniqueOrPKConflict(err) {
			return fmt.Errorf("step %s, attempt %d, unit %q: %w",
				model.FormatStepID(row.StepID), row.Attempt, row.Unit,
				ErrUsageAlreadyRecorded)
		}
		return fmt.Errorf("recording usage for step %s: %w",
			model.FormatStepID(row.StepID), err)
	}
	return nil
}

// MarkStepUsageRecordedTx sets `steps.usage_recorded` — the fast path group 2's
// discrepancy probe reads (§2.3).
//
// Group 1 writes it so the column is populated from the moment the ledger
// exists: a probe that arrived later and found the column empty on steps that
// DID record usage would report every one of them as a discrepancy.
//
// WHAT THE COLUMN MEANS is "the ledger question is SETTLED for this step", and
// it has exactly one reader — engine's `missingUsage` — which is what makes
// that reading the operative one. There are three ways to settle it: recording
// usage (budget.go), backfilling it (backfill.go), and an operator's
// `dispatch close --accept-missing-usage` (DKT-315), which settles the question
// without answering it. The three are distinguished in the RECORD — the ledger
// rows exist for the first two and not the third, and the acceptance rides in
// the `dispatch-closed` event — not in this flag, whose whole job is to let the
// probe skip a join.
func MarkStepUsageRecordedTx(tx *sql.Tx, stepID int) error {
	if _, err := tx.Exec(
		`UPDATE steps SET usage_recorded = 1 WHERE id = ?`, stepID); err != nil {
		return fmt.Errorf("marking usage recorded on step %s: %w",
			model.FormatStepID(stepID), err)
	}
	return nil
}

// ReportedUsageTx sums the ledger for ONE unit — B16's `reported`.
//
// One unit, never all of them. §4.5's whole argument is that summing
// `{tokens: 4000, seconds: 12}` to 4012 would be core asserting those add up.
func ReportedUsageTx(tx *sql.Tx, runID int, unit string) (float64, error) {
	var total float64
	err := tx.QueryRow(
		`SELECT COALESCE(SUM(quantity), 0) FROM usage_ledger
		  WHERE run_id = ? AND unit = ?`, runID, unit,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("summing reported usage for %s: %w",
			model.FormatRunID(runID), err)
	}
	return total, nil
}

// UnitTotal is one unit's rollup for the report (R2's reported-per-unit line).
type UnitTotal struct {
	Unit     string  `json:"unit"`
	Quantity float64 `json:"quantity"`
	Rows     int     `json:"rows"`
}

// UsageByUnit rolls the run's ledger up per unit, ordered by unit name.
//
// ORDERED BY A TOTAL KEY, never by map iteration (R9): the report is
// deterministic given the same rows, for the same golden-stability reason
// `referencedSchemas` is ordered.
func UsageByUnit(db *sql.DB, runID int) ([]UnitTotal, error) {
	rows, err := db.Query(
		`SELECT unit, SUM(quantity), COUNT(*) FROM usage_ledger
		  WHERE run_id = ? GROUP BY unit ORDER BY unit`, runID)
	if err != nil {
		return nil, fmt.Errorf("rolling up usage for %s: %w",
			model.FormatRunID(runID), err)
	}
	return scanRows(rows,
		fmt.Sprintf("usage rollup for %s", model.FormatRunID(runID)),
		func(r *sql.Rows) (UnitTotal, error) {
			var u UnitTotal
			if err := r.Scan(&u.Unit, &u.Quantity, &u.Rows); err != nil {
				return UnitTotal{}, fmt.Errorf("reading a usage rollup row: %w", err)
			}
			return u, nil
		})
}

// StepUsageRow is one row of the per-step ledger view: which step, which
// attempt, which unit, how much, and who measured it.
type StepUsageRow struct {
	Step     string  `json:"step"`
	Instance string  `json:"instance"`
	Attempt  int     `json:"attempt"`
	Unit     string  `json:"unit"`
	Quantity float64 `json:"quantity"`
	Source   string  `json:"source"`
}

// UsageByStep lists the run's ledger row by row, joined to each step's
// instance, ordered by (step, attempt, unit).
//
// It exists because nothing exposed per-step usage. `run report` rolled the
// ledger up per UNIT only, so the one question a back-fill's refusal sends you
// to answer — WHICH steps already have usage recorded — could not be answered
// from any read verb, and conductors hand-filtered batches by trial and error
// (DKT-241). The rollup is still the headline; this is the detail behind it.
//
// ORDERED BY A TOTAL KEY (R9), like UsageByUnit and for the same reason: two
// reports of the same rows must be byte-identical.
func UsageByStep(db *sql.DB, runID int) ([]StepUsageRow, error) {
	rows, err := db.Query(
		`SELECT u.step_id, s.instance, u.attempt, u.unit, u.quantity, u.source
		   FROM usage_ledger u JOIN steps s ON s.id = u.step_id
		  WHERE u.run_id = ?
		  ORDER BY u.step_id, u.attempt, u.unit`, runID)
	if err != nil {
		return nil, fmt.Errorf("listing per-step usage for %s: %w",
			model.FormatRunID(runID), err)
	}
	return scanRows(rows,
		fmt.Sprintf("per-step usage for %s", model.FormatRunID(runID)),
		func(r *sql.Rows) (StepUsageRow, error) {
			var (
				row    StepUsageRow
				stepID int
			)
			if err := r.Scan(&stepID, &row.Instance, &row.Attempt,
				&row.Unit, &row.Quantity, &row.Source); err != nil {
				return StepUsageRow{}, fmt.Errorf("reading a per-step usage row: %w", err)
			}
			row.Step = model.FormatStepID(stepID)
			return row, nil
		})
}

// BreachRunBudgetTx is B20 and B22: the run flips `active -> waiting-human` with
// its reason, CAS-guarded on the status it is moving FROM.
//
// The CAS is the whole mechanism (C6). Two invocations that both observe the cap
// crossed both call this; exactly one matches a row, and the loser writes
// neither a second reason nor — because the caller keys the event on this
// return — a second event. The guard is the STATUS ITSELF rather than a flag we
// maintain, so there is no second piece of state that could disagree with it.
//
// `reason` is written to BOTH `reason` and `breach_reason`. `reason` is the run
// machine's general "why is it parked" field that `run status` already renders,
// and `breach_reason` is the budget's own, so a later `pause` for an unrelated
// cause cannot overwrite the record of the breach.
//
// `pause_origin` is written in the SAME statement (DKT-305): this park is a
// RUN-LEVEL decision — it parks no step — and the reconciliation rollup reads
// that column to know it must not auto-resume. Before the column existed the
// rollup read `breach_reason` for the same purpose, which worked only because a
// breach is the one run-level park that leaves a second trace; an operator's
// `run pause` leaves none, and was silently undone.
func BreachRunBudgetTx(tx *sql.Tx, runID int, reason string, nowMS int64) (bool, error) {
	res, err := tx.Exec(
		`UPDATE runs
		    SET status = ?, reason = ?, breach_reason = ?, pause_origin = ?,
		        updated_at_ms = ?, row_version = row_version + 1
		  WHERE id = ? AND status = ?`,
		string(model.RunWaitingHuman), reason, reason,
		string(model.RunPauseOriginBudget), nowMS,
		runID, string(model.RunActive),
	)
	if err != nil {
		return false, fmt.Errorf("pausing %s at its budget: %w",
			model.FormatRunID(runID), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("pausing %s at its budget: %w",
			model.FormatRunID(runID), err)
	}
	return n > 0, nil
}

// CacheRunFloorTx writes `runs.usage_floor`.
//
// IT IS A CACHE FOR THE REPORT AND NOTHING ELSE (§4.3, §3.2). No enforcement
// path reads it: the floor that decides is a SUM over claim events, computed
// inside the deciding transaction, because a stored running total is a
// read-modify-write and a read-modify-write is the one shape C4 has no defense
// against. TestFloorIsNeverReadFromCache poisons this column and asserts every
// decision behaves as though it said the truth.
//
// It does NOT bump `row_version`: the cache is a derived number, not a state
// change an operator's CAS should collide with. A claim that bumped the run's
// version for a number nobody asserted against would make `--if-version` on runs
// unusable during any active run.
func CacheRunFloorTx(tx *sql.Tx, runID int, floor float64) error {
	if _, err := tx.Exec(
		`UPDATE runs SET usage_floor = ? WHERE id = ?`, floor, runID); err != nil {
		return fmt.Errorf("caching the floor for %s: %w",
			model.FormatRunID(runID), err)
	}
	return nil
}

// RunBudgetFacts are the stored budget columns a read verb renders: the cached
// floor and the breach reason, if any.
type RunBudgetFacts struct {
	CachedFloor  float64
	BreachReason string
}

// RunBudgetFactsTx is RunBudgetFactsFor inside a caller's transaction, for the
// one writer that must read the breach record while deciding whether a cap
// change resolved it (DKT-80). The pool is capped at one connection, so a pool
// read from inside a transaction deadlocks rather than merely racing.
func RunBudgetFactsTx(tx *sql.Tx, runID int) (RunBudgetFacts, error) {
	var (
		facts  RunBudgetFacts
		reason sql.NullString
	)
	err := tx.QueryRow(
		`SELECT usage_floor, breach_reason FROM runs WHERE id = ?`, runID,
	).Scan(&facts.CachedFloor, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return RunBudgetFacts{}, ErrRunNotFound
	}
	if err != nil {
		return RunBudgetFacts{}, fmt.Errorf("reading budget facts for %s: %w",
			model.FormatRunID(runID), err)
	}
	facts.BreachReason = reason.String
	return facts, nil
}

// ClearRunBreachTx clears `breach_reason` once a cap change has resolved the
// breach (DKT-80): a row still asserting "budget: spend N of cap M reached"
// after the cap moved past N misleads every reader that trusts it. When
// `newReason` is non-empty the run machine's general `reason` is rewritten
// too — the caller passes it only when the row's reason IS the breach reason,
// so an unrelated pause's reason is never overwritten.
//
// `row_version` is not bumped here: every caller runs inside a transaction
// that already bumped it for the cap write itself.
func ClearRunBreachTx(tx *sql.Tx, runID int, newReason string, nowMS int64) error {
	// The BUDGET's hold on the run goes with the breach record (DKT-305). B26
	// has always said a breach-resolving cap raise is one of the two things
	// that moves a run out of a breach — the other being `run resume` — and
	// before `pause_origin` existed the rollup enforced that by reading
	// `breach_reason` itself, so clearing it here re-enabled the automatic
	// resume. Clearing the origin keeps that exactly as it was.
	//
	// Only a `budget` origin is cleared. An operator who ran `run pause` on a
	// run that also carried a stale breach owns the park now, and a cap raise
	// is not their decision to resume — which is the whole reason the origin
	// names WHO parked rather than just recording THAT somebody did.
	query := `UPDATE runs
	             SET breach_reason = NULL,
	                 pause_origin = CASE WHEN pause_origin = ? THEN '' ELSE pause_origin END,
	                 updated_at_ms = ?`
	args := []any{string(model.RunPauseOriginBudget), nowMS}
	if newReason != "" {
		query += `, reason = ?`
		args = append(args, newReason)
	}
	query += ` WHERE id = ?`
	args = append(args, runID)
	if _, err := tx.Exec(query, args...); err != nil {
		return fmt.Errorf("clearing the breach record for %s: %w",
			model.FormatRunID(runID), err)
	}
	return nil
}

// ClearStaleBudgetReasonTx retires a run's `reason` when it still carries a
// PREVIOUS raise's "cap changed from X to Y" sentence (DKT-80's own text,
// written by `ClearRunBreachTx`'s `newReason` argument) and no breach is
// currently standing. `newReason` replaces it; an empty `newReason` blanks
// the field instead, for a run with no other reason to state.
//
// `ClearRunBreachTx` only rewrites `reason` on the raise that RESOLVES a
// standing breach — right for `breach_reason` itself, since there is nothing
// left to clear a second time, but the decorative sentence that raise wrote
// survives every later, unrelated raise untouched, naming a cap that has
// since moved again (DKT-47). This is the caller's own leftover text
// retired, never an operator's pause reason: SetRunBudget only reaches for
// it once it has confirmed `reason` carries the exact prefix this package
// writes, and it supplies `newReason` itself only for a run still parked on
// the sentence (waiting-human) — blanking that one would leave a parked run
// with no stated reason at all.
func ClearStaleBudgetReasonTx(tx *sql.Tx, runID int, newReason string, nowMS int64) error {
	if _, err := tx.Exec(
		`UPDATE runs SET reason = ?, updated_at_ms = ? WHERE id = ?`,
		newReason, nowMS, runID,
	); err != nil {
		return fmt.Errorf("clearing the stale budget reason for %s: %w",
			model.FormatRunID(runID), err)
	}
	return nil
}

// RunBudgetFactsFor reads the v10 columns for one run.
func RunBudgetFactsFor(db *sql.DB, runID int) (RunBudgetFacts, error) {
	var (
		facts  RunBudgetFacts
		reason sql.NullString
	)
	err := db.QueryRow(
		`SELECT usage_floor, breach_reason FROM runs WHERE id = ?`, runID,
	).Scan(&facts.CachedFloor, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return RunBudgetFacts{}, ErrRunNotFound
	}
	if err != nil {
		return RunBudgetFacts{}, fmt.Errorf("reading budget facts for %s: %w",
			model.FormatRunID(runID), err)
	}
	facts.BreachReason = reason.String
	return facts, nil
}

// configValueTx reads one config key INSIDE a transaction.
//
// It exists because the budget snapshot is loaded in the deciding transaction
// (§4.8 B31) and internal/db caps the connection pool at ONE connection: a pool
// read from inside an open transaction deadlocks permanently rather than
// failing. Every other config reader takes a *sql.DB and runs before a
// transaction opens; this one is for the readers that cannot.
//
// It returns the spec's default for an unset key, exactly as GetConfig does, so
// the two cannot disagree about what "unset" means — including the v12 ladder:
// the project's override, then the store-wide value, then the builtin default.
func configValueTx(tx *sql.Tx, projectID int, key string) (string, error) {
	spec, err := LookupConfigSpec(key)
	if err != nil {
		return "", err
	}
	keys := []string{}
	if projectID != 0 {
		keys = append(keys, projectConfigKey(projectID, key))
	}
	keys = append(keys, metaConfigPrefix+key)
	for _, metaKey := range keys {
		var value string
		err = tx.QueryRow(
			`SELECT value FROM meta WHERE key = ?`, metaKey).Scan(&value)
		if err == nil {
			return value, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("reading config %s: %w", key, err)
		}
	}
	return spec.Default, nil
}

// BudgetDefaultTx is B1's second branch: the config cap, read in the deciding
// transaction. 0 means unlimited (B2).
func BudgetDefaultTx(tx *sql.Tx, projectID int) (float64, error) {
	value, err := configValueTx(tx, projectID, KeyBudgetDefault)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %s %q: %w", KeyBudgetDefault, value, err)
	}
	return n, nil
}

// BudgetUnitTx is B16's resolution: WHICH recorded usage unit the run cap
// counts.
//
// Empty — the default and the only value core ships — means `reported` is 0 and
// the enforcement rests entirely on the floor (B17). That is the honest default
// and it is exactly §9 item 7's configuration: with reporting disabled, the run
// still pauses at the cap from the floor.
func BudgetUnitTx(tx *sql.Tx, projectID int) (string, error) {
	return configValueTx(tx, projectID, KeyBudgetUnit)
}

// UsageBudgetUnitTx reads `budget.usage.unit` — the unit the MEASURED cap
// counts (DKT-238). Empty leaves that whole dimension dormant.
func UsageBudgetUnitTx(tx *sql.Tx, projectID int) (string, error) {
	return configValueTx(tx, projectID, KeyUsageBudgetUnit)
}

// StepAttemptsFor reports each step's attempt count for the report's R3 line,
// keyed by step id.
func StepAttemptsFor(db *sql.DB, runID int) (map[int]int, error) {
	rows, err := db.Query(
		`SELECT id, attempt FROM steps WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading attempts for %s: %w",
			model.FormatRunID(runID), err)
	}
	defer rows.Close()

	out := make(map[int]int)
	for rows.Next() {
		var id, attempt int
		if err := rows.Scan(&id, &attempt); err != nil {
			return nil, fmt.Errorf("reading an attempt row: %w", err)
		}
		out[id] = attempt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading attempts for %s: %w",
			model.FormatRunID(runID), err)
	}
	return out, nil
}

// SortedUnits orders unit names for a deterministic rendering. It is here rather
// than at the call site so every renderer of an opaque-unit map orders it the
// same way (R9), whatever the map carries beside the names.
func SortedUnits[V any](byUnit map[string]V) []string {
	units := make([]string, 0, len(byUnit))
	for u := range byUnit {
		units = append(units, u)
	}
	sort.Strings(units)
	return units
}

// UsageUnitsMax is B36's first cap: at most 32 units per report.
//
// The caps exist because `--usage` is ATTACKER-CONTROLLED JSON FROM A CLAIMANT
// (§1.3) that lands in a ledger and in a report — bytes going to a terminal. A
// claimant that could write ten thousand units would make every subsequent
// `run report` on that run unreadable, which is a denial of the verb an operator
// reaches for when something has gone wrong.
const UsageUnitsMax = 32

// ParseUsage is B33, B35, and B36: `--usage '{"unit": n, …}'` parsed as a JSON
// object of string -> number, capped, with every refusal NAMING what it
// refused. It is the ONE implementation of the usage-report contract: the step
// ledger's writer (`step complete --usage`, via the engine) and the vote
// ledger's (`vote cast --usage`, DKT-95) both parse through it, so the two
// flags cannot drift on what a valid report is.
//
// It returns the units in SORTED ORDER so a call's ledger rows land in a
// deterministic order — the same total-key discipline the report's rendering
// uses (R9), applied at the writer so two identical calls produce identical row
// ids.
//
// Refusals are plain errors naming only the offending key or value, never a
// flag: each caller prefixes its own surface's name and shape (the engine as a
// VALIDATION_ERROR on `--usage`, the CLI as a `--usage` flag refusal), so this
// package never authors wording for a surface it cannot see.
func ParseUsage(raw string) ([]UsageRow, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf(
			"must be a JSON object of unit names to numbers: %v", err)
	}
	if len(parsed) > UsageUnitsMax {
		return nil, fmt.Errorf(
			"carries %d units; the limit is %d", len(parsed), UsageUnitsMax)
	}

	rows := make([]UsageRow, 0, len(parsed))
	for _, unit := range SortedUnits(parsed) {
		if err := ValidateUnitName(unit); err != nil {
			return nil, err
		}

		// Decoded per key so `{"tokens": "40"}` is refused NAMING "tokens" —
		// B33 says any other shape is a VALIDATION_ERROR naming the offending
		// key, and one decode of the whole object would name nothing.
		var quantity float64
		if err := json.Unmarshal(parsed[unit], &quantity); err != nil {
			return nil, fmt.Errorf(
				"%q must be a number, got %s", unit, parsed[unit])
		}
		// B35: any FINITE NON-NEGATIVE float. Negative is refused because
		// "usage" that reduces a total is not usage — it is an edit to a record
		// of what happened — and NaN/Inf are refused by the same check because
		// either would poison every sum the ledger feeds.
		if math.IsNaN(quantity) || math.IsInf(quantity, 0) {
			return nil, fmt.Errorf(
				"%q must be a finite number, got %s", unit, parsed[unit])
		}
		if quantity < 0 {
			return nil, fmt.Errorf(
				"%q must be >= 0, got %g — usage that reduces a total is not usage",
				unit, quantity)
		}
		rows = append(rows, UsageRow{Unit: unit, Quantity: quantity})
	}
	return rows, nil
}
