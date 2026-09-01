package engine

import (
	"database/sql"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// Events written — TDD §7.6, §7.7.
//
// Three obligations: every transition writes exactly one event of the expected
// kind, `seq` is strictly increasing across concurrent writers, and the kinds
// are drawn ONLY from the closed set.
//
// No read verb exists (§10 puts `events list --since` at stage 6), so these
// read the table with SQL directly — deliberately, rather than shipping a read
// surface now that would freeze a shape S6 is specified to define.

// ---------------------------------------------------------------------------
// The closed set
// ---------------------------------------------------------------------------

// TestEventKindsAreAClosedSet is §7.7's guard test: it enumerates the constants
// and asserts no other literal reaches the writer.
//
// The two halves matter separately. The first — every Event* constant is in
// `eventKinds` — catches a phase that adds a kind and forgets to admit it, so
// the writer would refuse an event the engine is trying to record. The second —
// every `eventKinds` entry has a constant — catches the reverse: a string
// admitted to the set that no constant names, which is how a literal gets in.
//
// §9 item 2 ("every transition is attributable") is checkable only because this
// set is closed, and it is closed only because both directions hold.
func TestEventKindsAreAClosedSet(t *testing.T) {
	constants := eventKindConstants(t)

	if len(constants) == 0 {
		t.Fatal("no Event* constants found in event.go")
	}

	for name, value := range constants {
		if !eventKinds[value] {
			t.Errorf("%s = %q is not in eventKinds; the writer would refuse it", name, value)
		}
	}

	byValue := make(map[string]string, len(constants))
	for name, value := range constants {
		byValue[value] = name
	}
	for kind := range eventKinds {
		if _, ok := byValue[kind]; !ok {
			t.Errorf("eventKinds contains %q, which no Event* constant names — "+
				"a literal in the set is how the closed set stops being closed", kind)
		}
	}

	// And the set matches the spec's enumeration exactly. Restating the list
	// here is the point: it is the one place the code is checked against the
	// document rather than against itself, so a kind renamed on both sides
	// still fails.
	//
	// The first 24 are engine-spine §7.6's; six are stage 4's and one is stage
	// 5's. Each addition is enumerated by the TDD that made it, precisely so
	// this check keeps passing rather than silently widening — a stage that
	// grew the set without naming its additions in its own TDD would make §9
	// item 2 vacuous.
	for _, kind := range []string{
		"run-started", "run-activated", "run-paused", "run-resumed",
		"run-abandoned", "run-done", "step-ready", "step-claimed",
		"step-heartbeat", "step-recorded", "gate-started", "gate-recorded",
		"step-routed", "step-failed", "step-superseded", "step-skipped",
		"step-resolved", "step-approved", "step-rejected", "loop-entered",
		"join-completed", "lease-reaped", "issue-promoted", "issue-abandoned",

		// The live-status mirror's other two writes (DKT-294) — TWO. Neither
		// `in-progress` nor `review` was ever written before this change, though
		// both were long validated and rendered; these are the transitions that
		// make the issue mirror match what §9 item 2 already required of it.
		"issue-in-progress", "issue-review",

		// gates-trust §6.4 plus §8.1 — SIX, not the four §6.4 counts. The
		// contradiction between those two sections is recorded as an amendment.
		"gate-unmatched", "gate-rerun", "trust-added", "trust-removed",
		"vote-opened", "vote-tallied",

		// payloads-thresholds §7.7 — ONE. Materializing a `<step>-held`
		// question is a transition, and §9 item 2 requires every transition be
		// attributable; without it a `type="human"` step nobody declared
		// appears in a run with nothing explaining where it came from. Its
		// RESOLUTION needs no new kind: `step-approved` and `step-rejected`
		// already say what an operator did to a human gate, and a materialized
		// one is a human gate in every respect the verbs can observe (H3).
		"step-held",

		// runs-dispatch §5 and §6, commit group 2 — FOUR. Three are the
		// dispatch lifecycle and one is the write-reap acknowledgment.
		//
		// `dispatch-abandoned` is the only kind in the whole set that
		// engine-spec §2 requires by implication rather than a TDD choosing it:
		// "a dispatch TTL lazily auto-abandoned by `next` (event-logged)".
		//
		// `dispatch-closed` and `dispatch-abandoned` are separate rather than
		// one kind with a reason, on `gate-unmatched`'s argument: an operator
		// following the feed must see "reconciled" and "given up on" as
		// different events, since they mean opposite things about whether the
		// relay's work is accounted for.
		//
		// `reap-acknowledged` earns its place on A3 — the ack must be
		// ATTRIBUTABLE — and on §9 item 2: releasing write headroom IS a
		// transition (a successor becomes claimable that was not), and without
		// this kind the release would appear in the feed as nothing at all.
		"dispatch-opened", "dispatch-closed", "dispatch-abandoned",
		"reap-acknowledged",

		// Stage 7's two (docs/tdd/events-follow.md §6, §7.3).
		//
		// `events-pruned` earns its place on an argument no earlier kind could
		// make: the prune is the one transition that DESTROYS EVIDENCE, so the
		// record that it happened is all that distinguishes a trimmed log from
		// a run that made fewer transitions.
		//
		// `run-budget-set` is the kind S6's own reasoning did not cover. That
		// stage refused `budget-breached` because `run-paused` already anchors
		// it; nothing anchors "the cap moved", and without this kind a run that
		// breached at one number and later admitted a claim at another would
		// have a trail with nothing explaining the difference.
		"events-pruned", "run-budget-set",

		// The annotation kind (DKT-35) — ONE. Merging metadata onto a
		// finished step's record is a mutation of the run record, and a
		// record that changed with no event would be evidence that rewrote
		// itself. It carries the annotation verbatim, so what was added
		// survives a later annotation overwriting the same key.
		"step-annotated",

		// The tenancy kind (DKT-61) — ONE. A project row is what every other
		// row is attributed TO, and until this kind existed the project itself
		// recorded nothing about its own origin: attributing one junk row to
		// the verb that minted it took a hand-join of raw table timestamps
		// against nine session transcripts.
		"project-registered",

		// The spawn carve-out kind (DKT-236) — ONE. It records a hold being
		// STEPPED PAST, which is the one case where "no event" and "nothing
		// happened" say the same thing while meaning opposite things: a spawn
		// admitted over an open reap hold would otherwise be indistinguishable
		// in the record from a spawn nothing was holding.
		"spawn-admitted",

		// The repin kind (DKT-408) — ONE. `run repin` moves the run's
		// recorded agreement — the `pins` rows every packet and render verify
		// bytes against — and this event is what still says what completed
		// steps actually worked under afterwards: old sha and new sha per
		// changed ref, with the operator's reason. Without it a repin would be
		// the one transition that rewrites the evidence other records are
		// checked against while leaving no trace of the value it replaced.
		"run-repinned",

		// The batch gate-override kinds (DKT-546) — TWO, each the
		// spawn-admitted argument again: a park stepped past on standing
		// authority. `gate-override-granted` is the authority being minted
		// (one operator ruling per failed gate), `step-batch-overridden` is
		// it being spent — without the second, an auto-applied override would
		// be indistinguishable in the feed from an engine-computed pass.
		"gate-override-granted",
		"step-batch-overridden",

		// The stale-target waiver kind (DKT-742) — ONE, the granted half of
		// the DKT-546 argument alone: an operator minting standing authority
		// over a repeating advisory. No "spent" counterpart, because applying
		// a waiver changes no step's state — the advisory is recomputed by
		// verbs that write nothing.
		"stale-target-waived",

		// The scope-refresh kind (DKT-869) — ONE, on the `run-repinned`
		// argument in its other column: this is the second and last transition
		// that moves a frozen premise while a run is live. Without it, two
		// steps of one run rendering two different declared scopes would be
		// indistinguishable in the record from the snapshot drift the freeze
		// exists to prevent — the discontinuity has to be dated and
		// attributable for `run refresh-scope` to be an exception rather than
		// a hole. No "spent" counterpart: the refreshed snapshot IS the new
		// premise, and every later render reads it the way every render always
		// read the frozen one.
		"issue-scope-refreshed",
	} {
		if !eventKinds[kind] {
			t.Errorf("the spec names %q but eventKinds does not contain it", kind)
		}
	}
	if len(eventKinds) != 47 {
		t.Errorf("eventKinds has %d entries; §7.6 plus gates-trust §6.4/§8.1, "+
			"payloads-thresholds §7.7, runs-dispatch §5/§6, events-follow "+
			"§6/§7.3, DKT-35's annotation kind, DKT-61's tenancy kind, "+
			"DKT-236's spawn carve-out, DKT-294's live-status mirror, "+
			"DKT-408's repin kind, DKT-546's batch-override pair, "+
			"DKT-742's stale-target waiver kind, and DKT-869's scope-refresh "+
			"kind enumerate 47 (see DKT-21)",
			len(eventKinds))
	}
}

// TestWriterRefusesAKindOutsideTheClosedSet is the enforcement, not just the
// bookkeeping: a kind outside the set fails the TRANSACTION.
//
// Failing the transaction is the correct severity. A transition whose event
// cannot be attributed is worse than a transition that does not happen, because
// the first leaves the database in a state no ledger explains.
func TestWriterRefusesAKindOutsideTheClosedSet(t *testing.T) {
	conn := mustDB(t)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()

	err = recordEvent(tx, eventRecord{Kind: "step-invented", RunID: 1})
	if err == nil {
		t.Fatal("recordEvent accepted a kind outside the closed set")
	}
	if !strings.Contains(err.Error(), "closed set") {
		t.Errorf("error = %q, want it to name the closed set", err)
	}
}

// ---------------------------------------------------------------------------
// One event per transition
// ---------------------------------------------------------------------------

// TestEveryStepStatusChangeHasAProducingEvent is §9 item 2 at the unit level:
// drive a full run and require that each transition produced its event.
//
// It is the same claim the QA section makes over the CLI, asserted here where a
// missing event is attributable to the transition that failed to write it
// rather than to a step somewhere in an eleven-step script.
func TestEveryStepStatusChangeHasAProducingEvent(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	// Activation itself: the issue was promoted and the run activated.
	assertEventCount(t, conn, EventIssuePromoted, 1)
	assertEventCount(t, conn, EventRunActivated, 1)

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

	// The claim, the artifact record, and the routing each wrote one event.
	assertEventCount(t, conn, EventStepClaimed, 1)
	assertEventCount(t, conn, EventStepRecorded, 1)
	assertEventCount(t, conn, EventStepRouted, 1)

	// `implement` declares five gates, and each spawn is PRECEDED by a
	// `gate-started` (§2, §7.6) — which is what makes the at-least-once gate
	// discipline observable before S4 supplies a real spawn.
	assertEventCount(t, conn, EventGateStarted, 5)
	assertEventCount(t, conn, EventGateRecorded, 5)

	// Every event carries the run, and step-level events carry the step.
	assertEventsAreAttributed(t, conn, run.ID)
}

// TestGateStartedPrecedesEachGateSpawn is §7.6's explicit ordering claim.
//
// The `gate-started` event commits BEFORE the runner is invoked, so a crash
// between the two leaves a started-but-unrecorded gate — the state a resume must
// handle, and one it can only observe if the event committed first. Asserting
// the ORDER by `seq` is what distinguishes this from "both events exist".
func TestGateStartedPrecedesEachGateSpawn(t *testing.T) {
	conn := mustDB(t)
	_, _ = activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

	started := eventSeqs(t, conn, EventGateStarted)
	recorded := eventSeqs(t, conn, EventGateRecorded)

	if len(started) != len(recorded) || len(started) != 5 {
		t.Fatalf("%d gate-started and %d gate-recorded events, want 5 of each",
			len(started), len(recorded))
	}
	for i := range started {
		if started[i] >= recorded[i] {
			t.Errorf("gate %d: gate-started seq %d is not before gate-recorded seq %d",
				i, started[i], recorded[i])
		}
	}
}

// TestLoopTransitionsWriteTheirEvents covers the two kinds this phase added,
// over the transitions that produce them.
func TestLoopTransitionsWriteTheirEvents(t *testing.T) {
	conn := mustDB(t)
	_, _ = activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)

	// One loop entry, one event, carrying the new ordinal in its opaque data.
	assertEventCount(t, conn, EventLoopEntered, 1)

	var data string
	err := conn.QueryRow(
		`SELECT data FROM events WHERE kind = ?`, EventLoopEntered).Scan(&data)
	testsupport.Must(t, err, "reading the loop-entered event: %v", err)
	var payload struct {
		Ordinal  int    `json:"ordinal"`
		Instance string `json:"instance"`
	}
	err = json.Unmarshal([]byte(data), &payload)
	testsupport.Must(t, err, "loop-entered data is not a JSON object: %v (%q)", err, data)
	if payload.Ordinal != 1 {
		t.Errorf("loop-entered ordinal = %d, want 1", payload.Ordinal)
	}
	if payload.Instance != "verify@0" {
		t.Errorf("loop-entered instance = %q, want verify@0", payload.Instance)
	}

	// The sweep superseded `commit-gate@0` and `commit@0`, each event-logged.
	assertEventCount(t, conn, EventStepSuperseded, 2)
}

