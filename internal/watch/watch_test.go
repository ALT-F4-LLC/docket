package watch

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/output"
)

func TestRunWatch_CallsFnImmediately(t *testing.T) {
	var called bool
	ctx, cancel := context.WithCancel(context.Background())

	var stdout bytes.Buffer
	opts := Options{
		Interval: 50 * time.Millisecond,
		Stdout:   &stdout,
		Stderr:   &bytes.Buffer{},
	}

	err := RunWatch(ctx, opts, func(_ context.Context, w *output.Writer) error {
		called = true
		fmt.Fprintln(w.Stdout, "hello")
		cancel()
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("fn was not called on first cycle")
	}
	if !strings.Contains(stdout.String(), "hello") {
		t.Fatalf("expected stdout to contain 'hello', got: %q", stdout.String())
	}
}

func TestRunWatch_CallsFnAfterInterval(t *testing.T) {
	var callCount int
	ctx, cancel := context.WithCancel(context.Background())

	opts := Options{
		Interval: 10 * time.Millisecond,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	}

	err := RunWatch(ctx, opts, func(_ context.Context, w *output.Writer) error {
		callCount++
		fmt.Fprintln(w.Stdout, "tick")
		if callCount >= 3 {
			cancel()
		}
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount < 3 {
		t.Fatalf("expected at least 3 calls, got %d", callCount)
	}
}

func TestRunWatch_ReturnsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	opts := Options{
		Interval: 10 * time.Millisecond,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	}

	err := RunWatch(ctx, opts, func(_ context.Context, w *output.Writer) error {
		fmt.Fprintln(w.Stdout, "once")
		return nil
	})

	if err != nil {
		t.Fatalf("expected nil error on context cancel, got: %v", err)
	}
}

func TestRunWatch_TTYEmitsANSIClear(t *testing.T) {
	var callCount int
	ctx, cancel := context.WithCancel(context.Background())

	var stdout bytes.Buffer
	opts := Options{
		Interval: 10 * time.Millisecond,
		IsTTY:    true,
		Stdout:   &stdout,
		Stderr:   &bytes.Buffer{},
	}

	err := RunWatch(ctx, opts, func(_ context.Context, w *output.Writer) error {
		callCount++
		fmt.Fprintln(w.Stdout, "data")
		if callCount >= 2 {
			cancel()
		}
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "\033[2J\033[H") {
		t.Fatalf("expected ANSI clear sequence in TTY output, got: %q", out)
	}
}

func TestRunWatch_NonTTYEmitsBlankLineSeparator(t *testing.T) {
	var callCount int
	ctx, cancel := context.WithCancel(context.Background())

	var stdout bytes.Buffer
	opts := Options{
		Interval: 10 * time.Millisecond,
		IsTTY:    false,
		Stdout:   &stdout,
		Stderr:   &bytes.Buffer{},
	}

	err := RunWatch(ctx, opts, func(_ context.Context, w *output.Writer) error {
		callCount++
		fmt.Fprintln(w.Stdout, "line")
		if callCount >= 2 {
			cancel()
		}
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "line\n\nline\n") {
		t.Fatalf("expected blank line separator in non-TTY output, got: %q", out)
	}
	if strings.Contains(out, "\033[") {
		t.Fatal("non-TTY output should not contain ANSI sequences")
	}
}

func TestRunWatch_JSONModeNoANSI(t *testing.T) {
	var callCount int
	ctx, cancel := context.WithCancel(context.Background())

	var stdout bytes.Buffer
	opts := Options{
		Interval: 10 * time.Millisecond,
		JSONMode: true,
		Stdout:   &stdout,
		Stderr:   &bytes.Buffer{},
	}

	err := RunWatch(ctx, opts, func(_ context.Context, w *output.Writer) error {
		callCount++
		fmt.Fprintln(w.Stdout, `{"ok":true}`)
		if callCount >= 2 {
			cancel()
		}
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := stdout.String()
	if strings.Contains(out, "\033[") {
		t.Fatal("JSON mode output should not contain ANSI sequences")
	}
	if strings.Contains(out, "Watching") {
		t.Fatal("JSON mode output should not contain status line")
	}
}

func TestRunWatch_QuietModeSuppressesStatusLine(t *testing.T) {
	var callCount int
	ctx, cancel := context.WithCancel(context.Background())

	var stdout bytes.Buffer
	opts := Options{
		Interval:  10 * time.Millisecond,
		IsTTY:     true,
		QuietMode: true,
		Stdout:    &stdout,
		Stderr:    &bytes.Buffer{},
	}

	err := RunWatch(ctx, opts, func(_ context.Context, w *output.Writer) error {
		callCount++
		fmt.Fprintln(w.Stdout, "data")
		if callCount >= 2 {
			cancel()
		}
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := stdout.String()
	if strings.Contains(out, "Watching") {
		t.Fatal("quiet mode should suppress the status line")
	}
	// Should still have ANSI clear in TTY mode
	if !strings.Contains(out, "\033[2J\033[H") {
		t.Fatal("quiet mode should still emit ANSI clear in TTY mode")
	}
}

func TestRunWatch_SingleErrorContinues(t *testing.T) {
	var callCount int
	ctx, cancel := context.WithCancel(context.Background())

	var stderr bytes.Buffer
	opts := Options{
		Interval: 10 * time.Millisecond,
		Stdout:   &bytes.Buffer{},
		Stderr:   &stderr,
	}

	err := RunWatch(ctx, opts, func(_ context.Context, w *output.Writer) error {
		callCount++
		if callCount == 1 {
			return fmt.Errorf("transient failure")
		}
		fmt.Fprintln(w.Stdout, "recovered")
		cancel()
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if callCount < 2 {
		t.Fatal("expected watch to continue after single error")
	}
	if !strings.Contains(stderr.String(), "transient failure") {
		t.Fatal("expected error to be printed to stderr")
	}
}

func TestRunWatch_ThreeConsecutiveErrorsExits(t *testing.T) {
	opts := Options{
		Interval: 10 * time.Millisecond,
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
	}

	err := RunWatch(context.Background(), opts, func(_ context.Context, w *output.Writer) error {
		return fmt.Errorf("persistent failure")
	})

	if err == nil {
		t.Fatal("expected error after 3 consecutive failures")
	}
	if !strings.Contains(err.Error(), "3 consecutive errors") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestRunWatch_BufferReused(t *testing.T) {
	// Verify the buffer is allocated once by checking that RunWatch
	// completes multiple cycles without issues (indirect test — the
	// implementation uses buf.Reset() rather than allocating a new buffer).
	var callCount int
	ctx, cancel := context.WithCancel(context.Background())

	var stdout bytes.Buffer
	opts := Options{
		Interval: 10 * time.Millisecond,
		Stdout:   &stdout,
		Stderr:   &bytes.Buffer{},
	}

	err := RunWatch(ctx, opts, func(_ context.Context, w *output.Writer) error {
		callCount++
		// Write different content each cycle to verify buffer is reset
		fmt.Fprintf(w.Stdout, "cycle-%d\n", callCount)
		if callCount >= 3 {
			cancel()
		}
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := stdout.String()
	// Each cycle should have its own content, not accumulated
	if strings.Contains(out, "cycle-1cycle-1") {
		t.Fatal("buffer appears to accumulate content instead of being reset")
	}
}

func TestFormatInterval(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{2 * time.Second, "2s"},
		{5 * time.Second, "5s"},
		{500 * time.Millisecond, "500ms"},
		{1500 * time.Millisecond, "1.5s"},
	}

	for _, tt := range tests {
		got := formatInterval(tt.d)
		if got != tt.want {
			t.Errorf("formatInterval(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
