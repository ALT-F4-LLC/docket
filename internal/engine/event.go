package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The event seam (TDD §7.6). Every transition writes an event, in the
// transaction that performs it.
//
// PHASE 4 SWITCHED THIS ON. Phases 2 and 3 wrote through the seam while it was
// a no-op, so they were independently stoppable without leaving dangling event
// calls behind them; this phase changed the BODY of recordEvent and not one of
// its sixteen call sites. That is the property the seam was built for: the
// transitions were already instrumented, in the transactions that perform them,
// in the order the code actually executes — not in whatever order someone
// auditing after the fact got to them.

// The CLOSED SET of event kinds (§7.6). Phases 2 and 3 named the kinds they
// emit; phase 4 completes the set with the two loop/join kinds and enforces it.
//
// §9 item 2 — "every transition is attributable to `next`, a gate, a threshold,
// or human input" — is checkable only because this set is closed. A call site
// passing a bare string literal would put a kind outside the set into the table
// and make the check vacuous, so the writer REFUSES a kind that is not one of
// these (see eventKinds and recordEvent), and TestEventKindsAreAClosedSet
// enumerates the constants against the writer's own table.
const (
	EventRunStarted     = "run-started"
	EventRunActivated   = "run-activated"
	EventRunPaused      = "run-paused"
	EventRunResumed     = "run-resumed"
	EventRunAbandoned   = "run-abandoned"
	EventRunDone        = "run-done"
	EventIssuePromoted  = "issue-promoted"
	EventIssueAbandoned = "issue-abandoned"

	// The live-status mirror kinds (DKT-294). `issue-in-progress` covers BOTH
	// directions that land an issue in `in-progress`: claim's `todo -> in-progress`
	// and review's bidirectional return once nothing on the issue is still
	// `waiting-human` — the two are one concept ("the issue is actively being
	// worked"), not two kinds artificially split by which edge produced it.
	// `issue-review` is the other direction: a gate/vote step of the issue
	// parked `waiting-human`, so an operator or voter is now who the issue is
	// waiting on. Both earn their place on §9 item 2's argument like every kind
	// above: the issue mirror moving is a transition, and before this change
	// nothing recorded it — `in-progress` and `review` were validated and
	// rendered but never written (engine-spec §2).
	EventIssueInProgress = "issue-in-progress"
	EventIssueReview     = "issue-review"

	// Step-lifecycle kinds (phase 3).
	EventStepReady      = "step-ready"
	EventStepClaimed    = "step-claimed"
	EventStepHeartbeat  = "step-heartbeat"
	EventStepRecorded   = "step-recorded"
	EventGateStarted    = "gate-started"
	EventGateRecorded   = "gate-recorded"
	EventStepRouted     = "step-routed"
	EventStepFailed     = "step-failed"
	EventStepSkipped    = "step-skipped"
	EventStepSuperseded = "step-superseded"
	EventStepResolved   = "step-resolved"
	EventStepApproved   = "step-approved"
	EventStepRejected   = "step-rejected"
	EventLeaseReaped    = "lease-reaped"

	// Loop and join kinds (phase 4). `loop-entered` records a `fix-loop`
	// routing that actually entered a loop — not one that hit `max_fix_loops`
	// and became `waiting-human`, which is a step-routed to `waiting-human` and
	// is not a loop entry at all.
	EventLoopEntered   = "loop-entered"
	EventJoinCompleted = "join-completed"

	// Gate and trust kinds (stage 4, gates-trust §6.4). The closed set gains
	// exactly four, and they are listed in the TDD so §9 item 2's check keeps
	// passing rather than silently widening.
	//
	// `gate-unmatched` is SEPARATE from `gate-recorded` so an operator
	// following the feed sees a refusal to execute as its own event, rather
	// than as a result they must open and inspect. `gate-rerun` precedes each
	// flaky re-run (§5.6) for the same at-least-once observability reason
	// `gate-started` exists.
	//
	// `trust-added` / `trust-removed` are what make the self-trust residual
	// (T9) auditable: a session that grants itself a command mid-run leaves the
	// grant in the run's own trail, so a retro can find it. They carry the
	// ARGV HASH, never the argv — a trusted command's arguments must not land
	// in an event feed a run report renders.
	EventGateUnmatched = "gate-unmatched"
	EventGateRerun     = "gate-rerun"
	EventTrustAdded    = "trust-added"
	EventTrustRemoved  = "trust-removed"

	// Vote-step lifecycle kinds (gates-trust §8.1 phases 2 and 4).
	//
	// RECORDED AMENDMENT: §6.4 of that TDD enumerates "exactly four" kinds this
	// stage adds and does not list these two, while §8.1's own lifecycle table
	// requires both by name ("writes a `vote-opened` event", "writes a
	// `vote-tallied` event with the score"). The two sections of one document
	// disagree; §8.1 is the operative requirement because it specifies
	// behavior, and §6.4's count is bookkeeping that did not travel with it.
	// So the closed set gains SIX at this stage, not four, and the deviation is
	// filed rather than made silently.
	//
	// They earn their place on the same argument §6.4 makes for
	// `gate-unmatched`: a vote opening and a vote tallying are transitions an
	// operator follows in the feed, and §9 item 2 requires every transition be
	// attributable. Without them a proposal appears with nothing explaining
	// which step opened it.
	EventVoteOpened  = "vote-opened"
	EventVoteTallied = "vote-tallied"

	// The held-cluster kind (stage 5, payloads-thresholds §7.7).
	//
	// The closed set gains exactly ONE, and it earns its place on the same
	// argument every kind above earns it: §9 item 2 requires every transition
	// to be attributable, and materializing a `<step>-held` question is a
	// transition an operator follows in the feed. Without it a gate step nobody
	// declared appears in a run with nothing explaining where it came from.
	//
	// The resolution needs no new kind whichever way the hold was minted.
	// `step-approved`/`step-rejected` already say what an operator did to a
	// gate, and a human-minted hold is a human gate in every respect the verbs
	// can observe (H3). A VOTE-MINTED hold reports through the vote kinds while
	// the tally runs (`vote-opened`, `vote-tallied`, `step-routed`), and through
	// the same approve/reject kinds once a failed tally parks it for an
	// operator — so every one of its transitions is already attributable to the
	// party that made it.
	EventStepHeld = "step-held"

	// Dispatch kinds (stage 6 group 2, docs/tdd/runs-dispatch.md §5).
	//
	// The closed set gains exactly THREE, and each is a transition §9 item 2
	// requires to be attributable. `dispatch-abandoned` is named by engine-spec
	// §2 VERBATIM — "a dispatch TTL lazily auto-abandoned by `next`
	// (event-logged)" — so it is the one kind here the spec asks for by
	// implication rather than the TDD choosing.
	//
	// `dispatch-closed` and `dispatch-abandoned` are SEPARATE kinds rather than
	// one `dispatch-ended` with a reason, for the same argument that keeps
	// `gate-unmatched` separate from `gate-recorded`: an operator following the
	// feed must see "the batch was reconciled" and "the batch was given up on"
	// as different events, not as one event they have to open and inspect. The
	// two mean opposite things about whether the relay's work is accounted for.
	//
	// `dispatch-abandoned` carries `data.reason` — `ttl` for P14's lazy path,
	// `abandoned` for P21's explicit one — so the feed distinguishes a crashed
	// relay's manifest expiring from an operator retiring it.
	EventDispatchOpened    = "dispatch-opened"
	EventDispatchClosed    = "dispatch-closed"
	EventDispatchAbandoned = "dispatch-abandoned"

	// The write-reap acknowledgment kind (§6.2).
	//
	// It earns its place on A3: "the ack must be ATTRIBUTABLE" is a requirement
	// of the mechanism, and §9 item 2 requires every transition to be traceable.
	// Releasing write headroom is a transition — a successor becomes claimable
	// that was not — and without this kind the release would appear in the feed
	// as nothing at all, with the run simply starting to offer write-class work
	// again for no recorded reason.
	//
	// It carries `data.acked_by`, which is the VERB (A8: `guard-spawn` |
	// `dispatch-open`) and never a user identity, because core has no identity
	// model.
	EventReapAcknowledged = "reap-acknowledged"

	// Stage 7's two kinds (docs/tdd/events-follow.md §6, §7.3).
	//
	// `events-pruned` earns its place on the argument every kind above earns it
	// — §9 item 2 requires every transition to be attributable — plus one that
	// is specific to it: THE PRUNE IS THE ONE TRANSITION THAT DESTROYS
	// EVIDENCE. The record that it happened is the only thing standing between
	// a trimmed log and a log that looks like it was never written, and a prune
	// leaving no event would be indistinguishable from a run that simply made
	// fewer transitions. Its `seq` is above everything it deleted, so the record
	// of the deletion survives the deletion.
	//
	// `run-budget-set` is the kind S6's reasoning did NOT already cover. That
	// stage refused a `budget-breached` kind on the grounds that `run-paused`
	// already anchors the fact; the counter does not apply here, because no
	// existing kind anchors "the cap moved". Without it, a run that breached at
	// 12 and later admitted a claim at 20 would have a trail with nothing
	// explaining the difference — the exact gap operations.md §4 warned about
	// when the only way to raise a cap was to edit the database.
	EventEventsPruned = "events-pruned"
	EventRunBudgetSet = "run-budget-set"

	// The post-completion annotation kind (DKT-35).
	//
	// It earns its place on §9 item 2's argument like every kind above:
	// merging metadata onto a finished step's record is a mutation of the run
	// record, and a record that changed with no event would be evidence that
	// rewrote itself. The event carries the annotation verbatim, so what was
	// added survives a later annotation overwriting the same key.
	EventStepAnnotated = "step-annotated"

	// The tenancy kind (DKT-61).
	//
	// It earns its place on §9 item 2's argument, extended one step: a project
	// row is the thing every other row is attributed TO, so a project that
	// appears with no event is a hole underneath the whole attribution chain.
	// Every issue, run, and step in the store says which project it belongs to,
	// and until this kind existed the project itself said nothing about where
	// it came from.
	//
	// The absence was measured, not theorized. Attributing one junk row to the
	// verb that minted it took a hand-join of raw table timestamps against nine
	// session transcripts, because the store held no other record of the act.
	//
	// It carries `cwd`, `identity`, and `verb` — where the invocation ran, what
	// it resolved to, and which command it was — which are exactly the three
	// facts that reconstruction had to recover by hand. Like `trust-added` it
	// has NO RUN: registration precedes any run of the project by definition.
	EventProjectRegistered = "project-registered"

	// The repin kind (DKT-408).
	//
	// It earns its place on §9 item 2's argument in the form `events-pruned`
	// made famous: a repin MOVES THE RUN'S RECORDED AGREEMENT. The `pins` rows
	// are what every packet, render, and payload validation verifies bytes
	// against, and after a repin they no longer say what completed steps
	// actually worked under — this event is what still does. It carries the
	// old sha AND the new one per changed ref, plus the operator's reason, so
	// the agreement any given step consumed stays recoverable from the trail:
	// steps recorded before this event's seq worked under `old_sha256`, steps
	// claimed after it work under `new_sha256`. One event per changed ref, all
	// in the repin's own transaction — the old hash's survival must not depend
	// on a second commit landing.
	EventRunRepinned = "run-repinned"

	// The spawn carve-out kind (DKT-236).
	//
	// It earns its place on §9 item 2's argument in its sharpest form: this
	// event records a hold being STEPPED PAST. Without it, a spawn admitted
	// over an open reap hold is indistinguishable in the record from a spawn
	// nothing was holding — the one case where "no event" and "nothing
	// happened" say the same thing while meaning opposite things.
	//
	// It carries `carve_out` (which rule admitted the spawn), `proposal` (the
	// open question the batch exists to decide), and `hold` (the refusal text
	// it was admitted over), which are exactly the three facts an auditor
	// asking "why was this allowed?" needs.
	EventSpawnAdmitted = "spawn-admitted"

	// The batch gate-override kinds (DKT-546).
	//
	// Both earn their place on §9 item 2's argument in the `spawn-admitted`
	// form: each records a park being STEPPED PAST on standing authority.
	// `gate-override-granted` is the authority being minted — one operator
	// ruling that a gate's failure signature is environmental for the rest of
	// the run — and it carries `gate#grantid` so the grant row the feed names
	// is findable. `step-batch-overridden` is the authority being SPENT: a
	// step whose failed gates were auto-passed under that ruling, carrying the
	// covering grant id(s). Without the second kind an auto-applied override
	// would be indistinguishable in the feed from an engine-computed pass —
	// the one case where "no event" and "the gates passed" say the same thing
	// while meaning opposite things.
	EventGateOverrideGranted = "gate-override-granted"
	EventStepBatchOverridden = "step-batch-overridden"

	// The stale-target waiver being minted (DKT-742) — the same §9 item 2
	// argument as `gate-override-granted`: a warning that stops appearing is
	// otherwise indistinguishable in the record from a warning that stopped
	// being true, and this kind is what says an operator ruled rather than
	// git relented. It carries `targetsha#waiverid` so the waiver row the
	// feed names is findable. The waiver being SPENT records no event,
	// deliberately: the advisory is recomputed by `dispatch verify`, which
	// writes nothing by contract, and an advisory suppressed is not a
	// transition — no step changes status because a warning stayed quiet.
	EventStaleTargetWaived = "stale-target-waived"

	// The activation snapshot's scope being REFRESHED mid-run (DKT-869).
	//
	// It earns its place on the `run-repinned` argument, one column over: this
	// is the only other transition that moves a frozen premise while a run is
	// live. Every packet an issue's remaining steps render, and every diff they
	// record, read `run_issues.issue_snapshot`; without this kind a reader of
	// the ledger comparing two steps of one run would see two different
	// declared scopes with nothing between them explaining the difference —
	// which is exactly the drift the freeze exists to prevent, reintroduced
	// silently. It carries `from`, `to`, the step instances the refresh
	// reaches, and the operator's reason, so the discontinuity is dated and
	// attributable rather than inferred.
	EventIssueScopeRefreshed = "issue-scope-refreshed"
)

