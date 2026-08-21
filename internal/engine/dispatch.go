package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DISPATCH — engine-spec §2's scheduling block, implemented clause by clause
// (docs/tdd/runs-dispatch.md §5).
//
// A dispatch is a FROZEN COPY OF ONE `next` ANSWER, recorded so a relay's spawns
// can be checked against what the engine actually offered. It is not a lock, not
// a reservation, and not a claim: the steps in it are still `pending` and any
// claimant may still claim them (§5.1, P28). What it buys is that the engine can
// refuse to OFFER A NEW BATCH while the previous one is unreconciled — which
// turns a relay that lost track of its own spawns from a silent double-executor
// into a stalled run with a reason.
//
// The genericity bar (docs/design/genericity.md) holds throughout: a manifest is
// an ordered list of opaque rows, a class is an opaque string, and a discrepancy
// is a statement about claims and usage rows. Nothing here knows what an
// executor executes.

// Manifest is §11.4's `dispatch` wire shape: `{dispatch, run, opened_seq, rows}`.
type Manifest struct {
	Dispatch string `json:"dispatch"`
	Run      string `json:"run"`
	// OpenedSeq is the event seq at open time — the manifest's place in the log
	// (P2), and §6's boundary for "reaps this relay has not yet seen".
	OpenedSeq int64 `json:"opened_seq"`
	// ExpiresMS is not in §11.4's shape and is emitted alongside it, because P6
	// and P24 both require the refusal to NAME the expiry: a relay told only
	// that a dispatch is open cannot tell whether waiting is a strategy.
	ExpiresMS int64           `json:"expires_ms"`
	Rows      []model.StepRow `json:"rows"`
	// StaleTargets rides BESIDE the §11.4 shape for ExpiresMS's reason: a
	// conductor about to spend review budget on these rows needs the staleness
	// named (DKT-193). Never a row field — rows are hashed at open and
	// byte-compared at verify, and a live-derived fact would either freeze an
	// open-time answer into row_json or need normalizing away.
	StaleTargets []StaleTarget `json:"stale_targets,omitempty"`
	// BudgetHeld names the ready rows this manifest withheld for lack of
	// budget headroom, with the numbers (DKT-242). It rides BESIDE the §11.4
	// shape for StaleTargets' reason, and is `omitempty` for the same reason
	// `next`'s equivalent is empty by default: a run with no cap says nothing
	// new.
	//
	// Measured cost of its absence: with 0.9 headroom the engine offered 1 of
	// 5 ready judges and named no reason, and the round serialized around an
	// invisible wall for ~10 minutes and an extra wave cycle. `run budget`
	// already states the numbers honestly; the verb that DECIDES on them did
	// not.
	BudgetHeld string `json:"budget_held,omitempty"`
	// PinDrift is the run's unsound pins, when any (DKT-408) — the same
	// advisory channel StaleTargets rides, for the same reader at the same
	// moment: the conductor deciding whether to spend a wave on these rows.
	// Harness RUN-14 dispatched 3.5M tokens against a drifted pin set and
	// every step reading the drifted contract refused at render; the refusals
	// were correct and the spend was not. Advisory rather than a refusal
	// because drift blocks only the steps that READ a drifted ref — the
	// per-step CONFLICT at claim/render stays the enforcement — but the
	// conductor must get to decide before the wave, not discover it from
	// exit codes inside one.
	PinDrift []PinVerdict `json:"pin_drift,omitempty"`
}

// StaleTarget names one manifest row whose packet will render from a recorded
// target sha that is NO LONGER an ancestor of the shared checkout's HEAD
// (DKT-193). A conductor that selectively integrated an executor's work — a
// sanctioned action — silently invalidates downstream review packets rendered
// from the recorded sha; this is the structural signal that lets it be caught
// before review budget is spent on a diverged tree.
type StaleTarget struct {
	Instance   string `json:"instance"`
	Issue      string `json:"issue"`
	TargetSHA  string `json:"target_sha"`
	SharedHead string `json:"shared_head"`
	Reason     string `json:"reason"`
}

// FormatDispatchID renders a dispatch's display identity.
//
// It follows the `RUN-N` / `STEP-N` convention of every other engine entity
// rather than exposing a bare integer, so a `CONFLICT` naming a dispatch names
// something an operator can pass back to a verb.
func FormatDispatchID(id int) string { return fmt.Sprintf("DISPATCH-%d", id) }

// canonicalRowBytes is P3: a `next` row's canonical JSON bytes plus their
// sha256.
//
// CANONICAL MEANS THE SAME MARSHALING THE WIRE USES. `model.StepRow` is a struct
// with declared field order and `encoding/json` emits fields in that order, so a
// stored row and a fetched row are byte-identical BY CONSTRUCTION rather than by
// a re-serialization that could differ in key order. The one map in the shape is
// `metadata`, and encoding/json sorts map keys — which is what makes an opaque KV
// bag safe to hash.
//
// This is the function `verify` compares against, so it is the ONE definition of
// what a manifest row's bytes are. Two callers marshaling their own way is
// exactly the drift byte-equality exists to detect, and it would detect it as a
// false mismatch on every verify.
func canonicalRowBytes(row model.StepRow) (string, string, error) {
	raw, err := json.Marshal(row)
	if err != nil {
		return "", "", fmt.Errorf("rendering manifest row %s: %w", row.Instance, err)
	}
	return string(raw), workflow.SHA256(raw), nil
}

