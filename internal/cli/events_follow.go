package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/watch"
	"github.com/spf13/cobra"
)

// `docket events list --follow` — the polling subscription
// (docs/tdd/events-follow.md §4).
//
// THERE IS NO DAEMON (engine-spec §7, engine-core §10). Nothing is registered,
// nothing is notified, and no process outlives the command. `--follow` is
// `events list` on a ticker, with a cursor that advances between cycles — which
// is the entire difference between it and the verb it extends.
//
// IT IS BUILT ON internal/watch.RunWatch RATHER THAN ON A NEW LOOP. That helper
// already does every non-cursor thing this needs — ticker, a fresh
// output.Writer per cycle over a shared buffer, context cancellation, the
// consecutive-error ceiling — and `docket next --watch` already wires it in the
// shape this verb wants (internal/cli/next.go). Rebuilding it here would be a
// second loop to keep in agreement with the first.

// THE POLL PERIOD IS `--interval`, THE PERSISTENT ROOT FLAG (W2). It defaults
// to 2s and has a 500ms floor, both defined once in root.go and shared with
// `--watch`. This file deliberately declares no interval flag of its own: a
// second one would shadow the global with its own default and bypass its
// minimum, which is two flags of one name disagreeing about what they mean.

// runEventsFollow is W1: the same ListEvents call, once per tick, with the
// cursor carried between cycles.
//
// THE CURSOR ADVANCES TO THE LAST SEQ ACTUALLY RETURNED (W3) — never to
// MAX(seq), never to a clock. That is what makes the cross-cycle property hold:
// an event inserted while a cycle was reading lands above the cursor the cycle
// set, so the next cycle returns it. Advancing to anything the read did not
// itself produce would open exactly the gap `--since` was designed to close.
//
// A cycle that returns nothing leaves the cursor alone, so a quiet feed does not
// drift forward past events that have not been written yet.
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

	// Ctrl-C ends a follow (W6). Interrupting is how this verb terminates
	// normally, so the signal cancels the context and RunWatch returns nil —
	// exit 0, not a failure.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := followEvents(ctx, followOptions{
		conn: conn,
		query: engine.EventQuery{
			Since: since, RunID: runID, Limit: limit, Tail: tail,
			ProjectID: eventsProjectScope(cmd),
		},
		interval:    interval,
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

// followOptions is one follow's inputs.
//
// The loop takes this rather than a *cobra.Command so the property tests can
// drive it directly — W3's cursor discipline and W8's termination are behaviors
// of the LOOP, and a test that had to spawn a process to observe them would be
// testing the process instead.
type followOptions struct {
	conn        *sql.DB
	query       engine.EventQuery
	interval    time.Duration
	jsonMode    bool
	jsonVersion output.JSONVersion
	quietMode   bool
	stdout      io.Writer
	stderr      io.Writer
}

