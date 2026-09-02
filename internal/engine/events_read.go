package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// THE EVENTS READ SURFACE — engine-spec §11.4's `event` shape, served
// (docs/tdd/runs-dispatch.md §8).
//
// engine-spec §1's surface line is `docket events --follow [--since SEQ]` and §10
// splits it: `--since` here, `--follow` at S7. This stage therefore ships
// `docket events list [--since SEQ]` — see §8.1's argument for the sub-verb, and
// the amendment it is filed under.
//
// IT IS A PURE READ. One `SELECT` in one transaction, no reap, no lazy anything.
// TestReadVerbsWriteNothing asserts the database is byte-identical afterwards,
// which is what makes an operator polling the feed safe: a read verb that
// advanced a lease would make observation a scheduling act.

// Event is §11.4's shape, field for field:
//
//	event { seq, at_ms, kind, run?, step?, data }
//
// plus `step_id`, which is an ADDITION to that shape and is FILED as an
// amendment (§8.2 E4). The argument is that the shape is otherwise unjoinable:
// `step` and `data.instance` both give the HUMAN identity (`name@k#i`), and a
// consumer following the feed into `step show` needs the id it addresses steps
// by. Shipping the field and recording the note follows precedent — the spec's
// silence would otherwise be resolved silently by whoever implemented first.
type Event struct {
	Seq  int64  `json:"seq"`
	AtMS int64  `json:"at_ms"`
	Kind string `json:"kind"`
	// Run is `RUN-N`, OMITTED WHEN NULL per the `?` in §11.4's shape. A trust
	// event has no run (gates-trust §3.6) and attributing it to one would be a
	// fabrication.
	Run string `json:"run,omitempty"`
	// Step is the RENDERED INSTANCE IDENTITY (`name@k#i`) — matching every other
	// wire shape's step rendering — and omitted when NULL (E1).
	Step string `json:"step,omitempty"`
	// StepID is `STEP-N`: the joinable id (E4, the filed addition).
	StepID string `json:"step_id,omitempty"`
	// Issue is `DKT-N`, omitted when the event has none (DKT-74). Instance
	// labels COLLIDE across issues in one run — two issues on the same
	// workflow both have a `fix@1` — and a feed filtered by instance once
	// misattributed a pass-route to a reaped step over exactly that collision.
	// The column was always stored; the wire now carries it.
	Issue string `json:"issue,omitempty"`
	// Project is the NAME of the project the event belongs to — the run's, else
	// the issue's — and is empty for a store-level event (DKT-67).
	//
	// It exists because `--all-projects` had no project discriminator at all
	// once the prefix stopped being one: two projects can hold a `fix@1` step
	// and a `RUN-6`, and the ids alone do not say whose. Omitted when empty so a
	// single-project feed carries no redundant column.
	Project string `json:"project,omitempty"`
	// Data is the stored JSON object, VERBATIM. Core never reshapes it (E2);
	// §7.6's writer already normalized it to an object on the way in.
	//
	// It is json.RawMessage rather than a map so the bytes reach `--json`
	// unaltered: re-marshaling a decoded map would reorder keys and re-escape
	// strings, which for a consumer diffing two feeds is a change with no cause.
	Data json.RawMessage `json:"data"`
}

// EventPage is one `--since` answer, with the truncation contract every list
// verb carries (E9, E11).
type EventPage struct {
	Events []Event
	// Total counts MATCHING events before the slice, so `truncated` is
	// computable rather than guessed (reliability-delta §4.2).
	Total int
}