// TestEventDataIsAlwaysAJSONObject is the column's contract (§11.4's `event`
// shape puts `data` in an envelope).
//
// Call sites pass bare details — a gate name, a routing — because that is the
// natural thing to pass and the engine never reads them back. Normalizing in
// the writer is what keeps `data` parseable for every S6/S7 consumer without
// making sixteen call sites construct JSON.
func TestEventDataIsAlwaysAJSONObject(t *testing.T) {
	conn := mustDB(t)
	_, _ = activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

	rows, err := conn.Query(`SELECT seq, kind, data FROM events ORDER BY seq`)
	testsupport.Must(t, err, "reading events: %v", err)
	defer rows.Close()

	n := 0
	for rows.Next() {
		var (
			seq  int
			kind string
			data string
		)
		err := rows.Scan(&seq, &kind, &data)
		testsupport.Must(t, err, "scanning event: %v", err)
		var obj map[string]any
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			t.Errorf("event %d (%s) data = %q, which is not a JSON object: %v",
				seq, kind, data, err)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no events written; the assertion above is vacuous")
	}
}

// ---------------------------------------------------------------------------
// seq is strictly increasing
// ---------------------------------------------------------------------------

// TestSeqIsStrictlyIncreasingUnderConcurrentWriters is §7.7's concurrency
// obligation.
//
// `seq` is INTEGER PRIMARY KEY AUTOINCREMENT, which SQLite guarantees is
// monotonic and never reused. The test writes from several goroutines at once
// and requires the resulting sequence to be strictly increasing with no
// duplicates — the property S7's `--since` cursor depends on, and the one a
// `MAX(seq) + 1` implementation would lose under exactly this load.
func TestSeqIsStrictlyIncreasingUnderConcurrentWriters(t *testing.T) {
	conn := mustDB(t)
	_, _ = activatedRun(t, conn)

	const (
		writers = 4
		each    = 25
	)

	var wg sync.WaitGroup
	errs := make(chan error, writers*each)

	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				tx, err := conn.Begin()
				if err != nil {
					errs <- err
					return
				}
				if err := recordEvent(tx, eventRecord{
					Kind: EventStepHeartbeat, RunID: 1, AtMS: nowMS,
				}); err != nil {
					tx.Rollback()
					errs <- err
					return
				}
				if err := tx.Commit(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write: %v", err)
	}

	rows, err := conn.Query(`SELECT seq FROM events ORDER BY seq`)
	testsupport.Must(t, err, "reading events: %v", err)
	defer rows.Close()

	prev := 0
	n := 0
	for rows.Next() {
		var seq int
		err := rows.Scan(&seq)
		testsupport.Must(t, err, "scanning seq: %v", err)
		if seq <= prev {
			t.Fatalf("seq %d follows %d; the sequence must be STRICTLY increasing", seq, prev)
		}
		prev = seq
		n++
	}
	if n < writers*each {
		t.Errorf("%d events recorded, want at least %d — writes were lost",
			n, writers*each)
	}
}

