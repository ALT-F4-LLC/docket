package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestEventShapeIsSection114 is §8.2 E1-E4: the wire shape, field for field.
func TestEventShapeIsSection114(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	claimInstance(t, conn, "implement@0", nowMS)

	page, err := ListEvents(conn, EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents: %v", err)

	var claimed *Event
	for i := range page.Events {
		if page.Events[i].Kind == EventStepClaimed {
			claimed = &page.Events[i]
		}
	}
	if claimed == nil {
		t.Fatal("no step-claimed event in the feed")
	}

	if claimed.Seq <= 0 {
		t.Errorf("seq is %d; it is the AUTOINCREMENT rowid and starts at 1", claimed.Seq)
	}
	if claimed.AtMS <= 0 {
		t.Errorf("at_ms is %d; E3 makes it epoch-milliseconds", claimed.AtMS)
	}
	// E1: `run` is RUN-N and `step` is the RENDERED INSTANCE IDENTITY, matching
	// every other wire shape's step rendering.
	if claimed.Run != "RUN-1" {
		t.Errorf("run is %q, want RUN-1", claimed.Run)
	}
	if claimed.Step != "implement@0" {
		t.Errorf("step is %q, want the rendered instance identity implement@0", claimed.Step)
	}
	// E4, THE FILED ADDITION: the joinable id. `step` and `data.instance` both
	// give the human identity; a consumer joining to `step show` needs this.
	if claimed.StepID == "" {
		t.Error("step_id is empty; E4 ships it so the shape is joinable")
	}
	// E2: `data` is the stored object, VERBATIM — an object, never a bare string.
	var data map[string]any
	err = json.Unmarshal(claimed.Data, &data)
	testsupport.Must(t, err, "data is not a JSON object: %v (%s)", err, claimed.Data)
}

// TestEventShapeOmitsNullRunAndStep is E1's `?`: both are OMITTED when NULL.
//
// A trust event is the case that exists in the product (gates-trust §3.6): it
// belongs to no run, and attributing it to one would be a fabrication.
func TestEventShapeOmitsNullRunAndStep(t *testing.T) {
	conn := mustDB(t)
	err := RecordTrustEvent(
		conn, EventTrustAdded,
		TrustGrant{Name: "checks", ArgvSHA256: "abc123", Repo: "/repo", Actor: "tester", Cwd: "/repo"}, nowMS,
	)
	testsupport.Must(t, err, "RecordTrustEvent: %v", err)

	page, err := ListEvents(conn, EventQuery{})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want the one trust grant", len(page.Events))
	}

	e := page.Events[0]
	if e.Run != "" || e.Step != "" || e.StepID != "" {
		t.Errorf("a run-less event rendered run=%q step=%q step_id=%q; E1 omits them",
			e.Run, e.Step, e.StepID)
	}

	// And the omission reaches the WIRE, which is the half that matters: the
	// `?` in §11.4's shape is about the JSON, not about the Go zero value.
	raw, err := json.Marshal(e)
	testsupport.Must(t, err, "marshaling: %v", err)
	var fields map[string]json.RawMessage
	err = json.Unmarshal(raw, &fields)
	testsupport.Must(t, err, "unmarshaling: %v", err)
	for _, key := range []string{"run", "step", "step_id"} {
		if _, present := fields[key]; present {
			t.Errorf("%q is present in the wire shape of a run-less event; E1 omits it", key)
		}
	}
}

// TestUnattributedTrustEventIsRefused is DKT-595, at the one entry point a
// trust event has into the table.
//
// The 2026-08-19 batch was written by a writer that supplied neither actor nor
// cwd, and the ledger could not afterwards say whether that writer was an old
// binary or a non-interactive path. The guard lives in RecordTrustEvent itself —
// not only in the CLI that resolves the fields today — so a future writer that
// cannot supply them is refused rather than trusted to degrade honestly. The
// refusal is BEFORE the write: no event of any shape lands.
func TestUnattributedTrustEventIsRefused(t *testing.T) {
	conn := mustDB(t)

	for _, tc := range []struct {
		name  string
		grant TrustGrant
	}{
		{"no actor", TrustGrant{Name: "checks", ArgvSHA256: "abc123", Repo: "/repo", Cwd: "/repo"}},
		{"no cwd", TrustGrant{Name: "checks", ArgvSHA256: "abc123", Repo: "/repo", Actor: "tester"}},
		{"neither", TrustGrant{Name: "checks", ArgvSHA256: "abc123", Repo: "/repo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := RecordTrustEvent(conn, EventTrustAdded, tc.grant, nowMS)
			if err == nil {
				t.Fatal("an unattributed trust event was recorded; a writer that cannot say who and from where must be refused")
			}
		})
	}

	page, err := ListEvents(conn, EventQuery{})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if len(page.Events) != 0 {
		t.Errorf("a refused trust event left %d event(s) behind; the refusal must precede the write", len(page.Events))
	}
}