// EventQuery is `events list`'s filters.
type EventQuery struct {
	// Since returns events with `seq > Since` — STRICTLY GREATER (E5), so a
	// consumer stores the last seq it saw and passes it back without re-reading
	// it. Zero is the default and returns from the beginning (E6).
	Since int64
	// RunID filters to one run using `idx_events_run_seq`. Zero is the
	// repo-wide feed, which is the only place a trust event is visible (E8).
	RunID int
	// Limit applies AFTER ordering (E9). Zero means the caller's default.
	Limit int
	// Tail selects the NEWEST N matching events instead of the oldest N.
	// Zero is off.
	//
	// This is a SELECTION change, not an ordering one. The returned page is
	// still `seq ASC` — the newest N rows, handed back oldest-first — so a
	// reader consumes them in the same direction as every other answer and the
	// last seq is still the cursor to store. E7's "no reverse mode" stands:
	// what varies is WHICH window of the feed is returned, never its direction.
	//
	// Tail is for the mid-incident question ("what just happened"), which the
	// cursor cannot answer without first paging through the entire history to
	// reach the end.
	Tail int
	// ProjectID scopes the feed to one project (v12); 0 is the whole store.
	//
	// An event's project is its RUN's when it has one, else its ISSUE's, else
	// the REPOSITORY ITS PAYLOAD NAMES (DKT-68) — `repo` for a trust change,
	// `identity` for a registration. A store-level event that names no
	// repository at all is a fact about the store and appears in every scoped
	// view, because a scoped feed that hid it would be an audit trail with a
	// blind spot; one that names another repository belongs to that
	// repository's trail, not to this one's. See eventFilter.
	//
	// IT IS IGNORED WHEN RunID IS SET (DKT-583). A run belongs to exactly ONE
	// project, so `--run` is ALREADY a project scope — a narrower one. Anding
	// the invoking project's scope onto it cannot select a meaningful subset:
	// it either changes nothing (the run is this project's) or empties the page
	// entirely (the run is a neighbor's), and the second case answered
	// `{"ok":true,"events":null,"total":0}` for a run with hundreds of events.
	// A successful empty feed is the WORST available answer — a consumer
	// polling it waits forever on a run it can see in `run report`, which
	// resolves a run's project independent of the cwd. `--run` now does the
	// same, which is the disposition DKT-583 prefers.
	ProjectID int
}

