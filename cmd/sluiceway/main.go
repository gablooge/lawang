// Command sluiceway is the single Sluiceway binary. Its roles are subcommands.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/gablooge/sluiceway/internal/appversion"
	"github.com/gablooge/sluiceway/internal/config"
)

const usage = `Usage: sluiceway <command>

Commands:
  serve     run the operator API and the webhook edge
  worker    drain the outbox and run the maintenance sweeps
  migrate   apply database migrations
  version   print the version

Configuration comes from SLUICEWAY_* environment variables only.
`

// errNotBuilt marks a role whose backlog item has not landed yet. It exits non-zero so nothing can
// mistake a stub for a running role.
var errNotBuilt = errors.New("not built yet, see docs/backlog.md")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// The first signal starts a graceful shutdown. Handing the signals back to the runtime as soon
	// as it arrives means a second one ends the process at once instead of being swallowed for the
	// whole shutdown grace.
	context.AfterFunc(ctx, stop)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	// Commands that need no configuration.
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, appversion.String())
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}

	var cmd func(context.Context, config.Config, *slog.Logger) error
	switch args[0] {
	case "serve":
		cmd = serve
	case "worker":
		cmd = func(context.Context, config.Config, *slog.Logger) error { return errNotBuilt }
	case "migrate":
		cmd = func(context.Context, config.Config, *slog.Logger) error { return errNotBuilt }
	default:
		fmt.Fprintf(stderr, "sluiceway: unknown command %q\n\n%s", args[0], usage) //nolint:gosec // G705: a terminal, not a browser
		return 2
	}

	cfg, err := config.Load(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "sluiceway: invalid configuration:\n%v\n", err)
		return 1
	}

	logger := newLogger(cfg, stderr).With("role", args[0])
	if err := cmd(ctx, cfg, logger); err != nil {
		logger.Error("exiting", "error", err)
		return 1
	}
	return 0
}

func newLogger(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