// OpenDispatch records a batch manifest for a run (§5.2).
//
// P1: it computes the ready set EXACTLY AS `next` DOES — the same LoadScheduler,
// the same predicate, the same SortSteps — by calling the same helper `next`
// calls. The identity is structural rather than asserted: a change to readiness
// reaches both verbs or neither, which is what TestManifestMatchesNext checks by
// comparing bytes rather than by re-deriving the answer.
//
// P5: it PERFORMS THE SAME LAZY REAP `next` does before computing. It is a
// scheduling verb offering a batch, and offering a stale step that a reap would
// have freed would make the manifest wrong the moment it was written.
//
// The acks (§6.2) are applied FIRST, inside the same transaction, because a new
// relay taking over from a crashed one acknowledges as part of claiming its next
// batch — and an ack applied after the ready set was computed would produce a
// manifest that omitted exactly the write-class steps the ack just released.
func (e *Engine) OpenDispatch(
	conn *sql.DB, runID int, limit int, ackSeqs []int64, nowMS int64,
) (*Manifest, error) {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return nil, err
	}
	ttls, err := loadTTLConfig(conn, runID)
	if err != nil {
		return nil, err
	}

	// DKT-105 / DKT-89: action and vote steps are ENGINE-RUN — no dispatcher
	// ever claims one, `claim` refuses them outright — and a dispatcher that
	// only ever calls `dispatch open` never calls `next`, so this verb must
	// drive their lifecycle too or a `kind:"action"` row rides into manifest
	// after manifest, ready and unexecuted.
	//
	// The drives run BEFORE the manifest is computed — the read-your-writes
	// shape DKT-55 closed in `next`, closed here by ORDER instead of
	// recomputation. `next` patches and re-derives because its first pass IS
	// its answer; a manifest is COMMITTED (P6/C1), and rows committed before
	// the drives could describe a step this same call then routed — a relay
	// spawning a panel against an already-decided proposal, the same
	// booted-and-died waste DKT-101 closed for scopes. Driving first means
	// the one manifest below is computed over the routed state: a proposal
	// the drive opened renders on its row natively (stepRow reads
	// sched.voteProposals), a tally it resolved routes before rendering, and
	// a step a route un-deferred is offered rather than missing. The drives
	// own their transactions and none is open yet, so the pool-of-one rule
	// that forced `next`'s drives after its commit is satisfied by
	// construction.
	preSched, err := readySnapshot(conn, runID, defs, nowMS)
	if err != nil {
		return nil, err
	}
	preRows, _, preSteps, err := readyRows(preSched, ttls, limit)
	if err != nil {
		return nil, err
	}
	// The READY prefix only (next.go's identical clause): staged rows are
	// previews, and their lifecycles belong to the `step record` that readies
	// them mid-wave, not to this open.
	preSteps = readyOnly(preRows, preSteps)
	if _, _, err := e.driveVoteSteps(conn, defs, preSteps, preSched.holdTally, nowMS); err != nil {
		return nil, err
	}
	if _, err := e.driveActionSteps(conn, preSteps, nowMS); err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("opening a dispatch: %w", err)
	}
	defer tx.Rollback()

	projectID, err := db.RunProjectIDTx(tx, runID)
	if err != nil {
		return nil, err
	}
	ttl, err := db.DispatchTTLTx(tx, projectID)
	if err != nil {
		return nil, err
	}

	// A9/A10: the acks, before anything reads headroom.
	if err := ackReapsTx(tx, runID, ackSeqs, db.AckByDispatchOpen, nowMS); err != nil {
		return nil, err
	}

	sched, err := LoadScheduler(tx, runID, defs, nowMS)
	if err != nil {
		return nil, err
	}

	// P5's reap, and with it §6.4's ack rows — the same helper `next` uses, so
	// the two scheduling verbs cannot reap differently.
	if _, err := reapExpiredTx(tx, sched, runID, nowMS); err != nil {
		return nil, err
	}
	if err := resolveQuorumMisses(tx, sched, nowMS); err != nil {
		return nil, err
	}

	// P25, the same refusal `next` applies (DKT-315). AFTER the reap, because
	// the reap is what clears D1: a claimed-but-unrecorded step whose lease has
	// lapsed is reaped one statement above and is no longer a discrepancy here.
	if err := refuseIfDiscrepanciesTx(tx, sched, runID, nowMS); err != nil {
		return nil, err
	}

	rows, _, rowSteps, err := readyRows(sched, ttls, limit)
	// Read off the SAME snapshot the rows came from, so the manifest cannot
	// name a hold that a different pass decided (DKT-242).
	budgetHeld := sched.BudgetHoldReason()
	if err != nil {
		return nil, err
	}

	// DKT-193: collect, while the snapshot is open, the rows whose packets
	// will render from a recorded target sha. The git question they pose is
	// answered after the commit — no subprocess inside a transaction (§6).
	candidates, err := staleTargetCandidates(tx, sched, runID, readyOnly(rows, rowSteps))
	if err != nil {
		return nil, err
	}

	// P6 / C1: the INSERT is the exclusion. Two relays racing both reach here
	// and SQLite's partial unique index admits exactly one; the loser's
	// computation above is discarded rather than merged (§5.4), because a merge
	// would produce a manifest neither relay saw.
	//
	// `opened_seq` is read BEFORE the insert's own event so it names the log
	// position the manifest was computed at.
	openedSeq, err := lastEventSeqTx(tx)
	if err != nil {
		return nil, err
	}

	// A dispatch must outlive the wave its manifest feeds. Stages run
	// sequentially and rows within a stage in parallel, so the manifest's
	// worst case is the sum over stages of each stage's largest lease — the
	// configured TTL stays the floor for empty and quick manifests. Under the
	// flat TTL a staged manifest carrying a long-lease writer ahead of judge
	// stages expired mid-wave and was auto-abandoned with its judges still
	// working, detonating claimed-but-unrecorded discrepancies the close then
	// had to explain.
	ttlMS := ttl.Milliseconds()
	if wave := stagedLeaseSumMS(rows); wave > ttlMS {
		ttlMS = wave
	}
	expiresMS := nowMS + ttlMS

	dispatchID, err := db.InsertDispatchTx(tx, runID, openedSeq, expiresMS, nowMS)
	if err != nil {
		if isAlreadyOpen(err) {
			open, readErr := db.OpenDispatchTx(tx, runID)
			if readErr != nil {
				return nil, conflictErr(
					"a dispatch is already open for %s", model.FormatRunID(runID))
			}
			return nil, conflictErr("%s", alreadyOpenMessage(open))
		}
		return nil, err
	}

	for i, row := range rows {
		raw, sum, err := canonicalRowBytes(row)
		if err != nil {
			return nil, err
		}
		stepID, err := model.ParseStepID(row.Step)
		if err != nil {
			return nil, fmt.Errorf("rendering manifest row %s: %w", row.Instance, err)
		}
		if err := db.InsertDispatchRowTx(tx, dispatchID, db.DispatchRow{
			Position: i, StepID: stepID, Instance: row.Instance,
			RowJSON: raw, RowSHA256: sum,
		}); err != nil {
			return nil, err
		}
	}

	if err := recordEvent(tx, eventRecord{
		Kind: EventDispatchOpened, RunID: runID, AtMS: nowMS,
		Data: fmt.Sprintf(`{"dispatch":%q,"rows":%d,"expires_ms":%d}`,
			FormatDispatchID(dispatchID), len(rows), expiresMS),
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("opening a dispatch: %w", err)
	}

	return &Manifest{
		Dispatch: FormatDispatchID(dispatchID), Run: model.FormatRunID(runID),
		OpenedSeq: openedSeq, ExpiresMS: expiresMS, Rows: rows,
		StaleTargets: e.staleTargets(conn, runID, candidates),
		BudgetHeld:   budgetHeld,
		PinDrift:     pinDriftAdvisory(conn, runID),
	}, nil
}

// readySnapshot re-derives a run's scheduler snapshot from scratch — one
// rolled-back transaction around LoadScheduler — for a caller that then
// projects the ready set from it through the shared readyRows tail (the
// scheduler retains no transaction; every fact is loaded eagerly). READ-ONLY
// by construction: the transaction never commits, and the reap is
// deliberately absent (engine-run steps hold no leases; each verb's own
// writing pass re-reaps).
//
// Both scheduling verbs project from the returned scheduler via readyRows, so
// a re-derived snapshot cannot disagree with the shared tail about ordering,
// truncation, or staging:
//
//   - NextSteps, after a driver routed something (DKT-55): the rows its first
//     pass rendered describe the run from BEFORE the routing, and reloading
//     rather than patching also catches the wider case a patch cannot — a
//     routed step un-deferring a downstream dependency that would otherwise
//     be missing from the response entirely until the caller polled again.
//   - OpenDispatch, before its drives (DKT-105): the ready steps the vote and
//     action drives act on — the same pipeline the manifest will run, one
//     snapshot earlier. The scheduler itself matters because driveVoteSteps
//     resolves materialized specs against its holdTally, and a second config
//     read could hand the drive a different roster than the manifest's own
//     snapshot would.
func readySnapshot(
	conn *sql.DB, runID int, defs map[int]*workflow.Definition, nowMS int64,
) (*Scheduler, error) {
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("reading the ready snapshot: %w", err)
	}
	defer tx.Rollback()

	return LoadScheduler(tx, runID, defs, nowMS)
}

// isAlreadyOpen reports C1's loser.
func isAlreadyOpen(err error) bool {
	return err != nil && strings.Contains(err.Error(), db.ErrDispatchAlreadyOpen.Error())
}

// alreadyOpenMessage is P6's refusal text: it NAMES the open dispatch's id and
// its expiry, because a relay told only "a dispatch is open" cannot tell whether
// waiting for the TTL is a strategy.
func alreadyOpenMessage(open *db.Dispatch) string {
	return fmt.Sprintf(
		"a dispatch is already open for %s: %s, expiring at %d — close it, "+
			"abandon it, or wait for the TTL",
		model.FormatRunID(open.RunID), FormatDispatchID(open.ID), open.ExpiresMS)
}

// readyRows is the shared tail of `next` and `dispatch open`: the readiness
// pass, the ordering, the limit, and the rendering (P1, P4).
//
// It exists so P1's "exactly as `next` does" is ONE code path rather than two
// that look alike. §6.3's ordering-then-slicing rule falls out of it: the sort
// runs over the whole ready set and the limit truncates afterward, so a
// `--limit 2` manifest holds the two highest-priority steps rather than two
// arbitrary ones.
//
// It returns the TRUE total before the slice, for the same truncation contract
// `next` reports (reliability-delta §4.2).
// stagedLeaseSumMS is a manifest's worst-case wall clock in milliseconds:
// stages run sequentially and rows within a stage in parallel, so it is the
// sum over stages of each stage's largest lease TTL. Zero for an empty
// manifest, which leaves the configured dispatch TTL in charge.
func stagedLeaseSumMS(rows []model.StepRow) int64 {
	maxByStage := make(map[int]int, 2)
	for _, r := range rows {
		if r.LeaseTTLS > maxByStage[r.Stage] {
			maxByStage[r.Stage] = r.LeaseTTLS
		}
	}
	var sum int64
	for _, leaseS := range maxByStage {
		sum += int64(leaseS) * 1000
	}
	return sum
}

