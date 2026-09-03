package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// `guard record | spawn` — the two predicates engine-spec §2 assigns to this
// stage (docs/tdd/runs-dispatch.md §7).
//
// §2, verbatim: *"`spawn` — proposed rows byte-match the open dispatch and no
// unacknowledged write reaps; `record` — no unreconciled dispatch."*
//
// Both follow guard.go's established §6.12 contract — EXIT 0 ALLOW / EXIT 2 DENY
// WITH A REASON, independent of the error taxonomy — because a guard's caller
// tests a boolean rather than mapping a code.
//
// THEY WRITE NOTHING, EXCEPT THE ACK (§7.3). `guard record` writes nothing ever;
// `guard spawn` writes ONLY the `reap_acks` update, and only when `--ack-reap` is
// passed. Neither reaps and neither auto-abandons a dispatch: a guard that
// mutated scheduling state would make a hook's MERE PRESENCE change a run's
// behavior, which is the property that lets an operator wire one without
// thinking about it.

// GuardRecord answers `docket guard record`: is there no unreconciled dispatch?
//
// G2: "unreconciled" is an open dispatch OR any discrepancy (§5.8) — THE SAME
// TWO PROBES `next` USES (P24/P25), COMPUTED BY THE SAME FUNCTION. That identity
// is the point rather than an economy: a guard and the scheduler disagreeing
// about whether a run is reconciled would let a harness record into a picture
// `next` had already refused to extend.
//
// WHY `record` IS THE VERB A HARNESS WIRES BEFORE LETTING A WORKER CALL `step
// complete`: an unreconciled batch means the engine's picture of what is running
// is already wrong. Recording an artifact into that picture is how drift becomes
// durable.
//
// G4: `--run` is OPTIONAL. Without it the guard answers over every non-terminal
// run, denying if ANY is unreconciled — matching `guard stop`'s existing
// all-active-runs shape, so a hook wired once keeps working as runs come and go.
//
// projectID scopes the no-`--run` enumeration to one project; 0 answers over
// every project (see GuardStop). An EXPLICIT `--run` is exempt from scoping:
// naming a run is naming intent, and refusing a cross-project reference a hook
// spelled out would trade one surprise for another.
func GuardRecord(conn *sql.DB, runID, projectID int, nowMS int64) (*GuardVerdict, error) {
	runIDs, err := guardRunScope(conn, runID, projectID)
	if err != nil {
		return nil, err
	}

	for _, id := range runIDs {
		reason, err := unreconciledReason(conn, id, nowMS)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			// G3: the denial names WHICH of the two and the resolution, IN THE
			// SAME WORDS `next`'s refusal uses. An operator who has already read
			// one refusal should not have to learn a second vocabulary for the
			// same state.
			return &GuardVerdict{Allowed: false, Reason: reason}, nil
		}
	}
	return &GuardVerdict{Allowed: true}, nil
}

// unreconciledReason is G2's shared computation: the open-dispatch probe and the
// discrepancy probe, in `next`'s own order and words.
//
// It runs in a transaction it ROLLS BACK unconditionally (G11). The rollback is
// the mechanism rather than a cleanup — stronger than "this function has no
// INSERT", because it also covers anything a helper might write.
func unreconciledReason(conn *sql.DB, runID int, nowMS int64) (string, error) {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return "", err
	}

	tx, err := conn.Begin()
	if err != nil {
		return "", fmt.Errorf("checking whether %s is reconciled: %w",
			model.FormatRunID(runID), err)
	}
	defer tx.Rollback()

	sched, err := LoadScheduler(tx, runID, defs, nowMS)
	if err != nil {
		return "", err
	}

	// THE SAME FUNCTION `next` CALLS. G2 requires the guard and the scheduler to
	// be unable to disagree, and sharing the code is the only way that stays
	// true as either changes.
	//
	// G13: this does NOT reap and does NOT auto-abandon, which is the one way
	// the guard's answer differs from `next`'s — `next` performs both before it
	// refuses. The difference is deliberate and it is conservative in the right
	// direction: the guard may report a state `next` would have cleared, which
	// costs a hook one denial, while a guard that reaped would have made a
	// read-shaped hook a scheduling act.
	if err := refuseIfUnreconciledTx(tx, sched, runID, nowMS); err != nil {
		// The refusal's MESSAGE becomes the guard's reason, verbatim. That is
		// G3 mechanically: the guard cannot phrase the state differently from
		// `next` because it does not phrase it at all. A non-refusal error — a
		// failed read — is propagated as an error, since a guard that reported
		// "denied" when it could not check would be answering a question it
		// never asked.
		var refusal *Error
		if !errors.As(err, &refusal) {
			return "", err
		}
		return refusal.Message, nil
	}
	return "", nil
}

