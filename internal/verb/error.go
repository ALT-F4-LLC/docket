package verb

import "github.com/ALT-F4-LLC/docket/internal/output"

// Error is a verb failure carrying the docket error code each transport
// reports: the CLI maps it to its CmdError, the daemon to a Connect error
// through output.NewConnectError.
type Error struct {
	Code output.ErrorCode
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

func newError(err error, code output.ErrorCode) *Error {
	return &Error{Code: code, Err: err}
}
