package output

import (
	"encoding/json"
	"io"
)

// ErrorCode represents a machine-readable error classification.
type ErrorCode string

// Error code constants.
const (
	ErrGeneral    ErrorCode = "GENERAL_ERROR"
	ErrNotFound   ErrorCode = "NOT_FOUND"
	ErrValidation ErrorCode = "VALIDATION_ERROR"
	ErrConflict   ErrorCode = "CONFLICT"
)

// Exit code constants.
const (
	ExitSuccess    = 0
	ExitGeneral    = 1
	ExitNotFound   = 2
	ExitValidation = 3
	ExitConflict   = 4
)

// ExitCodeForError maps an ErrorCode to its corresponding exit code.
func ExitCodeForError(code ErrorCode) int {
	switch code {
	case ErrNotFound:
		return ExitNotFound
	case ErrValidation:
		return ExitValidation
	case ErrConflict:
		return ExitConflict
	default:
		return ExitGeneral
	}
}

// successEnvelope is the JSON structure for successful responses.
type successEnvelope struct {
	OK      bool        `json:"ok"`
	Data    any `json:"data"`
	Message string      `json:"message,omitempty"`
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