// eventKinds is the closed set, as a set. The writer checks membership here, so
// "the closed set" is one list rather than a convention every call site is
// trusted to follow.
//
// It is derived from the constants above by hand rather than by reflection
// because a test asserts the two agree (TestEventKindsAreAClosedSet): a
// generated set could never disagree with itself, and would therefore prove
// nothing about a constant someone adds without thinking about §9 item 2.
var eventKinds = map[string]bool{
	EventRunStarted: true, EventRunActivated: true, EventRunPaused: true,
	EventRunResumed: true, EventRunAbandoned: true, EventRunDone: true,
	EventIssuePromoted: true, EventIssueAbandoned: true,
	EventIssueInProgress: true, EventIssueReview: true,
	EventStepReady: true, EventStepClaimed: true, EventStepHeartbeat: true,
	EventStepRecorded: true, EventGateStarted: true, EventGateRecorded: true,
	EventStepRouted: true, EventStepFailed: true, EventStepSkipped: true,
	EventStepSuperseded: true, EventStepResolved: true, EventStepApproved: true,
	EventStepRejected: true, EventLeaseReaped: true,
	EventLoopEntered: true, EventJoinCompleted: true,
	EventGateUnmatched: true, EventGateRerun: true,
	EventTrustAdded: true, EventTrustRemoved: true,
	EventVoteOpened: true, EventVoteTallied: true,
	EventStepHeld:       true,
	EventDispatchOpened: true, EventDispatchClosed: true,
	EventDispatchAbandoned: true, EventReapAcknowledged: true,
	EventEventsPruned: true, EventRunBudgetSet: true,
	EventStepAnnotated:       true,
	EventProjectRegistered:   true,
	EventRunRepinned:         true,
	EventSpawnAdmitted:       true,
	EventGateOverrideGranted: true, EventStepBatchOverridden: true,
	EventStaleTargetWaived:   true,
	EventIssueScopeRefreshed: true,
}

