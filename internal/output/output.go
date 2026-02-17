package output

import (
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/render"
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
// envelope written to Stdout. In human mode the error is printed to Stderr
// with an "Error: " prefix. The corresponding exit code is returned so the
// caller can pass it to os.Exit.
func (w *Writer) Error(err error, code ErrorCode) int {
	if w.JSONMode {
		writeJSONError(w.Stdout, err, code)
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
	msg := fmt.Sprintf(format, args...)
	if render.ColorsEnabled() {
		icon := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render("\u2139")
		text := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(msg)
		fmt.Fprintf(w.Stderr, "%s %s\n", icon, text)
	} else {
		fmt.Fprintln(w.Stderr, msg)
	}
}

// Warn writes a warning to Stderr. Warnings are always emitted in human mode,
// even in quiet mode, but are suppressed in JSON mode (the JSON envelope
// on Stdout is the sole output channel).
func (w *Writer) Warn(format string, args ...any) {
	if w.JSONMode {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if render.ColorsEnabled() {
		icon := lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true).Render("\u26a0")
		label := lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true).Render("Warning:")
		fmt.Fprintf(w.Stderr, "%s %s %s\n", icon, label, msg)
	} else {
		fmt.Fprintf(w.Stderr, "Warning: %s\n", msg)
	}
}
