package exec

import (
	"sync"
	"unicode/utf8"
)

// CaptureCap is §5.5 C2's bound: 256 KiB.
//
// A PACKAGE CONSTANT, not config. It is NOT the context bundle's error-bytes
// knob — that governs a different thing with a different consumer, and reusing
// it would couple a security bound to a workflow-authoring setting.
const CaptureCap = 256 * 1024

// captureWriter collects a child's output into one interleaved stream, bounded
// at the cap (§5.5).
//
// C1: stdout and stderr are captured into ONE INTERLEAVED STREAM, in write
// order, which is what an operator reading a failed check wants — an error
// message and the line that produced it belong next to each other. §11.4 has a
// single `output` field, so this is the spec's shape rather than a choice.
//
// C2: reaching the cap sets truncated and the writer STOPS CONSUMING. It does
// not keep reading and discarding: a check that emits forever must not be able
// to spin the reader, which is the difference between a bounded capture and a
// bounded-memory busy loop.
type captureWriter struct {
	mu sync.Mutex
	// buf holds at most CaptureCap bytes.
	buf []byte
	// truncated records that at least one byte was dropped.
	truncated bool
}

// Write implements io.Writer. Both the stdout and stderr pipes point at the
// SAME captureWriter, which is what interleaves them; the mutex is what makes
// that safe, since the two pipes are drained by two goroutines.
func (w *captureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	remaining := CaptureCap - len(w.buf)
	if remaining <= 0 {
		// Already full. Report the write as accepted so the child is not
		// handed an I/O error — a check that dies from EPIPE on its own
		// verbose output would turn a truncation into a spurious failure. The
		// bytes are dropped and `truncated` is already set.
		w.truncated = true
		return len(p), nil
	}

	if len(p) > remaining {
		// C3: a truncated capture keeps the FIRST cap bytes, not the last. The
		// first bytes of a failing check contain the invocation and the first
		// error; the last contain a summary that is usually reproducible from
		// the first. Stated because the opposite is a defensible choice, and a
		// future editor should have to argue against a recorded reason.
		w.buf = append(w.buf, p[:remaining]...)
		w.truncated = true
		return len(p), nil
	}

	w.buf = append(w.buf, p...)
	return len(p), nil
}

// result returns the captured output and whether it was truncated.
//
// C4: truncation is byte-exact and then BACKED OFF TO THE LAST VALID RUNE
// BOUNDARY, so a multi-byte character split by the cap does not become an
// invalid UTF-8 sequence — which would, downstream, produce invalid JSON in a
// run report.
func (w *captureWriter) result() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	out := w.buf
	if w.truncated {
		out = trimToRuneBoundary(out)
	}
	return string(out), w.truncated
}

// trimToRuneBoundary drops a trailing partial UTF-8 sequence.
//
// It walks back at most 3 bytes: a UTF-8 sequence is at most 4 bytes, so a
// valid prefix is never more than 3 bytes behind the end. Walking further would
// risk trimming legitimate content on input that is not UTF-8 at all — binary
// output from a check is captured as-is, and mangling it beyond the boundary
// fix would be this function exceeding its job.
func trimToRuneBoundary(b []byte) []byte {
	for i := 0; i < utf8.UTFMax && i < len(b); i++ {
		candidate := b[:len(b)-i]
		if r, size := utf8.DecodeLastRune(candidate); r != utf8.RuneError || size > 1 {
			return candidate
		}
	}
	if len(b) < utf8.UTFMax {
		return b
	}
	return b[:len(b)-utf8.UTFMax+1]
}
