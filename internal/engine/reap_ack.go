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

// THE WRITE-REAP ACKNOWLEDGMENT — engine-spec §2's last scheduling sentence, and
// §9 item 10's deferred half (docs/tdd/runs-dispatch.md §6).
//
// engine-spec §2, verbatim:
//
//	Reaping a write-class lease additionally holds write headroom until the
//	relay acknowledges the `reaped` event (surfaced by `guard spawn`) — the DB
//	fence is not a tree fence; a wedged writer must be confirmed gone before a
//	successor writes.
//
// THE HAZARD, CONCRETELY (§6.1). A write-class step's lease lapses. The engine
// believes the executor is dead and returns the step to `ready`. But the lease is
// a DATABASE fence — nothing about it stops a still-running process from writing
// to the working tree. If a successor write-class step starts while the
// supposedly-dead writer is still alive, two processes edit one tree and every
// gate computed over it is meaningless.
//
// WHAT CORE CAN AND CANNOT KNOW. Core knows the DB fence lapsed. It CANNOT check
// a process it did not start, so it refuses to pretend the DB fence is a tree
// fence: it holds the headroom and requires somebody — a new relay session, or a
// person typing — to supply the one fact core cannot observe. That is the honest
// division, and A2 is what makes it survivable: THE ACK NEVER REQUIRES THE DEAD
// RELAY, because the relay may be the thing that died and an ack only a corpse
// can give is a permanent wedge.

// writeClassOf is §6.5's TRANSLATION, and it is the whole reason this mechanism
// is generic: *write-class* means A CLASS WHOSE EFFECTIVE `[limits] max` IS
// FINITE.
//
// engine-core §5 says "write-class lease". engine-spec §2 says the reference
// instance's config "sets its write class to 1 — serialization is INSTANCE
// POLICY, not core behavior". A core mechanism keyed on a class NAMED `write`
// would therefore be unimplementable: the name is the instance's to choose, and
// core never learns it.
//
// Keying on the DECLARED BOUND is the only reading available, and it produces
// exactly the reference instance's behavior for exactly its configuration. The
// reasoning is engine-core §5's own argument for why writers serialize — "gates
// computed on a shared mutable tree race even across disjoint scopes" — and the
// instance expresses that by bounding the class. A class the author left
// unbounded is a class the author declared safe to parallelize (a read fan-out),
// and holding headroom on it would contradict "read-only fan-outs parallelize
// freely".
//
// A20/A21 fall straight out: a finite `max` gets ack rows and the hold; no `max`
// gets neither, and `next` behaves exactly as it did at S5. A22 —
// TestNoWriteClassLiteral — greps the implementation for the string, in the
// shape of the existing genericity guards.
//
// This is A SPEC READING AND IT IS RECORDED AS ONE (§6.5), not filed as a
// deviation: §2 already tells us the class name is instance policy.
func (s *Scheduler) writeClassOf(class string) bool {
	limit, ok := s.limits[class]
	return ok && limit.Max > 0
}

// reapExpiredTx is `next`'s and `dispatch open`'s SHARED reap: the lease write,
// the `lease-reaped` event, and — A16 — the `reap_acks` row, ALL IN THE CALLER'S
// TRANSACTION.
//
// It is one function rather than two similar loops because the two scheduling
// verbs reaping differently is a bug with no symptom until a manifest disagrees
// with the `next` that follows it. §5.2 P5 requires `dispatch open` to perform
// "the same lazy reap `next` does", and sharing the code is how that stays true.
//
// A17: `reaped_seq` is the seq of the `lease-reaped` event written by THIS reap,
// read back with `last_insert_rowid()` in the same transaction — so the ack row
// and the event it acknowledges are inserted together or not at all. An ack row
// pointing at an event that never committed would be an unacknowledgeable hold,
// which is the wedge A2 exists to forbid.
//
// A18: a class with no declared `max` inserts NOTHING. That is D3's dormancy,
// and it is structural: a repo with no `[limits]` never creates a `reap_acks`
// row and therefore never holds anything.
func reapExpiredTx(tx *sql.Tx, sched *Scheduler, runID int, nowMS int64) ([]string, error) {
	var reaped []string
	for _, step := range sched.Steps() {
		if !sched.Expired(step) {
			continue
		}
		if err := reapOneTx(tx, sched, runID, step, "", nowMS); err != nil {
			return nil, err
		}
		reaped = append(reaped, step.Instance)
	}
	return reaped, nil
}