// ListEvents serves `events list --since` (§8.3).
//
// C9 — THE CURSOR RACE — is closed structurally rather than by a lock: the read
// is ONE `SELECT ... WHERE seq > ?` in ONE transaction, and `seq` is
// `AUTOINCREMENT` and monotonic, so an event inserted while this runs lands
// ABOVE the cursor and is returned by the NEXT call. No event is ever skipped
// and none is ever returned twice. TestCursorNeverSkipsUnderConcurrentInsert
// runs inserts against a looping reader and asserts the union is exactly the
// inserted set — the property stated as a test rather than as a comment.
//
// The ordering is `seq ASC`, ALWAYS. There is no reverse mode (E7): a cursor
// feed that could run backwards is a cursor feed that skips.
func ListEvents(conn *sql.DB, q EventQuery) (*EventPage, error) {
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("reading events: %w", err)
	}
	// NEVER COMMITTED. The rollback is the mechanism that makes this a read,
	// the same discipline `dispatch verify` uses (§5.3 P11) — stronger than
	// "the code has no INSERT", because it also covers anything a helper writes.
	defer tx.Rollback()

	// E15/E16: the GONE probe comes FIRST, because a cursor below the retained
	// minimum must not be answered with the events that happen to remain. A
	// short list would tell a consumer it had caught up.
	if err := checkRetainedMinimumTx(tx, q.Since); err != nil {
		return nil, err
	}

	where, args := eventFilter(q)

	// The COUNT carries the SAME alias and the SAME filter as the page query, so
	// `total` counts exactly what the page slices from. Two filters written
	// separately is how a `truncated` flag ends up describing a different set
	// from the one it was computed over.
	var total int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM events e`+where, args...,
	).Scan(&total); err != nil {
		return nil, fmt.Errorf("counting events: %w", err)
	}

	// `--tail N` selects the NEWEST N rows, then hands them back OLDEST FIRST.
	//
	// The descending ORDER BY lives in an inner query whose only job is to pick
	// the window; the outer one restores `seq ASC` before anything is scanned.
	// The rows a caller sees are therefore ascending in every mode, which is
	// what E7 protects — a feed that emitted newest-first would invite a
	// consumer to store the FIRST seq as its cursor and skip everything under it.
	// The joins resolve each row's OWNING project (DKT-67): the issue's, for
	// rendering its id under the right prefix, and the run's-or-issue's, for
	// naming the project itself. They are LEFT joins throughout because a
	// store-level event has neither.
	selectCols := `e.seq, e.at_ms, e.kind, e.run_id, e.step_id, e.issue_id, ` +
		`s.instance, ip.prefix, COALESCE(rp.name, ip.name), e.data`
	from := ` FROM events e` +
		` LEFT JOIN steps s ON s.id = e.step_id` +
		` LEFT JOIN issues i ON i.id = e.issue_id` +
		` LEFT JOIN projects ip ON ip.id = i.project_id` +
		` LEFT JOIN runs r ON r.id = e.run_id` +
		` LEFT JOIN projects rp ON rp.id = r.project_id`

	var query string
	switch {
	case q.Tail > 0:
		query = `SELECT * FROM (SELECT ` + selectCols + from + where +
			fmt.Sprintf(` ORDER BY e.seq DESC LIMIT %d) ORDER BY seq ASC`, q.Tail)
	default:
		query = `SELECT ` + selectCols + from + where + ` ORDER BY e.seq ASC`
		if q.Limit > 0 {
			query += fmt.Sprintf(` LIMIT %d`, q.Limit)
		}
	}

	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var (
			e           Event
			runID       sql.NullInt64
			stepID      sql.NullInt64
			issueID     sql.NullInt64
			instance    sql.NullString
			issuePrefix sql.NullString
			projectName sql.NullString
			data        string
		)
		if err := rows.Scan(
			&e.Seq, &e.AtMS, &e.Kind, &runID, &stepID, &issueID, &instance,
			&issuePrefix, &projectName, &data,
		); err != nil {
			return nil, fmt.Errorf("reading an event: %w", err)
		}
		// E1: all are OMITTED when NULL, per the `?` in §11.4's shape.
		if runID.Valid {
			e.Run = model.FormatRunID(int(runID.Int64))
		}
		if stepID.Valid {
			e.StepID = model.FormatStepID(int(stepID.Int64))
		}
		if issueID.Valid {
			// DKT-67: the OWNING project's prefix, not the querying project's.
			e.Issue = model.FormatIDWithPrefix(int(issueID.Int64), issuePrefix.String)
		}
		if instance.Valid {
			e.Step = instance.String
		}
		if projectName.Valid {
			e.Project = projectName.String
		}
		e.Data = json.RawMessage(data)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading events: %w", err)
	}

	return &EventPage{Events: out, Total: total}, nil
}

// eventFilter builds the WHERE for E5's strict cursor and E8's run scope.
//
// The cursor clause is present even at `--since 0`, where `seq > 0` matches
// every row: writing it unconditionally keeps ONE query shape, and the index
// serves both. A version that dropped the clause for the default would be two
// plans to reason about for no gain.
func eventFilter(q EventQuery) (string, []any) {
	where := ` WHERE e.seq > ?`
	args := []any{q.Since}
	if q.RunID != 0 {
		where += ` AND e.run_id = ?`
		args = append(args, q.RunID)
	}
	// THE PROJECT SCOPE IS DROPPED UNDER `--run` (DKT-583). A run has exactly
	// one project, so the run clause above is already that project's scope and
	// anding a second one on top can only turn a neighbouring project's feed
	// into an empty page that reports success. `run report`, `step show`, and
	// `step artifact` all answer for a run outside the invoking project; the
	// event feed now agrees with them instead of contradicting them silently.
	if q.ProjectID != 0 && q.RunID == 0 {
		// Three-way attribution, per the field comment: the run's project,
		// else the issue's, else store-level. The cursor contract survives
		// scoping — seq stays monotonic, and a filtered-out neighbor is an
		// event this view never owes, not one it skipped.
		//
		// THE STORE-LEVEL ARM IS NARROWER THAN IT WAS (DKT-68). It used to
		// admit every project-less row into every project's feed, on the
		// argument that a user-level allowlist is as relevant to one project as
		// to the store. Measured against eight concurrent sessions that made a
		// scoped `--tail 60` return 8 rows of local history and 52 trust rows
		// generated by other repositories — 87% noise, with the history the
		// operator asked for pushed off the top.
		//
		// The argument was right about the FACTS and wrong about the ROWS. A
		// trust grant bound to another repository is not this project's audit
		// trail; a grant bound to THIS one is, and so is a genuinely
		// store-global grant. Both of those still appear. The payloads already
		// carry the binding — `trust-added`/`trust-removed` write `repo`,
		// `project-registered` writes `identity` — so the feed can tell the
		// cases apart instead of showing all of them to everyone.
		//
		// A row that names NO repository stays visible everywhere, which is the
		// original argument surviving where it actually applies.
		where += ` AND (
			e.run_id IN (SELECT id FROM runs WHERE project_id = ?)
			OR (e.run_id IS NULL AND e.issue_id IN (SELECT id FROM issues WHERE project_id = ?))
			OR (e.run_id IS NULL AND e.issue_id IS NULL AND (
				` + eventRepoBinding + ` IS NULL
				OR ` + eventRepoBinding + ` = (SELECT identity FROM projects WHERE id = ?))))`
		args = append(args, q.ProjectID, q.ProjectID, q.ProjectID)
	}
	return where, args
}

// eventRepoBinding is the repository path a store-level event's payload names,
// or NULL when it names none (DKT-68).
//
// NULLIF collapses the empty string to NULL because a GLOBAL trust entry writes
// `"repo": ""` — it binds to no repository by design — and an entry that
// applies everywhere must remain visible everywhere. Without it, every global
// grant would vanish from every scoped feed, which is the opposite of the
// audit-completeness property this filter is trying to preserve.
//
// THE json_valid GUARD IS LOAD-BEARING, not defensive dressing. SQLite's
// `json_extract` does not return NULL for malformed input — it ABORTS THE
// QUERY with "malformed JSON", so one corrupt `data` cell anywhere in the table
// would make every scoped `events list` fail outright, and an operator would
// lose the whole feed over a single bad row. The writer normalizes `data` to a
// JSON object (§7.6) so this is unreachable through product code, but a
// hand-edited row reaches it, and eventDetail already takes the position that a
// corrupted row should still render rather than break the read. An unparseable
// payload names no repository, so it stays visible everywhere — the same answer
// this filter gives every other unattributable store-level event.
const eventRepoBinding = `NULLIF(COALESCE(
	CASE WHEN json_valid(e.data) THEN json_extract(e.data, '$.repo') END,
	CASE WHEN json_valid(e.data) THEN json_extract(e.data, '$.identity') END), '')`

// checkRetainedMinimumTx is E15 and §8.6.1: GONE when the cursor names events
// below what the table still holds.
//
// HOW "A PRUNE HAS OCCURRED" IS KNOWN WITHOUT A PRUNER. The condition is
// `MIN(seq) > 1`, which in a v10 repo is achievable ONLY by deleting row 1.
// `seq` is `AUTOINCREMENT` and never reused — §7.6's comment says so and
// explains why — so `MIN(seq) > 1` is a sound and cheap proxy for "something
// below your cursor is gone", and it STAYS sound when S7's pruner arrives. No
// watermark column is needed, and adding one now would store a fact the table
// already tells us.
//
// E16: THE TRIGGER CANNOT FIRE AT THIS STAGE. Nothing prunes — `events prune` is
// S7's (§10 stage 7) and no other path deletes an event — so `MIN(seq)` is
// always 1 in a v10 repo and the condition below is unreachable through any
// product code path. The SHAPE ships here anyway (E17) because `--since` is the
// verb that must return it, and a stage that shipped the cursor without its
// out-of-range answer would ship a verb whose contract S7 has to retrofit.
// TestGoneShapeIsReachableOnlyByPruning constructs the state by deleting rows
// directly and asserts the code, the exit, and the message.
func checkRetainedMinimumTx(tx *sql.Tx, since int64) error {
	var minSeq sql.NullInt64
	if err := tx.QueryRow(`SELECT MIN(seq) FROM events`).Scan(&minSeq); err != nil {
		return fmt.Errorf("reading the retained minimum: %w", err)
	}
	// An EMPTY table is not GONE (E15's "and the table is non-empty"). A repo
	// that has never written an event has retained everything it ever had, and
	// answering GONE would tell a fresh consumer its cursor was stale.
	if !minSeq.Valid {
		return nil
	}
	if minSeq.Int64 <= 1 {
		return nil
	}
	// The cursor is below the retained minimum when the events it asks for
	// START below what remains. `since + 1` is the first seq this call would
	// return, and the comparison is against that rather than against `since`
	// itself, because a cursor sitting exactly ON the retained minimum has
	// missed nothing — it read that event and is asking for what follows.
	if since+1 >= minSeq.Int64 {
		return nil
	}
	return goneErr(
		"events at seq > %d are below the retained minimum (%d): they no longer "+
			"exist and cannot be replayed; resume from seq %d, accepting the gap",
		since, minSeq.Int64, minSeq.Int64-1)
}

// ---- §8.7: the attribution table ------------------------------------------

// Actor is one of the FOUR causes engine-spec §9 item 2 requires every
// transition to be traceable to, verbatim: *"every transition in events
// traceable to next/gate/threshold/human input."*
//
// It is a closed enumeration for the same reason the event kinds are: an
// attribution surface with a free-string actor could answer "some other thing"
// and item 2's audit would be vacuous.
type Actor string

const (
	// ActorNext is the SCHEDULER: readiness, reaping, joins, loop entry, the
	// TTL auto-abandon, and promotion.
	ActorNext Actor = "next"
	// ActorGate is a DETERMINISTIC CHECK — gates and actions both, since an
	// action is a check whose verdict is computed rather than declared.
	ActorGate Actor = "gate"
	// ActorThreshold is COMPUTED ROUTING: which way a step went, and every
	// status a routing produces.
	ActorThreshold Actor = "threshold"
	// ActorHuman is an OPERATOR VERB — including one a harness relays on an
	// operator's behalf, which is exactly the boundary item 2 exposes.
	ActorHuman Actor = "human"
)

// eventActors maps every kind in the closed set to exactly ONE actor (§8.7's
// table, complete).
//
// TestEveryEventKindHasAnActor asserts one entry per kind, so a stage adding a
// kind must SAY WHO CAUSES IT or fail a test. That is the mechanism that keeps
// §9 item 2 checkable as the set grows: the audit is only as good as the
// attribution, and an unattributed kind would pass an audit that never asked.
//
// TWO ROWS NEED THEIR SENTENCE:
//
//   - `step-claimed` is `human`, NOT `next`. A claim is an EXECUTOR'S act —
//     `next` offers, something else takes. In the reference instance that
//     something is a spawned worker, which is precisely the case item 2 checks:
//     the claim is attributable to the harness's dispatch decision, and the
//     SCHEDULING decision (which steps were claimable at all) was `next`'s.
//     Calling it `next` would hide exactly the boundary item 2 exists to expose.
//
//   - `run-paused` covers the BUDGET BREACH (§4.6 B23), and its actor is `human`
//     even when the engine flips it, because the transition's MEANING is "a
//     person must now decide". `data.reason` distinguishes `budget` from an
//     operator's `run pause`, so the trail is unambiguous without a new kind.
//
//   - `run-resumed` and `run-done` are the same shape and are read the same way
//     (DKT-304). `reconcileRun`'s rollup writes all three kinds too, with
//     `data.reason = "rollup"`, which is what tells an auditor that no verb was
//     typed. It used to write `{}`, so a rollup resume was byte-identical in
//     the feed to an operator's `run resume` — RUN-30's operator paused
//     mid-wave and could not tell from the trail that the resume one
//     millisecond later was nobody's decision.
//
// `dispatch-abandoned` appears once, as `next`. It has two causes — the TTL
// (P13, the scheduler's) and the explicit verb (P21, an operator's) — and the
// table maps KINDS rather than occurrences. It is attributed to the actor whose
// path is AUTOMATIC, because that is the one an auditor cannot otherwise see: an
// explicit abandon is a command in someone's history, while a TTL abandon
// happened because `next` ran. `data.reason` carries `ttl` or `abandoned`, so an
// audit that needs the distinction reads it from the event.
var eventActors = map[string]Actor{
	// The scheduler.
	EventStepReady:         ActorNext,
	EventLeaseReaped:       ActorNext,
	EventJoinCompleted:     ActorNext,
	EventLoopEntered:       ActorNext,
	EventDispatchAbandoned: ActorNext,
	EventIssuePromoted:     ActorNext,
	// The issue mirror's other two writes (DKT-294) sit beside `issue-promoted`
	// for the same reason: neither is a raw human/gate/threshold act, each is
	// the engine's own COMPUTED reflection of step state back onto the issue —
	// `todo -> in-progress` on a claim, or the review flip on either edge (a
	// gate/vote step parking `waiting-human`, or none doing so any longer).
	EventIssueInProgress: ActorNext,
	EventIssueReview:     ActorNext,

	// Deterministic checks.
	EventGateStarted:   ActorGate,
	EventGateRecorded:  ActorGate,
	EventGateUnmatched: ActorGate,
	EventGateRerun:     ActorGate,
	EventVoteOpened:    ActorGate,
	EventVoteTallied:   ActorGate,

	// Computed routing.
	EventStepRouted:     ActorThreshold,
	EventStepFailed:     ActorThreshold,
	EventStepSuperseded: ActorThreshold,
	EventStepSkipped:    ActorThreshold,
	EventStepHeld:       ActorThreshold,
	// The batch override being SPENT (DKT-546) is `threshold` on
	// `dispatch-abandoned`'s argument: the kind is attributed to the automatic
	// path, because that is the one an auditor cannot otherwise see. The
	// operator's decision is already in the feed as its own `human` kind
	// (`gate-override-granted`, below); this one records the routing stage
	// COMPUTING that a standing grant covers this step's failure, and
	// `data` carries the grant id(s) so the audit walks back to the person.
	EventStepBatchOverridden: ActorThreshold,

	// Operator verbs, and the harness relaying them.
	EventRunStarted:       ActorHuman,
	EventRunActivated:     ActorHuman,
	EventRunPaused:        ActorHuman,
	EventRunResumed:       ActorHuman,
	EventRunAbandoned:     ActorHuman,
	EventRunDone:          ActorHuman,
	EventStepClaimed:      ActorHuman,
	EventStepHeartbeat:    ActorHuman,
	EventStepRecorded:     ActorHuman,
	EventStepResolved:     ActorHuman,
	EventStepApproved:     ActorHuman,
	EventStepRejected:     ActorHuman,
	EventIssueAbandoned:   ActorHuman,
	EventTrustAdded:       ActorHuman,
	EventTrustRemoved:     ActorHuman,
	EventDispatchOpened:   ActorHuman,
	EventDispatchClosed:   ActorHuman,
	EventReapAcknowledged: ActorHuman,
	// The spawn carve-out is `human` for the same reason the ack is: nothing
	// in the engine passes `--deciding-vote`. A person, or the relay acting
	// under one, names the open proposal and asks to be let past the hold
	// (DKT-236).
	EventSpawnAdmitted: ActorHuman,

	// Stage 7's two. Both are `human` and neither is a close call: NOTHING IN
	// THE ENGINE PRUNES and nothing raises a cap on breach. There is no
	// automatic retention sweep, no compaction at `run done`, and no path that
	// re-caps a run — each of these events exists because a person ran a verb,
	// which is precisely what `human` means in this table.
	EventEventsPruned: ActorHuman,
	EventRunBudgetSet: ActorHuman,

	// DKT-61's tenancy kind is `human` for the same reason: a project row comes
	// into being because someone ran a verb from a repository, and the event's
	// whole purpose is to name that verb and that directory. It is the one kind
	// whose actor is the point of the record rather than a classification of it.
	EventProjectRegistered: ActorHuman,

	// The annotation verb (DKT-35): nothing in the engine annotates — a
	// finished record changes only because a person (or the harness relaying
	// one) ran `step annotate`.
	EventStepAnnotated: ActorHuman,

	// The repin verb (DKT-408): nothing in the engine repins — RA2's rule is
	// that re-activation INHERITS the pin set, and every automatic path keeps
	// it. The recorded agreement moves only because a person ran `run repin`
	// with a reason, which is precisely what `human` means in this table.
	EventRunRepinned: ActorHuman,

	// The batch override being MINTED (DKT-546): nothing in the engine grants
	// itself standing authority over a gate — a grant exists only because a
	// person ran `step resolve --as override-pass --batch`, and the event's
	// purpose is to anchor every later `step-batch-overridden` to that verb.
	EventGateOverrideGranted: ActorHuman,

	// The stale-target waiver being minted (DKT-742): nothing in the engine
	// waives its own advisory — a waiver exists only because a person ran
	// `dispatch waive-target`, and unlike the batch override there is no
	// "spent" counterpart kind, because applying a waiver changes no step's
	// state (the advisory is a recomputed read, served by a verb that writes
	// nothing).
	EventStaleTargetWaived: ActorHuman,

	// The scope refresh (DKT-869): nothing in the engine re-snapshots a bound
	// issue — activation writes the blob once and every automatic path,
	// re-activation included, leaves it alone. It moves only because a person
	// widened the scope through `issue edit --scope` and then ran `run
	// refresh-scope` naming the run it should reach, which is precisely what
	// `human` means in this table.
	EventIssueScopeRefreshed: ActorHuman,

	// The issue.diff re-pin (DKT-1034): nothing in the engine re-records a
	// step's diff after the fact — the routing stage records it once at
	// completion and every resolution leaves it standing (rerun-gates by
	// contract, retry by DKT-259's guard, override-pass by recording a pass).
	// It moves only because a person ran `step resolve --worktree` naming
	// the checkout the patched tree stands in, which is precisely what
	// `human` means in this table.
	EventIssueDiffRepinned: ActorHuman,
}

