package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// The usage back-fill (docs/tdd/usage-backfill-wedge.md).
//
// engine-core §7 promises that a relay "back-fills from its dispatch journal,
// source recorded". The ledger was built for it — `usage_ledger.source` exists
// precisely "so a harness back-filling from its own journal can record its own
// source later without a migration" (internal/db/usage.go) — and the verb was
// the one piece never written. Its absence is what made D2's refusal permanent:
// usage that had been measured had no way in, so the probe correctly and
// forever reported a gap nobody could close.
//
// THE STATE THAT WAS MISSING IS USAGE, NOT ACCEPTANCE. That is why this is the
// fix rather than a step-scoped acceptance flag: a flag would teach the probe
// to ignore the gap, leaving every run's spend null while the ledger reported
// success.

// UsageSourceBackfilled is the default `source` for a back-filled row.
//
// It is a DEFAULT, not an enumeration: `--source` overrides it with any string,
// because core enumerating valid sources would be core holding an opinion about
// who is allowed to have measured the work — the same reasoning that keeps
// `unit` opaque.
const UsageSourceBackfilled = "backfilled"

// VoteBackfillRow is one unit's quantity for one SEAT of a proposal, as named
// on the command line (DKT-115).
type VoteBackfillRow struct {
	Voter    string
	Unit     string
	Quantity float64
}