// TestSeqIsNeverReusedAfterDeletion is the AUTOINCREMENT half, and the reason
// §7.1 specifies it rather than a plain rowid.
//
// With a plain INTEGER PRIMARY KEY, SQLite reuses the highest rowid after the
// row holding it is deleted. A consumer holding `--since = N` would then never
// see the replacement event, because its `seq` is one it has already passed.
// AUTOINCREMENT is the difference between a `seq` that is monotonic and one
// that merely usually is — and S7 ships a `prune` verb, so deletion is not
// hypothetical.
func TestSeqIsNeverReusedAfterDeletion(t *testing.T) {
	conn := mustDB(t)
	_, _ = activatedRun(t, conn)

	writeEvent := func() int {
		t.Helper()
		tx, err := conn.Begin()
		testsupport.Must(t, err, "Begin: %v", err)
		if err := recordEvent(tx, eventRecord{
			Kind: EventStepHeartbeat, RunID: 1, AtMS: nowMS,
		}); err != nil {
			tx.Rollback()
			t.Fatalf("recordEvent: %v", err)
		}
		err = tx.Commit()
		testsupport.Must(t, err, "Commit: %v", err)
		var seq int
		err = conn.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&seq)
		testsupport.Must(t, err, "reading seq: %v", err)
		return seq
	}

	first := writeEvent()

	// Delete the newest event — what `events prune` will do at S7.
	execSQL(t, conn, `DELETE FROM events WHERE seq = ?`, first)

	second := writeEvent()
	if second <= first {
		t.Errorf("seq %d was reused after deleting %d; AUTOINCREMENT must never "+
			"reuse a number, or an S7 --since cursor would skip the replacement",
			second, first)
	}
}