// TestCursorIsStrictlyGreater is E5 and E6: `--since SEQ` returns `seq > SEQ`, so
// a consumer stores the last seq it saw and passes it back WITHOUT re-reading it.
func TestCursorIsStrictlyGreater(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	claimInstance(t, conn, "implement@0", nowMS)

	all, err := ListEvents(conn, EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if len(all.Events) < 2 {
		t.Fatalf("the fixture produced %d events; this needs 2", len(all.Events))
	}

	// E6: `--since 0` is the default and returns from the beginning.
	if all.Events[0].Seq == 0 {
		t.Error("seq 0 was returned; AUTOINCREMENT starts at 1")
	}

	cursor := all.Events[0].Seq
	after, err := ListEvents(conn, EventQuery{RunID: runID, Since: cursor})
	testsupport.Must(t, err, "ListEvents after the cursor: %v", err)
	for _, e := range after.Events {
		if e.Seq <= cursor {
			t.Errorf("seq %d was returned for --since %d; E5 is STRICTLY greater",
				e.Seq, cursor)
		}
	}
	if len(after.Events) != len(all.Events)-1 {
		t.Errorf("--since the first seq returned %d events, want %d — the cursor "+
			"skips exactly the event it names",
			len(after.Events), len(all.Events)-1)
	}

	// E7: the order is seq ASC, ALWAYS. There is no reverse mode: a cursor feed
	// that could run backwards is a cursor feed that skips.
	for i := 1; i < len(all.Events); i++ {
		if all.Events[i].Seq <= all.Events[i-1].Seq {
			t.Fatalf("events are not ascending by seq: %d after %d",
				all.Events[i].Seq, all.Events[i-1].Seq)
		}
	}
}

// TestCursorNeverSkipsUnderConcurrentInsert is §10.3's standing assertion and
// E10's property, stated as an experiment rather than as an argument.
//
// C9 IS THE RACE: events are inserted while the list is being read. The defense
// is structural — the read is one SELECT in one transaction and `seq` is
// monotonic, so a concurrent insert lands ABOVE the cursor and is returned by the
// next call. This test runs inserts against a LOOPING READER and asserts the
// union of everything the reader saw is EXACTLY the inserted set: nothing
// skipped, nothing returned twice.
func TestCursorNeverSkipsUnderConcurrentInsert(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)

	const inserts = 60

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range inserts {
			tx, err := conn.Begin()
			if err != nil {
				continue
			}
			if err := recordEvent(tx, eventRecord{
				Kind: EventStepHeartbeat, RunID: runID,
				Data: fmt.Sprintf(`{"n":%d}`, i), AtMS: nowMS + int64(i),
			}); err != nil {
				tx.Rollback()
				continue
			}
			tx.Commit()
		}
	}()

	// The reader follows the cursor exactly as a consumer would: it stores the
	// last seq it saw and passes it back. `seen` is a map so a DUPLICATE is
	// caught as loudly as a gap — returning an event twice is the other half of
	// E10 and the easier bug to write.
	seen := make(map[int64]int)
	cursor := int64(0)
	for range inserts * 4 {
		page, err := ListEvents(conn, EventQuery{RunID: runID, Since: cursor, Limit: 7})
		testsupport.Must(t, err, "ListEvents at cursor %d: %v", cursor, err)
		for _, e := range page.Events {
			seen[e.Seq]++
			if e.Seq <= cursor {
				t.Fatalf("seq %d returned at cursor %d: the read went backwards",
					e.Seq, cursor)
			}
			cursor = e.Seq
		}
	}
	wg.Wait()

	// Drain whatever landed after the reader's last pass, so the union is over
	// the whole inserted set rather than over a prefix of it.
	for {
		page, err := ListEvents(conn, EventQuery{RunID: runID, Since: cursor, Limit: 50})
		testsupport.Must(t, err, "draining at cursor %d: %v", cursor, err)
		if len(page.Events) == 0 {
			break
		}
		for _, e := range page.Events {
			seen[e.Seq]++
			cursor = e.Seq
		}
	}

	for seq, times := range seen {
		if times != 1 {
			t.Errorf("seq %d was returned %d times; E10 returns each event exactly once",
				seq, times)
		}
	}

	// The union is EXACTLY the inserted set: every row in the table was seen.
	var total int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE run_id = ?`, runID).Scan(&total)
	testsupport.Must(t, err, "counting: %v", err)
	if len(seen) != total {
		t.Errorf("the cursor feed saw %d of %d events; E10 skips none", len(seen), total)
	}
}

// TestEventLimitAppliesAfterOrdering is E9: the limit slices an ORDERED feed, and
// `total` counts matching events BEFORE the slice.
func TestEventLimitAppliesAfterOrdering(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	claimInstance(t, conn, "implement@0", nowMS)

	all, err := ListEvents(conn, EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if len(all.Events) < 3 {
		t.Fatalf("the fixture produced %d events; this needs 3", len(all.Events))
	}

	page, err := ListEvents(conn, EventQuery{RunID: runID, Limit: 2})
	testsupport.Must(t, err, "ListEvents with a limit: %v", err)
	if len(page.Events) != 2 {
		t.Fatalf("the limit returned %d rows, want 2", len(page.Events))
	}
	// The TRUE total, undistorted by the limit — the Collection contract, so
	// `truncated` is computable rather than guessed.
	if page.Total != all.Total {
		t.Errorf("total under a limit is %d, want the unlimited %d", page.Total, all.Total)
	}
	// And the two returned are the OLDEST two, not two arbitrary rows: the sort
	// runs over the whole matching set and the limit truncates afterwards.
	if page.Events[0].Seq != all.Events[0].Seq || page.Events[1].Seq != all.Events[1].Seq {
		t.Error("the limit did not slice an ordered feed; E9 applies it AFTER ordering")
	}
}

// TestRunFilterScopesTheFeed is E8: `--run` filters, and WITHOUT it the feed is
// repo-wide — which is the only place an event belonging to no run is visible.
func TestRunFilterScopesTheFeed(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	err := RecordTrustEvent(
		conn, EventTrustAdded,
		TrustGrant{Name: "checks", ArgvSHA256: "abc123", Repo: "/repo", Actor: "tester", Cwd: "/repo"}, nowMS,
	)
	testsupport.Must(t, err, "RecordTrustEvent: %v", err)

	scoped, err := ListEvents(conn, EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents scoped: %v", err)
	for _, e := range scoped.Events {
		if e.Kind == EventTrustAdded {
			t.Error("a run-less trust event appeared in a run-scoped feed")
		}
	}

	repoWide, err := ListEvents(conn, EventQuery{})
	testsupport.Must(t, err, "ListEvents repo-wide: %v", err)
	var sawTrust bool
	for _, e := range repoWide.Events {
		if e.Kind == EventTrustAdded {
			sawTrust = true
		}
	}
	if !sawTrust {
		t.Error("the trust event is invisible in the unfiltered feed too; E8 makes " +
			"the repo-wide feed the one place it shows")
	}
}

// TestGoneShapeIsReachableOnlyByPruning is §8.6 E18 and §10.3's standing
// assertion.
//
// E16: THE TRIGGER CANNOT FIRE AT THIS STAGE. Nothing prunes — `events prune` is
// S7's — so this test constructs the state BY DELETING ROWS DIRECTLY, which is
// the honest way to test a shape whose product trigger does not exist yet. That
// the test must reach into the table to reach the code is itself the assertion
// that no product path does.
func TestGoneShapeIsReachableOnlyByPruning(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	claimInstance(t, conn, "implement@0", nowMS)

	// Before any deletion: MIN(seq) is 1, so a cursor of 0 is in range. That is
	// E16 stated as a fact about a v10 repo rather than as a claim.
	_, err := ListEvents(conn, EventQuery{Since: 0})
	testsupport.Must(t, err, "a fresh repo answered %v; nothing has pruned, so nothing is GONE", err)

	// §8.6.1: `MIN(seq) > 1` is the proxy, and in a v10 repo it is achievable
	// ONLY by deleting row 1. `seq` is AUTOINCREMENT and never reused, so the
	// proxy is sound and stays sound now that S7's pruner has arrived.
	//
	// THE PRUNE IS REAL AT THIS STAGE, and that is the change this test records.
	// S6 constructed the state with a `DELETE` written here, because nothing in
	// the product could produce it: E16 said in terms that "the trigger cannot
	// fire at this stage". The test's NAME asserted the state was reachable only
	// by pruning while the test itself did the pruning by hand — aspirational,
	// and honestly labelled as such.
	//
	// It now calls the verb. The name stops being aspirational, and the claim it
	// makes — this state is reachable ONLY by pruning — is proven by the only
	// thing that can produce it doing so.
	//
	// The run is moved to a TERMINAL status first, because prune refuses a live
	// run's events (§5.2) — a refusal that is itself part of what makes GONE
	// reachable only through a deliberate act.
	execSQL(t, conn, `UPDATE runs SET status = 'done' WHERE id = ?`, runID)

	var maxSeq int64
	err = conn.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&maxSeq)
	testsupport.Must(t, err, "reading MAX(seq): %v", err)
	if maxSeq < 2 {
		t.Fatalf("the fixture wrote %d events; this needs at least 2", maxSeq)
	}
	// The prune keeps the NEWEST event, because the shape under test needs a
	// non-empty table with a raised floor — an empty one is not GONE, it is a
	// repo that retained everything it ever had.
	_, err = PruneEvents(conn, PruneQuery{Before: maxSeq, NowMS: nowMS})
	testsupport.Must(t, err, "PruneEvents: %v", err)

	_, err = ListEvents(conn, EventQuery{Since: 0})
	if err == nil {
		t.Fatal("a cursor below the retained minimum was answered; E15 returns GONE " +
			"rather than silently skipping")
	}
	code, ok := CodeOf(err)
	if !ok || code != CodeGone {
		t.Fatalf("the refusal is %v (code %q); E14 makes it GONE", err, code)
	}
	// E14's message must say what happened AND where to resume, because a
	// consumer receiving it has to choose a new cursor and the alternative is
	// re-reading from the beginning.
	for _, want := range []string{"retained minimum", "resume"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the GONE message %q does not mention %q", err.Error(), want)
		}
	}

	// A cursor AT or ABOVE the retained minimum is answered normally: it has
	// missed nothing. This is the boundary that keeps GONE from firing on every
	// caller of a pruned repo.
	var minSeq int64
	err = conn.QueryRow(`SELECT MIN(seq) FROM events`).Scan(&minSeq)
	testsupport.Must(t, err, "reading MIN(seq): %v", err)
	if _, err := ListEvents(conn, EventQuery{Since: minSeq - 1}); err != nil {
		t.Errorf("a cursor at the retained minimum was refused: %v", err)
	}
	if _, err := ListEvents(conn, EventQuery{Since: minSeq}); err != nil {
		t.Errorf("a cursor above the retained minimum was refused: %v", err)
	}

	// The prune left its OWN event behind (P18), above everything it deleted, so
	// the record of the deletion survived the deletion. A consumer that hit GONE
	// and resumed at the retained minimum reads the explanation for its own gap.
	page, err := ListEvents(conn, EventQuery{Since: minSeq - 1})
	testsupport.Must(t, err, "ListEvents at the retained minimum: %v", err)
	var sawPrune bool
	for _, e := range page.Events {
		if e.Kind == EventEventsPruned {
			sawPrune = true
		}
	}
	if !sawPrune {
		t.Error("the prune left no `events-pruned` event: a trimmed log would be " +
			"indistinguishable from a run that made fewer transitions (P18)")
	}

	// And an EMPTY table is not GONE: a repo that never wrote an event has
	// retained everything it ever had.
	execSQL(t, conn, `DELETE FROM events`)
	if _, err := ListEvents(conn, EventQuery{Since: 99}); err != nil {
		t.Errorf("an empty event table answered GONE: %v", err)
	}
}

// TestEveryEventKindHasAnActor is §8.7 E20 and §10.3's standing assertion.
//
// §9 item 2 — "every transition in events traceable to next/gate/threshold/human
// input" — is checkable only if EVERY kind maps to exactly one actor. This is the
// test that makes a stage adding a kind SAY WHO CAUSES IT: an unattributed kind
// would pass an audit that never asked about it.
func TestEveryEventKindHasAnActor(t *testing.T) {
	for _, kind := range EventKinds() {
		actor, ok := ActorFor(kind)
		if !ok {
			t.Errorf("event kind %q has no entry in the §8.7 attribution table; "+
				"a kind whose cause is unstated makes §9 item 2's audit vacuous", kind)
			continue
		}
		switch actor {
		case ActorNext, ActorGate, ActorThreshold, ActorHuman:
		default:
			t.Errorf("event kind %q is attributed to %q, which is not one of the "+
				"four causes engine-spec §9 item 2 names", kind, actor)
		}
	}

	// The other direction: no actor entry names a kind outside the closed set.
	// Without this, the table could carry a stale row for a kind that was
	// renamed and the count would still match.
	for kind := range eventActors {
		if !eventKinds[kind] {
			t.Errorf("the attribution table names %q, which is not in the closed set", kind)
		}
	}

	// The three kinds group 2 added are attributed, which is §8.8's discipline
	// ("added to the constants, to eventKinds, and to §8.7's attribution table
	// in the same commit") asserted rather than trusted.
	for _, kind := range []string{
		EventDispatchOpened, EventDispatchClosed, EventDispatchAbandoned,
	} {
		if _, ok := ActorFor(kind); !ok {
			t.Errorf("dispatch kind %q is unattributed", kind)
		}
	}
}

// TestAttributionCoversACompletedRun is §8.7 E22 and §9 item 2 at the Go level:
// a run driven to `done` has EVERY event attributable and NO event outside the
// closed set.
//
// The QA section runs the same audit over the CLI's read surface, which is where
// item 2 becomes an operator-checkable fact. This is its unit twin, so a
// regression fails fast rather than at the end of a QA sweep.
func TestAttributionCoversACompletedRun(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	e := testEngine()
	completeWithUsage(t, conn, e, "implement@0", `{"pages": 5}`)

	page, err := ListEvents(conn, EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if len(page.Events) == 0 {
		t.Fatal("the run produced no events; there is nothing to audit")
	}

	counts := make(map[Actor]int)
	for _, ev := range page.Events {
		if !eventKinds[ev.Kind] {
			t.Errorf("event %d has kind %q, outside the closed set", ev.Seq, ev.Kind)
			continue
		}
		actor, ok := ActorFor(ev.Kind)
		if !ok {
			t.Errorf("event %d (%s) is attributable to nothing", ev.Seq, ev.Kind)
			continue
		}
		counts[actor]++
	}

	// The rollup agrees with the feed, which is what makes the report's
	// per-actor section (E21) a view of the same fact rather than a second one.
	rolled, err := ActorCounts(conn, runID)
	testsupport.Must(t, err, "ActorCounts: %v", err)
	for actor, n := range counts {
		if rolled[actor] != n {
			t.Errorf("actor %s: the feed shows %d events and the rollup %d",
				actor, n, rolled[actor])
		}
	}
}

// TestEventsListWritesNothing is §10.3's row for this verb, in the shape
// TestReadVerbsWriteNothing established: hashing the FILE, because a row count
// would miss an in-place update.
func TestEventsListWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/issues.db"
	conn, err := sql.Open("sqlite", path)
	testsupport.Must(t, err, "opening: %v", err)
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	err = db.Initialize(conn)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = db.Migrate(conn)
	testsupport.Must(t, err, "Migrate: %v", err)

	runID, _ := budgetRun(t, conn, 0)
	claimInstance(t, conn, "implement@0", nowMS)
	// A LAPSED lease, the write a read verb is most likely to make by accident:
	// `next` would reap it, and a feed that shared that code path would too.
	execSQL(t, conn, `UPDATE steps SET expires_ms = 1 WHERE instance = 'implement@0'`)

	_, err = conn.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	testsupport.Must(t, err, "checkpointing: %v", err)
	before := hashFile(t, path)

	_, err = ListEvents(conn, EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents: %v", err)

	_, err = conn.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	testsupport.Must(t, err, "checkpointing after the read: %v", err)
	if after := hashFile(t, path); after != before {
		t.Error("`events list` changed the database; it is a pure read — not even " +
			"the reap `next` would do")
	}
}

// seedEvents writes n run-less events and returns their seqs in insertion
// order. Run-less keeps the fixture independent of run/step scaffolding: the
// tail window is a property of the feed, not of what wrote to it.
func seedEvents(t *testing.T, conn *sql.DB, n int) []int64 {
	t.Helper()
	seqs := make([]int64, 0, n)
	for i := 1; i <= n; i++ {
		res, err := conn.Exec(
			`INSERT INTO events (at_ms, kind, data) VALUES (?, ?, '{}')`,
			int64(1700000000000+i), fmt.Sprintf("probe-%d", i))
		testsupport.Must(t, err, "seeding event %d: %v", i, err)
		seq, err := res.LastInsertId()
		testsupport.Must(t, err, "reading seq of event %d: %v", i, err)
		seqs = append(seqs, seq)
	}
	return seqs
}

func tailSeqs(events []Event) []int64 {
	out := make([]int64, 0, len(events))
	for _, e := range events {
		out = append(out, e.Seq)
	}
	return out
}

// TestTailReturnsNewestButStillAscending pins --tail.
//
// The ordering half is the load-bearing one. E7 forbids a reverse mode because
// a feed that ran backwards would let a consumer store the FIRST seq it saw as
// its cursor and silently skip everything below it. --tail changes WHICH window
// is returned, never its direction, so the assertion below is not "the newest
// N" alone — it is "the newest N, ascending".
func TestTailReturnsNewestButStillAscending(t *testing.T) {
	conn := mustDB(t)
	seqs := seedEvents(t, conn, 12)
	newest3 := seqs[len(seqs)-3:]

	page, err := ListEvents(conn, EventQuery{Tail: 3})
	testsupport.Must(t, err, "ListEvents(Tail=3): %v", err)

	got := tailSeqs(page.Events)
	if len(got) != 3 {
		t.Fatalf("Tail=3 returned %d events, want 3 (seqs %v)", len(got), got)
	}
	for i := range got {
		if got[i] != newest3[i] {
			t.Fatalf("Tail=3 seqs = %v, want the newest three ascending %v", got, newest3)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Errorf("seqs %v are not strictly ascending; E7 forbids a reverse feed", got)
		}
	}

	// `total` counts MATCHING events, not returned ones, so `truncated` stays
	// computable against the real size of the feed.
	if page.Total != 12 {
		t.Errorf("Total = %d, want 12 (the matching count, not the page size)", page.Total)
	}
}

// TestTailLargerThanFeedReturnsEverything: asking for more than exists is not
// an error, and must not drop back to the oldest-N window.
func TestTailLargerThanFeedReturnsEverything(t *testing.T) {
	conn := mustDB(t)
	seqs := seedEvents(t, conn, 5)

	page, err := ListEvents(conn, EventQuery{Tail: 100})
	testsupport.Must(t, err, "ListEvents(Tail=100): %v", err)
	got := tailSeqs(page.Events)
	if len(got) != len(seqs) {
		t.Fatalf("Tail=100 over a 5-event feed returned %d, want 5", len(got))
	}
	if got[0] != seqs[0] {
		t.Errorf("first seq = %d, want the true oldest %d", got[0], seqs[0])
	}
}

// TestTailRespectsRunFilter keeps --tail composing with --run: the window is
// the newest N OF THE FILTERED SET, not the newest N overall then filtered,
// which would return fewer rows than asked for whenever another run interleaves.
func TestTailRespectsRunFilter(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)

	// Interleave run-less events among the run's own, so a tail computed
	// before filtering would come back short.
	seedEvents(t, conn, 4)
	claimInstance(t, conn, "implement@0", nowMS)
	seedEvents(t, conn, 4)

	page, err := ListEvents(conn, EventQuery{RunID: runID, Tail: 2})
	testsupport.Must(t, err, "ListEvents(RunID, Tail=2): %v", err)
	if len(page.Events) == 0 {
		t.Fatal("Tail with a run filter returned nothing; the window was computed before the filter")
	}
	for _, e := range page.Events {
		if e.Run != "RUN-1" {
			t.Errorf("event %d has run %q, want only RUN-1 rows", e.Seq, e.Run)
		}
	}
}

// TestTailZeroIsUnchanged: the default path must be byte-identical to the
// behaviour that predates --tail, since every existing consumer reads it.
func TestTailZeroIsUnchanged(t *testing.T) {
	conn := mustDB(t)
	seqs := seedEvents(t, conn, 6)

	page, err := ListEvents(conn, EventQuery{Tail: 0, Limit: 3})
	testsupport.Must(t, err, "ListEvents(Tail=0): %v", err)
	got := tailSeqs(page.Events)
	if len(got) != 3 || got[0] != seqs[0] {
		t.Errorf("Tail=0 with Limit=3 gave %v, want the OLDEST three starting at %d",
			got, seqs[0])
	}
}
