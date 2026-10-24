package output

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
)

// NewConnectError builds the Connect error the daemon returns for a failed
// verb. Its one detail is an ErrorDetail holding only the docket error code;
// the client reads that code, never the Connect code, which is set for
// generic clients only.
func NewConnectError(code ErrorCode, err error) *connect.Error {
	connectErr := connect.NewError(connectCodeFor(code), err)
	detail, detailErr := connect.NewErrorDetail(&docketv1.ErrorDetail{Code: string(code)})
	if detailErr != nil {
		// Marshalling fails only for a code that is not valid UTF-8, which no
		// ErrorCode constant is.
		panic(fmt.Sprintf("output: encode error detail %q: %v", code, detailErr))
	}
	connectErr.AddDetail(detail)
	return connectErr
}

// ErrorCodeFromConnectError recovers the docket error code from an RPC error,
// from which the client derives both the envelope's code and the exit status.
// An error with no docket detail never reached a docket handler: a transport
// failure maps to UNAVAILABLE, an HTTP 401 from the bearer check maps to
// UNAUTHENTICATED, and anything else maps to GENERAL_ERROR.
func ErrorCodeFromConnectError(err error) ErrorCode {
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		for _, detail := range connectErr.Details() {
			value, valueErr := detail.Value()
			if valueErr != nil {
				continue
			}
			if errorDetail, ok := value.(*docketv1.ErrorDetail); ok {
				code := ErrorCode(errorDetail.GetCode())
				if code != ErrGeneral && ExitCodeForError(code) == ExitGeneral {
					// A code this client does not know.
					return ErrGeneral
				}
				return code
			}
		}
	}
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable:
		return ErrUnavailable
	case connect.CodeUnauthenticated:
		return ErrUnauthenticated
	default:
		return ErrGeneral
	}
}

// connectCodeFor is the Connect code a generic client sees for a docket code,
// following the assignments in docs/design/connect-api-v1.md.
func connectCodeFor(code ErrorCode) connect.Code {
	switch code {
	case ErrNotFound:
		return connect.CodeNotFound
	case ErrValidation:
		return connect.CodeInvalidArgument
	case ErrConflict:
		return connect.CodeAborted
	case ErrAuth, ErrUntrusted:
		return connect.CodePermissionDenied
	case ErrStaleLease, ErrIncompatible:
		return connect.CodeFailedPrecondition
	case ErrTimeout:
		return connect.CodeDeadlineExceeded
	case ErrGone:
		return connect.CodeOutOfRange
	case ErrUnavailable:
		return connect.CodeUnavailable
	case ErrUnauthenticated:
		return connect.CodeUnauthenticated
	default:
		return connect.CodeInternal
	}
}
