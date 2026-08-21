// Package engine implements run activation — the fat transaction of
// engine-core §3.2 and engine-spec §2 (TDD docs/tdd/engine-spine.md §5.3).
//
// The package carries no agent, model, or LLM vocabulary of any kind
// (docs/design/genericity.md). It binds workflows to issues by their declared
// `[match]` predicates, pins what a run must reproduce, freezes the issue state
// a context bundle will read, harvests the commands an operator approved, and
// expands the step topology. Every one of those is scheduling, dependency, or
// packaging work; none of them reads a key inside `params` or `metadata`, and
// `executor` stays the opaque string §11.1 makes it.
package engine

import (
	"errors"
	"fmt"
)

// ErrorCode classifies an activation failure so the CLI can map it onto the
// error taxonomy (TDD §5.5) without re-deriving the mapping from the message.
type ErrorCode string

const (
	// CodeValidation is VALIDATION_ERROR (exit 3): a binding that resolves to
	// zero or several workflows, a work-DAG cycle, a run with no issues, or a
	// context bundle over the configured cap.
	CodeValidation ErrorCode = "VALIDATION_ERROR"
	// CodeNotFound is NOT_FOUND (exit 2): a run that does not exist, or a
	// `--pin` path that is missing or is not a regular file.
	CodeNotFound ErrorCode = "NOT_FOUND"
	// CodeConflict is CONFLICT (exit 4): re-activating a terminal run (RA5).
	CodeConflict ErrorCode = "CONFLICT"
	// CodeGone is GONE: an `events list --since` cursor below the retained
	// minimum (docs/tdd/runs-dispatch.md §8.6). No product code path reaches it
	// at this stage — nothing prunes until S7 — and the SHAPE ships here because
	// `--since` is the verb that must return it.
	CodeGone ErrorCode = "GONE"
)

// Error is an activation failure carrying its taxonomy code.
type Error struct {
	Code    ErrorCode
	Message string
	// Err is the underlying cause when there is one, so errors.Is still
	// reaches a db sentinel through this wrapper.
	Err error
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Err }

func validationErr(format string, args ...any) *Error {
	return &Error{Code: CodeValidation, Message: fmt.Sprintf(format, args...)}
}

func notFoundErr(err error, format string, args ...any) *Error {
	return &Error{Code: CodeNotFound, Message: fmt.Sprintf(format, args...), Err: err}
}

func conflictErr(format string, args ...any) *Error {
	return &Error{Code: CodeConflict, Message: fmt.Sprintf(format, args...)}
}

func goneErr(format string, args ...any) *Error {
	return &Error{Code: CodeGone, Message: fmt.Sprintf(format, args...)}
}

// CodeOf reports the taxonomy code of an error raised by this package, and
// whether it was one.
func CodeOf(err error) (ErrorCode, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, true
	}
	return "", false
}
