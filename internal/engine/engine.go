// Package engine implements run activation as one transaction.
//
// The package carries no agent, model, or LLM vocabulary of any kind. It binds
// workflows to issues by their declared `[match]` predicates, pins what a run
// must reproduce, freezes the issue state a context bundle will read, harvests
// the commands an operator approved, and expands the step topology. Every one
// of those is scheduling, dependency, or packaging work; none of them reads a
// key inside `params` or `metadata`, and `executor` stays an opaque string.
package engine

import (
	"errors"
	"fmt"
)

// ErrorCode classifies an activation failure so the CLI can map it onto the
// error taxonomy without re-deriving the mapping from the message.
type ErrorCode string

const (
	// CodeValidation is VALIDATION_ERROR (exit 3): a binding that resolves to
	// zero or several workflows, a work-DAG cycle, a run with no issues, or a
	// context bundle over the configured cap.
	CodeValidation ErrorCode = "VALIDATION_ERROR"
	// CodeNotFound is NOT_FOUND (exit 2): a run that does not exist, or a
	// `--pin` path that is missing or is not a regular file.
	CodeNotFound ErrorCode = "NOT_FOUND"
	// CodeConflict is CONFLICT (exit 4): re-activating a terminal run.
	CodeConflict ErrorCode = "CONFLICT"
	// CodeGone is GONE: history that `events prune` removed. Two product paths
	// return it: an `events list --since` cursor below the retained minimum,
	// and the recorded issue body and scope read-back (recordedIssue in
	// context.go, reached by step context and step render) when the prune
	// removed the step's claim event.
	CodeGone ErrorCode = "GONE"
	// CodeAuth is AUTH_ERROR (exit 5): a caller presenting a token that is not
	// the run's conductor capability on one of the operator verbs (DKT-2465).
	// The same code the lease refusals use for "you do not hold this", and
	// for the same reason: the answer to a caller with no capability never
	// says more than that.
	CodeAuth ErrorCode = "AUTH_ERROR"
)

// Error is an activation failure carrying its taxonomy code.
type Error struct {
	Code    ErrorCode
	Message string
	// Err is the underlying cause when there is one, so errors.Is still
	// reaches a db sentinel through this wrapper.
	Err error
	// Holders names the steps that made a claim refusal (DKT-4264). Empty for
	// every error that is not a scope or headroom refusal.
	Holders []Holder
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