func readyRows(sched *Scheduler, ttls ttlConfig, limit int) ([]model.StepRow, int, []*db.Step, error) {
	var ready []*db.Step
	for _, step := range sched.Steps() {
		if ok, _ := sched.Ready(step); ok {
			ready = append(ready, step)
		}
	}
	sched.SortSteps(ready)

	// DKT-101: R4 excludes a step against ALREADY-CLAIMED holders, so two
	// ready steps whose scopes overlap EACH OTHER both pass it independently —
	// `next` narrows exactly this with ClaimablePrefix (ready.go), and this is
	// the manifest's tail `next` shares, so it must narrow identically. Without
	// it, `dispatch open` offered a row `next` had just excluded: the wave
	// spawned both, and the second row's own claim refused on the scope
	// conflict the first row's claim had just created — a spawn that fully
	// booted and died to a refusal the manifest should never have offered.
	// DKT-23 widened the same narrowing to class headroom, for the same waste
	// in the same shape: five write-class rows over one slot spawned five
	// executors and claimed one.
	ready = sched.ClaimablePrefix(ready)

	// The claimable cohort widens to its staged closure (lookahead.go): every
	// pending step this offer's own execution unlocks joins at a later stage,
	// rows rendered `staged` rather than `ready` so the wire never overstates
	// claimability. The entries come back in stage-major order — predecessors
	// first, SortSteps within a stage — which is DKT-38's rule over the whole
	// closure, so the `--limit` cut below keeps a runnable prefix: every
	// dependency of a surviving row sits at a strictly lower stage and
	// therefore survives with it. That prefix property is also what retires
	// the post-cut loop-body eviction the flat offer needed (DKT-58/DKT-75):
	// membership already refuses a row whose open loop body is absent from
	// the offer, and the cut cannot separate a row from a body the leveling
	// placed below it.
	entries := sched.lookaheadOffer(ready)

	total := len(entries)
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}

	rows := make([]model.StepRow, 0, len(entries))
	steps := make([]*db.Step, 0, len(entries))
	for _, e := range entries {
		row, err := stepRow(sched, e.step, ttls)
		if err != nil {
			return nil, 0, nil, err
		}
		row.Stage = e.stage
		row.Conditional = e.conditional
		if e.staged {
			row.Status = db.StepStaged
		}
		rows = append(rows, row)
		steps = append(steps, e.step)
	}

	// `steps` (the limited, index-aligned []*db.Step behind `rows`) travels
	// back to the caller so the drive passes — `next`'s own, and `dispatch
	// open`'s pre-drive readySnapshot — act on the SAME set this pipeline
	// admitted (DKT-105). Callers that drive lifecycles must take the READY
	// rows only (rows[i].Status == db.StepReady): a staged step's lifecycle
	// belongs to the `step record` that readies it, not to the offer that
	// previewed it.
	return rows, total, steps, nil
}

// readyOnly narrows readyRows' index-aligned steps to the rows rendered
// `ready` — the set the lifecycle drivers may act on. It reads the rendered
// status rather than re-asking the predicate so a driver and the response it
// rides in cannot disagree about which rows were ready.
func readyOnly(rows []model.StepRow, steps []*db.Step) []*db.Step {
	out := make([]*db.Step, 0, len(steps))
	for i, row := range rows {
		if row.Status == db.StepReady {
			out = append(out, steps[i])
		}
	}
	return out
}

// lastEventSeqTx reads the highest event seq — the manifest's `opened_seq` (P2).
//
// A run with no events yet yields 0, which is a legitimate boundary meaning
// "before everything" rather than a missing value: §6 uses `opened_seq` to ask
// which reaps postdate the manifest, and every seq is > 0.
func lastEventSeqTx(tx *sql.Tx) (int64, error) {
	var seq sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("reading the event cursor: %w", err)
	}
	return seq.Int64, nil
}

// VerifyResult is `dispatch verify`'s answer (P8, P9).
type VerifyResult struct {
	Verified bool   `json:"verified"`
	Dispatch string `json:"dispatch"`
	Run      string `json:"run"`
	// StaleTargets is the same advisory OpenDispatch attaches (DKT-193),
	// recomputed against the shared checkout as it stands NOW — byte-equal
	// rows can still have gone stale since the open.
	StaleTargets []StaleTarget `json:"stale_targets,omitempty"`
	// Rows is every stored manifest row with its own verdict, in stored order
	// — DKT-243's per-row report.
	//
	// The comparison used to stop at the FIRST shifted row and hard-error, so
	// a dispatch where several steps had moved mid-flight (a park resolved, a
	// retry minted) reported one of them and hid the rest. ~16 occurrences
	// across three sessions, each costing a manual per-step confirm round
	// before a `close` that reconciles the same state without complaint. The
	// verb still FAILS when anything is off — that contract is unchanged —
	// but it now says everything it knows in one answer instead of one fact
	// per invocation.
	Rows []RowVerdict `json:"rows,omitempty"`
}

// The verdicts a stored manifest row can draw (DKT-243).
const (
	// RowMatched: the row renders today exactly as it was stored.
	RowMatched = "matched"
	// RowRecorded: the row's step has moved OFF the scheduler — recorded,
	// failed, parked at waiting-human. Its absence from the recomputation is
	// the dispatch working, not a conflict (DKT-10/DKT-65), and it does not
	// make the verify fail.
	RowRecorded = "recorded"
	// RowShifted: the step is still offerable, but renders differently than
	// it did at open — an attempt or status moved mid-dispatch.
	RowShifted = "rendering-shifted"
	// RowMissing: the step is still NON-TERMINAL and yet is no longer
	// offerable — claimed since the manifest opened, or excluded by a scope
	// conflict with a row admitted since. The narrower, more alarming case.
	RowMissing = "genuinely-missing"
)

// RowVerdict is one stored row's outcome. Stored and Computed carry the bytes
// on a non-matching verdict, for the reason RowMismatch does: the evidence
// rather than the engine's opinion about it. Both are empty on a `matched` or
// `recorded` row, where there is nothing to show.
type RowVerdict struct {
	Position int    `json:"position"`
	Step     string `json:"step"`
	Instance string `json:"instance"`
	Verdict  string `json:"verdict"`
	Stored   string `json:"stored,omitempty"`
	Computed string `json:"computed,omitempty"`
}

// RowMismatch is P9's refusal detail: the FIRST differing position, with both
// renderings.
//
// Position indexes the STORED MANIFEST's row list (`stored[i]` in
// VerifyDispatch) — the order the manifest was opened with — never the
// recomputed ready set, which a step recording, retiring, or simply losing
// rank can reorder freely without that reordering being what this field
// reports.
//
// Both sides are carried rather than a diff, because §5.3 asks for "the
// differing bytes, so an operator can see whether a lease lapsed or a
// priority changed". A computed summary would be the engine's opinion about
// which of those happened; the bytes are the evidence. A step that legitimately
// recorded, failed, or otherwise reached a terminal status never reaches this
// struct at all — VerifyDispatch's DKT-10 clause skips it before the
// comparison runs — so an empty `Computed` here means something narrower and
// more alarming: the stored row's step is STILL non-terminal and yet is no
// longer offerable (renderRowOrAbsent explains the reading further).
type RowMismatch struct {
	Position int
	Stored   string
	Computed string
}

// VerifyDispatch recomputes the ready set and compares each stored manifest
// row against ITS OWN current rendering, BYTE FOR BYTE (§5.3) — with `Stage`
// excluded from that comparison (DKT-19, below).
//
// P11 IS THE LOAD-BEARING PROPERTY AND IT IS A NEGATIVE ONE: this is a READ VERB
// AND IT WRITES NOTHING — INCLUDING NO REAP. It is the one scheduling-shaped
// verb that must not reap, because reaping would change the very ready set it
// was asked to compare against, and a verify that mutated its own subject could
// never fail. TestVerifyDoesNotReap asserts the step row still carries its stale
// owner afterward.
//
// The transaction is therefore read-only by construction: it is rolled back
// unconditionally, never committed. That is stronger than "the code has no
// INSERT", because it also covers anything a helper might write.
//
// DKT-10: a stored row whose step has since reached a TERMINAL status (§5.6's
// `db.StepTerminal`) is skipped rather than compared. `step record`/`step fail`
// retiring a step — or the step reaching `skipped`/`superseded` any other way —
// is exactly what a dispatch promises will keep happening while it is open
// (§5.1, P28 — a manifest is not a lock), so that step's absence from the
// recomputed ready set is the expected effect of the batch working, not a
// conflict shaped like one. The predicate is `db.StepTerminal`, the SAME one
// `missingUsage` reads to ask a related question (§5.8) — the classification
// itself is new to `verify`, not a line `discrepanciesTx`/`CloseDispatch`
// already drew: their split is by DISCREPANCY KIND, not terminal-vs-not, and
// close_reason "reconciled" means only "no discrepancy blocked", which is a
// different fact than the one this clause checks. A stored row is looked up
// by the STEP IT NAMES, never by position, so a step that recorded does not
// shift every row after it out of alignment: readiness among the other rows
// is judged unlimited (no `limit`) so a lower-ranked still-ready row is not
// mistaken for missing merely because new work outranks it now.
//
// DKT-19: `Stage` (stage.go) is excluded from the comparison because it is a
// SET-RELATIVE ordering hint, not a fact about the row — its value depends on
// which OTHER rows share the current ready set, and that set legitimately
// shrinks across a dispatch's lifetime as siblings retire (or was truncated
// differently at open time by `--limit`). Comparing it verbatim reported a
// conflict on two kinds of untouched manifest: one opened with `--limit N`
// smaller than the unlimited set this function recomputes (a stored row's
// stage-0 rendering never saw the predecessor that stage depends on), and one
// whose stage-0 tree-holder had itself already retired terminal (its
// surviving stage-1 rows recompute unstaged, because their one predecessor is
// gone). Neither is a conflict — the set membership driving `Stage` changed
// for a reason `verify` already accounts for elsewhere in this function, and
// core makes no claim that `Stage` is enforced or stable (stage.go: "Core
// enforces nothing … that is the deliberate limit of a scheduling hint").
func (e *Engine) VerifyDispatch(
	conn *sql.DB, runID int, nowMS int64,
) (*VerifyResult, *RowMismatch, error) {
	result, mismatch, candidates, err := e.verifyDispatchTx(conn, runID, nowMS)
	if err != nil || mismatch != nil {
		return result, mismatch, err
	}
	// The git pass runs HERE, after verifyDispatchTx's read-only transaction
	// has ended — no subprocess inside a transaction (§6), and no pool read
	// while one is open.
	result.StaleTargets = e.staleTargets(conn, runID, candidates)
	return result, nil, nil
}

