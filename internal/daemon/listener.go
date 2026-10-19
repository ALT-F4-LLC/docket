package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Server serves a handler on a TCP address: over TLS behind the bearer secret
// (ListenHTTPS), or in plaintext on loopback only (ListenLoopbackHTTP).
type Server struct {
	server   *http.Server
	listener net.Listener
}

// ListenHTTPS loads the configured key pair and binds cfg.Listen. It takes the
// config by value and keeps no path to it, so the secret and key pair are
// fixed for the server's lifetime: rotating either means a restart. A key pair
// that does not load refuses start; there is no plaintext fallback.
func ListenHTTPS(cfg config.DaemonConfig, handler http.Handler) (*Server, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading the daemon's TLS key pair: %w", err)
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", cfg.Listen, err)
	}
	srv := newHTTPServer(requireBearer(cfg.BearerSecret, handler))
	srv.TLSConfig = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	return &Server{server: srv, listener: ln}, nil
}

// Resolver looks up a host's addresses; *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// ListenLoopbackHTTP binds listen for plaintext HTTP only when every address
// its host resolves to is loopback, and binds the checked address rather than
// the name, so a second lookup cannot move the socket off-host. Requests need
// no bearer: the exemption adds nothing to the request, so it never stands in
// for the conductor credential.
func ListenLoopbackHTTP(ctx context.Context, listen string, resolver Resolver, handler http.Handler) (*Server, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("parsing listen address %q: %w", listen, err)
	}
	if host == "" {
		return nil, fmt.Errorf("refusing plaintext on %q: no host names loopback", listen)
	}
	resolved, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolving %q for a plaintext listener: %w", host, err)
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("refusing plaintext on %q: it resolves to no address", host)
	}
	for _, addr := range resolved {
		if !addr.IsLoopback() {
			return nil, fmt.Errorf("refusing plaintext on %q: it resolves to %s, which is not loopback", host, addr)
		}
	}
	bind := net.JoinHostPort(resolved[0].Unmap().WithZone("").String(), port)
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", bind, err)
	}
	return &Server{server: newHTTPServer(handler), listener: ln}, nil
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler: handler,
		// Bound what an unauthenticated peer can hold open or send.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
}

// Addr is the bound address, with the port resolved when the listen address
// named port 0.
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// Serve answers requests until Shutdown, then returns http.ErrServerClosed.
// It serves TLS exactly when the server was built with a TLS config.
func (s *Server) Serve() error {
	if s.server.TLSConfig != nil {
		return s.server.ServeTLS(s.listener, "", "")
	}
	return s.server.Serve(s.listener)
}

// Shutdown stops accepting, waits for in-flight requests until ctx ends, and
// releases the port whether or not Serve ever ran.
func (s *Server) Shutdown(ctx context.Context) error {
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

// ConductorTokenHeader carries a run's conductor capability. It is separate
// from Authorization so the bearer, or its loopback exemption, never
// satisfies a conductor check.
const ConductorTokenHeader = "Docket-Conductor-Token"

// requireConductor admits a request only when ConductorTokenHeader holds the
// token whose hash is conductorHash; any other request gets 403 before next
// runs. An empty hash admits nothing.
func requireConductor(conductorHash string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !model.TokenMatches(r.Header.Get(ConductorTokenHeader), conductorHash) {
			http.Error(w, "missing or invalid conductor credential", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
