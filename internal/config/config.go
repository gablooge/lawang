// Package config reads Lawang's configuration from the environment.
//
// Defaults fail closed: an unset LAWANG_ENV means production, and production refuses to start
// on anything it would otherwise have to guess.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Env is the deployment environment.
type Env string

// Production is the default. Development has to be asked for by name.
const (
	Production  Env = "production"
	Development Env = "development"
)

// devDatabaseURL is where the compose stack of backlog item B27 will put Postgres. Until that item
// lands nothing in this repository starts such a database. It is only ever used when development
// is explicit.
const devDatabaseURL = "postgres://lawang:lawang@localhost:5432/lawang?sslmode=disable" //nolint:gosec // local development only, never reachable in production

// Config is the full process configuration.
type Config struct {
	Env         Env
	DatabaseURL string
	ListenAddr  string
	LogLevel    slog.Level
	LogFormat   string // "json" or "text"

	// PublicBaseURL is the scheme, host and any stripped path prefix that a provider reaches
	// /ingress/{provider} at from the public internet, with no trailing slash, or the empty
	// string when the deployment did not configure one. It is what the webhook edge builds the
	// URL a signature covers out of (ingress.Options.PublicBaseURL, architecture 4); backlog
	// item B07 wires it, because the edge is not mounted until a hub exists.
	PublicBaseURL string
}

// defaultListenAddr is where serve listens when LAWANG_LISTEN_ADDR is unset.
const defaultListenAddr = ":8080"

// The values of the enumerated variables. Load validates against these slices and Variables
// publishes the same slices, so a value added here is accepted and documented in one step.
var (
	envValues       = []string{string(Production), string(Development)}
	logLevelValues  = []string{"debug", "info", "warn", "error"}
	logFormatValues = []string{"json", "text"}
)

// Variable documents one environment variable that Load reads.
type Variable struct {
	Name    string
	Doc     string
	Values  []string // every value Load accepts, or nil when the variable is not an enumeration
	Default string   // what Load uses when the variable is unset, in words an operator can act on
}

// Variables lists every variable Load reads, in the order an operator should think about them.
// The command's usage text is built from it, so the binary documents its own configuration, and
// tests hold it to what Load really reads, to the defaults Load really applies, and to the values
// Load really accepts.
//
// The development database URL is described, not printed: it is a URL with a password in it, and
// usage text ends up in logs and tickets. Its host, port and database name are not secret.
func Variables() []Variable {
	return []Variable{
		{
			Name:    "LAWANG_ENV",
			Doc:     "The deployment environment. Anything unrecognized is refused, never treated as development.",
			Values:  slices.Clone(envValues),
			Default: string(Production),
		},
		{
			Name: "LAWANG_DATABASE_URL",
			Doc: "Postgres URL (postgres:// or postgresql://) of the non-superuser application role. " +
				"Pool and connection settings travel in the URL: pool_max_conns (the default is " +
				"max(4, NumCPU), shared by every in-flight webhook and worker row), pool_min_conns, " +
				"pool_max_conn_lifetime and connect_timeout (seconds).",
			Default: "none, it is required in production. Development connects to localhost:5432, database lawang.",
		},
		{
			Name:    "LAWANG_LISTEN_ADDR",
			Doc:     "host:port or :port for serve. The port is a number from 0 to 65535, not a service name.",
			Default: defaultListenAddr,
		},
		{
			Name: "LAWANG_PUBLIC_BASE_URL",
			Doc: "The absolute URL providers reach this deployment at, scheme and host and any " +
				"path prefix a reverse proxy strips before forwarding (https://lawang.example.com). " +
				"No query, no fragment, no credentials, no trailing slash. It is configuration and " +
				"never a header on purpose: a signature scheme that covers the request URL " +
				"(HubSpot v3) is checked against this, and Host, X-Forwarded-Host and " +
				"X-Forwarded-Proto are all chosen by whoever sent the request, so building the " +
				"signed URL from them would let a sender pick part of what it signed.",
			Default: "none. The edge still serves, and a provider whose signature covers the URL " +
				"refuses every delivery rather than verify against a URL Lawang guessed.",
		},
		{
			Name:    "LAWANG_LOG_LEVEL",
			Doc:     "The lowest level that is logged. Case does not matter.",
			Values:  slices.Clone(logLevelValues),
			Default: strings.ToLower(slog.LevelInfo.String()),
		},
		{
			Name:    "LAWANG_LOG_FORMAT",
			Doc:     "The encoding of the log on stderr. Case does not matter.",
			Values:  slices.Clone(logFormatValues),
			Default: "json in production, text in development",
		},
	}
}

// notOneOf is the refusal for an enumerated variable. It lists what is accepted and, like every
// error here, never what was received.
func notOneOf(name string, values []string) error {
	return fmt.Errorf("%s: must be one of: %s", name, strings.Join(values, ", "))
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
		// Not redacted: it is a public hostname, it is what a provider posts to, and an operator
		// debugging a signature failure needs to see the spelling the process actually holds.
		slog.String("public_base_url", c.PublicBaseURL),
	)
}

// String covers %v, %+v and %s, so printing a Config cannot leak the database URL either.
func (c Config) String() string {
	return fmt.Sprintf("{Env:%s DatabaseURL:%s ListenAddr:%s LogLevel:%s LogFormat:%s PublicBaseURL:%s}",
		c.Env, redacted, c.ListenAddr, c.LogLevel, c.LogFormat, c.PublicBaseURL)
}

// GoString covers %#v.
func (c Config) GoString() string { return "config.Config" + c.String() }

