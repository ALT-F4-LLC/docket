package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// serveHosted serves the meta handler behind the client window cfg declares
// and returns a client for it.
func serveHosted(t *testing.T, cfg config.DaemonConfig) docketv1connect.DocketServiceClient {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	path, handler := api.NewHandler(WithClientWindow(api.MetaHandler{}, cfg), logger)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return api.NewClient(server.Client(), server.URL, logger)
}

// handshakeAs calls Handshake as a client speaking API major major and
// returns the docket error code, or "" when the daemon admits it.
func handshakeAs(t *testing.T, client docketv1connect.DocketServiceClient, major uint32) output.ErrorCode {
	t.Helper()
	_, err := client.Handshake(context.Background(), connect.NewRequest(&docketv1.HandshakeRequest{ApiMajor: major}))
	if err == nil {
		return ""
	}
	return output.ErrorCodeFromConnectError(err)
}

func TestHostedWindow(t *testing.T) {
	client := serveHosted(t, config.DaemonConfig{MinClientAPIMajor: 2, MaxClientAPIMajor: 4})
	cases := []struct {
		name  string
		major uint32
		want  output.ErrorCode
	}{
		{"below min", 1, output.ErrIncompatible},
		{"min", 2, ""},
		{"inside", 3, ""},
		{"max", 4, ""},
		{"above max", 5, output.ErrIncompatible},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := handshakeAs(t, client, tc.major); got != tc.want {
				t.Errorf("client API major %d against window [2, 4]: code = %q, want %q", tc.major, got, tc.want)
			}
		})
	}
}

func TestHostedWindowFromDeclaredLocation(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	certFile, keyFile, _ := testKeyPair(t)
	writeDaemonConfigWindow(t, xdg, certFile, keyFile, testSecret(t), 7, 9)
	client := serveHosted(t, loadDaemonConfig(t))

	for major, want := range map[uint32]output.ErrorCode{
		1:  output.ErrIncompatible,
		6:  output.ErrIncompatible,
		7:  "",
		9:  "",
		10: output.ErrIncompatible,
	} {
		if got := handshakeAs(t, client, major); got != want {
			t.Errorf("client API major %d against declared window [7, 9]: code = %q, want %q", major, got, want)
		}
	}
}
