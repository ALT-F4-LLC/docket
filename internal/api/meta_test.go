package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
)

func serve(t *testing.T, svc docketv1connect.DocketServiceHandler) *httptest.Server {
	t.Helper()
	path, handler := NewHandler(svc, discardLogger)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestServiceHandshakeReportsBuildVersion(t *testing.T) {
	const build = "v4.5.6-service-test+9a2e"
	server := serve(t, NewService(nil, build))
	client := NewClient(server.Client(), server.URL, discardLogger)

	res, err := client.Handshake(context.Background(), connect.NewRequest(&docketv1.HandshakeRequest{}))

	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if got := res.Msg.GetDaemonVersion(); got != build {
		t.Errorf("daemon_version = %q, want the NewService build %q", got, build)
	}
}