// reapOneTx reaps ONE step: the row reset, the event, and — for a bounded
// class — the acknowledgment hold, in the caller's transaction. It is shared
// by the lazy expiry reap above and by ForceReapStep (DKT-83), so a forced
// reap cannot drift from an expiry's consequences: same event kind, same
// headroom hold, same snapshot reflection.
//
// `data` rides in the `lease-reaped` event when non-empty; the expiry path
// passes none, and the forced path records who-said-so and why — which is how
// a reader distinguishes them, the same data.reason discipline that separates
// a budget pause from an operator's.
func reapOneTx(
	tx *sql.Tx, sched *Scheduler, runID int, step *db.Step, data string, nowMS int64,
) error {
	{
		if err := db.ReapStepTx(tx, step.ID, nowMS); err != nil {
			return err
		}
		// The outcome, counted where it is decided (DKT-490): this claim ended
		// in a reap, not a failure — expiry and the forced path alike — and the
		// row's breakdown says so, so a consumer needing "how many attempts
		// failed" never has to reconstruct it from the (prunable) event feed.
		if err := db.MarkStepClaimReapedTx(tx, step.ID, nowMS); err != nil {
			return err
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventLeaseReaped, RunID: runID,
			Instance: step.Instance, IssueID: step.IssueID, Data: data, AtMS: nowMS,
		}); err != nil {
			return err
		}

		// A16's other half: the ack row, ONLY for a class with a finite bound.
		if sched.writeClassOf(step.Class) {
			seq, err := lastEventSeqTx(tx)
			if err != nil {
				return err
			}
			// A19: created UNACKNOWLEDGED. Nothing here sets `acked_at_ms`; the
			// hold is live from this instant until somebody names this seq.
			if err := db.InsertReapAckTx(tx, db.ReapAck{
				RunID: runID, StepID: step.ID, Class: step.Class, ReapedSeq: seq,
			}, nowMS); err != nil {
				return err
			}

			// THE HOLD IS LIVE IN THIS INVOCATION, not the next one. The
			// snapshot was loaded before this reap, so the row just inserted is
			// not in it — and the readiness pass that follows would offer a
			// SUCCESSOR write-class step that the hold forbids. W3 is precisely
			// this case: `next` reaps the writer and must offer the reaped step
			// itself while offering no OTHER write-class step, in the same
			// answer. Reflecting the insert here is the same discipline the reap
			// already applies to the step row two lines below: the snapshot is
			// updated to match what this transaction has done, so one call sees
			// its own consequences.
			sched.holdReap(db.ReapAck{
				RunID: runID, StepID: step.ID, Class: step.Class,
				ReapedSeq: seq, Instance: step.Instance,
			})
		}

		// Reflect the reap in the loaded snapshot, so the readiness pass sees
		// the step it just freed rather than the row it read a moment ago —
		// the counter too, so the offer this same call renders carries the
		// reap it performed.
		step.Status = db.StepPending
		step.Owner, step.TokenHash, step.ExpiresMS = "", "", 0
		step.StartedMS = nil
		step.ReapedClaims++
	}
	return nil
}

// ForceReapStep is `docket step reap` (DKT-83): an operator or relay that has
// ESTABLISHED an executor is dead clears its claim now, instead of waiting
// out the full lease TTL.
//
// Liveness was TTL-only, and the TTL cannot be sized right in both
// directions: raised to cover healthy long writers, it multiplies how long a
// dead agent's claim blocks its row (a claim whose process died two minutes
// in once sat until expiry). The engine cannot probe a process it did not
// start — the write-reap acknowledgment says so — but the RELAY that spawned
// the executor can, and this verb is the channel for what it observed.
//
// TOKEN-FREE, like approve/resolve: the authority is repository access plus
// the assertion, recorded with `--reason`, that the holder is gone. It is not
// an eviction primitive a bystander reaches casually — a forced reap of a
// LIVE worker has exactly the risks a lease expiry has, which is why every
// consequence is the expiry reap's own: same event kind (`lease-reaped`, with
// `data.forced` and the reason distinguishing it), same write-class headroom
// hold, same return of the step to the pool.
func ForceReapStep(conn *sql.DB, stepID int, reason string, nowMS int64) error {
	if reason == "" {
		return validationErr(
			"--reason is required: a forced reap asserts the holder is gone, " +
				"and somebody will ask on whose word")
	}

	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return err
	}
	if step.Status != db.StepClaimed && step.Status != db.StepRunning {
		return conflictErr(
			"step %s is %s; only a claimed or running step holds a lease to reap",
			step.Instance, step.Status)
	}

	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return err
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("reaping %s: %w", step.Instance, err)
	}
	defer tx.Rollback()

	sched, err := LoadScheduler(tx, step.RunID, defs, nowMS)
	if err != nil {
		return err
	}
	// The scheduler's own copy of the row, so the snapshot reflection inside
	// reapOneTx lands on the instance every predicate reads.
	target := step
	for _, s := range sched.Steps() {
		if s.ID == step.ID {
			target = s
			break
		}
	}

	data, err := json.Marshal(map[string]any{"forced": true, "reason": reason})
	if err != nil {
		return fmt.Errorf("recording the forced reap: %w", err)
	}
	if err := reapOneTx(tx, sched, step.RunID, target, string(data), nowMS); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reaping %s: %w", step.Instance, err)
	}
	return nil
}

