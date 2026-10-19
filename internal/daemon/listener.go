package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/config"
)

// HTTPSServer serves a handler over TLS on a TCP address and admits only
// requests that present the configured bearer secret.
type HTTPSServer struct {
	server   *http.Server
	listener net.Listener
}

// ListenHTTPS loads the configured key pair and binds cfg.Listen. It takes the
// config by value and keeps no path to it, so the secret and key pair are
// fixed for the server's lifetime: rotating either means a restart. A key pair
// that does not load refuses start; there is no plaintext fallback.
func ListenHTTPS(cfg config.DaemonConfig, handler http.Handler) (*HTTPSServer, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading the daemon's TLS key pair: %w", err)
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", cfg.Listen, err)
	}
	return &HTTPSServer{
		server: &http.Server{
			Handler: requireBearer(cfg.BearerSecret, handler),
			TLSConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				Certificates: []tls.Certificate{cert},
			},
			// Bound what an unauthenticated peer can hold open or send.
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    64 << 10,
		},
		listener: ln,
	}, nil
}

// Addr is the bound address, with the port resolved when cfg.Listen named
// port 0.
func (s *HTTPSServer) Addr() net.Addr { return s.listener.Addr() }

// Serve answers requests until Shutdown, then returns http.ErrServerClosed.
func (s *HTTPSServer) Serve() error {
	return s.server.ServeTLS(s.listener, "", "")
}

// Shutdown stops accepting, waits for in-flight requests until ctx ends, and
// releases the port whether or not Serve ever ran.
func (s *HTTPSServer) Shutdown(ctx context.Context) error {
	shutdownErr := s.server.Shutdown(ctx)
	closeErr := s.listener.Close()
	if errors.Is(closeErr, net.ErrClosed) {
		closeErr = nil
	}
	return errors.Join(shutdownErr, closeErr)
}

// requireBearer admits a request only when its Authorization header is
// "Bearer <secret>"; any other request gets 401 before next runs or the body
// is read. The bearer authenticates the transport and nothing else: it puts
// no value in the request context, so it never stands in for a lease or
// conductor token.
func requireBearer(secret config.BearerSecret, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") || !secret.Matches(token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "missing or invalid bearer secret", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
