package daemon

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// bearerTransport presents the daemon's bearer secret on every request.
type bearerTransport struct {
	secret string
	next   http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.secret)
	return b.next.RoundTrip(r)
}

// TestHostedEntrypointAnswersHandshake: the hosted entrypoint, configured
// from the daemon config on disk, answers a Handshake over HTTPS inside the
// config's client window with the build version its caller passed, refuses
// one outside it, and stops serving once its context ends.
func TestHostedEntrypointAnswersHandshake(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	certFile, keyFile, roots := testKeyPair(t)
	secret := testSecret(t)
	writeDaemonConfigWindow(t, xdg, certFile, keyFile, secret, 1, 1)
	logger := slog.New(slog.DiscardHandler)
	const build = "v9.8.7-hosted"

	srv, err := ListenHosted(loadDaemonConfig(t), build, logger)
	if err != nil {
		t.Fatalf("ListenHosted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var serveErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveErr = srv.ServeUntil(ctx, 5*time.Second)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	_, port, err := net.SplitHostPort(srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	t.Cleanup(transport.CloseIdleConnections)
	httpClient := &http.Client{Timeout: 5 * time.Second, Transport: bearerTransport{secret: secret, next: transport}}
	daemonClient := client.NewClient(httpClient, "https://127.0.0.1:"+port, logger)

	res, err := daemonClient.Handshake(context.Background(), connect.NewRequest(&docketv1.HandshakeRequest{ApiMajor: 1}))
	if err != nil {
		t.Fatalf("Handshake inside the window: %v", err)
	}
	if got := res.Msg.GetDaemonVersion(); got != build {
		t.Errorf("daemon version = %q, want the build passed to ListenHosted %q", got, build)
	}
	if got := handshakeAs(t, daemonClient, 2); got != output.ErrIncompatible {
		t.Errorf("Handshake outside the window [1, 1]: code = %q, want %q", got, output.ErrIncompatible)
	}

	cancel()
	select {
	case <-done:
		if serveErr != nil {
			t.Fatalf("ServeUntil after its context ended: %v, want nil", serveErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeUntil did not return after its context ended")
	}

	transport.CloseIdleConnections()
	if _, err := daemonClient.Handshake(context.Background(), connect.NewRequest(&docketv1.HandshakeRequest{ApiMajor: 1})); err == nil {
		t.Fatal("Handshake succeeded after the daemon shut down")
	}
}

// TestHostedEntrypointClientWindow: the hosted entrypoint enforces the
// config's inclusive client window over HTTPS, admitting both bounds and
// refusing a major on either side as INCOMPATIBLE.
func TestHostedEntrypointClientWindow(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	certFile, keyFile, roots := testKeyPair(t)
	secret := testSecret(t)
	writeDaemonConfigWindow(t, xdg, certFile, keyFile, secret, 2, 4)
	logger := slog.New(slog.DiscardHandler)

	srv, err := ListenHosted(loadDaemonConfig(t), "dev", logger)
	if err != nil {
		t.Fatalf("ListenHosted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.ServeUntil(ctx, 5*time.Second); err != nil {
			t.Errorf("ServeUntil: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	_, port, err := net.SplitHostPort(srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	t.Cleanup(transport.CloseIdleConnections)
	httpClient := &http.Client{Timeout: 5 * time.Second, Transport: bearerTransport{secret: secret, next: transport}}
	daemonClient := client.NewClient(httpClient, "https://127.0.0.1:"+port, logger)

	cases := []struct {
		name  string
		major uint32
		want  output.ErrorCode
	}{
		{"below min", 1, output.ErrIncompatible},
		{"min", 2, ""},
		{"max", 4, ""},
		{"above max", 5, output.ErrIncompatible},
	}
	for _, tc := range cases {
		if got := handshakeAs(t, daemonClient, tc.major); got != tc.want {
			t.Errorf("%s: client API major %d against window [2, 4]: code = %q, want %q", tc.name, tc.major, got, tc.want)
		}
	}
}
