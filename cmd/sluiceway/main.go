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
	"strings"
	"syscall"

	"github.com/gablooge/sluiceway/internal/appversion"
	"github.com/gablooge/sluiceway/internal/config"
	"github.com/gablooge/sluiceway/internal/store"
	"github.com/gablooge/sluiceway/migrations"
)

const usageHead = `Usage: sluiceway <command>

Commands:
  serve     run the operator API and the webhook edge
  worker    drain the outbox and run the maintenance sweeps
  migrate   apply database migrations, as the non-superuser application role
            "migrate bootstrap" prints the one-time SQL an administrator runs first
  version   print the version
  help      print this text (also -h and --help)

Configuration comes from SLUICEWAY_* environment variables only.
`

const usageExitCodes = `
Exit codes:
  0   success, including a clean shutdown after SIGINT or SIGTERM
  1   the configuration was refused, or the command failed
  2   usage error: no command, an unknown command, or an argument the command does not take
`

// usage is what "sluiceway help" prints, so that an operator never has to read Go source to
// configure the service. The variables come from config.Variables, in the package that reads
// them. What is held to Load there: the names (by recording what Load reads), the defaults (by
// comparing them with what Load applies) and the values of the enumerated variables (Load
// validates against the same lists that are printed here). The descriptions are prose, and
// nothing checks prose.
var usage = buildUsage(config.Variables())

func buildUsage(vars []config.Variable) string {
	var b strings.Builder
	b.WriteString(usageHead)
	b.WriteString("\nVariables:\n")
	for _, v := range vars {
		fmt.Fprintf(&b, "  %s\n      %s\n", v.Name, v.Doc)
		if len(v.Values) > 0 {
			fmt.Fprintf(&b, "      values: %s\n", strings.Join(v.Values, ", "))
		}
		fmt.Fprintf(&b, "      default: %s\n", v.Default)
	}
	b.WriteString(usageExitCodes)
	return b.String()
}

// errNotBuilt marks a role whose backlog item has not landed yet. It exits non-zero so nothing can
// mistake a stub for a running role.
var errNotBuilt = errors.New("not built yet, see docs/backlog.md")

// refuseArguments reports anything after the command name as a usage error, and says whether it
// did. A command that takes an argument handles it before this check runs, so whatever reaches
// here is unexpected, and ignoring it is worse than refusing it: "serve --listen :9090" would
// start on the default port without a word.
//
// It is called once the command is known to exist, so args[0] is one of our own words. The
// arguments themselves are not printed: stderr is the container log, and an argument can be a
// pasted secret as easily as a variable can. The unknown command path in run keeps the same rule.
func refuseArguments(stderr io.Writer, args []string) bool {
	if len(args) == 1 {
		return false
	}
	fmt.Fprintf(stderr, "sluiceway: %s takes no arguments\n\n%s", args[0], usage) //nolint:gosec // G705: a terminal, not a browser
	return true
}

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
		if refuseArguments(stderr, args) {
			return 2
		}
		fmt.Fprintln(stdout, appversion.String())
		return 0
	case "help", "-h", "--help":
		if refuseArguments(stderr, args) {
			return 2
		}
		fmt.Fprint(stdout, usage)
		return 0
	case "migrate":
		if len(args) > 1 {
			if args[1] != "bootstrap" || len(args) > 2 {
				fmt.Fprintf(stderr, "sluiceway: usage: sluiceway migrate [bootstrap]\n")
				return 2
			}
			fmt.Fprint(stdout, migrations.Bootstrap)
			return 0
		}
	}

	var cmd func(context.Context, config.Config, *slog.Logger) error
	switch args[0] {
	case "serve":
		cmd = serve
	case "worker":
		cmd = func(context.Context, config.Config, *slog.Logger) error { return errNotBuilt }
	case "migrate":
		cmd = migrate
	default:
		// The word is not repeated: it is whatever was typed first, which can be a pasted database
		// URL as easily as a typo, and stderr is the container log. The usage lists the real ones.
		fmt.Fprintf(stderr, "sluiceway: unknown command\n\n%s", usage)
		return 2
	}
	if refuseArguments(stderr, args) {
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

// migrate applies pending migrations. store.Open has already refused a superuser login by the
// time anything runs, so the tables are always owned by the role that will use them.
func migrate(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	applied, err := db.Migrate(ctx, logger)
	if err != nil {
		return err
	}
	logger.Info("migrations applied", "count", applied)
	return nil
}

func newLogger(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
