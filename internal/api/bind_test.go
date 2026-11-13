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
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// Header values no host identity or working directory equals, so a unary that
// fills Env from anything but the request cannot match them.
const (
	authorSentinel  = "header-author-sentinel"
	cwdSentinel     = "/tmp/a b/ü"
	projectSentinel = "/srv/repo-sentinel"
)

// attributedRequest is a non-Handshake request carrying both attribution
// headers, so unary gets past its header check.
func attributedRequest() *connect.Request[docketv1.HandshakeRequest] {
	req := connect.NewRequest(&docketv1.HandshakeRequest{})
	req.Header().Set("Docket-Author", authorSentinel)
	req.Header().Set("Docket-Cwd", cwdSentinel)
	return req
}

func decodeOK(*docketv1.HandshakeRequest) (string, error) { return "in", nil }

// sentinelStore is a store.Store whose identity a test can compare; unary
// only passes it through, so no method is called.
type sentinelStore struct{ store.Store }

// recordEnv returns an op that records the Env it ran with.
func recordEnv(got *verb.Env) func(context.Context, verb.Env, string) (string, error) {
	return func(_ context.Context, env verb.Env, _ string) (string, error) {
		*got = env
		return "ok", nil
	}
}

func TestUnaryMapsDecodeFailureToValidationWithoutRunningOp(t *testing.T) {
	ran := false
	decode := func(*docketv1.HandshakeRequest) (string, error) { return "", errors.New("bad field") }
	op := func(context.Context, verb.Env, string) (string, error) {
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
	op := func(context.Context, verb.Env, string) (string, error) { return "", errors.New("op failed") }

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
			op := func(context.Context, verb.Env, string) (string, error) { return "", tc.err }

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

// secondDetailVersion returns the daemon_version of the HandshakeResponse err
// carries as its second detail, failing the test when it carries none.
func secondDetailVersion(t *testing.T, err error) string {
	t.Helper()
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("err = %v, want a *connect.Error", err)
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
	return res.GetDaemonVersion()
}

func detailCount(t *testing.T, err error) int {
	t.Helper()
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("err = %v, want a *connect.Error", err)
	}
	return len(connectErr.Details())
}

func TestUnaryAttachesEncodedResultOnPartialFailure(t *testing.T) {
	refused := &verb.Error{Code: output.ErrConflict, Err: errors.New("one of two failed")}

	t.Run("result with verb error", func(t *testing.T) {
		op := func(context.Context, verb.Env, string) (string, error) { return "partial-report", refused }

		_, err := unary(context.Background(), NewService(nil, "dev"), attributedRequest(), decodeOK, op, encodeHandshake)

		if got := output.ErrorCodeFromConnectError(err); got != output.ErrConflict {
			t.Errorf("code = %q, want %q", got, output.ErrConflict)
		}
		if got := secondDetailVersion(t, err); got != "partial-report" {
			t.Errorf("second detail daemon_version = %q, want %q", got, "partial-report")
		}
	})

	t.Run("result with plain error", func(t *testing.T) {
		op := func(context.Context, verb.Env, string) (string, error) {
			return "partial-report", errors.New("one of two failed")
		}

		_, err := unary(context.Background(), NewService(nil, "dev"), attributedRequest(), decodeOK, op, encodeHandshake)

		if got := output.ErrorCodeFromConnectError(err); got != output.ErrGeneral {
			t.Errorf("code = %q, want %q", got, output.ErrGeneral)
		}
		if got := secondDetailVersion(t, err); got != "partial-report" {
			t.Errorf("second detail daemon_version = %q, want %q", got, "partial-report")
		}
	})

	t.Run("error without result", func(t *testing.T) {
		op := func(context.Context, verb.Env, string) (string, error) { return "", refused }

		_, err := unary(context.Background(), NewService(nil, "dev"), attributedRequest(), decodeOK, op, encodeHandshake)

		if got := detailCount(t, err); got != 1 {
			t.Errorf("details = %d, want only the ErrorDetail when the op returned no result", got)
		}
	})

	type report struct{ Summary string }
	encodeReport := func(r report) *docketv1.HandshakeResponse {
		return &docketv1.HandshakeResponse{DaemonVersion: r.Summary}
	}

	t.Run("struct result with error", func(t *testing.T) {
		op := func(context.Context, verb.Env, string) (report, error) { return report{Summary: "1 of 2"}, refused }

		_, err := unary(context.Background(), NewService(nil, "dev"), attributedRequest(), decodeOK, op, encodeReport)

		if got := secondDetailVersion(t, err); got != "1 of 2" {
			t.Errorf("second detail daemon_version = %q, want %q", got, "1 of 2")
		}
	})

	t.Run("zero struct result with error", func(t *testing.T) {
		op := func(context.Context, verb.Env, string) (report, error) { return report{}, refused }

		_, err := unary(context.Background(), NewService(nil, "dev"), attributedRequest(), decodeOK, op, encodeReport)

		if got := detailCount(t, err); got != 1 {
			t.Errorf("details = %d, want only the ErrorDetail for a zero result", got)
		}
	})
}

func TestUnaryBuildsEnvFromServiceAndRequest(t *testing.T) {
	st := &sentinelStore{}
	const build = "v1.2.3-env-sentinel"
	req := attributedRequest()
	req.Header().Set("Docket-Project", projectSentinel)
	var got verb.Env

	_, err := unary(context.Background(), NewService(st, build), req, decodeOK, recordEnv(&got), encodeHandshake)

	if err != nil {
		t.Fatalf("unary: %v", err)
	}
	if got.Store != store.Store(st) {
		t.Errorf("Env.Store = %v, want the Service store", got.Store)
	}
	if want := (verb.BuildInfo{Version: build}); got.Build != want {
		t.Errorf("Env.Build = %+v, want %+v", got.Build, want)
	}
	if want := (verb.ProjectInput{Identity: projectSentinel, Anchored: false}); got.Project != want {
		t.Errorf("Env.Project = %+v, want %+v: the header verbatim, never anchored", got.Project, want)
	}
}

func TestUnaryEnvFromHeaders(t *testing.T) {
	t.Run("both headers reach Env verbatim", func(t *testing.T) {
		var got verb.Env

		_, err := unary(context.Background(), NewService(nil, "dev"), attributedRequest(), decodeOK, recordEnv(&got), encodeHandshake)

		if err != nil {
			t.Fatalf("unary: %v", err)
		}
		if got.Author != authorSentinel {
			t.Errorf("Env.Author = %q, want the Docket-Author header %q", got.Author, authorSentinel)
		}
		if got.Cwd != cwdSentinel {
			t.Errorf("Env.Cwd = %q, want the Docket-Cwd header %q", got.Cwd, cwdSentinel)
		}
	})

	refusals := []struct {
		name    string
		author  string
		cwd     string
		missing []string
	}{
		{"missing Docket-Cwd", authorSentinel, "", []string{"Docket-Cwd"}},
		{"missing Docket-Author", "", cwdSentinel, []string{"Docket-Author"}},
		{"missing both", "", "", []string{"Docket-Author", "Docket-Cwd"}},
	}
	for _, tc := range refusals {
		t.Run(tc.name+" is refused before decode and op run", func(t *testing.T) {
			req := connect.NewRequest(&docketv1.HandshakeRequest{})
			if tc.author != "" {
				req.Header().Set("Docket-Author", tc.author)
			}
			if tc.cwd != "" {
				req.Header().Set("Docket-Cwd", tc.cwd)
			}
			decoded, ran := false, false
			decode := func(*docketv1.HandshakeRequest) (string, error) {
				decoded = true
				return "in", nil
			}
			op := func(context.Context, verb.Env, string) (string, error) {
				ran = true
				return "", nil
			}

			_, err := unary(context.Background(), NewService(nil, "dev"), req, decode, op, encodeHandshake)

			if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
				t.Errorf("connect code = %v, want %v (err %v)", got, connect.CodeInvalidArgument, err)
			}
			if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
				t.Errorf("code = %q, want %q", got, output.ErrValidation)
			}
			if err != nil && !namesAny(err.Error(), tc.missing) {
				t.Errorf("error %q names none of the missing headers %v", err, tc.missing)
			}
			if decoded {
				t.Error("decode ran on a request missing an attribution header")
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
		op := func(context.Context, verb.Env, string) (string, error) {
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

func namesAny(message string, headers []string) bool {
	for _, h := range headers {
		if strings.Contains(message, h) {
			return true
		}
	}
	return false
}
