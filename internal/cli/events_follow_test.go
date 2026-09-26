package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The `--follow` loop's properties (docs/tdd/events-follow.md §4.3).
//
// EVERY TEST HERE DRIVES followEvents DIRECTLY rather than spawning a process.
// W3's cursor discipline and W8's termination are behaviors of the LOOP, and a
// test that had to read a subprocess's stdout would be observing the shell
// between it and the property.
//
// NOTHING HERE SLEEPS TO MAKE AN ASSERTION TRUE. The interval is set small and
// the loop is stopped by CANCELLING ITS CONTEXT once the events it was supposed
// to see have arrived — the repo's standing TTL-flake discipline (§5.9 of
// runs-dispatch), applied to a poller.

// followTrustEvent writes one event with no run, which is the cheapest event the
// writer will accept: `events.run_id` is nullable and a trust grant legitimately
// has none (gates-trust §3.6). The feed under test is repo-wide, so what these
// rows MEAN does not matter — only that they are events with ascending seqs.
func followTrustEvent(t *testing.T, conn *sql.DB, name string) {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	err := engine.RecordTrustEvent(
		conn, engine.EventTrustAdded,
		// Actor and Cwd are REQUIRED (DKT-595) — the writer refuses an
		// unattributed grant — so even this plumbing fixture supplies them.
		engine.TrustGrant{
			Name: name, ArgvSHA256: fmt.Sprintf("%x", sum), Repo: "repo",
			Actor: "tester", Cwd: "/repo",
		},
		time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("RecordTrustEvent(%q): %v", name, err)
	}
}

