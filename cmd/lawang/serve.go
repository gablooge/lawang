package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/gablooge/lawang/internal/appversion"
	"github.com/gablooge/lawang/internal/config"
)

const shutdownGrace = 15 * time.Second

// serve runs the HTTP role until ctx is cancelled. For now it carries only /healthz. When the
// webhook edge lands (B07 wires it, because it needs a hub), this mux goes away: ingress.New
// builds the routing table, takes /healthz as an ingress.Route and returns the handler to serve,
// so that no route is behind a mux that answers redirects.
func serve(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return listenError(ctx, err)
	}
	return serveOn(ctx, ln, logger)
}

// listenError turns a listen failure into an error that is safe to log.
//
// The error from net quotes the address it was given ("listen tcp 10.0.0.5:8080: bind: ...",
// "lookup some-host: no such host"), and config.Load only vouches for the port half of that
// address. A value pasted into the wrong variable can still sit in the host half. So the original
// error is never returned, wrapped or formatted: only the parts of it that cannot carry the address
// are kept, which is enough to tell "in use" from "not permitted" from "does not resolve".
func listenError(ctx context.Context, err error) error {
	const prefix = "LAWANG_LISTEN_ADDR: cannot listen"

	// An errno's text comes from the operating system's table ("address already in use",
	// "permission denied", "can't assign requested address"), never from the input. Wrapping it
	// alone keeps errors.Is(err, syscall.EADDRINUSE) working for callers.
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Errorf("%s: %w", prefix, errno)
	}
	// Cancelled or out of time before the socket was bound, for example during a slow lookup.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", prefix, ctxErr)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// DNSError.Error() quotes the name, so only its classification is used.
		switch {
		case dnsErr.IsNotFound:
			return errors.New(prefix + ": the host name does not resolve")
		case dnsErr.IsTimeout:
			return errors.New(prefix + ": the host name lookup timed out")
		default:
			return errors.New(prefix + ": the host name lookup failed")
		}
	}
	return errors.New(prefix + ": the address is not valid for a TCP listener")
}

// The timeouts every request on this server is held to. They are what stops a slow-loris client
// from holding a connection, and they matter most for the webhook edge, which faces the public
// internet: readHeaderTimeout cuts a client that dribbles headers and readTimeout one that
// dribbles a body, whatever the path.
//
// readTimeout covers the headers and the body together, so it is the larger of the two.
// writeTimeout is above the accept path's own bound (ingress.DefaultAcceptTimeout), so a slow
// accept is answered by the handler with a status a provider understands rather than cut off
// mid-response.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 60 * time.Second
)

// newServer builds the HTTP server every role shares, so the timeouts are written once.
func newServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
}

func serveOn(ctx context.Context, ln net.Listener, logger *slog.Logger) error {
	srv := newServer(newMux())

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