// ---------------------------------------------------------------------------
// The read verb's boundary
// ---------------------------------------------------------------------------

// TestEventTailShips is where a three-stage succession ENDS, and the ending is
// as deliberate as the deferral was.
//
// The lineage: at S3 this file asserted no event read verb shipped at all
// (`TestNoEventReadVerbShips`), because §10 puts `events list --since` at stage
// 6 and an early read verb would have frozen a wire shape S6 was specified to
// define. At S6 the guard MOVED rather than disappeared
// (`TestNoEventFollowVerbShips`): the read shipped, and `--follow`/`prune` were
// asserted absent for one more stage, on the same argument one stage later.
//
// THIS IS STAGE 7. The tail is what this stage exists to define, so a guard
// asserting its absence would now be asserting the stage did not happen. It
// INVERTS rather than being deleted: the same file, the same parse, the same
// two names — and the assertion is that both are PRESENT.
//
// Inverting rather than deleting keeps the property the succession was
// protecting. A deleted guard leaves nothing behind that would notice if the
// tail were later removed or quietly reduced to a stub; this one fails if the
// verb that S3, S4, S5, and S6 all deferred to stage 7 ever stops shipping.
func TestEventTailShips(t *testing.T) {
	// The verb is spread over three files now that it has three halves, so the
	// guard reads the DIRECTORY's events sources rather than one filename. Its
	// predecessors could name one file because they were asserting an absence,
	// and an absence is the same in every file.
	var stripped strings.Builder
	for _, name := range []string{"events.go", "events_follow.go", "events_prune.go"} {
		src, err := os.ReadFile(filepath.Join("..", "cli", name))
		testsupport.Must(t, err, "reading %s: %v", name, err)

		// The check runs over the source with COMMENTS STRIPPED, the same
		// decision scripts/qa/genericity.sh makes and the same one this guard's
		// predecessors made: prose about a flag is not the flag, in either
		// direction.
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, 0)
		testsupport.Must(t, err, "parsing %s: %v", name, err)
		file.Comments = nil
		err = printer.Fprint(&stripped, fset, file)
		testsupport.Must(t, err, "printing %s: %v", name, err)
	}

	for _, shipped := range []string{"follow", "prune"} {
		if !strings.Contains(stripped.String(), shipped) {
			t.Errorf("the events verb does not name %q outside a comment; §10 "+
				"stage 7 IS the observability tail, and three earlier stages "+
				"deferred to it", shipped)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// eventKindConstants parses event.go and returns every `Event*` constant, by
// name, with its string value.
//
// Parsed from SOURCE rather than listed here, so a constant added without being
// admitted to `eventKinds` fails this test instead of quietly widening the set.
func eventKindConstants(t *testing.T) map[string]string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "event.go", nil, 0)
	testsupport.Must(t, err, "parsing event.go: %v", err)

	out := make(map[string]string)
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if !strings.HasPrefix(name.Name, "Event") || i >= len(spec.Values) {
				continue
			}
			lit, ok := spec.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			out[name.Name] = value
		}
		return true
	})
	return out
}

