package api

import (
	"context"
	"errors"
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

var discardLogger = slog.New(slog.DiscardHandler)

// echoRunProcedure is a test-only procedure whose request is a Run: no
// DocketService request carries a run status, so the server test mounts its
// own handler with the daemon's handler options.
const echoRunProcedure = "/docket.v1.Probe/EchoRun"

func TestServerRejectsUnknownEnum(t *testing.T) {
	codecs := []struct {
		name   string
		option connect.ClientOption
	}{
		{"json", connect.WithProtoJSON()},
		{"proto", connect.WithClientOptions()},
	}
	for _, codec := range codecs {
		t.Run(codec.name, func(t *testing.T) {
			var handlerRan atomic.Bool
			handler := connect.NewUnaryHandler(echoRunProcedure,
				func(_ context.Context, req *connect.Request[docketv1.Run]) (*connect.Response[docketv1.Run], error) {
					handlerRan.Store(true)
					return connect.NewResponse(req.Msg), nil
				},
				handlerOptions(discardLogger),
			)
			server := httptest.NewServer(handler)
			defer server.Close()
			client := connect.NewClient[docketv1.Run, docketv1.Run](server.Client(), server.URL+echoRunProcedure, codec.option)

			_, err := client.CallUnary(context.Background(), connect.NewRequest(&docketv1.Run{Status: "bogus"}))

			if err == nil {
				t.Fatal("call succeeded, want the unknown run status rejected")
			}
			if handlerRan.Load() {
				t.Error("handler ran on a request the daemon cannot interpret")
			}
			if got := output.ErrorCodeFromConnectError(err); got != output.ErrIncompatible {
				t.Errorf("docket code = %q, want %q (error: %v)", got, output.ErrIncompatible, err)
			}
			if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
				t.Errorf("connect code = %v, want %v", got, connect.CodeFailedPrecondition)
			}

			handlerRan.Store(false)
			res, err := client.CallUnary(context.Background(), connect.NewRequest(&docketv1.Run{Status: "active"}))
			if err != nil {
				t.Fatalf("known run status rejected: %v", err)
			}
			if !handlerRan.Load() || res.Msg.GetStatus() != "active" {
				t.Errorf("handler ran = %v, echoed status = %q; want the known status served", handlerRan.Load(), res.Msg.GetStatus())
			}
		})
	}
}

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
	client := NewClient(server.Client(), server.URL, discardLogger)
	return client.RunStatus(context.Background(), connect.NewRequest(&docketv1.RunStatusRequest{}))
}

func TestClientRejectsUnknownEnum(t *testing.T) {
	res, err := callRunStatus(t, "bogus")

	if err == nil {
		t.Fatalf("RunStatus succeeded with %v, want the unknown run status rejected", res.Msg)
	}
	if res != nil {
		t.Errorf("response = %v, want none returned with the error", res.Msg)
	}
	if got := output.ExitCodeForError(output.ErrorCodeFromConnectError(err)); got != output.ExitIncompatible {
		t.Errorf("exit = %d, want ExitIncompatible (%d) (error: %v)", got, output.ExitIncompatible, err)
	}
	var incompatible *output.IncompatibleError
	if !errors.As(err, &incompatible) {
		t.Errorf("error %v does not carry the *output.IncompatibleError cause", err)
	}

	res, err = callRunStatus(t, "active")
	if err != nil {
		t.Fatalf("known run status rejected: %v", err)
	}
	if got := res.Msg.GetItems()[0].GetStatus(); got != "active" {
		t.Errorf("status = %q, want %q", got, "active")
	}
}