// ackReapsTx applies a verb's `--ack-reap SEQ` flags (A7-A10).
//
// The three outcomes are DISTINGUISHED rather than collapsed:
//
//   - A7: a real, unacknowledged reap of this run is acknowledged, CAS on
//     `acked_at_ms IS NULL` (C7) — so two relays acking the same seq in the same
//     millisecond produce one acknowledgment and one no-op, not two.
//   - A10: an already-acknowledged seq is a SUCCESS THAT CHANGES NOTHING, so a
//     relay retrying its hook does not fail. It writes no second event either:
//     the transition already happened and the feed records it once.
//   - A9: a seq that is not a reap, or not this run's, is VALIDATION_ERROR
//     NAMING THE SEQ. This is the forgery point — an ack must name a real reap,
//     and the run scope is half of that: an ack naming another run's reap is a
//     forgery in exactly the way naming a non-reap is.
func ackReapsTx(tx *sql.Tx, runID int, seqs []int64, ackedBy string, nowMS int64) error {
	for _, seq := range seqs {
		result, err := db.AckReapTx(tx, runID, seq, ackedBy, nowMS)
		if err != nil {
			return err
		}
		switch result {
		case db.AckNoSuchReap:
			return validationErr(
				"--ack-reap %d names no unacknowledged reap of %s; an acknowledgment "+
					"must name the `seq` of a `lease-reaped` event for this run",
				seq, model.FormatRunID(runID))
		case db.AckAlreadyDone:
			// A10: idempotent. Nothing to write and nothing to report — the row
			// already says what this call would have made it say.
			continue
		}

		data, err := json.Marshal(map[string]any{
			"reaped_seq": seq,
			"acked_by":   ackedBy,
		})
		if err != nil {
			return fmt.Errorf("recording the acknowledgment of reap %d: %w", seq, err)
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventReapAcknowledged, RunID: runID, Data: string(data), AtMS: nowMS,
		}); err != nil {
			return err
		}
		// THE ACKNOWLEDGMENT IS THE ANSWER (DKT-262). A panel convened to
		// decide this reap has nothing left to decide once an operator
		// acknowledges it directly, and DKT-V21 and DKT-V46 stood open forever
		// for exactly that reason. Closing it here, in the same transaction as
		// the ack, is what keeps the two from disagreeing.
		//
		// Most reaps are acknowledged with no panel ever convened, so a missing
		// ballot is the ordinary case and reports nothing.
		if err := closeReapAckProposalTx(tx, runID, seq); err != nil {
			return err
		}
	}
	return nil
}

// loadReapHold reads the run's unacknowledged reaps into the scheduler's
// snapshot — A12's predicate, evaluated INSIDE the readiness transaction.
//
// A13: THIS IS A PREDICATE OVER ROWS, NOT A STORED "held" FLAG. C8 is the
// argument: a flag written at reap time and cleared at ack time has a window
// between the two writes in which a concurrent reader sees the wrong answer. A
// predicate over the rows has no window — the rows either exist unacknowledged
// at the instant of the snapshot or they do not.
//
// D3's dormancy is one indexed lookup on `idx_reap_acks_open`, a PARTIAL index
// on `acked_at_ms IS NULL`, returning no row and short-circuiting before any
// class arithmetic (§6.6).
func loadReapHold(tx *sql.Tx, runID int) (map[string]int, []db.ReapAck, error) {
	open, err := db.UnacknowledgedReapsTx(tx, runID)
	if err != nil {
		return nil, nil, err
	}
	if len(open) == 0 {
		// The dormant path is the first one, by construction — the same shape
		// loadBudget uses for D1.
		return nil, nil, nil
	}
	byClass := make(map[string]int, len(open))
	for _, ack := range open {
		byClass[ack.Class]++
	}
	return byClass, open, nil
}

