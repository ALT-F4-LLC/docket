package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// TestNotFound pins notFound's four-way dispatch: db.ErrNotFound (bare or
// wrapped) becomes a NOT_FOUND *CmdError with the caller's label; any other
// error, and nil, both fall through to nil so the caller's own fallback
// (typically a generic ErrGeneral wrap) still applies unchanged.
func TestNotFound(t *testing.T) {
	t.Run("bare ErrNotFound", func(t *testing.T) {
		e := notFound(db.ErrNotFound, "issue DKT-1")
		if e == nil {
			t.Fatal("notFound(db.ErrNotFound, ...) = nil, want a *CmdError")
		}
		cmdErr, ok := e.(*CmdError)
		if !ok {
			t.Fatalf("notFound returned %T, want *CmdError", e)
		}
		if cmdErr.Code != output.ErrNotFound {
			t.Errorf("Code = %q, want %q", cmdErr.Code, output.ErrNotFound)
		}
		if cmdErr.Error() != "issue DKT-1 not found" {
			t.Errorf("Error() = %q, want %q", cmdErr.Error(), "issue DKT-1 not found")
		}
	})

	t.Run("wrapped ErrNotFound unwraps via errors.Is", func(t *testing.T) {
		wrapped := fmt.Errorf("fetching row: %w", db.ErrNotFound)
		e := notFound(wrapped, "doc DKT-2")
		if e == nil {
			t.Fatal("notFound(wrapped ErrNotFound, ...) = nil, want a *CmdError")
		}
		cmdErr, ok := e.(*CmdError)
		if !ok || cmdErr.Code != output.ErrNotFound {
			t.Fatalf("notFound(wrapped) = %#v, want NOT_FOUND *CmdError", e)
		}
	})

	t.Run("other error falls through", func(t *testing.T) {
		other := errors.New("connection reset")
		if e := notFound(other, "issue DKT-3"); e != nil {
			t.Errorf("notFound(other error, ...) = %v, want nil so the caller's own fallback applies", e)
		}
	})

	t.Run("nil error yields nil", func(t *testing.T) {
		if e := notFound(nil, "issue DKT-4"); e != nil {
			t.Errorf("notFound(nil, ...) = %v, want nil", e)
		}
	})
}

// TestIssueArg pins the two outcomes every one of issueArg's 20+ call sites
// depends on: a valid "DKT-N" source parses to its numeric id, and anything
// else reports VALIDATION_ERROR rather than a bare parse error.
func TestIssueArg(t *testing.T) {
	t.Run("valid issue ID", func(t *testing.T) {
		id, err := issueArg("DKT-1")
		if err != nil {
			t.Fatalf("issueArg(%q) error = %v, want nil", "DKT-1", err)
		}
		if id != 1 {
			t.Errorf("issueArg(%q) = %d, want 1", "DKT-1", id)
		}
	})

	t.Run("invalid source reports VALIDATION_ERROR", func(t *testing.T) {
		_, err := issueArg("abc")
		if err == nil {
			t.Fatal("issueArg(\"abc\") error = nil, want a VALIDATION_ERROR")
		}
		cmdErr, ok := err.(*CmdError)
		if !ok {
			t.Fatalf("issueArg(\"abc\") error = %T, want *CmdError", err)
		}
		if cmdErr.Code != output.ErrValidation {
			t.Errorf("Code = %q, want %q", cmdErr.Code, output.ErrValidation)
		}
	})
}

// TestWatchable_NoWatch pins the non---watch arm: run is invoked exactly
// once with the command's normal Writer, and its error propagates unchanged.
// The --watch=true arm delegates to watch.RunWatch, an already-tested poll
// loop; re-driving it here through a fake terminal would be out of
// proportion to what this seam adds over watch's own tests.
func TestWatchable_NoWatch(t *testing.T) {
	conn := newTestDB(t)
	cmd := cmdWithDB(conn)

	calls := 0
	sentinel := errors.New("boom")
	run := func(_ *cobra.Command, _ []string, _ *output.Writer) error {
		calls++
		return sentinel
	}

	err := watchable(cmd, nil, run)
	if calls != 1 {
		t.Errorf("run invoked %d times, want 1", calls)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("watchable error = %v, want the run function's own error to propagate", err)
	}
}

// TestWatchable_Watch pins the --watch=true arm: watchable must read the
// watch/interval/quiet flags itself and hand run the tick's own Writer
// (backed by watch.RunWatch's buffer), not the command's normal getWriter
// one. A cancelled context makes RunWatch return after its first cycle, so
// the call completes promptly with no real sleeping.
//
// This closes a coverage gap: a mutation that scaled the interval by 1000x or
// inverted quietMode previously survived go test and all of qa.sh because
// nothing drove this branch.
func TestWatchable_Watch(t *testing.T) {
	conn := newTestDB(t)
	cmd := cmdWithDB(conn)
	cmd.Flags().Duration("interval", 20*time.Millisecond, "")
	if err := cmd.Flags().Set("watch", "true"); err != nil {
		t.Fatalf("Set(watch, true): %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd.SetContext(ctx)

	const wantCycles = 3
	var calls int
	var gotWriter *output.Writer
	run := func(_ *cobra.Command, _ []string, w *output.Writer) error {
		calls++
		gotWriter = w
		if calls == wantCycles {
			cancel()
		}
		return nil
	}

	start := time.Now()
	if err := watchable(cmd, nil, run); err != nil {
		t.Fatalf("watchable(watch=true) error = %v, want nil", err)
	}
	elapsed := time.Since(start)

	if calls != wantCycles {
		t.Fatalf("run invoked %d times, want %d", calls, wantCycles)
	}
	if gotWriter == nil {
		t.Fatal("run's Writer = nil, want the tick's own Writer")
	}
	if gotWriter.QuietMode {
		t.Errorf("Writer.QuietMode = true, want false (the command's --quiet flag was left at its default)")
	}
	// Two inter-cycle ticks at the configured 20ms interval should complete
	// well under a second; RunWatch.Interval being scaled to seconds (a
	// prior mutant multiplied it 1000x) would make this test time out.
	if elapsed > 2*time.Second {
		t.Errorf("elapsed = %s for %d cycles at a 20ms interval, want well under 2s — RunWatch.Interval looks unhonored", elapsed, wantCycles)
	}
}
