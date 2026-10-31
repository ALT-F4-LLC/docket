// Command docket is the CLI entry point for Docket.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/cli"
	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/daemon"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// hostedShutdownGrace bounds how long a stopping hosted daemon waits for
// in-flight requests before it exits anyway.
const hostedShutdownGrace = 10 * time.Second

func main() {
	if len(os.Args) == 3 && os.Args[1] == "daemon" && os.Args[2] == "serve" {
		os.Exit(serveHostedDaemon())
	}
	os.Exit(cli.Execute())
}

// serveHostedDaemon runs `docket daemon serve`: the hosted daemon, configured
// only by the user-level daemon config, serving HTTPS until SIGINT or SIGTERM.
func serveHostedDaemon() int {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	failed := output.ExitCodeForError(output.ErrGeneral)

	cfg, err := config.LoadDaemonConfig()
	if err != nil {
		logger.Error("the hosted daemon cannot start", "err", err)
		return failed
	}
	srv, err := daemon.ListenHosted(*cfg, logger)
	if err != nil {
		logger.Error("the hosted daemon cannot start", "config", cfg.Path, "err", err)
		return failed
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("the hosted daemon is serving", "url", "https://"+srv.Addr().String(), "config", cfg.Path)
	if err := srv.ServeUntil(ctx, hostedShutdownGrace); err != nil {
		logger.Error("the hosted daemon stopped uncleanly", "err", err)
		return failed
	}
	logger.Info("the hosted daemon stopped")
	return 0
}
