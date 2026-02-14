package output

import (
	"fmt"
	"io"
	"os"
)

// Writer handles output for a command, dispatching between JSON and
// human-readable formats based on mode flags.
type Writer struct {
	JSONMode  bool
	QuietMode bool
	Stdout    io.Writer
	Stderr    io.Writer
}

// New creates a Writer configured by the given mode flags.
// Data output goes to os.Stdout; diagnostics go to os.Stderr.
func New(jsonMode, quietMode bool) *Writer {
	return &Writer{
		JSONMode:  jsonMode,
		QuietMode: quietMode,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
	}
}

// Success renders a successful result. In JSON mode the data is wrapped in a
// success envelope written to Stdout. In human mode the message is printed to
// Stdout.
func (w *Writer) Success(data any, message string) {
	if w.JSONMode {
		writeJSONSuccess(w.Stdout, data, message)
		return
	}
	writeHumanSuccess(w.Stdout, message)
}

// Error renders an error. In JSON mode the error is wrapped in an error
// envelope written to Stderr. In human mode the error is printed to Stderr
// with an "Error: " prefix. The corresponding exit code is returned so the
// caller can pass it to os.Exit.
func (w *Writer) Error(err error, code ErrorCode) int {
	if w.JSONMode {
		writeJSONError(w.Stderr, err, code)
	} else {
		writeHumanError(w.Stderr, err)
	}
	return ExitCodeForError(code)
}

// Info writes an informational message to Stderr. In quiet mode or JSON mode,
// Info is a no-op (the JSON envelope on Stdout is the sole structured output).
func (w *Writer) Info(format string, args ...any) {
	if w.QuietMode || w.JSONMode {
		return
	}
	fmt.Fprintf(w.Stderr, format+"\n", args...)
}

// Warn writes a warning to Stderr. Warnings are always emitted in human mode,
// even in quiet mode, but are suppressed in JSON mode (the JSON envelope
// on Stdout is the sole output channel).
func (w *Writer) Warn(format string, args ...any) {
	if w.JSONMode {
		return
	}
	fmt.Fprintf(w.Stderr, "Warning: "+format+"\n", args...)
}
