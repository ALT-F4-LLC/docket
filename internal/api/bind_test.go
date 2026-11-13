package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// attributedRequest is a non-Handshake request carrying both attribution
// headers, so unary gets past its header check.
func attributedRequest() *connect.Request[docketv1.HandshakeRequest] {
	req := connect.NewRequest(&docketv1.HandshakeRequest{})
	req.Header().Set("Docket-Author", "header-author-sentinel")
	req.Header().Set("Docket-Cwd", "/tmp/a b/ü")
	return req
}

func decodeOK(*docketv1.HandshakeRequest) (string, error) { return "in", nil }

func TestUnaryMapsDecodeFailureToValidationWithoutRunningOp(t *testing.T) {
	ran := false
	decode := func(*docketv1.HandshakeRequest) (string, error) { return "", errors.New("bad field") }
	op := func(context.Context, *Service, string) (string, error) {
		ran = true
		return "", nil
	}

	_, err := unary(context.Background(), NewService(nil, "dev"), attributedRequest(), decode, op, encodeHandshake)

	if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
		t.Errorf("code = %q, want %q (err %v)", got, output.ErrValidation, err)
	}
	if ran {
		t.Error("op ran on a request that failed to decode")
	}
}

func TestUnaryMapsOpFailureToGeneralError(t *testing.T) {
	op := func(context.Context, *Service, string) (string, error) { return "", errors.New("op failed") }

	_, err := unary(context.Background(), NewService(nil, "dev"), attributedRequest(), decodeOK, op, encodeHandshake)

	if err == nil {
		t.Fatal("unary returned no error for a failed op")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Errorf("connect code = %v, want %v, the code GENERAL_ERROR carries", got, connect.CodeInternal)
	}
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrGeneral {
		t.Errorf("code = %q, want %q", got, output.ErrGeneral)
	}
}

func TestUnaryMapsVerbErrorToItsCode(t *testing.T) {
	conflict := &verb.Error{Code: output.ErrConflict, Err: errors.New("issue moved")}
	cases := []struct {
		name        string
		err         error
		connectCode connect.Code
		code        output.ErrorCode
	}{
		{"verb error", conflict, connect.CodeAborted, output.ErrConflict},
		{"wrapped verb error", fmt.Errorf("close: %w", conflict), connect.CodeAborted, output.ErrConflict},
		{"typed nil verb error", (*verb.Error)(nil), connect.CodeInternal, output.ErrGeneral},
		{"verb error without cause", &verb.Error{Code: output.ErrConflict}, connect.CodeInternal, output.ErrGeneral},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := func(context.Context, *Service, string) (string, error) { return "", tc.err }

			_, err := unary(context.Background(), NewService(nil, "dev"), attributedRequest(), decodeOK, op, encodeHandshake)

			if err == nil {
				t.Fatal("unary returned no error for a failed op")
			}
			if got := connect.CodeOf(err); got != tc.connectCode {
				t.Errorf("connect code = %v, want %v", got, tc.connectCode)
			}
			if got := output.ErrorCodeFromConnectError(err); got != tc.code {
				t.Errorf("code = %q, want %q", got, tc.code)
			}
		})
	}
}