// verifyDispatchTx is VerifyDispatch's transactional body: everything except
// the DKT-193 git pass, which must wait for the rollback.
func (e *Engine) verifyDispatchTx(
	conn *sql.DB, runID int, nowMS int64,
) (*VerifyResult, *RowMismatch, []targetCandidate, error) {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return nil, nil, nil, err
	}
	ttls, err := loadTTLConfig(conn, runID)
	if err != nil {
		return nil, nil, nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("verifying a dispatch: %w", err)
	}
	// NEVER COMMITTED (P11). The rollback is the mechanism, not a cleanup.
	defer tx.Rollback()

	open, err := db.OpenDispatchTx(tx, runID)
	if err != nil {
		return nil, nil, nil, noOpenDispatchErr(err, runID)
	}
	stored, err := db.ListDispatchRowsTx(tx, open.ID)
	if err != nil {
		return nil, nil, nil, err
	}

	sched, err := LoadScheduler(tx, runID, defs, nowMS)
	if err != nil {
		return nil, nil, nil, err
	}
	// NO REAP, NO QUORUM RESOLUTION: both write. The recomputation therefore
	// answers the readiness predicate over the rows AS THEY STAND, which is
	// exactly the comparison P9 wants — a lapsed lease shows up as a row that
	// differs, computed and not written. UNLIMITED (limit 0): a stored row is
	// matched by identity below, not by slice position, so truncating here
	// could drop a still-ready row the comparison still needs to see.
	computed, _, computedSteps, err := readyRows(sched, ttls, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	// Keyed by step id, read from readyRows' own index-aligned pairing
	// (assignStages keeps `computedSteps[i]` <-> `computed[i]` true across its
	// own stage-sort — see sortRowsByStage) — never by re-parsing an id back
	// out of a string the row's own `Step` field was formatted from, and never
	// by slice position, which staging permutes.
	computedByStep := make(map[int]model.StepRow, len(computed))
	for i, step := range computedSteps {
		computedByStep[step.ID] = computed[i]
	}
	// statusByStep, not a map of whole steps: the only question asked of it is
	// `db.StepOffScheduler` on one status, and a miss (empty string) answers
	// that predicate `false` — the same fallthrough a comma-ok `*db.Step` guard
	// produced, with no guard needed to get there.
	statusByStep := make(map[int]string, len(sched.Steps()))
	for _, step := range sched.Steps() {
		statusByStep[step.ID] = step.Status
	}

	// DKT-193: collected here, judged by the wrapper after the rollback.
	candidates, err := staleTargetCandidates(tx, sched, runID, readyOnly(computed, computedSteps))
	if err != nil {
		return nil, nil, nil, err
	}

	result := &VerifyResult{
		Dispatch: FormatDispatchID(open.ID), Run: model.FormatRunID(runID),
	}
	// The first offending row, kept while the sweep continues past it.
	var mismatch *RowMismatch

	// The DKT-19 comparison is stageless and derived from `row_json`, so the
	// stored hash no longer participates in it directly. It still guards the
	// derivation: the stored bytes must hash to `row_sha256` before they are
	// trusted as the comparison's baseline, or the pair written together at
	// open time has diverged in storage — a self-inconsistency, not a
	// scheduling conflict, so it refuses as an error rather than a mismatch.
	for i, want := range stored {
		// THE SELF-CHECK RUNS FIRST, before any early return (DKT-65).
		//
		// It used to sit below the `!ok` bail, so on any row that had drifted
		// out of the recomputation the function returned a mismatch without
		// ever comparing the stored bytes to their hash — which meant genuine
		// manifest tampering went undetected on exactly the rows most likely to
		// have been tampered with. A self-inconsistency is a different and
		// more serious finding than a scheduling conflict, so it is established
		// before anything else about the row is decided.
		if workflow.SHA256([]byte(want.RowJSON)) != want.RowSHA256 {
			return nil, nil, nil, fmt.Errorf(
				"stored manifest row %d (%s): row_json no longer hashes to "+
					"row_sha256 — the stored pair has diverged", i, want.Instance)
		}
		// StepOffScheduler, not StepTerminal (DKT-65): a step that recorded and
		// legitimately moved to `waiting-human` is absent from the
		// recomputation BY DESIGN, and calling that a mismatch failed the verb
		// on two measured runs for dispatches that were entirely correct.
		verdict := RowVerdict{
			Position: i, Step: model.FormatStepID(want.StepID),
			Instance: want.Instance,
		}
		if db.StepOffScheduler(statusByStep[want.StepID]) {
			verdict.Verdict = RowRecorded
			result.Rows = append(result.Rows, verdict)
			continue
		}
		current, ok := computedByStep[want.StepID]
		if !ok {
			verdict.Verdict = RowMissing
			verdict.Stored = want.RowJSON
			result.Rows = append(result.Rows, verdict)
			// The FIRST offending row is still what the refusal names, so the
			// message an operator has read for two releases is unchanged; the
			// rest of the rows now ride along on the result instead of
			// needing another invocation each (DKT-243).
			if mismatch == nil {
				mismatch = &RowMismatch{Position: i, Stored: want.RowJSON, Computed: ""}
			}
			continue
		}
		var storedRow model.StepRow
		if err := json.Unmarshal([]byte(want.RowJSON), &storedRow); err != nil {
			return nil, nil, nil, fmt.Errorf("parsing stored manifest row %d: %w", i, err)
		}
		raw, _, err := canonicalRowBytes(current)
		if err != nil {
			return nil, nil, nil, err
		}
		wantSum, gotSum, err := comparableVerifyBytes(storedRow, current)
		if err != nil {
			return nil, nil, nil, err
		}
		if gotSum != wantSum {
			verdict.Verdict = RowShifted
			verdict.Stored, verdict.Computed = want.RowJSON, raw
			result.Rows = append(result.Rows, verdict)
			if mismatch == nil {
				mismatch = &RowMismatch{Position: i, Stored: want.RowJSON, Computed: raw}
			}
			continue
		}
		verdict.Verdict = RowMatched
		result.Rows = append(result.Rows, verdict)
	}

	// A self-inconsistency above returns an error and never reaches here; a
	// scheduling difference sets `mismatch` and the whole sweep still ran, so
	// `Rows` describes every stored row rather than everything up to the first
	// problem.
	if mismatch != nil {
		return result, mismatch, nil, nil
	}

	result.Verified = true
	return result, nil, candidates, nil
}

