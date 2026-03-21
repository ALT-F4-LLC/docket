package watch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/output"
)

// Options configures the watch loop behavior.
type Options struct {
	Interval  time.Duration
	JSONMode  bool
	QuietMode bool
	IsTTY     bool
	Stdout    io.Writer
	Stderr    io.Writer
}

// RunWatch executes fn repeatedly on the given interval until ctx is cancelled.
// It handles terminal clearing, output buffering, and newline separation.
// On each cycle, RunWatch creates a fresh output.Writer backed by a shared
// bytes.Buffer and passes it to fn. After fn returns, RunWatch clears the
// screen (if TTY) and flushes the buffer to real stdout atomically.
func RunWatch(ctx context.Context, opts Options, fn func(ctx context.Context, w *output.Writer) error) error {
	var buf bytes.Buffer
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()

	var consecutiveErrors int
	const maxConsecutiveErrors = 3

	for cycle := 0; ; cycle++ {
		buf.Reset()

		w := &output.Writer{
			JSONMode:  opts.JSONMode,
			QuietMode: opts.QuietMode,
			Stdout:    &buf,
			Stderr:    opts.Stderr,
		}

		if err := fn(ctx, w); err != nil {
			consecutiveErrors++
			fmt.Fprintf(opts.Stderr, "Error: %v\n", err)
			if consecutiveErrors >= maxConsecutiveErrors {
				return fmt.Errorf("watch: %d consecutive errors, last: %w", maxConsecutiveErrors, err)
			}
		} else {
			consecutiveErrors = 0
		}

		if cycle == 0 {
			// First cycle: write directly to stdout.
			buf.WriteTo(opts.Stdout)
		} else {
			if opts.JSONMode {
				// JSON mode: flush buffer directly (NDJSON).
				buf.WriteTo(opts.Stdout)
			} else if opts.IsTTY {
				// Human + TTY: ANSI clear, optional status line, flush.
				fmt.Fprint(opts.Stdout, "\033[2J\033[H")
				if !opts.QuietMode {
					fmt.Fprintf(opts.Stdout, "Watching every %s... (last update: %s) [Ctrl+C to exit]\n\n",
						formatInterval(opts.Interval),
						time.Now().Format("15:04:05"),
					)
				}
				buf.WriteTo(opts.Stdout)
			} else {
				// Human + non-TTY: blank line separator, flush.
				fmt.Fprintln(opts.Stdout)
				buf.WriteTo(opts.Stdout)
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// formatInterval formats a duration for the status line.
func formatInterval(d time.Duration) string {
	if d >= time.Second && d%time.Second == 0 {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return d.String()
}
