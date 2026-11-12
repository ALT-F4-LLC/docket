package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/api"
	"github.com/ALT-F4-LLC/docket/internal/config"
)

// ListenHosted builds the hosted daemon's Connect service from cfg, reporting
// build as its version, with Handshake gated on cfg's client window, and binds
// it with ListenHTTPS.
func ListenHosted(cfg config.DaemonConfig, build string, logger *slog.Logger) (*Server, error) {
	path, handler := api.NewHandler(WithClientWindow(api.NewService(nil, build), cfg), logger)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	return ListenHTTPS(cfg, mux)
}

// ServeUntil serves until ctx ends, then shuts down, giving in-flight
// requests up to grace to finish. It returns nil after a clean shutdown, the
// shutdown's error when grace runs out, and Serve's error when serving stops
// before ctx ends.
func (s *Server) ServeUntil(ctx context.Context, grace time.Duration) error {
	served := make(chan error, 1)
	go func() { served <- s.Serve() }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	err := s.Shutdown(shutdownCtx)
	if serveErr := <-served; !errors.Is(serveErr, http.ErrServerClosed) {
		err = errors.Join(err, serveErr)
	}
	return err
}