// followSeqs pulls the `seq` of every event a follow printed, in print order,
// out of its NDJSON stdout.
//
// It parses the ACTUAL OUTPUT rather than instrumenting the loop, because what
// the property is about is what a consumer receives. A test that counted an
// internal variable could pass over a loop that computed the right cursor and
// printed the wrong page.
func followSeqs(t *testing.T, out string) []int64 {
	t.Helper()
	var seqs []int64
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var envelope struct {
			Data struct {
				Events []struct {
					Seq int64 `json:"seq"`
				} `json:"events"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("a follow cycle emitted invalid JSON: %v\nline: %s", err, line)
		}
		for _, e := range envelope.Data.Events {
			seqs = append(seqs, e.Seq)
		}
	}
	return seqs
}

// syncBuffer guards a bytes.Buffer with a mutex so the follow goroutine's
// writes and the test goroutine's polling reads (via String, through
// waitFor/followSeqs) never race. followEvents only ever needs an io.Writer,
// so this stays test-only rather than changing the production Stdout type.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func followOpts(conn *sql.DB, stdout, stderr *syncBuffer) followOptions {
	return followOptions{
		conn:        conn,
		query:       engine.EventQuery{Limit: eventsDefaultLimit},
		interval:    5 * time.Millisecond,
		jsonMode:    true,
		jsonVersion: output.JSONV1,
		stdout:      stdout,
		stderr:      stderr,
	}
}

// TestFollowSeesNewEventsAndStops is §4.3's headline, and the one the work order
// names: a follow starts, a writer inserts events, the follow prints them, the
// context is cancelled, and the call returns having printed EACH NEW EVENT
// EXACTLY ONCE.
//
// The three assertions are separable and all three matter: it SAW the events
// (a follow that printed nothing would satisfy "never duplicates"), it printed
// each ONCE (the cursor advanced), and it STOPPED (W6 — cancellation is how a
// follow ends, and a loop that ignored its context would hang the test rather
// than fail it).
func TestFollowSeesNewEventsAndStops(t *testing.T) {
	conn := newTestDB(t)

	// Two events exist BEFORE the follow starts, so the first cycle has a
	// backlog to drain — a follow is not only a live tail.
	followTrustEvent(t, conn, "before-1")
	followTrustEvent(t, conn, "before-2")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr syncBuffer
	done := make(chan error, 1)
	go func() { done <- followEvents(ctx, followOpts(conn, &stdout, &stderr)) }()

	// Write more events WHILE the follow is running. This is the live half:
	// these rows did not exist when the loop was started.
	const live = 5
	for i := 0; i < live; i++ {
		followTrustEvent(t, conn, fmt.Sprintf("live-%d", i))
	}

	// Stop once everything is visible, rather than after a fixed duration.
	// Waiting a wall-clock interval and hoping is how a poller test flakes.
	waitFor(t, func() bool { return len(followSeqs(t, stdout.String())) >= 2+live })
	cancel()

	select {
	case err := <-done:
		testsupport.Must(t, err, "a cancelled follow returned %v; W6 makes Ctrl-C exit 0", err)
	case <-time.After(2 * time.Second):
		t.Fatal("the follow did not return after its context was cancelled (W6)")
	}

	seqs := followSeqs(t, stdout.String())
	if len(seqs) != 2+live {
		t.Fatalf("the follow printed %d events, want %d", len(seqs), 2+live)
	}

	// EXACTLY ONCE, and in order. Both fall out of W3's cursor, and a failure of
	// either is a failure of the same clause.
	seen := make(map[int64]bool, len(seqs))
	for i, seq := range seqs {
		if seen[seq] {
			t.Errorf("seq %d was printed twice; the cursor did not advance past it", seq)
		}
		seen[seq] = true
		if i > 0 && seq <= seqs[i-1] {
			t.Errorf("seq %d printed after %d; a follow is ordered oldest-first",
				seq, seqs[i-1])
		}
	}
}

// TestFollowCursorNeverRepeatsOrSkips extends E10's property ACROSS CYCLES.
//
// S6 proved it for one call: an insert landing during a read is returned by the
// next call. A follow makes "the next call" automatic, so the property becomes
// the union over every cycle being exactly the inserted set. That is the
// difference between a cursor that is sound and a poller that is sound.
func TestFollowCursorNeverRepeatsOrSkips(t *testing.T) {
	conn := newTestDB(t)

	const inserts = 40
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr syncBuffer
	// A small limit forces MULTIPLE CYCLES to drain the backlog, which is the
	// case W5 describes: a follower starting far behind catches up over several
	// cycles rather than in one page.
	opts := followOpts(conn, &stdout, &stderr)
	opts.query.Limit = 7

	done := make(chan error, 1)
	go func() { done <- followEvents(ctx, opts) }()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < inserts; i++ {
			followTrustEvent(t, conn, fmt.Sprintf("racer-%d", i))
		}
	}()
	wg.Wait()

	waitFor(t, func() bool { return len(followSeqs(t, stdout.String())) >= inserts })
	cancel()
	err := <-done
	testsupport.Must(t, err, "followEvents: %v", err)

	seqs := followSeqs(t, stdout.String())

	// The union is exactly the inserted set: every seq in the table appeared,
	// and nothing appeared twice.
	want, err := engine.ListEvents(conn, engine.EventQuery{})
	testsupport.Must(t, err, "ListEvents: %v", err)
	printed := make(map[int64]int, len(seqs))
	for _, seq := range seqs {
		printed[seq]++
	}
	for _, e := range want.Events {
		switch printed[e.Seq] {
		case 1:
			// Exactly right.
		case 0:
			t.Errorf("seq %d was never printed: the follow SKIPPED an event", e.Seq)
		default:
			t.Errorf("seq %d was printed %d times: the follow REPEATED an event",
				e.Seq, printed[e.Seq])
		}
	}
}

// TestFollowTailStartsAtTheNewestN is DKT-752: `--follow --tail N` opens at the
// newest N and then follows, instead of replaying the whole retained history.
//
// THE ASSERTION IS ON THE HISTORICAL HALF, not on the total. A follow that
// printed the newest 3 and then the live rows is right; a follow that printed
// all 30 first and then the live rows is the bug, and both end with the same
// live events on stdout. So the test pins WHICH events preceded the live ones:
// at most N of them, and specifically the last N seqs in the store when the
// follow opened.
func TestFollowTailStartsAtTheNewestN(t *testing.T) {
	conn := newTestDB(t)

	// A history comfortably larger than the tail, so "the newest N" and
	// "everything" are different answers.
	const history = 30
	for i := 0; i < history; i++ {
		followTrustEvent(t, conn, fmt.Sprintf("old-%d", i))
	}
	before, err := engine.ListEvents(conn, engine.EventQuery{})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if len(before.Events) != history {
		t.Fatalf("fixture wrote %d events, want %d", len(before.Events), history)
	}
	const tail = 3
	newest := make([]int64, 0, tail)
	for _, e := range before.Events[history-tail:] {
		newest = append(newest, e.Seq)
	}
	oldest := before.Events[0].Seq

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr syncBuffer
	opts := followOpts(conn, &stdout, &stderr)
	opts.query.Tail = tail

	done := make(chan error, 1)
	go func() { done <- followEvents(ctx, opts) }()

	// The opening page must land before the live rows are written, or a slow
	// first cycle could tail a feed that already contains them and the
	// historical half would be unidentifiable.
	waitFor(t, func() bool { return len(followSeqs(t, stdout.String())) >= tail })
	opened := followSeqs(t, stdout.String())

	// Then it FOLLOWS: rows written after the tail still arrive.
	const live = 2
	for i := 0; i < live; i++ {
		followTrustEvent(t, conn, fmt.Sprintf("live-%d", i))
	}
	waitFor(t, func() bool { return len(followSeqs(t, stdout.String())) >= tail+live })
	cancel()
	testsupport.Must(t, <-done, "a cancelled follow returned an error")

	seqs := followSeqs(t, stdout.String())
	if len(seqs) < tail+live {
		t.Fatalf("the follow printed %v, want the newest %d then %d live events",
			seqs, tail, live)
	}

	// The acceptance criterion: AT MOST N historical events, and they are the
	// newest N.
	historical := seqs[:len(seqs)-live]
	if len(historical) != tail {
		t.Errorf("the follow printed %d historical events before the live ones, want at most %d"+
			" (--tail was ignored and the full history replayed): %v",
			len(historical), tail, historical)
	}
	for i := range newest {
		if i >= len(historical) || historical[i] != newest[i] {
			t.Fatalf("the follow opened on %v, want the newest %d seqs %v",
				historical, tail, newest)
		}
	}
	for _, seq := range seqs {
		if seq == oldest {
			t.Errorf("the follow printed seq %d, the OLDEST retained event: "+
				"--tail did not move the starting cursor", oldest)
		}
	}

	// The opening page itself was the tail, not a first slice of the history.
	if len(opened) > tail {
		t.Errorf("the opening cycle printed %d events, want at most --tail=%d: %v",
			len(opened), tail, opened)
	}

	// Nothing is printed twice: the tail is a STARTING CURSOR, so the cycle
	// after it reads with --since rather than re-selecting the same newest N.
	seen := make(map[int64]bool, len(seqs))
	for _, seq := range seqs {
		if seen[seq] {
			t.Errorf("seq %d was printed twice; --tail was applied to more than the first cycle",
				seq)
		}
		seen[seq] = true
	}
}

// TestFollowTailOnAnEmptyFeedStillFollows is the boundary the tail's
// "first cycle only" rule turns on: an opening tailed read over an EMPTY store
// returns nothing, so the cursor never advances — and the follow must still
// print what is written next rather than sitting on a spent tail forever.
func TestFollowTailOnAnEmptyFeedStillFollows(t *testing.T) {
	conn := newTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr syncBuffer
	opts := followOpts(conn, &stdout, &stderr)
	opts.query.Tail = 5

	done := make(chan error, 1)
	go func() { done <- followEvents(ctx, opts) }()

	const live = 3
	for i := 0; i < live; i++ {
		followTrustEvent(t, conn, fmt.Sprintf("after-%d", i))
	}

	waitFor(t, func() bool { return len(followSeqs(t, stdout.String())) >= live })
	cancel()
	testsupport.Must(t, <-done, "a cancelled follow returned an error")

	if seqs := followSeqs(t, stdout.String()); len(seqs) != live {
		t.Fatalf("a tailed follow over an initially empty feed printed %v, want %d events",
			seqs, live)
	}
}

// TestFollowStopsOnGone is W8, the clause that keeps `--follow` honest about a
// prune: a cursor that falls below the retained minimum TERMINATES the follow
// with GONE rather than resuming at the new minimum.
//
// Resuming is the tempting behavior — the follow could simply carry on from
// what survives — and it is exactly the silent skip engine-spec §3 forbids: the
// consumer would receive a feed with a hole in it and no indication there was
// one.
func TestFollowStopsOnGone(t *testing.T) {
	conn := newTestDB(t)
	for i := 0; i < 12; i++ {
		followTrustEvent(t, conn, fmt.Sprintf("pre-%d", i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr syncBuffer
	opts := followOpts(conn, &stdout, &stderr)
	// A LIMIT OF 1 KEEPS THE FOLLOWER BEHIND, which is the situation the clause
	// is about: a follow that had already caught up would have a cursor above
	// anything a prune could reasonably remove, and the interesting case —
	// somebody trims the log out from under a lagging consumer — would never
	// arise. It drains one event per cycle, so after a couple of cycles its
	// cursor is a long way below the head.
	opts.query.Limit = 1

	done := make(chan error, 1)
	go func() { done <- followEvents(ctx, opts) }()

	// Let it establish itself, and stay behind, before the ground moves.
	waitFor(t, func() bool { return len(followSeqs(t, stdout.String())) >= 2 })

	// PRUNE THROUGH PRODUCT CODE, not by hand. That is the whole point of this
	// stage: at S6 the only way to reach GONE was a DELETE in a test.
	if _, err := engine.PruneEvents(conn, engine.PruneQuery{
		Before: 10, NowMS: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("PruneEvents: %v", err)
	}

	select {
	case err := <-done:
		code, ok := engine.CodeOf(err)
		if !ok || code != engine.CodeGone {
			t.Fatalf("a follow whose cursor was pruned away returned %v; want GONE", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the follow kept polling after its cursor fell below the retained " +
			"minimum: W8 requires it to stop rather than silently skip")
	}
}

// TestFollowWritesNothing is W7. Reading a log cannot advance a run, and a
// follow reads it repeatedly — so the property S6 asserted for one read is
// asserted here for MANY, which is the case that could plausibly differ: a
// per-cycle bookkeeping write would pass a one-shot test and fail this one.
//
// The comparison is over EVERY TABLE'S ROWS rather than the file's bytes. WAL
// mode moves bytes around for reasons that have nothing to do with whether a
// verb wrote a row, so a byte comparison here would be asserting something
// about SQLite's checkpointing instead of about this verb.
func TestFollowWritesNothing(t *testing.T) {
	conn := newTestDB(t)
	for i := 0; i < 4; i++ {
		followTrustEvent(t, conn, fmt.Sprintf("row-%d", i))
	}

	before := tableCounts(t, conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr syncBuffer
	done := make(chan error, 1)
	go func() { done <- followEvents(ctx, followOpts(conn, &stdout, &stderr)) }()

	// Several cycles, not one: the point is that polling repeatedly writes
	// nothing repeatedly.
	waitFor(t, func() bool {
		return len(followSeqs(t, stdout.String())) >= 4 &&
			strings.Count(stdout.String(), "\n") >= 1
	})
	time.Sleep(30 * time.Millisecond) // several more idle cycles at 5ms
	cancel()
	err := <-done
	testsupport.Must(t, err, "followEvents: %v", err)

	after := tableCounts(t, conn)
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s went from %d rows to %d while a follow ran: a read verb wrote (W7)",
				table, n, after[table])
		}
	}
}

// tableCounts snapshots every table's row count.
func tableCounts(t *testing.T, conn *sql.DB) map[string]int {
	t.Helper()
	rows, err := conn.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	testsupport.Must(t, err, "listing tables: %v", err)
	var tables []string
	for rows.Next() {
		var name string
		err := rows.Scan(&name)
		testsupport.Must(t, err, "reading a table name: %v", err)
		tables = append(tables, name)
	}
	rows.Close()

	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var n int
		if err := conn.QueryRow(`SELECT COUNT(*) FROM "` + table + `"`).Scan(&n); err != nil {
			t.Fatalf("counting %s: %v", table, err)
		}
		counts[table] = n
	}
	return counts
}

// waitFor polls a condition to a deadline.
//
// It exists so no test in this file asserts by SLEEPING A FIXED DURATION AND
// HOPING — the thing that makes poller tests flake on a loaded CI box. The
// condition is what the test is waiting for; the deadline is only a failure
// mode.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the follow to produce its events")
}
