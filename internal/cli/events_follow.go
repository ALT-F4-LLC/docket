package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/daemon"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// `docket events list --follow` reads the event stream (daemon.StreamEvents),
// not `events list` on a ticker. The stream owns the cursor: it sends every
// event after it exactly once, in seq order, and reports a prune that
// overtook it with a marker frame. `--watch` is the polling mode; it re-runs
// the one-shot list on the client interval and never opens the stream.
//
// THE POLL PERIOD IS `--interval`, THE PERSISTENT ROOT FLAG. It defaults to
// 2s with a 500ms floor, both defined once in root.go. Under `--follow` it
// is how often the stream's wake source checks for an append (see
// eventHeadWake) and how long a failed stream waits before it reopens.

// eventStream is a source of stream frames: it sends every event after
// q.Since to send until ctx ends or send fails.
type eventStream func(ctx context.Context, q engine.EventQuery,
	send func(daemon.StreamFrame) error) error

// openEventStream builds the stream `--follow` reads, woken by notify. It is
// a variable so a test can substitute a fake source and observe which mode
// opened it and with which wake source.
//
// The daemon API has no stream procedure, so the CLI runs the stream
// in-process; this is the CLI's one use of internal/daemon's stream.
var openEventStream = func(conn *sql.DB, notify daemon.AppendNotifier) eventStream {
	return func(ctx context.Context, q engine.EventQuery,
		send func(daemon.StreamFrame) error) error {
		return daemon.StreamEvents(ctx, conn, notify, q, send)
	}
}

// eventHeadWake rings a stream's wake channel when the event log grows.
//
// The writers that append events run in other processes, so nothing in this
// process publishes an append notice. Instead it reads the newest event seq
// once per period and rings only when that seq moves, so the stream lists the
// store after an append rather than on every period. A prune records an event
// of its own, so a prune moves the newest seq too. A failed check rings, so
// the stream's own read reports a store that stopped answering.
type eventHeadWake struct {
	conn  *sql.DB
	every time.Duration
}

func (w eventHeadWake) Subscribe() (<-chan struct{}, func()) {
	wake := make(chan struct{}, 1)
	done := make(chan struct{})
	// The baseline is read before Subscribe returns, which StreamEvents does
	// before its first read, so an append that read missed moves the head
	// past the baseline.
	head, err := newestEventSeq(w.conn)
	known := err == nil
	go func() {
		ticker := time.NewTicker(w.every)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
			case <-done:
				return
			}
			next, err := newestEventSeq(w.conn)
			if err != nil || !known || next != head {
				select {
				case wake <- struct{}{}:
				default:
				}
			}
			head, known = next, err == nil
		}
	}()
	var once sync.Once
	return wake, func() { once.Do(func() { close(done) }) }
}

// newestEventSeq reads the highest event seq, 0 for an empty log.
func newestEventSeq(conn *sql.DB) (int64, error) {
	var seq int64
	if err := conn.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("reading the newest event seq: %w", err)
	}
	return seq, nil
}

func runEventsFollow(cmd *cobra.Command) error {
	conn := getDB(cmd)

	since, _ := cmd.Flags().GetInt64("since")
	if since < 0 {
		return cmdErr(
			fmt.Errorf("--since must not be negative: %d", since), output.ErrValidation)
	}
	limit, _ := cmd.Flags().GetInt("limit")
	if err := validateLimit(cmd, limit); err != nil {
		return err
	}
	if limit == 0 {
		limit = eventsDefaultLimit
	}

	tail, err := eventsTailFlag(cmd)
	if err != nil {
		return err
	}
	kind, stepID, err := eventsPredicateFlags(cmd)
	if err != nil {
		return err
	}

	interval, _ := cmd.Flags().GetDuration("interval")
	if interval <= 0 {
		return cmdErr(
			fmt.Errorf("--interval must be a positive duration, got %s", interval),
			output.ErrValidation)
	}

	runRef, _ := cmd.Flags().GetString("run")
	runID, err := engine.ResolveRunFilter(conn, runRef)
	if err != nil {
		return runErr(err)
	}

	jsonMode, jsonVersion := jsonModeOf(cmd)
	quietMode, _ := cmd.Flags().GetBool("quiet")

	// Ctrl-C is how a follow ends, so a signal cancels the context and the
	// follow returns nil: exit 0, not a failure.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := followEvents(ctx, followOptions{
		conn:       conn,
		stream:     openEventStream(conn, eventHeadWake{conn: conn, every: interval}),
		retryEvery: interval,
		query: engine.EventQuery{
			Since: since, RunID: runID, Limit: limit, Tail: tail,
			Kind: kind, StepID: stepID,
			ProjectID: eventsProjectScope(cmd),
		},
		jsonMode:    jsonMode,
		jsonVersion: jsonVersion,
		quietMode:   quietMode,
		stdout:      os.Stdout,
		stderr:      os.Stderr,
	}); err != nil {
		return runErr(err)
	}
	return nil
}