// holdReap reflects a reap this transaction just recorded into the snapshot's
// hold, so the readiness pass that follows sees it.
//
// It mutates the snapshot rather than re-querying for the reason LoadScheduler
// exists at all (§6.3): readiness answers every step against ONE state of the
// world, and a re-query mid-pass could answer two steps against two different
// holds. What this transaction wrote is knowable without asking the database
// again, so it is applied directly.
func (s *Scheduler) holdReap(ack db.ReapAck) {
	if s.reapHold == nil {
		s.reapHold = make(map[string]int, 1)
	}
	s.reapHold[ack.Class]++
	s.openReaps = append(s.openReaps, ack)
}

// unacknowledgedReapsInClass is what `classHeadroom` folds in (A15).
//
// AN UNACKNOWLEDGED REAP OCCUPIES A SLOT IN ITS CLASS — which is exactly what
// "holds write headroom" says. With `[limits] write = { max = 1 }`, one
// unacknowledged reap makes `inFlight = 1`, `1 < 1` is false, and no write-class
// step is ready.
//
// WHY THE FOLD RATHER THAN AN R8. Because it IS headroom. The predicate answers
// "how many things may write to this tree right now", and a lapsed-but-
// unconfirmed writer is one of them. A separate ReadyCondition would be two
// mechanisms answering one question, and they would disagree the first time
// somebody changed one. TestReapHoldIsHeadroom asserts the fold by proving a
// class with `max = 2` and one unacknowledged reap still admits ONE step —
// which a separate blocking condition would not.
// A REAP DOES NOT HOLD AGAINST THE STEP IT REAPED. W3 is explicit: "the reaped
// step ITSELF is offered (it returned to the pool) but no OTHER write-class step
// is — the hold occupies the one slot."
//
// The exclusion is required by the arithmetic rather than being a softening of
// it. With `max = 1`, counting the hold against its own step would make
// `inFlight = 1` for that step too, and the reaped step would be withheld from
// the pool it was just returned to — so a class bounded at 1 would stop dead the
// first time a lease lapsed, with the ONLY step that could clear the hold being
// the one the hold forbade.
//
// It is also the right meaning. The hazard (§6.1) is TWO processes editing one
// tree: the possibly-alive writer, and a SUCCESSOR started beside it. Re-offering
// the same step to a claimant that will do the same work is the retry the lease
// mechanism exists to enable, and the `attempt` trail records it. What the hold
// forbids is a DIFFERENT writer joining it.
func (s *Scheduler) unacknowledgedReapsInClass(class string, stepID int) int {
	if s.reapHold == nil {
		return 0
	}
	held := s.reapHold[class]
	for _, ack := range s.openReaps {
		if ack.Class == class && ack.StepID == stepID {
			held--
		}
	}
	if held < 0 {
		return 0
	}
	return held
}

// UnacknowledgedReaps exposes the snapshot's open reaps for the REFUSAL MESSAGE.
//
// A headroom denial with nothing running is otherwise baffling (§6.3's closing
// paragraph), so `next`'s human output additionally names the unacknowledged
// reaps and the flag that clears them. The rows come from the same snapshot the
// predicate used, so the message cannot name a different set of reaps from the
// one that caused the denial.
func (s *Scheduler) UnacknowledgedReaps() []db.ReapAck { return s.openReaps }

// ReapHoldReason renders A11's guidance: each seq, and the exact flag to pass.
//
// It NAMES THE FLAG rather than describing it, because §2's "surfaced by `guard
// spawn`" is what makes the mechanism discoverable rather than documented — an
// operator reading the refusal should be able to copy the next command out of
// it.
//
// The `guard spawn` entry point lands in GROUP 3 (§6.2's table); this group
// ships `dispatch open --ack-reap`, which is the NEW RELAY's path and is what
// makes the mechanism complete without group 3. The text names both because the
// hold is what it is regardless of which verb clears it, and a message that
// omitted the other entry point would go stale the moment group 3 lands.
func ReapHoldReason(reaps []db.ReapAck) string {
	if len(reaps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("unacknowledged reaps in bounded classes hold headroom: ")
	for i, r := range reaps {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s (class %s) at seq %d", r.Instance, r.Class, r.ReapedSeq)
	}
	b.WriteString(" — confirm the previous holder is gone, then acknowledge with " +
		"`dispatch open --ack-reap <seq>` (or `guard spawn --ack-reap <seq>`)")
	return b.String()
}
