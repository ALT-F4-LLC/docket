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

	"github.com/ALT-F4-LLC/docket/internal/api"
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
// config's client window, refuses one outside it, and stops serving once its
// context ends.
func TestHostedEntrypointAnswersHandshake(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	certFile, keyFile, roots := testKeyPair(t)
	secret := testSecret(t)
	writeDaemonConfigWindow(t, xdg, certFile, keyFile, secret, 1, 1)
	logger := slog.New(slog.DiscardHandler)

	srv, err := ListenHosted(loadDaemonConfig(t), logger)
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
	client := api.NewClient(httpClient, "https://127.0.0.1:"+port, logger)

	res, err := client.Handshake(context.Background(), connect.NewRequest(&docketv1.HandshakeRequest{ApiMajor: 1}))
	if err != nil {
		t.Fatalf("Handshake inside the window: %v", err)
	}
	if got := res.Msg.GetDaemonVersion(); got != api.DaemonVersion {
		t.Errorf("daemon version = %q, want %q", got, api.DaemonVersion)
	}
	if got := handshakeAs(t, client, 2); got != output.ErrIncompatible {
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
	if _, err := client.Handshake(context.Background(), connect.NewRequest(&docketv1.HandshakeRequest{ApiMajor: 1})); err == nil {
		t.Fatal("Handshake succeeded after the daemon shut down")
	}
}
