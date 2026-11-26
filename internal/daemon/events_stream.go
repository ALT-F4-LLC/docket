package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/engine"
)

// AppendNotifier is the in-process broadcast the event stream waits on.
//
// Subscribe returns a wake channel and a cancel func. A publish rings every
// subscriber without blocking, and rings coalesce, so a wake carries no event
// data: the stream re-reads the store from its own cursor on each wake, which
// keeps the store the only source of events and makes a coalesced ring
// harmless.
type AppendNotifier interface {
	Subscribe() (wake <-chan struct{}, cancel func())
}

// EventBatch is one read of the event log after a cursor.
type EventBatch struct {
	// PrunedFloor is non-zero when a prune deleted events after the cursor:
	// it is the lowest retained seq, and Events resume at or above it.
	PrunedFloor int64
	Events      []engine.Event
}

// StreamFrame is one message of the event stream: a prune marker
// (PrunedFloor set, Event nil) or one event.
type StreamFrame struct {
	PrunedFloor int64
	Event       *engine.Event
}

var errNotifierClosed = errors.New("the event broadcast closed its wake channel")

// StreamEvents sends every event after q.Since to send in seq order, each
// exactly once, then waits on notify for appends until ctx ends, returning
// ctx's error. A cursor below pruned history first gets a marker frame
// naming the floor. q's other filters apply to every read; q.Tail is a
// one-shot selection with no meaning on a stream and must be zero.
func StreamEvents(ctx context.Context, conn *sql.DB, notify AppendNotifier,
	q engine.EventQuery, send func(StreamFrame) error) error {
	if q.Tail != 0 {
		return fmt.Errorf("an event stream resumes from a cursor; tail %d is not supported", q.Tail)
	}
	// Subscribing before the first read means an append that lands after
	// that read still rings the channel this loop waits on.
	wake, cancel := notify.Subscribe()
	defer cancel()

	for {
		batch, err := readEventsAfter(ctx, conn, q)
		if err != nil {
			return err
		}
		if batch.PrunedFloor != 0 {
			if err := send(StreamFrame{PrunedFloor: batch.PrunedFloor}); err != nil {
				return err
			}
			q.Since = batch.PrunedFloor - 1
		}
		for i := range batch.Events {
			if err := send(StreamFrame{Event: &batch.Events[i]}); err != nil {
				return err
			}
			q.Since = batch.Events[i].Seq
		}
		if len(batch.Events) > 0 {
			continue
		}
		select {
		case _, ok := <-wake:
			if !ok {
				return errNotifierClosed
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// LongPollEvents is the stream's fallback for a client that cannot hold a
// stream open. It answers at once when anything lies after q.Since, and
// otherwise waits for an append; at timeout it returns an empty batch.
func LongPollEvents(ctx context.Context, conn *sql.DB, notify AppendNotifier,
	q engine.EventQuery, timeout time.Duration) (EventBatch, error) {
	wake, cancel := notify.Subscribe()
	defer cancel()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		batch, err := readEventsAfter(ctx, conn, q)
		if err != nil || batch.PrunedFloor != 0 || len(batch.Events) > 0 {
			return batch, err
		}
		select {
		case _, ok := <-wake:
			if !ok {
				return EventBatch{}, errNotifierClosed
			}
		case <-ctx.Done():
			return EventBatch{}, ctx.Err()
		case <-timer.C:
			return EventBatch{}, nil
		}
	}
}

// readEventsAfter reads the events after q.Since. When the cursor sits below
// the event low watermark, ListEvents answers GONE; the stream instead
// resumes at the watermark and reports the floor, so the client learns what
// it lost rather than being refused. A prune landing between the watermark
// read and the list makes ListEvents answer GONE again, so the pair repeats.
func readEventsAfter(ctx context.Context, conn *sql.DB, q engine.EventQuery) (EventBatch, error) {
	var floor int64
	for {
		page, err := engine.ListEvents(conn, q)
		if code, ok := engine.CodeOf(err); ok && code == engine.CodeGone {
			watermark, err := eventLowWatermark(ctx, conn)
			if err != nil {
				return EventBatch{}, err
			}
			q.Since = watermark
			floor = watermark + 1
			continue
		}
		if err != nil {
			return EventBatch{}, err
		}
		return EventBatch{PrunedFloor: floor, Events: page.Events}, nil
	}
}

// eventLowWatermark reads the highest pruned event seq.
func eventLowWatermark(ctx context.Context, conn *sql.DB) (int64, error) {
	var watermark int64
	if err := conn.QueryRowContext(ctx,
		`SELECT seq FROM event_low_watermark WHERE id = 1`,
	).Scan(&watermark); err != nil {
		return 0, fmt.Errorf("reading the event low watermark: %w", err)
	}
	return watermark, nil
}
