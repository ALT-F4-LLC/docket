package output

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
)

// handshakeFailer serves Handshake by returning a fixed error, so a test can
// put a daemon-built error on the wire and read it back through a real client.
type handshakeFailer struct {
	docketv1connect.UnimplementedDocketServiceHandler
	err error
}

func (h handshakeFailer) Handshake(context.Context, *connect.Request[docketv1.HandshakeRequest]) (*connect.Response[docketv1.HandshakeResponse], error) {
	return nil, h.err
}

func callHandshake(t *testing.T, baseURL string) error {
	t.Helper()
	client := docketv1connect.NewDocketServiceClient(http.DefaultClient, baseURL)
	_, err := client.Handshake(context.Background(), connect.NewRequest(&docketv1.HandshakeRequest{}))
	if err == nil {
		t.Fatal("Handshake succeeded, want an error")
	}
	return err
}

func TestConnectErrorDetails(t *testing.T) {
	tests := []struct {
		code        ErrorCode
		connectCode connect.Code
	}{
		{ErrGeneral, connect.CodeInternal},
		{ErrNotFound, connect.CodeNotFound},
		{ErrValidation, connect.CodeInvalidArgument},
		{ErrConflict, connect.CodeAborted},
		{ErrAuth, connect.CodePermissionDenied},
		{ErrStaleLease, connect.CodeFailedPrecondition},
		{ErrTimeout, connect.CodeDeadlineExceeded},
		{ErrUntrusted, connect.CodePermissionDenied},
		{ErrGone, connect.CodeOutOfRange},
		{ErrUnavailable, connect.CodeUnavailable},
		{ErrIncompatible, connect.CodeFailedPrecondition},
		{ErrUnauthenticated, connect.CodeUnauthenticated},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			_, handler := docketv1connect.NewDocketServiceHandler(handshakeFailer{
				err: NewConnectError(tt.code, errors.New("server-side message")),
			})
			server := httptest.NewServer(handler)
			defer server.Close()

			err := callHandshake(t, server.URL)

			if got := ErrorCodeFromConnectError(err); got != tt.code {
				t.Errorf("ErrorCodeFromConnectError = %q, want %q", got, tt.code)
			}
			if got, want := ExitCodeForError(ErrorCodeFromConnectError(err)), ExitCodeForError(tt.code); got != want {
				t.Errorf("decoded exit = %d, want %d", got, want)
			}
			if got := connect.CodeOf(err); got != tt.connectCode {
				t.Errorf("connect code = %v, want %v", got, tt.connectCode)
			}

			var connectErr *connect.Error
			if !errors.As(err, &connectErr) {
				t.Fatalf("error %T is not a *connect.Error", err)
			}
			details := connectErr.Details()
			if len(details) != 1 {
				t.Fatalf("got %d details, want exactly the docket code", len(details))
			}
			value, valueErr := details[0].Value()
			if valueErr != nil {
				t.Fatalf("decode detail: %v", valueErr)
			}
			detail, ok := value.(*docketv1.ErrorDetail)
			if !ok {
				t.Fatalf("detail is %T, want *docketv1.ErrorDetail", value)
			}
			if detail.GetCode() != string(tt.code) {
				t.Errorf("detail code = %q, want %q", detail.GetCode(), tt.code)
			}
		})
	}
}

func TestConnectErrorUnknownDetailCodeIsGeneral(t *testing.T) {
	err := NewConnectError(ErrorCode("NEVER_DEFINED"), errors.New("boom"))

	if got := ErrorCodeFromConnectError(err); got != ErrGeneral {
		t.Errorf("ErrorCodeFromConnectError = %q, want %q", got, ErrGeneral)
	}
}

func TestConnectErrorFallbackExit(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := listener.Addr().String()
		listener.Close()

		callErr := callHandshake(t, "http://"+addr)

		if !errors.Is(callErr, syscall.ECONNREFUSED) {
			t.Fatalf("error = %v, want a connection-refused transport error", callErr)
		}
		if got := ExitCodeForError(ErrorCodeFromConnectError(callErr)); got != ExitUnavailable {
			t.Errorf("exit = %d, want ExitUnavailable (%d)", got, ExitUnavailable)
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}))
		defer server.Close()

		callErr := callHandshake(t, server.URL)

		if got := connect.CodeOf(callErr); got != connect.CodeUnauthenticated {
			t.Fatalf("connect code = %v, want unauthenticated from HTTP 401", got)
		}
		if got := ExitCodeForError(ErrorCodeFromConnectError(callErr)); got != ExitUnauthenticated {
			t.Errorf("exit = %d, want ExitUnauthenticated (%d)", got, ExitUnauthenticated)
		}
	})
}