// BackfillVoteUsage records panel spend a relay measured but the seats could
// not report at cast time (DKT-115).
//
// The step back-fill above cannot receive it: tribunal seats carry a proposal
// id, never a step id, so governance cost — measured at up to the whole of a
// run's visible spend — had no ledger path at all once the casts had landed.
// `vote cast --usage` remains the seat's own report; this is the relay's
// reconstruction, distinguishable forever by the `source` column v17 added.
//
// ROWS ATTACH TO A SEAT'S CAST. A seat that never cast has no row to attach to
// and is refused by name — usage on a vote nobody cast is spend on a decision
// that did not happen, and inventing a cast to hold it would put a phantom
// seat in the tally's own table.
//
// ONE TRANSACTION for the whole batch, for the same reason the step back-fill
// is: a half-applied batch would strand its remainder behind the ledger's
// unique key on a re-run.
func (e *Engine) BackfillVoteUsage(
	conn *sql.DB, proposalID int, rows []VoteBackfillRow, source string, nowMS int64,
) error {
	if len(rows) == 0 {
		return validationErr("no usage rows to back-fill; pass at least one " +
			"--voter with its --unit and --quantity")
	}
	if source == "" {
		source = UsageSourceBackfilled
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("beginning the back-fill: %w", err)
	}
	defer tx.Rollback()

	var proposalCount int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM proposals WHERE id = ?`, proposalID,
	).Scan(&proposalCount); err != nil {
		return fmt.Errorf("resolving proposal %s: %w",
			model.FormatProposalID(proposalID), err)
	}
	if proposalCount == 0 {
		return notFoundErr(db.ErrNotFound, "proposal %s not found",
			model.FormatProposalID(proposalID))
	}

	// Every named seat is resolved BEFORE anything is written, so a batch
	// naming one wrong voter refuses without having written the rows that
	// preceded it in the list.
	seats := make(map[string]int64, len(rows))
	for _, r := range rows {
		if _, seen := seats[r.Voter]; seen {
			continue
		}
		if r.Voter == "" {
			return validationErr("--voter is required on every row; a vote " +
				"usage row attaches to a named seat's cast")
		}
		var voteID int64
		err := tx.QueryRow(
			`SELECT id FROM votes WHERE proposal_id = ? AND voter_name = ?`,
			proposalID, r.Voter).Scan(&voteID)
		if errors.Is(err, sql.ErrNoRows) {
			return validationErr(
				"%q has no cast on %s; vote usage attaches to a seat's cast, "+
					"and a seat that never cast has nothing to attach to",
				r.Voter, model.FormatProposalID(proposalID))
		}
		if err != nil {
			return fmt.Errorf("resolving the %q seat on %s: %w",
				r.Voter, model.FormatProposalID(proposalID), err)
		}
		seats[r.Voter] = voteID
	}

	for _, r := range rows {
		if err := db.ValidateUnitName(r.Unit); err != nil {
			return validationErr("voter %q: %s", r.Voter, err)
		}
		if math.IsNaN(r.Quantity) || math.IsInf(r.Quantity, 0) || r.Quantity < 0 {
			return validationErr(
				"voter %q, unit %q: quantity must be a finite non-negative "+
					"number, got %g", r.Voter, r.Unit, r.Quantity)
		}
		if err := db.InsertVoteUsageTx(
			tx, seats[r.Voter], r.Unit, r.Quantity, source, nowMS); err != nil {
			if errors.Is(err, db.ErrUsageAlreadyRecorded) {
				return conflictErr(
					"seat %q already has %q usage recorded on %s; a back-fill "+
						"adds to the ledger and never overwrites it",
					r.Voter, r.Unit, model.FormatProposalID(proposalID))
			}
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing the back-fill: %w", err)
	}
	return nil
}

// BackfillRow is one unit's quantity for one step, as named on the command
// line. There is deliberately NO attempt field: see BackfillUsage.
type BackfillRow struct {
	Step     int
	Unit     string
	Quantity float64
}

// OnDuplicateRefuse and OnDuplicateSkip are `--on-duplicate`'s two settings.
//
// Refusing is the default and stays the default: a duplicate usually means the
// operator is about to double-count real spend, and the append-only ledger is
// right to say so. What was wrong was the GRANULARITY — the whole batch
// aborted on the first already-recorded row, and cross-wave duplicates are
// structural (a gate probed in wave N and seated in wave N+1 emits usage in
// both journals), so conductors hand-filtered rows seven times across three
// sessions (DKT-241).
const (
	OnDuplicateRefuse = "refuse"
	OnDuplicateSkip   = "skip"
)

// SkippedRow names one row --on-duplicate=skip passed over, so the skip is
// reported rather than silent.
type SkippedRow struct {
	Step     string `json:"step"`
	Instance string `json:"instance"`
	Attempt  int    `json:"attempt"`
	Unit     string `json:"unit"`
}

// BackfillOutcome is what the back-fill did: rows written, steps touched, the
// source they carry, and every row skipped as already recorded.
type BackfillOutcome struct {
	Written int          `json:"written"`
	Steps   int          `json:"steps"`
	Source  string       `json:"source"`
	Skipped []SkippedRow `json:"skipped,omitempty"`
}

// BackfillUsage records usage for steps whose claimant could not report it.
//
// ONE TRANSACTION for the whole batch. A back-fill that half-applied would
// leave a dispatch that is neither closable nor honestly re-runnable, and an
// operator re-running the verb after a partial failure would hit the ledger's
// unique key on the rows that did land. `--on-duplicate=skip` does not weaken
// this: a skipped row writes nothing, so the batch is still all-or-nothing
// over the rows it actually records.
//
// THE ATTEMPT IS THE STEP'S RECORDED ATTEMPT, and there is no way to name a
// different one. Back-filling an arbitrary historical attempt is rewriting
// history: the ledger's (step_id, attempt, unit) key exists so a retried step's
// second attempt records BESIDE its first, and a flag that let a caller choose
// the number could forge a row against an attempt that never ran or overwrite
// the accounting of one that did. Refused by omission is the strongest refusal
// available — there is no flag to misuse.
//
// `source` is written EXPLICITLY on every row. InsertUsageRowTx defaults an
// empty source to UsageSourceReported, which means "a claimant said so"; a
// back-fill falling through that default would label a relay's reconstruction
// as the claimant's own report and destroy the distinction the column exists
// to preserve.
func (e *Engine) BackfillUsage(
	conn *sql.DB, runID int, rows []BackfillRow, source string,
	onDuplicate string, nowMS int64,
) (*BackfillOutcome, error) {
	if len(rows) == 0 {
		return nil, validationErr("no usage rows to back-fill; pass at least one " +
			"--step with its --unit and --quantity")
	}
	if source == "" {
		source = UsageSourceBackfilled
	}
	switch onDuplicate {
	case "", OnDuplicateRefuse:
		onDuplicate = OnDuplicateRefuse
	case OnDuplicateSkip:
	default:
		return nil, validationErr(
			"invalid --on-duplicate %q: one of %s, %s",
			onDuplicate, OnDuplicateRefuse, OnDuplicateSkip)
	}

	var (
		written int
		skipped []SkippedRow
	)

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning the back-fill: %w", err)
	}
	defer tx.Rollback()

	// Every named step is resolved and checked against the run BEFORE anything
	// is written, so a batch naming one wrong step refuses without having
	// written the rows that preceded it in the list.
	steps := make(map[int]*db.Step, len(rows))
	for _, r := range rows {
		if _, seen := steps[r.Step]; seen {
			continue
		}
		step, err := db.GetStepTx(tx, r.Step)
		if err != nil {
			return nil, notFoundErr(err, "step %s not found",
				model.FormatStepID(r.Step))
		}
		if step.RunID != runID {
			return nil, validationErr(
				"step %s belongs to %s, not %s — a back-fill may only record "+
					"usage for steps of the run it names",
				model.FormatStepID(r.Step), model.FormatRunID(step.RunID),
				model.FormatRunID(runID))
		}
		steps[r.Step] = step
	}

	for _, r := range rows {
		if r.Unit == "" {
			return nil, validationErr("step %s: --unit is required; core has no "+
				"default unit", model.FormatStepID(r.Step))
		}
		step := steps[r.Step]
		if err := db.InsertUsageRowTx(tx, db.UsageRow{
			RunID:    runID,
			StepID:   r.Step,
			Attempt:  step.Attempt,
			Unit:     r.Unit,
			Quantity: r.Quantity,
			Source:   source,
		}, nowMS); err != nil {
			// The ledger's (step, attempt, unit) key firing means this unit was
			// ALREADY recorded for this attempt — by an earlier back-fill or by
			// the claimant itself. It is phrased rather than passed through as
			// raw SQLite text, on §2's rule that a refusal names its way out:
			// merging silently would hide a double-count of real spend.
			if errors.Is(err, db.ErrUsageAlreadyRecorded) {
				// --on-duplicate=skip: this row is already in the ledger, so
				// there is nothing to add and nothing to overwrite. The batch
				// carries on and the skip is REPORTED, never silent — a
				// back-fill that quietly dropped rows would be worse than one
				// that refuses (DKT-241).
				if onDuplicate == OnDuplicateSkip {
					skipped = append(skipped, SkippedRow{
						Step:     model.FormatStepID(r.Step),
						Instance: step.Instance,
						Attempt:  step.Attempt,
						Unit:     r.Unit,
					})
					continue
				}
				return nil, conflictErr(
					"step %s already has %q usage recorded for attempt %d; a "+
						"back-fill adds to the ledger and never overwrites it. "+
						"`docket run report` lists what is already recorded, "+
						"per step, under `step_usage`; `--on-duplicate=skip` "+
						"records the rest of the batch and reports what it "+
						"skipped.",
					model.FormatStepID(r.Step), r.Unit, step.Attempt)
			}
			return nil, err
		}
		written++
	}

	// The fast path D2's probe reads (§2.3). Marked once per step rather than
	// once per row.
	for id := range steps {
		if err := db.MarkStepUsageRecordedTx(tx, id); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing the back-fill: %w", err)
	}
	return &BackfillOutcome{
		Written: written, Steps: len(steps), Source: source, Skipped: skipped,
	}, nil
}