// comparableVerifyBytes hashes a stored/computed row pair with the fields the
// dispatch's own progress legitimately moves normalized away, so lifecycle
// progress cannot report a false conflict:
//
//   - `Stage` is cleared on both sides — VerifyDispatch's DKT-19 clause: a
//     SET-RELATIVE ordering hint whose value depends on which other rows share
//     the current offer.
//   - When either side is `staged`, both statuses normalize to `ready` and
//     both `Proposal` fields clear. A staged row BECOMING ready is the batch
//     working — its predecessors recorded, exactly as the manifest scheduled —
//     and a staged VOTE row acquiring a proposal is the same progress one step
//     on: `step record`'s driver opened it at the moment the row became ready,
//     which is a fact the manifest could not have carried (the proposal did
//     not exist at open time).
//
// The returned sums are only ever compared here; `RowMismatch`'s rendered
// sides come from canonicalRowBytes (with the real, current values) so an
// operator inspecting a genuine mismatch sees the truth, not a laundered copy.
func comparableVerifyBytes(stored, current model.StepRow) (string, string, error) {
	stored.Stage, current.Stage = 0, 0
	// Conditional is Stage's sibling (DKT-26): a set-relative hint whose value
	// legitimately changes as the hold-capable predecessor routes or retires.
	stored.Conditional, current.Conditional = false, false
	if stored.Status == db.StepStaged || current.Status == db.StepStaged {
		stored.Status, current.Status = db.StepReady, db.StepReady
		stored.Proposal, current.Proposal = "", ""
	}
	_, wantSum, err := canonicalRowBytes(stored)
	if err != nil {
		return "", "", err
	}
	_, gotSum, err := canonicalRowBytes(current)
	if err != nil {
		return "", "", err
	}
	return wantSum, gotSum, nil
}

// targetCandidate is one offered step whose packet will render from a recorded
// target sha — collected inside a transaction, judged outside it (DKT-193).
type targetCandidate struct {
	instance string
	issue    string
	sha      string
}

// staleTargetCandidates collects, from an offer's READY steps, every one that
// consumes `issue.diff` and whose resolved round record names a target sha.
// Callers pass the readyOnly prefix: staged rows are previews whose target may
// legitimately be re-recorded by an earlier wave stage before they ready, so
// an open-time answer about them is noise, not signal.
//
// It runs INSIDE the caller's transaction — resolution is snapshot reads —
// and shells out to nothing; the git question is the caller's, afterward.
func staleTargetCandidates(
	tx *sql.Tx, sched *Scheduler, runID int, steps []*db.Step,
) ([]targetCandidate, error) {
	var out []targetCandidate
	var artifacts []*db.Artifact
	for _, step := range steps {
		spec := materializedSpec(sched.defs[step.WorkflowID], step, sched.holdTally)
		if spec == nil || !consumesIssueDiff(spec) {
			continue
		}
		if artifacts == nil {
			loaded, err := db.ListRunArtifactsTx(tx, runID)
			if err != nil {
				return nil, err
			}
			artifacts = loaded
		}
		sha, _, err := resolvedTargetFor(tx, sched, step, spec, artifacts)
		if err != nil {
			return nil, err
		}
		if sha == "" {
			continue
		}
		out = append(out, targetCandidate{
			instance: step.Instance,
			issue:    model.FormatID(step.IssueID),
			sha:      sha,
		})
	}
	return out, nil
}

func consumesIssueDiff(spec *workflow.Step) bool {
	return slices.Contains(spec.Inputs, inputIssueDiff)
}

// staleTargets judges collected candidates against the shared checkout's
// history, OUTSIDE every transaction (§6: no subprocess inside one). A sha
// that is provably not an ancestor of the shared HEAD is stale — the
// conductor integrated something other than the recorded commit, so a packet
// rendered from it reviews a tree the branch no longer carries. An
// unanswerable question (missing git, GC'd object, no ancestry seam) warns
// about nothing: absence of evidence is not staleness.
//
// ANCESTRY ALONE IS NOT THE TEST (DKT-424). The sanctioned integration flow
// cherry-picks an isolated worktree's commit onto the shared branch, which
// mints a NEW sha for byte-identical content — so ancestry fails by
// construction on the correct flow, and RUN-33 drew a stale_targets entry on
// all five judge rows of a run whose `git diff <target> <head>` was empty. A
// disproved ancestry therefore opens a SECOND question rather than closing
// the matter: does HEAD still carry this target's tree on the paths the work
// touched, or — where that has no evidence to answer with — the same root tree
// outright (TreeMatchFn)? Only a tree that also differs is stale. RUN-37's
// integration was identical down to the tree object f36e7b157a5d and still
// warned, and the conductor spent ~50s proving by hand what the probe now
// establishes (DKT-451).
//
// The tree probe may only ACQUIT. Unanswerable (no TreeMatchFn wired, git
// absent, unrelated histories with differing trees, an empty touched-path set
// whose commits' root trees also differ) leaves DKT-193's verdict exactly as it
// stood, and the reason then says the tree question went unanswered rather than
// claiming a difference nothing measured.
//
// THE REASON NAMES THE CLAIM-TIME SEMANTICS (DKT-415), because the decision the
// warning exists to inform is "is dispatching through this safe". Claim
// re-resolves the step's inputs (AssembleContext -> resolveIssueDiff) and takes
// the target from the winning `issue.diff` artifact's recorded round record; it
// never asks git for HEAD, and the diff body is the text the producer recorded
// at completion. So the answer is neither "frozen at open" nor "re-derived from
// HEAD": a newer diff artifact recorded for the issue before the claim replaces
// this sha, and with none the packet renders at the diverged one. RUN-26's
// conductor dispatched through this warning on a guess at exactly that, and the
// string gave it nothing to check the guess against.
func (e *Engine) staleTargets(
	conn *sql.DB, runID int, candidates []targetCandidate,
) []StaleTarget {
	if len(candidates) == 0 || e == nil || e.IsAncestorFn == nil || e.HeadFn == nil {
		return nil
	}
	execRoot := runExecRoot(conn, runID)
	sharedHead := e.HeadFn(execRoot)
	if sharedHead == "" {
		return nil
	}
	// One verdict per SHA, not per row: a review fanout's siblings all render
	// from the same recorded target, and the git questions are identical.
	type verdict struct {
		ancestor, known      bool
		treeMatch, treeKnown bool
	}
	cache := make(map[string]verdict, len(candidates))
	var out []StaleTarget
	for _, c := range candidates {
		v, seen := cache[c.sha]
		if !seen {
			v.ancestor, v.known = e.IsAncestorFn(execRoot, c.sha)
			if v.known && !v.ancestor && e.TreeMatchFn != nil {
				v.treeMatch, v.treeKnown = e.TreeMatchFn(execRoot, c.sha)
			}
			cache[c.sha] = v
		}
		if !v.known || v.ancestor {
			continue
		}
		if v.treeKnown && v.treeMatch {
			// DKT-424: the sha was rewritten (cherry-pick integration), the
			// tree was not. The branch carries exactly what the packet
			// renders on the paths this work touched — nothing to warn about.
			continue
		}
		out = append(out, StaleTarget{
			Instance: c.instance, Issue: c.issue,
			TargetSHA: c.sha, SharedHead: sharedHead,
			Reason: staleTargetReason(c.sha, sharedHead, v.treeKnown),
		})
	}
	return out
}

// staleTargetReason renders the advisory's sentence, in the two shapes the
// evidence actually supports (DKT-424).
//
// `treeChecked` is the difference between "the branch's content differs from
// the packet's, measured" and "the sha diverged and the content question could
// not be asked". The conductor's next move differs between them — the first is
// a divergence to look at, the second is a probe to repair or a tree to
// hand-check — so the string may not blur the two.
func staleTargetReason(sha, sharedHead string, treeChecked bool) string {
	evidence := fmt.Sprintf(
		"the recorded target sha %.12s is not an ancestor of the shared "+
			"checkout's HEAD %.12s", sha, sharedHead)
	if treeChecked {
		evidence += ", and its tree still differs from that HEAD on the paths " +
			"the work touched — integration diverged from the recorded commit " +
			"rather than merely rewriting its sha, so a packet rendered from it " +
			"reviews a tree the branch no longer carries"
	} else {
		evidence += ", and whether HEAD still carries its tree could not be " +
			"determined (git could not answer, or the two share no comparable " +
			"history) — a cherry-picked integration mints a new sha for " +
			"identical content, so confirm the tree before reading this as a " +
			"divergence; if integration did diverge, a packet rendered from " +
			"this sha reviews a tree the branch no longer carries"
	}
	return evidence +
		". A claim does not re-derive the target from HEAD: the packet renders " +
		"from whichever recorded diff artifact resolves at claim time, so this " +
		"row stays on this sha unless an upstream step records a newer diff for " +
		"the issue first"
}

// noOpenDispatchErr maps the storage sentinel onto the taxonomy once.
func noOpenDispatchErr(err error, runID int) error {
	if strings.Contains(err.Error(), db.ErrNoOpenDispatch.Error()) {
		return conflictErr("no dispatch is open for %s", model.FormatRunID(runID))
	}
	return err
}

// ---- Discrepancies (§5.8) --------------------------------------------------

// DiscrepancyKind names one of engine-core §5's TWO classes. There are exactly
// two and there will not be a third without a spec amendment, so they are
// constants rather than free strings.
type DiscrepancyKind string

