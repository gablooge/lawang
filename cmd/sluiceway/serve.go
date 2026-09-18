package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/gablooge/sluiceway/internal/appversion"
	"github.com/gablooge/sluiceway/internal/config"
)

const shutdownGrace = 15 * time.Second

// serve runs the HTTP role until ctx is cancelled. For now it carries only /healthz; the webhook
// edge and the operator API mount here as they land.
func serve(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	return serveOn(ctx, ln, logger)
}

func serveOn(ctx context.Context, ln net.Listener, logger *slog.Logger) error {
	srv := &http.Server{
		Handler:           newMux(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	logger.Info("listening", "addr", ln.Addr().String(), "version", appversion.String())

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// The grace ran out with connections still open. Drop them so nothing outlives serveOn.
		_ = srv.Close()
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	// Liveness only: the process is up. Readiness (/readyz, which checks Postgres) is separate.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
