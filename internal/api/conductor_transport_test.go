package api_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api"
	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/daemon"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// TestConductorRulesAreTransportAware: the daemon's Connect service decides
// RunConduct by the listener the request arrived on. Over the Unix socket the
// first conductor of an unbound run is refused with AUTH_ERROR and the run
// stays unbound; over the daemon's HTTP listener the same request seats it;
// once bound, the socket re-seats it.
func TestConductorRulesAreTransportAware(t *testing.T) {
	st := openTransportStore(t)
	logger := slog.New(slog.DiscardHandler)
	path, handler := api.NewHandler(daemon.NewService(st, "test"), logger)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	overSocket := serveSocket(t, mux, logger)
	overHTTP := serveLoopbackHTTP(t, mux, logger)
	ctx := context.Background()
	conduct := func(c docketv1connect.DocketServiceClient, run string) (string, error) {
		res, err := c.RunConduct(ctx, connect.NewRequest(&docketv1.RunConductRequest{Run: run}))
		if err != nil {
			return "", err
		}
		return res.Msg.GetToken(), nil
	}

	t.Run("socket refused on an unbound run", func(t *testing.T) {
		runID, run := unboundRun(t, st)
		_, err := conduct(overSocket, run)
		if err == nil {
			t.Fatal("the socket seated the first conductor of an unbound run")
		}
		if code := output.ErrorCodeFromConnectError(err); code != output.ErrAuth {
			t.Errorf("refusal code = %q, want %s (%v)", code, output.ErrAuth, err)
		}
		if hash := conductorHash(t, st, runID); hash != "" {
			t.Error("the refused conduct stored a conductor hash")
		}
	})

	t.Run("HTTP seats an unbound run", func(t *testing.T) {
		runID, run := unboundRun(t, st)
		token, err := conduct(overHTTP, run)
		if err != nil {
			t.Fatalf("RunConduct over HTTP: %v", err)
		}
		if !model.TokenMatches(token, conductorHash(t, st, runID)) {
			t.Error("the run is not bound to the token the HTTP conduct returned")
		}
	})

	t.Run("socket re-seats a bound run", func(t *testing.T) {
		runID, run := unboundRun(t, st)
		first, err := conduct(overHTTP, run)
		if err != nil {
			t.Fatalf("binding over HTTP: %v", err)
		}
		token, err := conduct(overSocket, run)
		if err != nil {
			t.Fatalf("RunConduct over the socket on a bound run: %v", err)
		}
		if token == first || !model.TokenMatches(token, conductorHash(t, st, runID)) {
			t.Error("the socket conduct did not rotate the run to the token it returned")
		}
	})
}

func openTransportStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "issues.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

// unboundRun inserts a planning run that holds no conductor capability.
func unboundRun(t *testing.T, st store.Store) (int, string) {
	t.Helper()
	issue, _, err := st.CreateIssueIdempotent(&model.Issue{
		Title: "conduct subject", Status: model.StatusTodo, Priority: model.PriorityMedium, Kind: model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent: %v", err)
	}
	run, _, err := st.InsertRunWithContextIdempotent(
		store.DefaultProjectID, "", 0, model.NowMS(), store.RunContext{}, "", issue)
	if err != nil {
		t.Fatalf("InsertRunWithContextIdempotent: %v", err)
	}
	if hash := conductorHash(t, st, run.ID); hash != "" {
		t.Fatal("a fresh run already holds a conductor capability")
	}
	return run.ID, run.Ref()
}

func conductorHash(t *testing.T, st store.Store, runID int) string {
	t.Helper()
	hash, err := st.RunConductorHash(runID)
	if err != nil {
		t.Fatalf("RunConductorHash: %v", err)
	}
	return hash
}

// serveSocket serves handler on a Unix socket, as the daemon would serve its
// socket peers, and returns a client that dials it.
func serveSocket(t *testing.T, handler http.Handler, logger *slog.Logger) docketv1connect.DocketServiceClient {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listening on %s: %v", sock, err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return client.NewClient(&http.Client{Timeout: 5 * time.Second, Transport: transport}, "http://docket", logger)
}

// serveLoopbackHTTP serves handler on the daemon's plaintext loopback HTTP
// listener and returns a client for it.
func serveLoopbackHTTP(t *testing.T, handler http.Handler, logger *slog.Logger) docketv1connect.DocketServiceClient {
	t.Helper()
	srv, err := daemon.ListenLoopbackHTTP(context.Background(), "127.0.0.1:0", net.DefaultResolver, handler)
	if err != nil {
		t.Fatalf("ListenLoopbackHTTP: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()
	t.Cleanup(func() {
		srv.Shutdown(context.Background())
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	})
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	return client.NewClient(&http.Client{Timeout: 5 * time.Second, Transport: transport}, "http://"+srv.Addr().String(), logger)
}