// ActorFor reports which of the four causes an event kind is attributable to,
// and whether the kind is in the table at all.
//
// The second return is not decoration: §9 item 2's audit asks whether EVERY
// event maps, and a lookup that returned `""` for an unknown kind would let the
// audit read an unattributed event as attributed to nothing in particular.
func ActorFor(kind string) (Actor, bool) {
	actor, ok := eventActors[kind]
	return actor, ok
}

// EventKinds returns the closed set, for the tests and the audit that enumerate
// it.
//
// It is derived from the writer's own membership table rather than from a second
// list, so an audit cannot pass because it checked a stale copy.
func EventKinds() []string {
	out := make([]string, 0, len(eventKinds))
	for kind := range eventKinds {
		out = append(out, kind)
	}
	return out
}

// ActorCounts rolls the feed up per actor — the report's fifth section (E21).
//
// It is here rather than in the report because the attribution is this file's
// subject, and a rollup computed beside the table cannot disagree with it.
func ActorCounts(conn *sql.DB, runID int) (map[Actor]int, error) {
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("counting events by kind: %w", err)
	}
	defer tx.Rollback()
	return actorCountsTx(tx, runID)
}

// actorCountsTx is the same rollup inside a caller's transaction, which is where
// the report needs it: R8's zero-write property comes from the report computing
// every section in ONE read-only transaction, and a pool read from inside it
// would deadlock against the one-connection pool.
func actorCountsTx(tx *sql.Tx, runID int) (map[Actor]int, error) {
	rows, err := tx.Query(
		`SELECT kind, COUNT(*) FROM events WHERE run_id = ? GROUP BY kind ORDER BY kind`,
		runID)
	if err != nil {
		return nil, fmt.Errorf("counting events by kind: %w", err)
	}
	defer rows.Close()

	counts := make(map[Actor]int, 4)
	for rows.Next() {
		var (
			kind string
			n    int
		)
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, fmt.Errorf("counting events by kind: %w", err)
		}
		actor, ok := eventActors[kind]
		if !ok {
			// Unreachable: the writer refuses a kind outside the closed set, so
			// a row here with no actor means the table and the set disagree —
			// which TestEveryEventKindHasAnActor exists to catch at build time.
			// Refusing rather than dropping keeps §9 item 2's audit honest: a
			// rollup that silently omitted an unattributable event would report
			// full attribution over a feed that did not have it.
			return nil, fmt.Errorf(
				"event kind %q has no actor in the attribution table (§8.7)", kind)
		}
		counts[actor] += n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("counting events by kind: %w", err)
	}
	return counts, nil
}