// guardRunScope resolves G4's optional `--run` into the set of runs to answer
// over: the one named, or every non-terminal run.
//
// A named run that does not exist is a REFUSAL rather than a vacuous allow. A
// guard asked about a run that is not there has not established that anything is
// safe, and answering exit 0 would let a typo in a hook read as permission.
//
// projectID filters the every-non-terminal-run enumeration only; a named run is
// returned regardless of project (see GuardRecord).
func guardRunScope(conn *sql.DB, runID, projectID int) ([]int, error) {
	if runID != 0 {
		if _, err := db.GetRun(conn, runID); err != nil {
			return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
		}
		return []int{runID}, nil
	}

	query := `SELECT id FROM runs WHERE status NOT IN ('done', 'abandoned')`
	var args []any
	if projectID != 0 {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY id`

	rows, err := conn.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading active runs: %w", err)
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("reading an active run: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading active runs: %w", err)
	}
	return out, nil
}

// SpawnOptions are `guard spawn`'s inputs beyond the run.
type SpawnOptions struct {
	// Rows is the JSON array a relay is about to spawn (G6), as raw bytes from
	// `--rows FILE` or stdin. Nil means the flag was not passed, which is a
	// DIFFERENT case from an empty array: the first asks only the reap half
	// (G7), the second proposes spawning nothing.
	Rows []byte
	// AckSeqs are `--ack-reap SEQ`, repeatable. G10: they are processed BEFORE
	// the predicate, so one invocation both acknowledges and answers — which is
	// what lets a relay's hook be a single command.
	AckSeqs []int64
	// DecidingVote is `--deciding-vote PROPOSAL-N`: the id of the OPEN
	// proposal the batch about to be spawned exists to decide (DKT-236).
	//
	// Without it the reap hold denies the one spawn that can clear it. The
	// sanctioned path for an ack-reap decision is a judge panel, and the
	// panel's spawn was denied for the exact state the panel exists to decide
	// — measured cost: one ~10h operator round trip, then two self-passed
	// `--ack-reap` calls with no panel at all, which is authorization creep
	// arriving by way of a deadlock.
	//
	// It relaxes the REAP half only, and never the row comparison: (a) is
	// about a relay spawning a batch the engine never issued, which has
	// nothing to do with who decides a hold.
	DecidingVote int
	NowMS        int64
}

// GuardSpawn answers `docket guard spawn`: may the relay start this batch?
//
// G5 — it allows iff BOTH:
//
//	(a) the proposed rows byte-match the open dispatch, and
//	(b) no unacknowledged write reaps exist.
//
// G7: WITH NO OPEN DISPATCH AND NO `--rows`, (a) IS VACUOUSLY SATISFIED. A
// harness that does not use dispatch manifests still gets (b) — the reap check —
// which is the half §2 assigns to this verb BY NAME ("surfaced by `guard
// spawn`"). Requiring a manifest would make the reap-ack mechanism unavailable
// to any relay that batches differently, which would be core deciding how a
// harness must batch.
//
// G8: WITH `--rows` AND NO OPEN DISPATCH, IT IS A DENIAL, not a vacuous pass.
// The relay believes it is spawning a batch the engine never issued, and that
// belief is exactly the drift this verb exists to catch.
//
// C11'S RESIDUAL, STATED: between this allow and the relay's actual spawn, the
// dispatch could be abandoned or a lease reaped. THE GUARD IS AN EARLY CHECK,
// NOT A LOCK, and the real enforcement remains where it has always been —
// `step claim`'s CAS. This is the same honest bound `guard gate` carries at S3,
// and it is recorded rather than papered over.
func (e *Engine) GuardSpawn(
	conn *sql.DB, runID int, opts SpawnOptions,
) (*GuardVerdict, error) {
	if runID == 0 {
		return nil, validationErr(
			"--run is required: name the run whose batch is being spawned")
	}
	if _, err := db.GetRun(conn, runID); err != nil {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}

	// G10 / G12: the acks come FIRST and are the ONLY thing this verb writes.
	// Without `--ack-reap` the transaction below never opens and the verb is a
	// pure read, which is what §7.3 promises and TestReadVerbsWriteNothing
	// asserts.
	if len(opts.AckSeqs) > 0 {
		if err := applyAcks(conn, runID, opts.AckSeqs, opts.NowMS); err != nil {
			return nil, err
		}
	}

	// (a) THE ROW COMPARISON, read AFTER the acks — an ack can release
	// write-class headroom, and a manifest compared against a pre-ack ready set
	// would be compared against a state that no longer exists.
	if verdict, err := spawnRowsVerdict(conn, runID, opts); err != nil || verdict != nil {
		return verdict, err
	}

	// (b) THE REAP CHECK, which is the half §2 assigns to this verb by name.
	return spawnReapVerdict(conn, runID, opts.DecidingVote, opts.NowMS)
}

// decidingVoteCarveOut is DKT-236: the reap hold does not deny the panel that
// exists to decide it.
//
// THE DEADLOCK IT BREAKS. Unacknowledged reaps hold headroom; the sanctioned
// way to decide whether to acknowledge them is a judge panel; spawning that
// panel is denied by the hold. Nothing can move, so what actually happened was
// a ~10h operator round trip followed by two self-passed `--ack-reap` calls
// with no panel — the authorization creep a gate exists to prevent, arriving
// through the gate's own deadlock.
//
// IT IS NARROW, AND EVERY CLAUSE IS LOAD-BEARING:
//
//   - The proposal must EXIST. A carve-out keyed on an id nobody created is a
//     bypass with a plausible-looking flag.
//   - It must be OPEN. A decided proposal is not a decision in progress, so
//     naming one would let any past vote authorize any future spawn forever.
//   - It relaxes the REAP half only. Row drift (G6/G8) is a different fact
//     about a different actor and no vote makes it acceptable.
//
// The verdict it returns is ALLOWED WITH THE CARVE-OUT NAMED IN ITS REASON, so
// a spawn that used it is legible in the relay's own logs rather than
// indistinguishable from a spawn nothing was holding.
func decidingVoteCarveOut(
	conn *sql.DB, proposalID int, hold string,
) (*GuardVerdict, error) {
	proposal, err := db.GetProposal(conn, proposalID)
	if err != nil {
		return nil, notFoundErr(err,
			"proposal %s not found; --deciding-vote must name the OPEN proposal "+
				"this batch exists to decide", model.FormatProposalID(proposalID))
	}
	if proposal.Status != model.ProposalStatusOpen {
		return nil, conflictErr(
			"proposal %s is %s, not open; --deciding-vote admits a panel that is "+
				"deciding a live question, and a decided one authorizes nothing",
			model.FormatProposalID(proposalID), proposal.Status)
	}
	return &GuardVerdict{
		Allowed: true,
		Reason: fmt.Sprintf(
			"allowed to decide %s: %s — the reap hold does not deny the panel "+
				"that exists to decide it",
			model.FormatProposalID(proposalID), hold),
	}, nil
}

// applyAcks writes A7's CAS in its own transaction.
//
// It is separate from the predicate's read so the ack COMMITS even when the
// predicate then denies. That is deliberate and it matters for A2's crashed-relay
// path: a new session acknowledges a reap and is denied for a DIFFERENT reason
// (a second unacknowledged reap, say). If the ack rolled back with the denial,
// the session would have to re-pass every seq on every attempt, and a relay
// clearing four reaps one refusal at a time would never converge.
func applyAcks(conn *sql.DB, runID int, seqs []int64, nowMS int64) error {
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("acknowledging reaps: %w", err)
	}
	defer tx.Rollback()

	if err := ackReapsTx(tx, runID, seqs, db.AckByGuardSpawn, nowMS); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("acknowledging reaps: %w", err)
	}
	return nil
}