const (
	// DiscrepancyClaimedUnrecorded is D1: a step in `claimed`/`running` whose
	// activity is older than `dispatch.grace`.
	DiscrepancyClaimedUnrecorded DiscrepancyKind = "claimed-but-unrecorded"
	// DiscrepancyMissingUsage is D2: a step that reached a terminal status after
	// the run's activation with zero `usage_ledger` rows, IN A RUN THAT HAS EVER
	// OPENED A DISPATCH.
	DiscrepancyMissingUsage DiscrepancyKind = "usage-rows-missing"
)

// Discrepancy is one unreconciled fact about a run, with the resolution §2
// enumerates for it.
//
// COMPUTED, NEVER STORED (§3.2). There is no `dispatch_discrepancies` table,
// because storing one would create a second source of truth that could disagree
// with the rows it summarizes — and the disagreement would be resolved by
// whichever code path a reader happened to call.
type Discrepancy struct {
	Kind     DiscrepancyKind `json:"kind"`
	Step     string          `json:"step"`
	Instance string          `json:"instance"`
	// Resolution is §2's enumerated way out, rendered for this specific row.
	// engine-spec §2 enumerates the resolutions precisely so reconciliation is
	// an operator's act rather than a guess the engine makes (§14).
	Resolution string `json:"resolution"`
}

// Discrepancies computes both classes for a run, D1 first (§5.8).
//
// The ORDER is D1 then D2 and it is not cosmetic: D1's resolution is "wait for
// the lease to expire", which is the one an operator can act on immediately, and
// a list that led with the acceptance flag would invite closing over work that
// is still running.
func discrepanciesTx(tx *sql.Tx, sched *Scheduler, runID int, nowMS int64) ([]Discrepancy, error) {
	grace, err := db.DispatchGraceTx(tx, sched.run.ProjectID)
	if err != nil {
		return nil, err
	}

	var out []Discrepancy

	// ---- D1: claimed but unrecorded past grace. ---------------------------
	//
	// The resolution is LEASE EXPIRY, verbatim from §2 ("lease expiry clears
	// claimed-but-unrecorded"): the step's TTL lapses, `next` reaps it, and the
	// discrepancy dissolves on its own. The message names the expiry TIME so an
	// operator knows how long to wait rather than being told to wait.
	//
	// D1 IS SCOPED TO AN ACTIVE RUN, for the same reason the reap it names is
	// (Scheduler.Expired). This clause and that reap are one mechanism
	// stated twice — the refusal's whole content is "wait for the reap" — so
	// suspending the reap on a paused run while leaving this clause firing
	// would produce a refusal naming a resolution that CANNOT happen, and wedge
	// the run behind it until the park was resolved. That is strictly worse
	// than the false reap the run-status scoping removed: the operator is now told to wait for
	// an event the engine has decided not to perform.
	//
	// It is also the honest reading of what D1 detects. D1 is RELAY DRIFT — a
	// step claimed and never recorded past `dispatch.grace`. On a paused run
	// there is no relay being offered work to drift from; the grace clock is
	// measuring an interval the operator deliberately created. The drift, if it
	// is real, is still there when the run resumes, and the first `next` after
	// that reports it with the same clock.
	//
	// D2 BELOW IS NOT SUSPENDED. Its resolution is an operator's acceptance
	// flag, not a reap, so it stays reachable on a paused run and suppressing
	// it would hide a real accounting gap behind an unrelated pause.
	runActive := sched.Run() == nil || sched.Run().Status == model.RunActive

	for _, step := range sched.Steps() {
		if !runActive {
			break
		}
		if step.Status != db.StepClaimed && step.Status != db.StepRunning {
			continue
		}
		activity := stepActivityMS(step)
		if nowMS-activity < grace.Milliseconds() {
			continue
		}
		out = append(out, Discrepancy{
			Kind: DiscrepancyClaimedUnrecorded,
			Step: model.FormatStepID(step.ID), Instance: step.Instance,
			Resolution: fmt.Sprintf(
				"lease expiry clears it: the lease lapses at %d, after which `next` "+
					"reaps the step and the discrepancy dissolves", step.ExpiresMS),
		})
	}

	// ---- D2: usage rows missing. ------------------------------------------
	//
	// THE SCOPE IS RUN-LEVEL DISPATCH HISTORY, per the 2026-08-03 review fix to
	// §5.8. A relay that ever dispatched is accountable for every step of that
	// run, including out-of-manifest spawns — the drift D6 exists to catch —
	// while a run no relay drove has nobody owing usage. That is what keeps a
	// solo rehearsal and a human-only demo refusal-free: they complete without
	// `--usage` and open no dispatch.
	//
	// The probe is skipped ENTIRELY when the run never dispatched, so the
	// dormancy is one indexed existence check rather than a per-step scan that
	// happens to find nothing.
	dispatched, err := db.RunEverDispatchedTx(tx, runID)
	if err != nil {
		return nil, err
	}
	if !dispatched {
		return out, nil
	}

	activatedMS := int64(0)
	if a := sched.Run().ActivatedAtMS; a != nil {
		activatedMS = *a
	}

	for _, step := range sched.Steps() {
		if !missingUsage(step, activatedMS) {
			continue
		}
		out = append(out, Discrepancy{
			Kind: DiscrepancyMissingUsage,
			Step: model.FormatStepID(step.ID), Instance: step.Instance,
			Resolution: "record the usage, or close with " +
				"`dispatch close --accept-missing-usage`, which records the acceptance",
		})
	}

	// A TOTAL ORDER over the result, so two identical states produce identical
	// refusals — the same golden-stability discipline the report's rendering
	// uses. Without it the list would follow step-load order, which is stable
	// today and is not a contract.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Instance < out[j].Instance
	})
	return out, nil
}

// missingUsage is D2's predicate over one step, with D3, D4, and D5 each as a
// named branch rather than a clause in one condition.
func missingUsage(step *db.Step, activatedMS int64) bool {
	// D2 asks about steps that REACHED A TERMINAL STATUS. A step still running
	// owes nothing yet.
	if !db.StepTerminal(step.Status) {
		return false
	}

	// D5: ACTION AND HUMAN STEPS ARE EXEMPT. No worker claims them — they are
	// engine-run or operator-resolved — so there is nobody to have reported
	// usage. Including them would make every fixture run permanently
	// un-closable. Vote steps are exempt on the identical argument: core casts
	// for nobody, so a tallied vote had no claimant either.
	switch step.Kind {
	case workflow.ClassAction, workflow.TypeHuman, workflow.TypeVote:
		return false
	}

	// D5's other half: A STEP THAT WAS NEVER CLAIMED PRODUCED NOTHING TO
	// REPORT. `attempt` counts claims and nothing else — it is not bumped on
	// failure, and `step resolve --as retry` moves `attempt_base` rather than
	// zeroing it — so `attempt == 0` is exactly "no worker ever held this
	// step", for every terminal status at once.
	//
	// It was a list of two statuses (`skipped`, `superseded`), and the list was
	// the defect (DKT-315). `failed-routed` was missing from it, which is the
	// status the abandon cascade and `run abandon --issue` write onto steps
	// that were expanded and then terminated UNRUN. Harness RUN-32's STEP-1070
	// was failed-routed at attempt 0 with zero usage rows and zero events
	// mentioning it anywhere in the run — it never claimed, never spawned,
	// never spent — and the probe counted it, refusing `next` for the whole
	// run over a step that could not possibly owe anything.
	//
	// D4's "expected_cost = 0 steps still require usage rows" is untouched:
	// the floor and the ledger are independent, so a free step that RAN and
	// reported nothing is still a discrepancy.
	if step.Attempt == 0 {
		return false
	}

	// D3: THE HISTORICAL EXCLUSION. Steps completed before v10 have no ledger
	// rows and are not discrepancies, so the probe excludes steps whose
	// `updated_at_ms` predates the run's activation. Without this, upgrading the
	// binary would instantly make every historical run's dispatch un-closable —
	// a migration that breaks in-flight work.
	if activatedMS == 0 || step.UpdatedAtMS < activatedMS {
		return false
	}

	// `usage_recorded` is the fast path group 1 populates on every `--usage`
	// (§2.3): the column is the ledger's own answer to "did this step report",
	// written in the recording transaction, so the probe needs no join.
	return !step.UsageRecorded
}

// stepActivityMS resolves the clock D1 measures silence against.
//
// It falls back to `updated_at_ms` when a step has no activity stamp, because a
// step with no heartbeat has still been touched — and treating a missing stamp
// as 0 would make every such step instantly, permanently past grace.
func stepActivityMS(step *db.Step) int64 {
	if step.ActivityMS != nil && *step.ActivityMS > 0 {
		return *step.ActivityMS
	}
	return step.UpdatedAtMS
}

