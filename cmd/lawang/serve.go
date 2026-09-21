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
	"github.com/gablooge/lawang/internal/hub"
	"github.com/gablooge/lawang/internal/ingress"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/store"
)

const shutdownGrace = 15 * time.Second

// serve runs the HTTP role until ctx is cancelled: the webhook edge under /ingress/{provider} and
// /healthz, on one handler that internal/ingress builds.
//
// The order is deliberate. Everything that can refuse to start does so before the socket is bound:
// the database and its preflight, the provider registry, and the hub, which is the one that knows
// whether a registered provider's signature scheme needs a public base URL that nothing has
// configured. A deployment that is going to answer 401 to every delivery should never reach the
// point of answering at all.
func serve(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	// Every provider this binary serves, known at compile time and never changed afterwards.
	// There is none yet: ClickUp is B11. Until then the edge answers 404 to every segment, which
	// is the same answer a scanner gets, and /healthz is what serve is for.
	reg, err := provider.NewRegistry()
	if err != nil {
		return err
	}
	h, err := hub.New(db, reg, hub.Options{PublicBaseURL: cfg.PublicBaseURL, Logger: logger})
	if err != nil {
		return err
	}
	handler, err := newHandler(reg, h, cfg.PublicBaseURL, logger)
	if err != nil {
		return err
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return listenError(ctx, err)
	}
	return serveOn(ctx, ln, handler, logger)
}

// newHandler is the whole routing table of the serve role.
//
// Every route goes to ingress.New, and what it returns is what the server serves. There is no mux
// here, and no route is registered anywhere else, because net/http.ServeMux answers two kinds of
// 307 redirect before any handler runs and a 307 preserves the method and the body: a provider
// follows it and re-POSTs to a path it did not sign, so every delivery becomes a 401 that reads as
// a forgery. One of the two depends on the routing table rather than on the request, so only the
// package that builds the table can take it away (architecture 3.1). For the same reason a route
// is never a sub-mux: ingress.New cannot see inside a handler, and a nested mux brings the
// trailing-slash redirect straight back. The /v1 operator API (B14) is therefore one Route per
// endpoint.
func newHandler(reg *provider.Registry, h ingress.Hub, publicBaseURL string, logger *slog.Logger) (http.Handler, error) {
	return ingress.New(reg, h, ingress.Options{
		PublicBaseURL: publicBaseURL,
		Logger:        logger,
		Routes: []ingress.Route{
			{Method: http.MethodGet, Path: "/healthz", Handler: http.HandlerFunc(healthz)},
		},
	})
}

// healthz is liveness only: the process is up. Readiness (/readyz, which checks Postgres) is
// separate, and is B25's.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
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

func serveOn(ctx context.Context, ln net.Listener, handler http.Handler, logger *slog.Logger) error {
	srv := newServer(handler)

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
