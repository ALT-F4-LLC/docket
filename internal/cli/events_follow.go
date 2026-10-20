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
// is the stream's wake period (see intervalWake).

// eventStream is a source of stream frames: it sends every event after
// q.Since to send until ctx ends or send fails.
type eventStream func(ctx context.Context, q engine.EventQuery,
	send func(daemon.StreamFrame) error) error

// openEventStream builds the stream `--follow` reads. It is a variable so a
// test can substitute a fake source and observe which mode opened it.
var openEventStream = func(conn *sql.DB, wakeEvery time.Duration) eventStream {
	return func(ctx context.Context, q engine.EventQuery,
		send func(daemon.StreamFrame) error) error {
		return daemon.StreamEvents(ctx, conn, intervalWake(wakeEvery), q, send)
	}
}

// intervalWake rings a stream's wake channel once per period.
//
// The writers that append events run in other processes, so nothing in this
// process publishes an append notice. The stream re-reads the store from its
// own cursor on each ring, and a ring with nothing new prints nothing.
type intervalWake time.Duration

func (d intervalWake) Subscribe() (<-chan struct{}, func()) {
	wake := make(chan struct{}, 1)
	done := make(chan struct{})
	ticker := time.NewTicker(time.Duration(d))
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				select {
				case wake <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return wake, func() { once.Do(func() { close(done) }) }
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
		conn:   conn,
		stream: openEventStream(conn, interval),
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
	conn        *sql.DB
	stream      eventStream
	query       engine.EventQuery
	jsonMode    bool
	jsonVersion output.JSONVersion
	quietMode   bool
	stdout      io.Writer
	stderr      io.Writer
}

// followEvents prints each event the stream sends, one envelope per event,
// until ctx ends (nil) or the stream reports that a prune overtook the
// cursor (GONE).
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

	err = opts.stream(ctx, query, func(f daemon.StreamFrame) error {
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
		events := []engine.Event{*f.Event}
		var message string
		if !w.JSONMode {
			message = renderEventList(events, withProject)
		}
		w.Success(eventsListResult{Events: events, Total: len(events)}, message)
		return nil
	})
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
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