// followEvents is the loop itself: §4's clauses, one per comment below.
func followEvents(ctx context.Context, opts followOptions) error {
	// cursor is the follow's state. It lives here rather than inside the
	// per-cycle closure because it is the ONE thing that must survive a cycle.
	cursor := opts.query.Since

	// tail is the STARTING CURSOR, and it applies to the FIRST CYCLE ONLY
	// (DKT-752).
	//
	// `--tail N` means "start me at the newest N". Before this, the follow
	// dropped the flag entirely and opened at seq 0, so a watcher arming
	// `--follow --tail 20` received the whole retained history — thousands of
	// rows on a busy store — before reaching anything live.
	//
	// It cannot survive past the first cycle: `Tail` is a SELECTION over the
	// whole feed, not a window above a cursor, so a second tailed read would
	// re-print the same newest N every interval and never advance. So the first
	// cycle selects the newest N, the cursor advances over what it returned by
	// the ordinary W3 rule, and every cycle after it is a plain `--since` read.
	//
	// A first cycle that returns NOTHING leaves the cursor at Since (0 — `--tail`
	// and `--since` are mutually exclusive), which is correct: an empty feed has
	// nothing to skip past, and whatever is written next lands above 0.
	tail := opts.query.Tail

	// fatal carries a TERMINAL condition out of the loop (W8).
	//
	// It exists because RunWatch tolerates two errors before giving up — the
	// right policy for a transient failure, and the wrong one for GONE. A cursor
	// below the retained minimum is PERMANENT: polling it twice more produces
	// the same refusal twice more, and prints it to stderr each time. So the
	// cycle records it, cancels the loop, and this function returns it — the
	// follow stops on the FIRST one, with one message.
	var fatal error

	// followCtx is cancelled either by a signal (W6) or by the fatal path above.
	followCtx, endFollow := context.WithCancel(ctx)
	defer endFollow()

	err := watch.RunWatch(followCtx, watch.Options{
		Interval:    opts.interval,
		JSONMode:    opts.jsonMode,
		JSONVersion: opts.jsonVersion,
		QuietMode:   opts.quietMode,
		// W4: IsTTY IS FALSE UNCONDITIONALLY, and that is the deliberate
		// departure from `next --watch`.
		//
		// RunWatch clears the screen between cycles only when IsTTY && !JSONMode.
		// `next --watch` wants that: it shows a CURRENT STATE — the ready set —
		// and replacing it is right. A feed shows a SEQUENCE, and a sequence that
		// erased itself every two seconds could not be piped, grepped, or read
		// after the fact. Passing false makes the output append-only, which is
		// what a log is.
		IsTTY:  false,
		Stdout: opts.stdout,
		Stderr: opts.stderr,
	}, func(ctx context.Context, w *output.Writer) error {
		query := opts.query
		query.Since = cursor
		query.Tail = tail
		// The first tailed read is a selection over the whole feed, so it starts
		// from no cursor; ListEvents ignores Limit in that mode and the window is
		// N itself.
		if tail > 0 {
			query.Since = 0
		}
		page, err := engine.ListEvents(opts.conn, query)
		if err != nil {
			// GONE is terminal; anything else is transient and gets RunWatch's
			// ordinary tolerance. A locked database or a transient I/O error is
			// worth one more poll — a pruned-away cursor never is.
			if code, ok := engine.CodeOf(err); ok && code == engine.CodeGone {
				fatal = err
				endFollow()
				return nil
			}
			return err
		}

		// W3: advance only over what this read produced.
		if n := len(page.Events); n > 0 {
			cursor = page.Events[n-1].Seq
		}

		// The window that actually bounded this page: N under the opening tailed
		// read, `--limit` for every cycle after it. It is captured BEFORE `tail`
		// is spent so `truncated` describes the bound that applied.
		//
		// `tail` is cleared only after a SUCCESSFUL read: a cycle that failed
		// transiently returned above without reaching here, so the retry is still
		// the opening read and still starts at the newest N.
		effectiveLimit := opts.query.Limit
		if tail > 0 {
			effectiveLimit = tail
			tail = 0
		}

		// A quiet cycle prints NOTHING — not an empty array, not "No events."
		// A follower's output must be the events and only the events, or a
		// consumer piping it would receive a page of nothing every interval
		// forever. `events list` prints "No events." because a one-shot answer
		// that printed nothing would look like a failure; a follow that printed
		// it every cycle would look like a fault.
		if len(page.Events) == 0 {
			return nil
		}

		result := eventsListResult{
			Events: page.Events, Total: page.Total, limit: effectiveLimit}
		var message string
		if !w.JSONMode {
			// A follow with no project scope is the cross-project feed, so it
			// carries the project column for the same reason `--all-projects`
			// does: without it the rows say nothing about whose work they are.
			message = renderEventList(page.Events, opts.query.ProjectID == 0)
		}
		w.Success(result, message)
		return nil
	})

	// W8: GONE mid-follow TERMINATES THE FOLLOW.
	//
	// The alternative — resetting the cursor to the new retained minimum and
	// carrying on — is precisely the silent skip engine-spec §3 forbids
	// ("returns GONE rather than silently skipping"). A follower that resumed
	// would print a feed with a hole in it and no indication there was one. It
	// exits with the code, and the message names the seq to resume from, so an
	// operator restarts the follow having been told what it cost.
	//
	// The caller maps both through runErr, so the exit code is the same 9
	// `events list` returns for the same condition — one contract, two entry
	// points.
	if fatal != nil {
		return fatal
	}
	return err
}
