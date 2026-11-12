package client

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// runStatusStub is a daemon that answers RunStatus with one run in a fixed
// status, built with Connect's default options so it sends any value.
type runStatusStub struct {
	docketv1connect.UnimplementedDocketServiceHandler
	status string
}

func (s runStatusStub) RunStatus(context.Context, *connect.Request[docketv1.RunStatusRequest]) (*connect.Response[docketv1.RunStatusResponse], error) {
	return connect.NewResponse(&docketv1.RunStatusResponse{Items: []*docketv1.Run{{Id: "RUN-1", Status: s.status}}}), nil
}

func callRunStatus(t *testing.T, status string) (*connect.Response[docketv1.RunStatusResponse], error) {
	t.Helper()
	path, handler := docketv1connect.NewDocketServiceHandler(runStatusStub{status: status})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := NewClient(server.Client(), server.URL, slog.New(slog.DiscardHandler))
	return c.RunStatus(context.Background(), connect.NewRequest(&docketv1.RunStatusRequest{}))
}

func TestNewClientRejectsUnknownEnum(t *testing.T) {
	_, err := callRunStatus(t, "bogus")
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrIncompatible {
		t.Errorf("docket code = %q, want %q (error: %v)", got, output.ErrIncompatible, err)
	}

	res, err := callRunStatus(t, "active")
	if err != nil {
		t.Fatalf("known run status rejected: %v", err)
	}
	if got := res.Msg.GetItems()[0].GetStatus(); got != "active" {
		t.Errorf("status = %q, want %q", got, "active")
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
	path, handler := docketv1connect.NewDocketServiceHandler(countingDaemon{handshakes: &handshakes})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	_, meta, err := Connect(context.Background(), server.Client(), server.URL, slog.New(slog.DiscardHandler))

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