// DiscrepancyReason renders a list for a refusal, one line per row.
//
// Every line carries its RESOLUTION, because §2 enumerates the resolutions and a
// refusal that named the problem without the way out would make reconciliation a
// documentation lookup rather than an operator's act.
//
// It names the STEP ID beside the instance (DKT-315). Instances repeat across
// issues in one run — `design-qa@1` is four different steps in a four-issue run
// — so a refusal naming instances alone could not be acted on: the other
// documented way out, `dispatch backfill-usage --step STEP-N`, needs the id,
// and no read verb exposed which rows the probe was counting. The id is already
// on the row; it was simply not rendered.
func DiscrepancyReason(ds []Discrepancy) string {
	var b strings.Builder
	for i, d := range ds {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s %s (%s): %s", d.Step, d.Instance, d.Kind, d.Resolution)
	}
	return b.String()
}

// ---- close and abandon (§5.6) ----------------------------------------------

// CloseOutcome reports what a closing verb did, so the CLI renders the fact
// rather than re-deriving it.
type CloseOutcome struct {
	Dispatch string `json:"dispatch"`
	Run      string `json:"run"`
	Status   string `json:"status"`
	Reason   string `json:"close_reason"`
	// Accepted lists the steps closed over under `--accept-missing-usage`
	// (P19), as "STEP-N instance". It is part of the RECORD rather than of the
	// message: §2 says the flag "records the acceptance", and a list only
	// stderr carried would be gone by the time anyone audited. It names ids
	// because instances repeat across issues (DKT-315).
	Accepted []string `json:"accepted,omitempty"`
}

// CloseDispatch is P18, P19, P20, and P22.
//
// It closes the open manifest ONLY IF no discrepancy exists. With one, it
// refuses `CONFLICT` enumerating each discrepancy and its resolution — because
// "unreconciled batch" is an engine-refusal state (§2) and a close that
// proceeded over one would be the silent proceeding the whole mechanism exists
// to prevent.
//
// WITH `--accept-missing-usage` IT DOES NOT REQUIRE AN OPEN DISPATCH
// (DKT-315). That combination is the one documented way out of a
// usage-rows-missing refusal, and requiring a manifest to reach it made the
// refusal a cycle: `next` would not offer work until the discrepancy cleared,
// and the named way to clear it needed a dispatch that was not open. Harness
// RUN-14 sat in exactly that state — active, 190 steps done, unadvanceable by
// any normal verb — from 2026-08-19.
//
// The flag RECORDS AN ACCEPTANCE, which is a statement about steps, not about
// a manifest. So with no dispatch open it settles the accepted steps, writes
// the same event against the run, and reports what it accepted. Without the
// flag the refusal is unchanged: closing a manifest that is not open is still
// a conflict, because there is nothing to close.
func (e *Engine) CloseDispatch(
	conn *sql.DB, runID int, acceptMissingUsage bool, nowMS int64,
) (*CloseOutcome, error) {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("closing a dispatch: %w", err)
	}
	defer tx.Rollback()

	open, err := db.OpenDispatchTx(tx, runID)
	noOpen := err != nil && strings.Contains(err.Error(), db.ErrNoOpenDispatch.Error())
	switch {
	case err == nil:
	case noOpen && acceptMissingUsage:
		// The acceptance path, above: there is no manifest to reconcile and
		// the operator is not asking to reconcile one.
	default:
		return nil, noOpenDispatchErr(err, runID)
	}

	sched, err := LoadScheduler(tx, runID, defs, nowMS)
	if err != nil {
		return nil, err
	}
	found, err := discrepanciesTx(tx, sched, runID, nowMS)
	if err != nil {
		return nil, err
	}

	// P20: `--accept-missing-usage` does NOT accept the other class. D1 has its
	// own resolution — lease expiry — and a flag that accepted both would let a
	// relay close over work that is still running.
	var blocking, accepted []Discrepancy
	for _, d := range found {
		if acceptMissingUsage && d.Kind == DiscrepancyMissingUsage {
			accepted = append(accepted, d)
			continue
		}
		blocking = append(blocking, d)
	}
	if len(blocking) > 0 {
		subject := model.FormatRunID(runID)
		if !noOpen {
			subject = FormatDispatchID(open.ID)
		}
		return nil, conflictErr(
			"%s cannot close with %d unreconciled discrepancy(s): %s",
			subject, len(blocking), DiscrepancyReason(blocking))
	}

	reason := db.CloseReasonReconciled
	if len(accepted) > 0 {
		reason = db.CloseReasonAcceptedMissingUsage
	}

	instances, err := settleAcceptedTx(tx, accepted)
	if err != nil {
		return nil, err
	}

	if noOpen {
		// No manifest to CAS. The acceptance is still recorded — against the
		// run, with the same kind and the same `accepted` list — because that
		// record is the whole substance of what the operator just did.
		if len(accepted) == 0 {
			return nil, conflictErr(
				"no dispatch is open for %s and there is no missing usage to "+
					"accept", model.FormatRunID(runID))
		}
		data, err := json.Marshal(map[string]any{
			"accepted": instances, "reason": reason,
		})
		if err != nil {
			return nil, fmt.Errorf("recording the acceptance: %w", err)
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventDispatchClosed, RunID: runID, Data: string(data), AtMS: nowMS,
		}); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("recording the acceptance: %w", err)
		}
		return &CloseOutcome{
			Run: model.FormatRunID(runID), Status: db.DispatchClosed,
			Reason: reason, Accepted: instances,
		}, nil
	}

	// P19: the accepted step list rides in the EVENT's data, which is what
	// "records the acceptance" means. An acceptance visible only in a terminal
	// scrollback is not a record.
	moved, err := closeDispatchTx(tx, open.ID, runID, db.DispatchClosed, reason,
		EventDispatchClosed, map[string]any{"accepted": instances},
		"recording the close", nowMS)
	if err != nil {
		return nil, err
	}
	if !moved {
		// P22 / C2: a close racing the TTL abandon loses. It reports what the
		// winner did — the status and reason now on the row — so the relay
		// learns WHY rather than being told "not open".
		return nil, lostTheCloseRace(tx, open.ID)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("closing a dispatch: %w", err)
	}
	return &CloseOutcome{
		Dispatch: FormatDispatchID(open.ID), Run: model.FormatRunID(runID),
		Status: db.DispatchClosed, Reason: reason, Accepted: instances,
	}, nil
}

// settleAcceptedTx makes `--accept-missing-usage` MEAN something to the verb
// that is actually blocked (DKT-315).
//
// The flag's own help says it "records the acceptance", and it did: the
// accepted list went into the `dispatch-closed` event and nowhere else. The
// probe `next` runs recomputes from step rows and never read that event, so
// following the documented remedy exactly left the run refusing in precisely
// the same words — harness RUN-32 accepted one missing-usage row, the close
// reported success, and the very next `next` reported the same discrepancy.
//
// The acceptance is therefore written where the probe looks: `usage_recorded`,
// the flag whose meaning is "the ledger question is settled for this step"
// (see MarkStepUsageRecordedTx). The event still carries the fact that it was
// an ACCEPTANCE rather than a recording — the ledger rows are what tell the
// two apart, and an accepted step has none.
//
// It resolves each row by STEP ID, never by instance. Instances repeat across
// issues in one run, so an acceptance keyed on `design-qa@1` could not name
// which of four steps it settled.
//
// The returned list is what the event and the outcome carry: "STEP-N instance",
// so a reader of either can act on it without a join.
func settleAcceptedTx(tx *sql.Tx, accepted []Discrepancy) ([]string, error) {
	out := make([]string, 0, len(accepted))
	for _, d := range accepted {
		id, err := model.ParseStepID(d.Step)
		if err != nil {
			return nil, fmt.Errorf(
				"accepting missing usage on %q: %w", d.Step, err)
		}
		if err := db.MarkStepUsageRecordedTx(tx, id); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s %s", d.Step, d.Instance))
	}
	return out, nil
}

