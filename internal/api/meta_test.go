package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

func TestHandshakeReturnsDaemonVersion(t *testing.T) {
	const build = "v9.8.7-meta-test+4f1c"
	previous := DaemonVersion
	DaemonVersion = build
	t.Cleanup(func() { DaemonVersion = previous })
	server := serve(t, MetaHandler{})
	client := NewClient(server.Client(), server.URL, discardLogger)

	res, err := client.Handshake(context.Background(), connect.NewRequest(&docketv1.HandshakeRequest{}))

	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if got := res.Msg.GetDaemonVersion(); got != build {
		t.Errorf("daemon_version = %q, want %q", got, build)
	}
}

// countingDaemon answers Handshake and counts the calls it receives.
type countingDaemon struct {
	docketv1connect.UnimplementedDocketServiceHandler
	handshakes *atomic.Int32
}

func (d countingDaemon) Handshake(context.Context, *connect.Request[docketv1.HandshakeRequest]) (*connect.Response[docketv1.HandshakeResponse], error) {
	d.handshakes.Add(1)
	return connect.NewResponse(&docketv1.HandshakeResponse{DaemonVersion: "counted"}), nil
}

func TestConnectCallsHandshakeOnce(t *testing.T) {
	var handshakes atomic.Int32
	server := serve(t, countingDaemon{handshakes: &handshakes})

	_, meta, err := Connect(context.Background(), server.Client(), server.URL, discardLogger)

	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := handshakes.Load(); got != 1 {
		t.Errorf("handshake calls = %d, want exactly 1 per connect", got)
	}
	if got := meta.GetDaemonVersion(); got != "counted" {
		t.Errorf("daemon_version = %q, want the daemon's %q", got, "counted")
	}
}