// spawnRowsVerdict is G5(a), G6, G7, and G8.
//
// It returns a non-nil verdict ONLY on a denial, so the caller falls through to
// the reap check on the vacuous and matching paths alike. That shape keeps G5's
// "iff BOTH" true by construction: there is no way to return an ALLOW here and
// skip (b).
func spawnRowsVerdict(
	conn *sql.DB, runID int, opts SpawnOptions,
) (*GuardVerdict, error) {
	if opts.Rows == nil {
		// G7: no proposal, nothing to compare. (a) is vacuously satisfied and
		// the relay still gets (b).
		return nil, nil
	}

	proposed, err := parseProposedRows(opts.Rows)
	if err != nil {
		return nil, err
	}

	stored, err := storedManifestRows(conn, runID)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		// G8: rows were proposed and no dispatch is open. This is a DENIAL, not
		// a vacuous pass — the relay believes it is spawning a batch the engine
		// never issued, which is precisely the drift the manifest exists to
		// detect.
		return &GuardVerdict{Allowed: false, Reason: fmt.Sprintf(
			"%s has no open dispatch, but %d row(s) were proposed — open a "+
				"manifest with `docket dispatch open --run %s` before spawning "+
				"against one",
			model.FormatRunID(runID), len(proposed), model.FormatRunID(runID))}, nil
	}

	// G6: byte-matching against the STORED manifest, position by position. The
	// canonical bytes come from the SAME function the manifest was written
	// with (§5.2 P3), so a match is a match by construction rather than by two
	// marshalers agreeing. Unlike `dispatch verify` — which recomputes
	// readiness and compares STAGELESS (DKT-19) — this guard asks whether a
	// relay is spawning the offered rows VERBATIM, stage included, so any byte
	// difference is a real drift.
	if len(proposed) != len(stored) {
		return &GuardVerdict{Allowed: false, Reason: fmt.Sprintf(
			"the proposed batch has %d row(s) and the open dispatch has %d; "+
				"spawn what the manifest offered, or reconcile it first",
			len(proposed), len(stored))}, nil
	}
	for i, want := range stored {
		raw, sum, err := canonicalRowBytes(proposed[i])
		if err != nil {
			return nil, err
		}
		if sum != want.RowSHA256 {
			// The DIFFERING BYTES, both sides — the same evidence-not-opinion
			// discipline `dispatch verify`'s P9 refusal carries. A summary would
			// be the engine's guess about which field moved.
			return &GuardVerdict{Allowed: false, Reason: fmt.Sprintf(
				"proposed row %d does not byte-match the open dispatch\n"+
					"  manifest: %s\n  proposed: %s",
				i, want.RowJSON, raw)}, nil
		}
	}
	return nil, nil
}