func TestUnaryAttachesEncodedResultOnPartialFailure(t *testing.T) {
	refused := &verb.Error{Code: output.ErrConflict, Err: errors.New("one of two failed")}

	t.Run("result with error", func(t *testing.T) {
		op := func(context.Context, *Service, string) (string, error) { return "partial-report", refused }

		_, err := unary(context.Background(), NewService(nil, "dev"), attributedRequest(), decodeOK, op, encodeHandshake)

		var connectErr *connect.Error
		if !errors.As(err, &connectErr) {
			t.Fatalf("err = %v, want a *connect.Error", err)
		}
		if got := output.ErrorCodeFromConnectError(err); got != output.ErrConflict {
			t.Errorf("code = %q, want %q", got, output.ErrConflict)
		}
		details := connectErr.Details()
		if len(details) != 2 {
			t.Fatalf("details = %d, want 2: the ErrorDetail and the encoded result", len(details))
		}
		value, valueErr := details[1].Value()
		if valueErr != nil {
			t.Fatalf("second detail: %v", valueErr)
		}
		res, ok := value.(*docketv1.HandshakeResponse)
		if !ok {
			t.Fatalf("second detail is %T, want *docketv1.HandshakeResponse", value)
		}
		if got := res.GetDaemonVersion(); got != "partial-report" {
			t.Errorf("second detail daemon_version = %q, want %q", got, "partial-report")
		}
	})

	t.Run("error without result", func(t *testing.T) {
		op := func(context.Context, *Service, string) (string, error) { return "", refused }

		_, err := unary(context.Background(), NewService(nil, "dev"), attributedRequest(), decodeOK, op, encodeHandshake)

		var connectErr *connect.Error
		if !errors.As(err, &connectErr) {
			t.Fatalf("err = %v, want a *connect.Error", err)
		}
		if got := len(connectErr.Details()); got != 1 {
			t.Errorf("details = %d, want only the ErrorDetail when the op returned no result", got)
		}
	})
}

func TestUnaryEnvFromHeaders(t *testing.T) {
	refusals := []struct {
		name    string
		author  string
		cwd     string
		missing string
	}{
		{"missing Docket-Cwd", "header-author-sentinel", "", "Docket-Cwd"},
		{"missing Docket-Author", "", "/tmp/a b/ü", "Docket-Author"},
		{"missing both", "", "", "Docket-Author"},
	}
	for _, tc := range refusals {
		t.Run(tc.name+" is refused before op runs", func(t *testing.T) {
			req := connect.NewRequest(&docketv1.HandshakeRequest{})
			if tc.author != "" {
				req.Header().Set("Docket-Author", tc.author)
			}
			if tc.cwd != "" {
				req.Header().Set("Docket-Cwd", tc.cwd)
			}
			ran := false
			op := func(context.Context, *Service, string) (string, error) {
				ran = true
				return "", nil
			}

			_, err := unary(context.Background(), NewService(nil, "dev"), req, decodeOK, op, encodeHandshake)

			if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
				t.Errorf("connect code = %v, want %v (err %v)", got, connect.CodeInvalidArgument, err)
			}
			if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
				t.Errorf("code = %q, want %q", got, output.ErrValidation)
			}
			if err != nil && !strings.Contains(err.Error(), tc.missing) {
				t.Errorf("error %q does not name the missing header %s", err, tc.missing)
			}
			if ran {
				t.Error("op ran on a request missing an attribution header")
			}
		})
	}

	t.Run("an empty header value is refused", func(t *testing.T) {
		req := attributedRequest()
		req.Header().Set("Docket-Author", "")
		ran := false
		op := func(context.Context, *Service, string) (string, error) {
			ran = true
			return "", nil
		}

		_, err := unary(context.Background(), NewService(nil, "dev"), req, decodeOK, op, encodeHandshake)

		if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
			t.Errorf("code = %q, want %q (err %v)", got, output.ErrValidation, err)
		}
		if ran {
			t.Error("op ran on a request with an empty Docket-Author")
		}
	})

	t.Run("Handshake succeeds with neither header", func(t *testing.T) {
		const build = "v0.0.0-no-headers"
		server := serve(t, NewService(nil, build))
		raw := docketv1connect.NewDocketServiceClient(server.Client(), server.URL)

		res, err := raw.Handshake(context.Background(), connect.NewRequest(&docketv1.HandshakeRequest{}))

		if err != nil {
			t.Fatalf("Handshake without attribution headers: %v", err)
		}
		if got := res.Msg.GetDaemonVersion(); got != build {
			t.Errorf("daemon_version = %q, want %q", got, build)
		}
	})
}
