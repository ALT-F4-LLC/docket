package daemon

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// waitBound caps every wait a test makes on the stream, so a defect that
// blocks fails the test instead of hanging the package.
const waitBound = 5 * time.Second

func newEventStore(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "issues.db"))
	testsupport.Must(t, err, "opening database: %v", err)
	t.Cleanup(func() { conn.Close() })
	err = db.Initialize(conn)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = db.Migrate(conn)
	testsupport.Must(t, err, "Migrate: %v", err)
	return conn
}

// appendEvents writes n store-level events and returns their seqs.
func appendEvents(t *testing.T, conn *sql.DB, n int) []int64 {
	t.Helper()
	var seqs []int64
	for range n {
		res, err := conn.Exec(`INSERT INTO events (at_ms, kind) VALUES (1, ?)`,
			engine.EventStepHeartbeat)
		testsupport.Must(t, err, "appending an event: %v", err)
		seq, err := res.LastInsertId()
		testsupport.Must(t, err, "reading the appended seq: %v", err)
		seqs = append(seqs, seq)
	}
	return seqs
}

// doorbell is a faithful stand-in for the in-process broadcast: Subscribe
// hands out a one-slot wake channel, and ring wakes every subscriber without
// blocking. subscribed receives once per Subscribe, so a test can wait for a
// stream to be listening before it appends.
type doorbell struct {
	mu         sync.Mutex
	subs       map[chan struct{}]struct{}
	subscribed chan struct{}
}

func newDoorbell() *doorbell {
	return &doorbell{subs: map[chan struct{}]struct{}{}, subscribed: make(chan struct{}, 16)}
}

func (d *doorbell) Subscribe() (<-chan struct{}, func()) {
	wake := make(chan struct{}, 1)
	d.mu.Lock()
	d.subs[wake] = struct{}{}
	d.mu.Unlock()
	d.subscribed <- struct{}{}
	return wake, func() {
		d.mu.Lock()
		delete(d.subs, wake)
		d.mu.Unlock()
	}
}