// recordEvent writes one event in the caller's transaction.
//
// This is the phase-4 body. The signature is the one phases 2 and 3 called, so
// switching the seam on edited this function and nothing else — every call site
// already handled the error an INSERT can produce.
//
// The arguments are the §11.4 `event` shape minus `seq`, which SQLite assigns:
// `kind` from the closed set, the run it belongs to, the step instance when
// there is one, and an opaque `data` payload.
//
// `seq` is INTEGER PRIMARY KEY AUTOINCREMENT (§7.1), so it is monotonic and
// NEVER REUSED — including after a delete. That is what makes S7's `--since`
// cursor sound: with a plain rowid, deleting the newest event would let the
// next insert reuse its number and a consumer holding that cursor would skip
// the replacement. Monotonic-in-practice is not the same property as monotonic.
func recordEvent(tx *sql.Tx, e eventRecord) error {
	// The closed set, enforced (§7.6). A kind outside it is a programming
	// error — a literal that got past the constants — and refusing it here is
	// what keeps §9 item 2's check from being vacuous. It fails the
	// TRANSACTION, which for a transition means the transition does not happen:
	// an unattributable state change is worse than a refused one.
	if !eventKinds[e.Kind] {
		return fmt.Errorf(
			"event kind %q is not in the closed set (§7.6); add it to the "+
				"constants and to eventKinds, or use an existing kind", e.Kind)
	}

	// The step is addressed by its rendered `name@k#i` identity at every call
	// site, because that is the identity §11.3 makes public and the one the
	// wire shapes and error strings carry. The TABLE stores `step_id`, so the
	// resolution happens here — once — rather than at sixteen call sites.
	//
	// The lookup runs in the CALLER'S transaction, so a step inserted and
	// event-logged in one transaction (expansion's `step-skipped`, §5.3.1)
	// resolves against the uncommitted row. A lookup through the pool would
	// both miss that row and deadlock: internal/db caps the pool at one
	// connection.
	var stepID any
	if e.Instance != "" && e.RunID != 0 {
		var id int
		err := tx.QueryRow(
			`SELECT id FROM steps WHERE run_id = ? AND instance = ?`+
				stepIssueClause(e.IssueID),
			stepQueryArgs(e)...,
		).Scan(&id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// A step-shaped event whose instance has no row: the identity is
			// still recorded in `data` below, so the event is not lost. This is
			// reachable for an instance named in a routing that never
			// materialized, and dropping the event would be worse than
			// recording it without the foreign key.
		case err != nil:
			return fmt.Errorf("resolving step %s for a %s event: %w", e.Instance, e.Kind, err)
		default:
			stepID = id
		}
	}

	data, err := eventData(e)
	if err != nil {
		return err
	}

	atMS, err := monotonicAtMS(tx, e.atMS())
	if err != nil {
		return fmt.Errorf("stamping a %s event: %w", e.Kind, err)
	}

	_, err = tx.Exec(
		`INSERT INTO events (at_ms, kind, run_id, step_id, issue_id, data)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		atMS, e.Kind, nullableID(e.RunID), stepID, nullableID(e.IssueID), data,
	)
	if err != nil {
		return fmt.Errorf("recording a %s event: %w", e.Kind, err)
	}
	return nil
}

// monotonicAtMS clamps an event's timestamp so `at_ms` never decreases as `seq`
// increases (DKT-66).
//
// THE FEED DOCUMENTS ITSELF AS OLDEST-FIRST ARRIVAL, and it was not. Callers may
// pass a `nowMS` computed at the top of a long transaction — the resume path
// carried one across an entire gate execution — so an event emitted a minute
// later was stamped a minute earlier than the event before it. Measured: a
// `gate-unmatched` at seq 265 stamped 57 seconds before seq 264, plus older
// inversions at seq 14 and 41. Any consumer that windows on `at_ms` — retro
// mining above all — mis-orders the history it reads.
//
// The clamp is here, in the writer, rather than at the call sites that were
// caught, because the defect is a CLASS: any future caller holding a stale
// clock reintroduces it, and a fix applied to two call sites would prove nothing
// about the third. `seq` remains the ordering of record (§7.1); this makes
// `at_ms` agree with it instead of quietly contradicting it.
//
// The anchor is the LAST EVENT BY SEQ, not MAX(at_ms): with the clamp in place
// they are the same value by induction, and the primary-key index makes the
// former O(1) while the latter would scan an unindexed column. The read runs in
// the caller's transaction, so a batch of events written together stays ordered
// among themselves.
//
// A clamped event is stamped one millisecond LATE, never early. That is a
// smaller lie than the inversion it replaces: it says "at or after the event
// before it", which is true of every event, where the raw value said the event
// happened before something that preceded it, which is true of none.
func monotonicAtMS(tx *sql.Tx, atMS int64) (int64, error) {
	var last sql.NullInt64
	err := tx.QueryRow(
		`SELECT at_ms FROM events ORDER BY seq DESC LIMIT 1`).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return atMS, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the last event's timestamp: %w", err)
	}
	if last.Valid && last.Int64 > atMS {
		return last.Int64, nil
	}
	return atMS, nil
}

// eventData renders the `data` column: always a JSON OBJECT, never a bare
// string.
//
// The call sites pass `Data` as whatever that transition's detail is — a gate
// name, a routing, a `--usage` blob — because that is the natural thing to pass
// and the engine never reads it back (genericity.md: "execution metadata is an
// opaque KV bag"). But §11.4's `event` shape puts `data` in a JSON envelope and
// the column defaults to `'{}'`, so a bare `pass` written verbatim would leave
// the column holding invalid JSON that every S6/S7 consumer would have to
// special-case. Normalizing HERE keeps the sixteen call sites simple and the
// column's contract true.
//
// Three cases, in order:
//
//   - already a JSON object (`--usage '{"tokens":10}'`): passed through, so a
//     caller's structure survives intact;
//   - a non-empty non-object string (`pass`, `build`): wrapped as
//     {"detail": "..."};
//   - empty: the object is whatever the instance contributes, or `{}`.
//
// The INSTANCE rides along in every case. When the step lookup misses, that
// string is the only record of which instance the event was about — which is
// what makes recording an unresolvable event better than dropping it.
func eventData(e eventRecord) (string, error) {
	fields := make(map[string]json.RawMessage, 2)

	if e.Instance != "" {
		instance, err := json.Marshal(e.Instance)
		if err != nil {
			return "", fmt.Errorf("recording the instance for a %s event: %w", e.Kind, err)
		}
		fields["instance"] = instance
	}

	if raw := strings.TrimSpace(e.Data); raw != "" {
		if json.Valid([]byte(raw)) && strings.HasPrefix(raw, "{") {
			// A caller-supplied object: merge its keys in, so `--usage`'s
			// structure is preserved rather than nested under a wrapper.
			var supplied map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &supplied); err != nil {
				return "", fmt.Errorf("reading the data for a %s event: %w", e.Kind, err)
			}
			for k, v := range supplied {
				fields[k] = v
			}
		} else {
			detail, err := json.Marshal(e.Data)
			if err != nil {
				return "", fmt.Errorf("recording the detail for a %s event: %w", e.Kind, err)
			}
			fields["detail"] = detail
		}
	}

	if len(fields) == 0 {
		return "{}", nil
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("recording the data for a %s event: %w", e.Kind, err)
	}
	return string(out), nil
}

// stepIssueClause narrows the instance lookup by issue when the caller knows
// it. `steps` is UNIQUE(run_id, issue_id, instance) — NOT unique on
// (run_id, instance) — so two issues in one run running the same workflow both
// have an `implement@0`, and a lookup without the issue would resolve to
// whichever came first.
func stepIssueClause(issueID int) string {
	if issueID == 0 {
		return ``
	}
	return ` AND issue_id = ?`
}

func stepQueryArgs(e eventRecord) []any {
	args := []any{e.RunID, e.Instance}
	if e.IssueID != 0 {
		args = append(args, e.IssueID)
	}
	return args
}

// nullableID maps a zero id to NULL, so an event with no run or no issue stores
// NULL rather than a 0 that references nothing.
func nullableID(id int) any {
	if id == 0 {
		return nil
	}
	return id
}

// eventRecord is one event's fields. It is a struct rather than a parameter
// list because phase 4 adds `data` payloads per kind, and a growing positional
// signature is how call sites end up passing the wrong string.
type eventRecord struct {
	Kind string
	// RunID is the run the event belongs to. Every event this phase writes has
	// one; step-level events in phase 3 add the instance.
	RunID int
	// Instance is the rendered `name@k#i` step identity, or "" for a
	// run-level event.
	Instance string
	// IssueID is the issue an issue-level event concerns, or 0.
	IssueID int
	// Data is an opaque JSON payload. Core never reads a key inside it.
	Data string
	// AtMS is the event's timestamp. It is OPTIONAL: a zero value means "now",
	// resolved by atMS below.
	//
	// It is optional because the seam's contract is that phase 4 changed the
	// body and no call site — and the sixteen existing sites pass no clock.
	// Ordering does not depend on it in any case: `seq` is the order (§7.1,
	// §11.4), and `at_ms` is the human-readable when. A caller that has a
	// meaningful `nowMS` — every routing transaction does — sets it so the
	// event agrees with the row it describes.
	//
	// IT IS A FLOOR, NOT THE FINAL VALUE. monotonicAtMS clamps whatever lands
	// here up to the previous event's stamp, so a caller holding a clock from
	// the top of a long transaction cannot write an event that appears to
	// precede the one before it (DKT-66). A call site that has no reason to
	// pass a stale clock should pass none at all and stamp at emission.
	AtMS int64
}

// atMS resolves the event's timestamp: the caller's when it set one, otherwise
// the wall clock.
//
// The fallback reads the clock rather than storing 0. An event stamped 0 would
// render as 1970 in every consumer S6 and S7 build, which is a worse answer
// than a millisecond of drift from the transaction's own `nowMS`.
func (e eventRecord) atMS() int64 {
	if e.AtMS != 0 {
		return e.AtMS
	}
	return time.Now().UnixMilli()
}

// TrustGrant is what a trust entry AUTHORIZES, in the shape gates-trust §3.6
// records: "the gate name, the argv hash …, the repo binding, and the flags".
//
// IT HAS NO ARGV FIELD, and that absence is load-bearing. §3.6 forbids the argv
// itself in the event body, so the type a caller must fill in cannot carry one —
// which is also why this is declared here rather than reusing the store's entry
// type. That type holds the argv one field access away from the event writer,
// and a mapping written by hand at the call site is where the hash discipline
// stays visible.
//
// EVERY BEHAVIOR-AFFECTING PROPERTY IS A FIELD. A grant that recorded only the
// name and the hash left the two flags that widen what a command may do — `tree`
// and `network` — invisible in the feed, so a remove and a re-add milliseconds
// apart could widen egress with a trail that showed only that something named
// the same thing was re-approved.
type TrustGrant struct {
	Name       string
	ArgvSHA256 string
	Repo       string
	Global     bool
	Prefix     bool
	ReRunnable bool
	Tree       bool
	Flaky      bool
	Network    []string
	Timeout    string
	// Stub is the operator's declaration that this entry authorizes a
	// PLACEHOLDER rather than the check its name implies (DKT-265).
	//
	// It rides in the event for the same reason `tree` and `network` do: it is
	// what the grant is really granting. A feed that showed a `secret-scan`
	// entry being added, without saying it points at `/usr/bin/true`, records
	// the name of an assurance rather than the assurance.
	Stub bool
	// StubReason is the operator's recorded why-and-what-tracks-it for a stub
	// (DKT-607). It rides in the event so the grant's trail carries the
	// documented decision, not only the fact of hollowness; empty when the
	// grant recorded none (every pre-DKT-607 stub) and always empty on a
	// non-stub grant.
	StubReason string
	// Actor is WHO ran the verb — the git identity, falling back to the OS
	// username (DKT-263). It is A CLAIM, NOT A VERIFIED FACT: `git config
	// user.name` is whatever the invoking environment says it is, and nothing
	// here authenticates it. That is the same footing step metadata stands on,
	// and it is worth having anyway — before this field, recovering by-whom
	// meant bracketing runs against wall-clock, which the retro had to do
	// twice.
	//
	// IT IS REQUIRED (DKT-595): RecordTrustEvent refuses an empty one. The
	// literal "unknown" — the resolver's last fallback when neither a git
	// identity nor an OS user exists — DOES count as supplied: it is the
	// resolver's honest report of an anonymous environment, the same value
	// every other authored row carries there. What is refused is the empty
	// string, which no resolver produces and which can only mean the writer
	// never filled the field.
	Actor string
	// Cwd is where the verb ran from.
	//
	// It is the DISAMBIGUATOR, and it is the field that does the real work on a
	// machine where every grant carries the same git identity: two concurrent
	// sessions are one operator with two working directories, and only the cwd
	// separates them. Same reasoning as ProjectRegistration's Cwd/Identity
	// pair — the case worth attributing is the one where the person is not the
	// distinguishing fact.
	//
	// IT IS REQUIRED (DKT-595), same as Actor: RecordTrustEvent refuses an
	// empty one, and a caller whose working directory cannot be read must
	// refuse the whole trust change rather than degrade the field.
	Cwd string
}

// RecordTrustEvent writes a `trust-added` / `trust-removed` event (§3.6).
//
// It is exported because the trust CLI is where a grant happens, and the CLI is
// outside this package. It takes the ARGV HASH rather than the argv, and the
// parameter type says so: a trusted command's arguments must not land in an
// event feed a run report renders, and a helper that ACCEPTED an argv would make
// that mistake available to the next caller.
//
// The event has NO RUN. `events.run_id` is nullable, and a trust grant is a
// user-level act that may happen with no run in flight at all — attributing it
// to an arbitrary run would be a fabrication. What makes it useful is the
// TIMESTAMP: a run whose trail brackets this event is a run during which the
// grant happened, which is exactly the retro question T9's residual raises.
//
// IT ALSO RECORDS WHO (DKT-263). The timestamp answers "during which run", and
// that was always the smaller half of the question — bracketing wall-clock
// still could not say which of two concurrent sessions widened what code may
// execute. `Actor` and `Cwd` answer it directly. Neither is authenticated and
// neither claims to be: events_read's actor CLASS says a person did this, which
// is a classification; these two are the person's own account of themselves,
// which is an identity. A grant is the one act in the system that widens what
// may execute, so its trail should not require a join against a clock.
//
// BOTH ARE MANDATORY, AND THE REFUSAL LIVES HERE (DKT-595). The 2026-08-19
// batch — 43 unattributed trust events, twelve of which widened network
// egress — came from a writer that supplied neither field, and the ledger
// could not say whether that writer was an older binary or a non-interactive
// path. That ambiguity is unrepairable after the fact, so it is closed at the
// door: this function, the single entry point through which a trust event
// reaches the table, refuses a grant whose Actor or Cwd is empty, BEFORE
// anything is written. Guarding here rather than only at the CLI call site is
// what makes the property hold for a future non-CLI writer too.
func RecordTrustEvent(conn *sql.DB, kind string, grant TrustGrant, atMS int64) error {
	// THE UNATTRIBUTED-GRANT REFUSAL (DKT-595), ahead of the marshal so a
	// refused event leaves no differently-shaped payload behind — the write
	// below still emits every key unconditionally or does not happen at all.
	if grant.Actor == "" || grant.Cwd == "" {
		return fmt.Errorf(
			"refusing to record an unattributed %s event (actor=%q cwd=%q): a trust change must say who made it and from where, and a writer that cannot supply both must not write one",
			kind, grant.Actor, grant.Cwd)
	}

	// EVERY KEY IS WRITTEN, including the false ones and the empty ones. A
	// payload that omitted `tree` when false and `network` when empty would be
	// byte-identical to one from a writer that never had the field, so a reader
	// diffing a `trust-removed` against the `trust-added` that followed it could
	// not tell a flag that was OFF from a flag that was NOT RECORDED. That
	// distinction is the difference between an audit trail and a feed of hints.
	network := grant.Network
	if network == nil {
		network = []string{}
	}
	data, err := json.Marshal(map[string]any{
		"name":        grant.Name,
		"argv_sha256": grant.ArgvSHA256,
		"repo":        grant.Repo,
		"global":      grant.Global,
		"prefix":      grant.Prefix,
		"re_runnable": grant.ReRunnable,
		"tree":        grant.Tree,
		"flaky":       grant.Flaky,
		"network":     network,
		"timeout":     grant.Timeout,
		"stub":        grant.Stub,
		"stub_reason": grant.StubReason,
		// Written unconditionally like every other key — the key set stays
		// exact in both directions — and, since the DKT-595 guard above,
		// guaranteed NON-EMPTY. The empty string was once accepted here as
		// "the field was unresolvable", but the 2026-08-19 batch showed what
		// that buys in practice: an empty value cannot be told apart from a
		// writer that never tried, so it distinguished nothing. Emptiness now
		// refuses before the write; a key that is present is a value that
		// attributes.
		"actor": grant.Actor,
		"cwd":   grant.Cwd,
	})
	if err != nil {
		return fmt.Errorf("encoding the trust event: %w", err)
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("recording the trust event: %w", err)
	}
	defer tx.Rollback()

	if err := recordEvent(tx, eventRecord{
		Kind: kind, Data: string(data), AtMS: atMS,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ProjectRegistration is what a `project-registered` event records (DKT-61):
// where the invocation ran, what that resolved to, and which verb did it.
//
// Cwd and Identity are SEPARATE fields even though they are usually the same
// path, because the case worth attributing is exactly the one where they
// differ: an executor running from a scratchpad directory registered a project
// under that directory's name, and only the pair says so.
type ProjectRegistration struct {
	// ID is the project row that came into being.
	ID int
	// Name is the row's display name.
	Name string
	// Identity is the path the row is keyed by.
	Identity string
	// Cwd is the working directory the invocation ran from.
	Cwd string
	// Verb is the full command path (`step complete`, `issue create`) that
	// triggered the registration.
	Verb string
	// Prefix is the display prefix the row was given.
	Prefix string
}

// RecordProjectRegisteredEvent writes a `project-registered` event (DKT-61).
//
// Exported for the same reason RecordTrustEvent is: registration happens in the
// CLI's root hook, outside this package, and the alternative — letting the hook
// write its own INSERT — would put a kind into the events table without passing
// the closed-set check that makes the set closed.
func RecordProjectRegisteredEvent(conn *sql.DB, reg ProjectRegistration, atMS int64) error {
	data, err := json.Marshal(map[string]any{
		"project_id": reg.ID,
		"name":       reg.Name,
		"identity":   reg.Identity,
		"cwd":        reg.Cwd,
		"verb":       reg.Verb,
		"prefix":     reg.Prefix,
	})
	if err != nil {
		return fmt.Errorf("encoding the project-registered event: %w", err)
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("recording the project-registered event: %w", err)
	}
	defer tx.Rollback()

	if err := recordEvent(tx, eventRecord{
		Kind: EventProjectRegistered, Data: string(data), AtMS: atMS,
	}); err != nil {
		return err
	}
	return tx.Commit()
}