// followOptions is one follow's inputs. The loop takes this rather than a
// *cobra.Command so tests can drive it directly with a chosen stream.
type followOptions struct {
	// conn serves the one opening read that resolves `--tail`.
	conn   *sql.DB
	stream eventStream
	// retryEvery is the wait before a failed stream is reopened.
	retryEvery  time.Duration
	query       engine.EventQuery
	jsonMode    bool
	jsonVersion output.JSONVersion
	quietMode   bool
	stdout      io.Writer
	stderr      io.Writer
}

// followMaxConsecutiveErrors is how many stream failures in a row end a
// follow, the ceiling `--watch` applies to its polls. A locked database or a
// transient I/O error is worth reopening the stream from the cursor; a store
// that keeps failing is not.
const followMaxConsecutiveErrors = 3

// followEvents prints each event the stream sends, one envelope per event,
// until ctx ends (nil), the stream reports that a prune overtook the cursor
// (GONE), or the stream fails followMaxConsecutiveErrors times with no event
// delivered between failures. Each failure is printed to stderr and the
// stream reopens from the cursor after opts.retryEvery.
func followEvents(ctx context.Context, opts followOptions) error {
	query := opts.query
	cursor, err := followStartCursor(opts.conn, query)
	if err != nil {
		return err
	}
	query.Since, query.Tail = cursor, 0

	w := &output.Writer{
		JSONMode:    opts.jsonMode,
		JSONVersion: output.NormalizeJSONVersion(opts.jsonMode, opts.jsonVersion),
		QuietMode:   opts.quietMode,
		Stdout:      opts.stdout,
		Stderr:      opts.stderr,
	}
	// A follow with no project scope is the cross-project feed, so it carries
	// the project column for the same reason `--all-projects` does.
	withProject := opts.query.ProjectID == 0

	failures := 0
	send := func(f daemon.StreamFrame) error {
		// A prune overtook the cursor. The stream would resume at the floor,
		// which is the silent skip the feed contract forbids, so the follow
		// ends with GONE and names the seq to resume from.
		if f.PrunedFloor != 0 {
			return &engine.Error{Code: engine.CodeGone, Message: fmt.Sprintf(
				"events at seq > %d are below the retained minimum (%d): they no longer "+
					"exist and cannot be replayed; resume from seq %d, accepting the gap",
				cursor, f.PrunedFloor, f.PrunedFloor-1)}
		}
		if f.Event == nil {
			return nil
		}
		cursor = f.Event.Seq
		failures = 0
		events := []engine.Event{*f.Event}
		var message string
		if !w.JSONMode {
			message = renderEventList(events, withProject)
		}
		w.Success(eventsListResult{Events: events, Total: len(events)}, message)
		return nil
	}

	for {
		err := opts.stream(ctx, query, send)
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return nil
		}
		if code, ok := engine.CodeOf(err); err == nil || (ok && code == engine.CodeGone) {
			return err
		}
		failures++
		fmt.Fprintf(opts.stderr, "Error: %v\n", err)
		if failures >= followMaxConsecutiveErrors {
			return fmt.Errorf("follow: %d consecutive errors, last: %w", failures, err)
		}
		query.Since = cursor
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(opts.retryEvery):
		}
	}
}

// followStartCursor resolves where the stream starts. `--tail N` selects the
// newest N once, and the stream starts just below the oldest of them so it
// delivers those N and then everything after; the stream itself has no
// tail. An empty feed leaves the cursor at Since.
func followStartCursor(conn *sql.DB, q engine.EventQuery) (int64, error) {
	if q.Tail == 0 {
		return q.Since, nil
	}
	q.Since = 0
	page, err := engine.ListEvents(conn, q)
	if err != nil {
		return 0, err
	}
	if len(page.Events) == 0 {
		return 0, nil
	}
	return page.Events[0].Seq - 1, nil
}