func (d *doorbell) ring() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for wake := range d.subs {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (d *doorbell) awaitSubscriber(t *testing.T) {
	t.Helper()
	select {
	case <-d.subscribed:
	case <-time.After(waitBound):
		t.Fatal("the stream never subscribed to the broadcast")
	}
}

// streamFrames runs StreamEvents in the background and returns its frames
// as they arrive, plus a stop func that cancels it and returns its error.
func streamFrames(t *testing.T, conn *sql.DB, bell *doorbell, since int64) (<-chan StreamFrame, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	frames := make(chan StreamFrame, 64)
	done := make(chan error, 1)
	go func() {
		done <- StreamEvents(ctx, conn, bell, engine.EventQuery{Since: since},
			func(f StreamFrame) error {
				select {
				case frames <- f:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
	}()
	return frames, func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(waitBound):
			t.Fatal("StreamEvents did not return after its context was cancelled")
			return nil
		}
	}
}

// collectSeqs reads frames until n events arrived and returns every frame's
// seq in arrival order; a prune marker is recorded as its negated floor.
func collectSeqs(t *testing.T, frames <-chan StreamFrame, n int) []int64 {
	t.Helper()
	var got []int64
	events := 0
	for events < n {
		select {
		case f := <-frames:
			if f.Event == nil {
				got = append(got, -f.PrunedFloor)
				continue
			}
			got = append(got, f.Event.Seq)
			events++
		case <-time.After(waitBound):
			t.Fatalf("received %d of %d events: %v", events, n, got)
		}
	}
	return got
}

// TestStreamResumesStrictlyAfterCursor feeds seq 1..N, reconnects at cursor
// k, and requires k+1..N once each, then one live append, with nothing
// repeated across the catch-up and the wake.
func TestStreamResumesStrictlyAfterCursor(t *testing.T) {
	conn := newEventStore(t)
	bell := newDoorbell()
	seqs := appendEvents(t, conn, 6)
	k := seqs[2]

	frames, stop := streamFrames(t, conn, bell, k)
	bell.awaitSubscriber(t)
	got := collectSeqs(t, frames, len(seqs)-3)

	live := appendEvents(t, conn, 1)
	bell.ring()
	got = append(got, collectSeqs(t, frames, 1)...)

	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("StreamEvents returned %v after cancel; want context.Canceled", err)
	}
	select {
	case f := <-frames:
		t.Errorf("an extra frame arrived after the expected events: %+v", f)
	default:
	}

	want := append(slices.Clone(seqs[3:]), live...)
	if !slices.Equal(got, want) {
		t.Errorf("resumed at cursor %d: delivered %v, want %v", k, got, want)
	}
}

// TestStreamMarksPrunedFloorBeforeFirstEvent prunes below seq p and opens
// the stream below it: the first frame names floor p, then events resume at
// p. A cursor at p-1 has missed nothing and gets no marker.
func TestStreamMarksPrunedFloorBeforeFirstEvent(t *testing.T) {
	conn := newEventStore(t)
	bell := newDoorbell()
	seqs := appendEvents(t, conn, 6)
	p := seqs[3]

	result, err := engine.PruneEvents(conn, engine.PruneQuery{Before: p, NowMS: time.Now().UnixMilli()})
	testsupport.Must(t, err, "PruneEvents: %v", err)
	if result.RetainedMinimum != p {
		t.Fatalf("fixture retained minimum = %d, want %d", result.RetainedMinimum, p)
	}
	// The prune records its own event above everything it kept.
	total := len(seqs[3:]) + 1

	frames, stop := streamFrames(t, conn, bell, 0)
	bell.awaitSubscriber(t)
	got := collectSeqs(t, frames, total)
	stop()
	if len(got) == 0 || got[0] != -p {
		t.Fatalf("cursor 0 below floor %d: frames %v; want a marker naming %d first", p, got, p)
	}
	if got[1] != p {
		t.Errorf("the first event after the marker is seq %d, want the floor %d", got[1], p)
	}

	frames, stop = streamFrames(t, conn, bell, p-1)
	bell.awaitSubscriber(t)
	got = collectSeqs(t, frames, total)
	stop()
	if got[0] != p {
		t.Errorf("cursor %d at the watermark: frames %v; want seq %d first and no marker", p-1, got, p)
	}
}

// TestLongPollReturnsAppendBeforeTimeout blocks on an empty log and requires
// the event appended while it waits, well before its timeout.
func TestLongPollReturnsAppendBeforeTimeout(t *testing.T) {
	conn := newEventStore(t)
	bell := newDoorbell()
	const timeout = time.Minute

	type answer struct {
		batch EventBatch
		err   error
	}
	answered := make(chan answer, 1)
	go func() {
		batch, err := LongPollEvents(context.Background(), conn, bell, engine.EventQuery{}, timeout)
		answered <- answer{batch, err}
	}()
	bell.awaitSubscriber(t)

	select {
	case a := <-answered:
		t.Fatalf("the long poll answered %+v (err %v) before anything was appended", a.batch, a.err)
	case <-time.After(50 * time.Millisecond):
	}

	appended := appendEvents(t, conn, 1)
	bell.ring()

	select {
	case a := <-answered:
		testsupport.Must(t, a.err, "LongPollEvents: %v", a.err)
		if len(a.batch.Events) != 1 || a.batch.Events[0].Seq != appended[0] {
			t.Errorf("the long poll returned %+v, want the appended seq %d", a.batch.Events, appended[0])
		}
	case <-time.After(waitBound):
		t.Fatal("the long poll did not return the appended event")
	}
}

// TestLongPollReturnsEmptyAtTimeout requires an empty, successful answer
// once the timeout passes with nothing appended.
func TestLongPollReturnsEmptyAtTimeout(t *testing.T) {
	conn := newEventStore(t)
	bell := newDoorbell()
	const timeout = 100 * time.Millisecond

	answered := make(chan error, 1)
	var batch EventBatch
	started := time.Now()
	go func() {
		var err error
		batch, err = LongPollEvents(context.Background(), conn, bell, engine.EventQuery{}, timeout)
		answered <- err
	}()

	select {
	case err := <-answered:
		testsupport.Must(t, err, "LongPollEvents: %v", err)
	case <-time.After(waitBound):
		t.Fatalf("the long poll blocked past its %s timeout", timeout)
	}
	if elapsed := time.Since(started); elapsed < timeout {
		t.Errorf("the long poll answered after %s, before its %s timeout", elapsed, timeout)
	}
	if len(batch.Events) != 0 || batch.PrunedFloor != 0 {
		t.Errorf("the long poll timed out with %+v, want an empty batch", batch)
	}
}