// assertEventCount requires exactly n events of a kind.
func assertEventCount(t *testing.T, conn *sql.DB, kind string, want int) {
	t.Helper()
	if got := countEvents(t, conn, kind); got != want {
		t.Errorf("%d %s events, want %d", got, kind, want)
	}
}

// eventSeqs returns the seqs of a kind, in order.
func eventSeqs(t *testing.T, conn *sql.DB, kind string) []int {
	t.Helper()
	rows, err := conn.Query(`SELECT seq FROM events WHERE kind = ? ORDER BY seq`, kind)
	testsupport.Must(t, err, "reading %s events: %v", kind, err)
	defer rows.Close()

	var out []int
	for rows.Next() {
		var seq int
		err := rows.Scan(&seq)
		testsupport.Must(t, err, "scanning seq: %v", err)
		out = append(out, seq)
	}
	return out
}

// assertEventsAreAttributed requires that every event names its run, and that a
// step-level event resolves to a real step row.
//
// The step half is what proves the writer's instance -> step_id resolution
// works, including for a step inserted and event-logged in ONE transaction —
// expansion's `step-skipped` — which resolves against its own uncommitted row.
func assertEventsAreAttributed(t *testing.T, conn *sql.DB, runID int) {
	t.Helper()

	var orphans int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE run_id IS NULL OR run_id != ?`, runID,
	).Scan(&orphans)
	testsupport.Must(t, err, "counting unattributed events: %v", err)
	if orphans > 0 {
		t.Errorf("%d events are not attributed to run %d", orphans, runID)
	}

	// Every step-level event resolved its instance to a real step.
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM events e
		  WHERE e.step_id IS NOT NULL
		    AND NOT EXISTS (SELECT 1 FROM steps s WHERE s.id = e.step_id)`,
	).Scan(&orphans)
	testsupport.Must(t, err, "counting dangling step references: %v", err)
	if orphans > 0 {
		t.Errorf("%d events reference a step that does not exist", orphans)
	}

	// And the step-shaped kinds actually resolved rather than silently storing
	// NULL — a resolution that never fired would pass the check above.
	var unresolved int
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE kind IN (?, ?, ?) AND step_id IS NULL`,
		EventStepClaimed, EventStepRecorded, EventStepRouted,
	).Scan(&unresolved)
	testsupport.Must(t, err, "counting unresolved step events: %v", err)
	if unresolved > 0 {
		t.Errorf("%d step-level events stored no step_id; the instance -> id "+
			"resolution did not fire", unresolved)
	}
}