// parseProposedRows decodes `--rows`' JSON array into the row shape the manifest
// stores.
//
// A malformed body is a VALIDATION_ERROR rather than a denial, and the
// distinction is the guard contract's: exit 2 means "I checked and the answer is
// no", while a body that does not parse means the question was not asked
// properly. A relay whose serializer broke should see that, not "denied".
func parseProposedRows(raw []byte) ([]model.StepRow, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, validationErr(
			"--rows is empty; pass the JSON array of rows being spawned, or omit " +
				"the flag to check only for unacknowledged reaps")
	}
	var rows []model.StepRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, validationErr(
			"--rows is not a JSON array of `next` rows: %v", err)
	}
	return rows, nil
}

// storedManifestRows reads the open dispatch's rows, or nil when none is open.
//
// "No open dispatch" is a nil result rather than an error because it is a
// LEGITIMATE STATE this verb must answer about differently depending on whether
// rows were proposed (G7 vs G8) — mapping it to an error here would make the
// caller unwrap a sentinel to tell the two apart.
func storedManifestRows(conn *sql.DB, runID int) ([]db.DispatchRow, error) {
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("reading the open dispatch: %w", err)
	}
	defer tx.Rollback()

	open, err := db.OpenDispatchTx(tx, runID)
	if err != nil {
		if strings.Contains(err.Error(), db.ErrNoOpenDispatch.Error()) {
			return nil, nil
		}
		return nil, err
	}
	return db.ListDispatchRowsTx(tx, open.ID)
}

