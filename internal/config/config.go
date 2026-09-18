// Package config reads Sluiceway's configuration from the environment.
//
// Defaults fail closed: an unset SLUICEWAY_ENV means production, and production refuses to start
// on anything it would otherwise have to guess.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
)

// Env is the deployment environment.
type Env string

// Production is the default. Development has to be asked for by name.
const (
	Production  Env = "production"
	Development Env = "development"
)

// devDatabaseURL matches the local compose stack. It is only ever used when development is explicit.
const devDatabaseURL = "postgres://sluiceway:sluiceway@localhost:5432/sluiceway?sslmode=disable" //nolint:gosec // local development only, never reachable in production

// Config is the full process configuration.
type Config struct {
	Env         Env
	DatabaseURL string
	ListenAddr  string
	LogLevel    slog.Level
	LogFormat   string // "json" or "text"
}

const redacted = "[redacted]"

// LogValue keeps the database URL, which carries a password, out of structured logs.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("env", string(c.Env)),
		slog.String("database_url", redacted),
		slog.String("listen_addr", c.ListenAddr),
		slog.String("log_level", c.LogLevel.String()),
		slog.String("log_format", c.LogFormat),
	)
}

// String covers %v, %+v and %s, so printing a Config cannot leak the database URL either.
func (c Config) String() string {
	return fmt.Sprintf("{Env:%s DatabaseURL:%s ListenAddr:%s LogLevel:%s LogFormat:%s}",
		c.Env, redacted, c.ListenAddr, c.LogLevel, c.LogFormat)
}

// GoString covers %#v.
func (c Config) GoString() string { return "config.Config" + c.String() }

// Load builds a Config from getenv (os.Getenv in production, a map lookup in tests). It reports
// every problem at once so a misconfigured deploy is fixed in one pass.
//
// No error echoes a received value, for any variable. Load cannot know which variable a secret was
// pasted into: a manifest with two entries swapped puts the database URL, password included, into
// SLUICEWAY_ENV, and the refusal is printed to the container log.
func Load(getenv func(string) string) (Config, error) {
	var errs []error

	cfg := Config{
		Env:        Production,
		ListenAddr: ":8080",
		LogLevel:   slog.LevelInfo,
	}

	switch v := getenv("SLUICEWAY_ENV"); v {
	case "", string(Production):
	case string(Development):
		cfg.Env = Development
	default:
		// An unrecognized value stays production. A typo must never relax anything.
		errs = append(errs, fmt.Errorf("SLUICEWAY_ENV: must be %q or %q", Production, Development))
	}

	cfg.DatabaseURL = getenv("SLUICEWAY_DATABASE_URL")
	switch {
	case cfg.DatabaseURL != "":
		if err := checkDatabaseURL(cfg.DatabaseURL); err != nil {
			errs = append(errs, fmt.Errorf("SLUICEWAY_DATABASE_URL: %w", err))
		}
	case cfg.Env == Development:
		cfg.DatabaseURL = devDatabaseURL
	default:
		errs = append(errs, errors.New("SLUICEWAY_DATABASE_URL: required in production"))
	}

	if v := getenv("SLUICEWAY_LISTEN_ADDR"); v != "" {
		// Checked here because the listener's own error quotes the address it was given.
		if _, _, err := net.SplitHostPort(v); err != nil {
			errs = append(errs, errors.New("SLUICEWAY_LISTEN_ADDR: must be host:port or :port"))
		} else {
			cfg.ListenAddr = v
		}
	}

	if v := getenv("SLUICEWAY_LOG_LEVEL"); v != "" {
		// slog's parse error quotes its input, so it is deliberately not wrapped.
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, errors.New("SLUICEWAY_LOG_LEVEL: must be debug, info, warn or error"))
		}
	}

	cfg.LogFormat = "json"
	if cfg.Env == Development {
		cfg.LogFormat = "text"
	}
	if v := strings.ToLower(getenv("SLUICEWAY_LOG_FORMAT")); v != "" {
		if v != "json" && v != "text" {
			errs = append(errs, errors.New(`SLUICEWAY_LOG_FORMAT: must be "json" or "text"`))
		} else {
			cfg.LogFormat = v
		}
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// checkDatabaseURL rejects anything that is not a Postgres URL. The error never echoes the value,
// because a database URL carries a password. In particular the url.Parse error is never wrapped or
// returned: it quotes the whole input.
func checkDatabaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("not a valid URL")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return errors.New("scheme must be postgres or postgresql")
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	return nil
}