// AbandonDispatch is P21: it closes the open manifest UNCONDITIONALLY.
//
// NO DISCREPANCY BLOCKS IT, and that is the whole point: §2 provides "explicit
// `dispatch abandon` for a crashed relay", and the relay is gone and cannot
// resolve anything. A version that checked discrepancies first would be a
// recovery verb that refuses to recover, which is how a crashed relay wedges a
// run — the exact failure §2's recovery design exists to make impossible.
func (e *Engine) AbandonDispatch(
	conn *sql.DB, runID int, reason string, nowMS int64,
) (*CloseOutcome, error) {
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("abandoning a dispatch: %w", err)
	}
	defer tx.Rollback()

	open, err := db.OpenDispatchTx(tx, runID)
	if err != nil {
		return nil, noOpenDispatchErr(err, runID)
	}

	// The operator's `--reason` rides in the event's `data` rather than in
	// `close_reason`, which stays core's short closed vocabulary. That keeps the
	// column renderable by every read verb while the free text — which is
	// operator-supplied and therefore untrusted — stays in the opaque payload.
	payload := map[string]any{}
	if reason != "" {
		payload["detail"] = reason
	}
	moved, err := closeDispatchTx(tx, open.ID, runID, db.DispatchAbandoned,
		db.CloseReasonOperator, EventDispatchAbandoned, payload,
		"recording the abandon", nowMS)
	if err != nil {
		return nil, err
	}
	if !moved {
		return nil, lostTheCloseRace(tx, open.ID)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("abandoning a dispatch: %w", err)
	}
	return &CloseOutcome{
		Dispatch: FormatDispatchID(open.ID), Run: model.FormatRunID(runID),
		Status: db.DispatchAbandoned, Reason: db.CloseReasonOperator,
	}, nil
}

// ---- `next`'s lazy abandon and its refusal (§5.5, §5.7) --------------------

// autoAbandonExpiredDispatchTx is P13, P14, and P15: `next` retires a manifest
// that outlived `dispatch.ttl`, before evaluating its own refusal.
//
// P14's CAS is `WHERE id=? AND status='open'` (C3): exactly one row matches and
// exactly one event is written, so two `next` invocations racing at the instant
// of expiry produce ONE abandon rather than two. The loser simply proceeds — it
// has nothing to report, because the winner already did what it would have done.
//
// P15: it is EVENT-LOGGED with `data.reason = "ttl"`, which is engine-spec §2's
// "(event-logged)" made specific. A manifest that vanished silently would leave
// an operator reading the feed with a run that started answering again for no
// recorded reason.
//
// The run's own dormancy (D2) is the first line: a run that never opened a
// dispatch has no row, the lookup returns nothing, and `next` proceeds exactly
// as group 1's did.
func autoAbandonExpiredDispatchTx(tx *sql.Tx, runID int, nowMS int64) error {
	open, err := db.OpenDispatchTx(tx, runID)
	if err != nil {
		if strings.Contains(err.Error(), db.ErrNoOpenDispatch.Error()) {
			return nil
		}
		return err
	}
	if !open.Expired(nowMS) {
		return nil
	}

	// C3's loser (another invocation abandoned it first) writes nothing: `moved`
	// comes back false and closeDispatchTx returns a nil error for it, which is
	// exactly the "nothing to write" outcome this caller wants — unlike
	// CloseDispatch/AbandonDispatch, which report the lost race as a conflict.
	_, err = closeDispatchTx(tx, open.ID, runID, db.DispatchAbandoned,
		db.CloseReasonTTL, EventDispatchAbandoned, map[string]any{},
		"recording the TTL abandon", nowMS)
	return err
}

// closeDispatchTx is the CAS-close-and-record-event core shared by
// CloseDispatch, AbandonDispatch, and autoAbandonExpiredDispatchTx (P18-P21):
// the `dispatches` CAS (P22/C2/C3's guard, common to all three) and, only on
// the winning side, ONE event recording what happened.
//
// It deliberately does NOT own the transaction (the three callers disagree on
// when to commit — autoAbandonExpiredDispatchTx's caller commits later, as
// part of `next`'s own transaction) and does NOT decide what a lost race
// means (CloseDispatch and AbandonDispatch treat it as P22's conflict;
// autoAbandonExpiredDispatchTx treats it as C3's silent no-op) — both stay
// with the caller, via the returned `moved`.
//
// `payload` carries the event-kind-specific extra fields (CloseDispatch's
// `accepted`, AbandonDispatch's optional `detail`); `dispatch` and `reason`
// are added here, the two keys every closing event carries.
func closeDispatchTx(
	tx *sql.Tx, dispatchID, runID int, status, reason, eventKind string,
	payload map[string]any, errContext string, nowMS int64,
) (moved bool, err error) {
	moved, err = db.CloseDispatchTx(tx, dispatchID, status, reason, nowMS)
	if err != nil {
		return false, err
	}
	if !moved {
		return false, nil
	}

	payload["dispatch"] = FormatDispatchID(dispatchID)
	payload["reason"] = reason
	data, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("%s: %w", errContext, err)
	}
	if err := recordEvent(tx, eventRecord{
		Kind: eventKind, RunID: runID, Data: string(data), AtMS: nowMS,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// refuseIfUnreconciledTx is P24, P25, and P26 — engine-core §5's sentence,
// implemented.
//
// The ORDER is the dispatch probe then the discrepancy probe, because an open
// dispatch is the more specific and more actionable state: a relay told "your
// batch is still open" knows to close it, where the same relay told about a
// discrepancy first would reconcile a step and then hit the open manifest anyway.
//
// P26 is carried by the RETURN TYPE: this returns an error, so a caller cannot
// accidentally render a refusal as an empty ready set. A version returning
// `(bool, reason)` would put that mistake one careless `if` away.
func refuseIfUnreconciledTx(tx *sql.Tx, sched *Scheduler, runID int, nowMS int64) error {
	// P24: an open dispatch refuses, naming the dispatch, its expiry, and the
	// THREE WAYS OUT. A refusal that named the state without the exits would
	// make recovery a documentation lookup, and §2's whole recovery design is
	// that a stalled run tells an operator how to unstall it.
	open, err := db.OpenDispatchTx(tx, runID)
	switch {
	case err == nil:
		return conflictErr(
			"%s has an open dispatch: %s expiring at %d — reconcile with "+
				"`docket dispatch close --run %s`, give up on it with "+
				"`docket dispatch abandon --run %s`, or wait for the TTL to "+
				"auto-abandon it",
			model.FormatRunID(runID), FormatDispatchID(open.ID), open.ExpiresMS,
			model.FormatRunID(runID), model.FormatRunID(runID))
	case strings.Contains(err.Error(), db.ErrNoOpenDispatch.Error()):
		// The dormant path: no dispatch was ever opened for this run, so the
		// probe was one indexed lookup that returned no row (D2).
	default:
		return err
	}

	// P25 / D6: discrepancies are probed EVEN WITH NO OPEN DISPATCH, because
	// they are a property of the RUN rather than of the manifest — a relay that
	// never opened a dispatch can still leave a claimed step unrecorded. D2's
	// half of the probe is scoped to runs that have ever dispatched (the
	// 2026-08-03 review fix), which is what keeps a run no relay drove behaving
	// exactly as group 1's did.
	return refuseIfDiscrepanciesTx(tx, sched, runID, nowMS)
}

// refuseIfDiscrepanciesTx is P25's half on its own, so `next` and
// `dispatch open` cannot disagree about whether a run is blocked (DKT-315).
//
// They did. `next` refused and `dispatch open` did not, so the same run
// answered "you get no work until these are resolved" and "here are three
// rows" to two verbs one second apart. A conductor following the documented
// loop — ask `next` first — stalled; one that reached for `dispatch open`
// proceeded, and the run's accountability gap went unreconciled either way.
//
// `dispatch open` calls only THIS half, not refuseIfUnreconciledTx: an
// already-open dispatch is P6/C1's INSERT exclusion there, which reports the
// race it actually lost rather than `next`'s "close it or wait" advice.
func refuseIfDiscrepanciesTx(tx *sql.Tx, sched *Scheduler, runID int, nowMS int64) error {
	found, err := discrepanciesTx(tx, sched, runID, nowMS)
	if err != nil {
		return err
	}
	if len(found) > 0 {
		return conflictErr(
			"%s has %d unreconciled discrepancy(s) and will not be offered new "+
				"work until they are resolved: %s",
			model.FormatRunID(runID), len(found), DiscrepancyReason(found))
	}
	return nil
}

// lostTheCloseRace builds P22's refusal by READING WHAT THE WINNER DID.
//
// The message names the status and reason now on the row rather than saying "not
// open", because the loser's next act depends on which one happened: a TTL
// abandon means the batch was never reconciled, and a close means it was.
func lostTheCloseRace(tx *sql.Tx, dispatchID int) error {
	current, err := db.GetDispatchTx(tx, dispatchID)
	if err != nil {
		return conflictErr("%s is no longer open", FormatDispatchID(dispatchID))
	}
	return conflictErr(
		"%s is no longer open: it is %s (%s) — another invocation closed it first",
		FormatDispatchID(dispatchID), current.Status, current.CloseReason)
}
