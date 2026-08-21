package output

import (
	"encoding/json"
	"io"
)

// ErrorCode represents a machine-readable error classification.
type ErrorCode string

// Error code constants.
//
// The taxonomy is extended once, here, for every verb the workflow engine will
// add (engine-spec.md §5). The first four codes and their exit numbers predate
// that extension and are never renumbered — exit codes are the most widely
// depended-upon part of a CLI contract. New codes only ever append.
//
// AUTH_ERROR, STALE_LEASE, TIMEOUT, and UNTRUSTED have no emitter yet; they are
// declared now so the taxonomy is frozen before the verbs that raise them exist.
//
// GONE arrives at stage 6 (docs/tdd/runs-dispatch.md §8.6) for `events list
// --since` below the retained minimum, and it APPENDS at exit 9 rather than
// taking the exit 6 that TDD's E14 names. E14's number was already spent:
// STALE_LEASE has held exit 6 since the reliability delta and EMITS TODAY
// (`issue heartbeat`, `token`), so the two codes would be indistinguishable to
// the one consumer an exit code exists for — a script testing `$?`, as opposed
// to one reading `.code` from the envelope. The rule stated three lines above
// is the one that decides it: "New codes only ever append."
//
// A RECORDED AMENDMENT notes the divergence rather than resolving it silently.
const (
	ErrGeneral    ErrorCode = "GENERAL_ERROR"
	ErrNotFound   ErrorCode = "NOT_FOUND"
	ErrValidation ErrorCode = "VALIDATION_ERROR"
	ErrConflict   ErrorCode = "CONFLICT"
	ErrAuth       ErrorCode = "AUTH_ERROR"
	ErrStaleLease ErrorCode = "STALE_LEASE"
	ErrTimeout    ErrorCode = "TIMEOUT"
	ErrUntrusted  ErrorCode = "UNTRUSTED"
	// ErrGone means a cursor names events below the retained minimum: they no
	// longer exist, and the read RETURNS THIS RATHER THAN SILENTLY SKIPPING
	// (engine-spec §3). A consumer that received a short list instead would
	// believe it had caught up.
	ErrGone ErrorCode = "GONE"
)

// Exit code constants. 0-4 are the pre-existing contract and are fixed.
const (
	ExitSuccess    = 0
	ExitGeneral    = 1
	ExitNotFound   = 2
	ExitValidation = 3
	ExitConflict   = 4
	ExitAuth       = 5
	ExitStaleLease = 6
	ExitTimeout    = 7
	ExitUntrusted  = 8
	ExitGone       = 9
)

// ExitCodeForError maps an ErrorCode to its corresponding exit code.
// Unknown codes map to ExitGeneral.
func ExitCodeForError(code ErrorCode) int {
	switch code {
	case ErrNotFound:
		return ExitNotFound
	case ErrValidation:
		return ExitValidation
	case ErrConflict:
		return ExitConflict
	case ErrAuth:
		return ExitAuth
	case ErrStaleLease:
		return ExitStaleLease
	case ErrTimeout:
		return ExitTimeout
	case ErrUntrusted:
		return ExitUntrusted
	case ErrGone:
		return ExitGone
	default:
		return ExitGeneral
	}
}

// successEnvelope is the JSON structure for successful responses.
type successEnvelope struct {
	OK      bool   `json:"ok"`
	Data    any    `json:"data"`
	Message string `json:"message,omitempty"`
}

// errorEnvelope is the JSON structure for error responses.
type errorEnvelope struct {
	OK    bool      `json:"ok"`
	Error string    `json:"error"`
	Code  ErrorCode `json:"code"`
}

// writeJSONSuccess writes a success envelope to w.
func writeJSONSuccess(w io.Writer, data any, message string) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(successEnvelope{
		OK:      true,
		Data:    data,
		Message: message,
	})
}

// writeJSONError writes an error envelope to w.
func writeJSONError(w io.Writer, err error, code ErrorCode) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(errorEnvelope{
		OK:    false,
		Error: err.Error(),
		Code:  code,
	})
}