// Load builds a Config from getenv (os.Getenv in production, a map lookup in tests). It reports
// every problem at once so a misconfigured deploy is fixed in one pass.
//
// No error echoes a received value, for any variable. Load cannot know which variable a secret was
// pasted into: a manifest with two entries swapped puts the database URL, password included, into
// LAWANG_ENV, and the refusal is printed to the container log.
func Load(getenv func(string) string) (Config, error) {
	var errs []error

	cfg := Config{
		Env:        Production,
		ListenAddr: defaultListenAddr,
		LogLevel:   slog.LevelInfo,
	}

	if v := getenv("LAWANG_ENV"); v != "" {
		if slices.Contains(envValues, v) {
			cfg.Env = Env(v)
		} else {
			// An unrecognized value stays production. A typo must never relax anything.
			errs = append(errs, notOneOf("LAWANG_ENV", envValues))
		}
	}

	cfg.DatabaseURL = getenv("LAWANG_DATABASE_URL")
	switch {
	case cfg.DatabaseURL != "":
		if err := checkDatabaseURL(cfg.DatabaseURL); err != nil {
			errs = append(errs, fmt.Errorf("LAWANG_DATABASE_URL: %w", err))
		}
	case cfg.Env == Development:
		cfg.DatabaseURL = devDatabaseURL
	default:
		errs = append(errs, errors.New("LAWANG_DATABASE_URL: required in production"))
	}

	if v := getenv("LAWANG_LISTEN_ADDR"); v != "" {
		if err := checkListenAddr(v); err != nil {
			errs = append(errs, fmt.Errorf("LAWANG_LISTEN_ADDR: %w", err))
		} else {
			cfg.ListenAddr = v
		}
	}

	if v := getenv("LAWANG_PUBLIC_BASE_URL"); v != "" {
		base, err := NormalizePublicBaseURL(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("LAWANG_PUBLIC_BASE_URL: %w", err))
		} else {
			cfg.PublicBaseURL = base
		}
	}

	if v := strings.ToLower(getenv("LAWANG_LOG_LEVEL")); v != "" {
		// Only the documented names. slog on its own would also take offsets such as "info+2",
		// which the usage text does not list. Its parse error quotes its input, so it is
		// deliberately not wrapped.
		if !slices.Contains(logLevelValues, v) || cfg.LogLevel.UnmarshalText([]byte(v)) != nil {
			errs = append(errs, notOneOf("LAWANG_LOG_LEVEL", logLevelValues))
		}
	}

	cfg.LogFormat = "json"
	if cfg.Env == Development {
		cfg.LogFormat = "text"
	}
	if v := strings.ToLower(getenv("LAWANG_LOG_FORMAT")); v != "" {
		if slices.Contains(logFormatValues, v) {
			cfg.LogFormat = v
		} else {
			errs = append(errs, notOneOf("LAWANG_LOG_FORMAT", logFormatValues))
		}
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// checkListenAddr requires host:port or :port with a numeric port. It is checked in Load because
// the listener's own error quotes the address it was given, and that error ends up in the log.
//
// net.SplitHostPort only checks the shape, so on its own it lets through anything with exactly one
// colon: a user:password pair, or a database URL with no password and no port. Either would then
// be looked up as a service name and quoted by the lookup error. Requiring a numeric port closes
// that, and deliberately stops accepting service names such as ":http", which nothing here needs.
//
// The host half is not checked: "localhost" is legitimate, and so is any name the deployment
// resolves. serve keeps the listener's error text out of the log for that half.
func checkListenAddr(raw string) error {
	_, port, err := net.SplitHostPort(raw)
	if err != nil {
		// Not wrapped: the net.AddrError quotes its input.
		return errors.New("must be host:port or :port")
	}
	// ParseUint takes ASCII digits only (no sign, no spaces), and 16 bits is exactly 0 to 65535.
	// Its error quotes the input too, so it is not wrapped either.
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return errors.New("port must be a number from 0 to 65535 (service names are not accepted)")
	}
	return nil
}

// NormalizePublicBaseURL checks LAWANG_PUBLIC_BASE_URL and returns it in the one spelling the
// webhook edge concatenates a request path onto: scheme, host, an optional path prefix, and no
// trailing slash. The empty string is not passed here; an unset variable is handled by the caller.
//
// It is exported and not private because internal/ingress checks the same value again in
// ingress.New, by calling this function rather than by carrying a copy of the rules. The string
// ends up inside a signature base string, so the two must agree to the byte: a base URL that Load
// accepted and the edge spelled differently would make every signature fail with nothing to see.
//
// No error echoes the value. Load cannot know which variable a secret was pasted into, and a
// manifest with two entries swapped puts the database URL here.
func NormalizePublicBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// Not wrapped: the url.Error quotes the whole input.
		return "", errors.New("not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("scheme must be http or https, and the URL must be absolute")
	}
	if u.Host == "" {
		return "", errors.New("missing host")
	}
	if u.User != nil {
		return "", errors.New("must not carry credentials")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("must not carry a query or a fragment")
	}
	// The edge appends the request's own escaped path, so a percent-escape in the prefix would
	// make the result depend on which spelling the provider happened to register.
	if u.RawPath != "" {
		return "", errors.New("the path prefix must not be percent-encoded")
	}
	p := strings.TrimSuffix(u.Path, "/")
	if p != "" && (!strings.HasPrefix(p, "/") || path.Clean(p) != p) {
		return "", errors.New("the path prefix must be a clean absolute path, for example /lawang")
	}
	return u.Scheme + "://" + u.Host + p, nil
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