// actorReportTx renders the rollup as the report's ordered rows.
//
// The order is the FOUR ACTORS' declared order — scheduler, check, routing,
// operator — rather than by count, because R9 requires a total order and a
// count-ordered list would reshuffle itself as a run progressed. An actor with
// no events is OMITTED rather than shown as zero: a run with no gates has no
// gate row, which is the same rule every other section in this document follows.
func actorReportTx(tx *sql.Tx, runID int) ([]ActorCount, error) {
	counts, err := actorCountsTx(tx, runID)
	if err != nil {
		return nil, err
	}
	var out []ActorCount
	for _, actor := range []Actor{ActorNext, ActorGate, ActorThreshold, ActorHuman} {
		if n := counts[actor]; n > 0 {
			out = append(out, ActorCount{Actor: string(actor), Count: n})
		}
	}
	return out, nil
}

// ResolveRunFilter maps `--run RUN-N` onto a run id, refusing a run that does
// not exist.
//
// The refusal matters for a CURSOR feed specifically: `events list --run RUN-99`
// over a missing run would otherwise return an empty page, and a consumer
// polling it would wait forever on a run that was never there.
func ResolveRunFilter(conn *sql.DB, ref string) (int, error) {
	if ref == "" {
		return 0, nil
	}
	runID, err := model.ParseRunID(ref)
	if err != nil {
		return 0, validationErr("%s is not a run reference (expected RUN-N)", ref)
	}
	if _, err := db.GetRun(conn, runID); err != nil {
		return 0, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}
	return runID, nil
}