// spawnReapVerdict is G5(b) and G9: no unacknowledged write reaps.
//
// G9: the denial ENUMERATES EACH UNACKNOWLEDGED SEQ AND NAMES `--ack-reap`
// (A11). That is §2's "surfaced by `guard spawn`", and it is what makes the
// mechanism DISCOVERABLE rather than documented — an operator reading the
// refusal copies the next command out of it. The rendering is ReapHoldReason,
// shared with `next`'s headroom message, so the two cannot describe the same
// hold differently.
func spawnReapVerdict(
	conn *sql.DB, runID, decidingVote int, nowMS int64,
) (*GuardVerdict, error) {
	// The hold is read in a transaction this function does not hold open: the
	// carve-out below opens its own, and the pool is capped at ONE connection.
	open, err := openReapHold(conn, runID)
	if err != nil {
		return nil, err
	}

	if len(open) == 0 {
		// The dormant path (D3): one indexed lookup on a partial index that
		// returns no row. A repo with no `[limits]` never creates a `reap_acks`
		// row and so never reaches anything below this line.
		return &GuardVerdict{Allowed: true}, nil
	}

	hold := ReapHoldReason(open)
	if decidingVote > 0 {
		verdict, err := decidingVoteCarveOut(conn, decidingVote, hold)
		if err != nil {
			return nil, err
		}
		// The carve-out is EVENT-LOGGED. A hold that was stepped past is a
		// fact an auditor must be able to find, and the alternative — a spawn
		// that simply succeeded — leaves the record identical to one nothing
		// was holding (DKT-236).
		if err := recordGuardCarveOut(conn, runID, decidingVote, hold, nowMS); err != nil {
			return nil, err
		}
		return verdict, nil
	}
	// The refusal NAMES the carve-out, so an operator reading it can copy the
	// next command out of it — the same discoverability rule the ack advice
	// follows.
	return &GuardVerdict{
		Allowed: false,
		Reason: hold + "; a panel spawned to DECIDE this hold passes " +
			"--deciding-vote PROPOSAL-N",
	}, nil
}

// GuardSpawnActive answers `docket guard spawn --active`: may every active run
// in the project accept a spawn right now?
//
// DKT-1287: docket-spawn-guard-hook.sh resolved `run status --active` and
// asked `guard spawn` about `runs[0]` alone, so with two concurrent active
// runs the OLDER run's reap half went unasked — a hold on it would not have
// denied the hook at all. This answers G5(b), the reap half, over EVERY
// non-terminal run in the project (guardRunScope's own scope), denying if ANY
// would deny and naming which.
//
// IT ANSWERS ONLY THE REAP HALF. G5(a)'s row comparison is a fact about ONE
// run's open dispatch and ONE proposed batch — there is no reading of "these
// rows against every active run" that means anything, and a relay spawning
// against a specific run's manifest already has that run's id to pass via
// `--run`. `--ack-reap` and `--deciding-vote` are the same kind of per-run act
// and stay on the `--run` path for the same reason.
//
// Runs are checked in guardRunScope's own oldest-first order, so the FIRST
// denial found names the OLDEST run that would deny.
func GuardSpawnActive(conn *sql.DB, projectID int, nowMS int64) (*GuardVerdict, error) {
	runIDs, err := guardRunScope(conn, 0, projectID)
	if err != nil {
		return nil, err
	}
	for _, id := range runIDs {
		verdict, err := spawnReapVerdict(conn, id, 0, nowMS)
		if err != nil {
			return nil, err
		}
		if !verdict.Allowed {
			return &GuardVerdict{
				Allowed: false,
				Reason:  fmt.Sprintf("%s: %s", model.FormatRunID(id), verdict.Reason),
			}, nil
		}
	}
	return &GuardVerdict{Allowed: true}, nil
}

// openReapHold reads a run's unacknowledged reaps in its own rolled-back
// transaction, so the caller holds none while it decides.
func openReapHold(conn *sql.DB, runID int) ([]db.ReapAck, error) {
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("reading unacknowledged reaps: %w", err)
	}
	defer tx.Rollback()
	_, open, err := loadReapHold(tx, runID)
	if err != nil {
		return nil, err
	}
	return open, nil
}

// recordGuardCarveOut writes the audit row for a spawn admitted past a reap
// hold — the one case where a missing event and "nothing happened" would say
// the same thing while meaning opposite things.
func recordGuardCarveOut(
	conn *sql.DB, runID, proposalID int, hold string, nowMS int64,
) error {
	data, err := json.Marshal(map[string]any{
		"carve_out": "deciding-vote",
		"proposal":  model.FormatProposalID(proposalID),
		"hold":      hold,
	})
	if err != nil {
		return fmt.Errorf("recording the spawn carve-out: %w", err)
	}
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("recording the spawn carve-out: %w", err)
	}
	defer tx.Rollback()
	if err := recordEvent(tx, eventRecord{
		Kind: EventSpawnAdmitted, RunID: runID, Data: string(data), AtMS: nowMS,
	}); err != nil {
		return err
	}
	return tx.Commit()
}
