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
				"No query, no fragment, no credentials, no trailing slash. The host is named in " +
				"ASCII, an internationalized name in its punycode (xn--) form, which is the " +
				"spelling a provider's dashboard holds. It is configuration and " +
				"never a header on purpose: a signature scheme that covers the request URL " +
				"(HubSpot v3) is checked against this, and Host, X-Forwarded-Host and " +
				"X-Forwarded-Proto are all chosen by whoever sent the request, so building the " +
				"signed URL from them would let a sender pick part of what it signed.",
			Default: "none. serve refuses to start when a registered provider's signature covers " +
				"the URL, because every one of its deliveries would be answered 401. With no such " +
				"provider registered it starts and serves as usual.",
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
// # Normalizing the result again must return it unchanged
//
// The same value goes through this function more than once in a running deployment: Load
// normalizes the variable, ingress.New normalizes what Load stored, and a Registrar (B13, B18)
// registers what Load stored with the provider. If a second pass moved the string, the URL the
// provider signs and the URL the edge verifies against would be different strings and every
// delivery would be a 401 with nothing to see. So every rule here is idempotent in itself: what
// it rewrites, it rewrites to a fixed point (trailing slashes are removed, all of them, so the
// result has none), and everything else it either leaves alone or refuses. A refusal is a fixed
// point too, since a refused value never becomes anyone's output.
// TestNormalizingTheResultAgainChangesNothing asserts that as a property over a corpus rather
// than over the table rows somebody happened to write.
//
// # What it deliberately does not touch
//
// The host is passed through exactly as written: its case is kept, and so is a default port
// (https://EXAMPLE.com:443 stays as it is). That looks like an omission and is not. This string
// has to match, byte for byte, the URL the operator gave the provider, which they copied from or
// pasted into the provider's own dashboard. Lowercasing a host or dropping :443 would create that
// mismatch rather than remove it, and the mismatch is invisible until every delivery answers 401.
// The scheme is the exception: url.Parse lowercases it before this function sees it, and no
// dashboard spells it differently.
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
	// u.Host carries the port, so an empty u.Host is not the only way to arrive naming no
	// machine: "http://:8080" parses with a host of ":8080", which is not empty, and the edge
	// would then sign "http://:8080/ingress/hubspot" while the provider signed the real public
	// URL. That is the silent 401 this whole function exists to prevent, and it is an easy thing
	// to write, because LAWANG_LISTEN_ADDR two entries above it in the usage text takes exactly
	// that spelling. u.Hostname is the half that names a machine, and no rule here can see the
	// problem otherwise: "http://:8080" parses, normalizes to itself, and is a fixed point.
	if u.Hostname() == "" {
		return "", errors.New("missing host")
	}
	// The same silent mismatch from the other side: "http://x:" keeps a colon no dashboard holds,
	// and it too is a fixed point of every other rule here.
	if strings.HasSuffix(u.Host, ":") {
		return "", errors.New("the host must not end in a colon with no port")
	}
	// The host is ASCII, and an internationalized name belongs here in its punycode (xn--) form.
	// Go does not convert one to the other, so "https://café.example" would go into a signature
	// base string as those bytes, while DNS, the provider's dashboard and therefore the URL the
	// provider signed all carry "xn--caf-dma.example": the same 401 with nothing to see. The path
	// prefix is refused outside ASCII a few lines below, for a related reason, and refusing both
	// keeps one answer for one question.
	//
	// 0x80 is the definition of ASCII and not a boundary any test can pin: every byte of a
	// non-ASCII character in valid UTF-8 starts a sequence with a lead byte of 0xC2 or more, so
	// no input this function can be given tells 0x80 from 0xC0 or from 0xC2. Nothing here is
	// weaker for it, and nobody should "tighten" it believing a test would notice.
	for _, b := range []byte(u.Host) {
		if b >= 0x80 {
			return "", errors.New("the host must be written in ASCII, and an internationalized name in its punycode (xn--) form")
		}
	}
	// url.Parse decodes percent-escapes in the host, so "http://%25" parses with a host of "%",
	// which is not a host any second pass can read back (FuzzNormalizePublicBaseURL found this
	// one). The only escape a host legitimately carries is an IPv6 zone id, which a public base
	// URL has no use for, so the whole shape is refused rather than re-encoded.
	if strings.Contains(u.Host, "%") {
		return "", errors.New("the host must be written plainly, with no percent-escape")
	}
	if u.User != nil {
		return "", errors.New("must not carry credentials")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("must not carry a query or a fragment")
	}
	// The path prefix must already be its own escaped form. The edge appends the request's own
	// escaped path to this string, so a prefix that is not escaped the same way would make the
	// signed URL depend on which spelling happened to be registered.
	//
	// Comparing EscapedPath with Path is what catches every escape, not only the ones Go
	// re-spells: %2F and %2e differ once url.Parse has canonicalized them, but %20, %23 and %3F
	// re-encode to themselves, so a RawPath check lets them through and the value comes back with
	// a literal space, hash or question mark in it. Those are exactly the shapes a second pass
	// then refuses. A prefix outside ASCII is refused here too, for the same reason: the edge
	// would be concatenating an unescaped prefix onto an escaped path.
	if u.EscapedPath() != u.Path {
		return "", errors.New("the path prefix must be written plainly, with no percent-escape and no character that needs one")
	}
	// Every trailing slash, not one: TrimSuffix would leave "//" as "/", which path.Clean calls
	// clean and which is the trailing slash this function promises to remove. What is left must
	// already be clean, so a doubled or relative segment inside the prefix is refused rather than
	// quietly rewritten into a URL the operator never registered.
	p := strings.TrimRight(u.Path, "/")
	if p != "" && path.Clean(p) != p {
		return "", errors.New("the path prefix must be a clean absolute path, for example /lawang")
	}
	// Written out rather than handed to url.URL.String, which escapes a path byte for byte by its
	// own rules and not by the ones url.Parse accepted: String turns the "!" in "/!a", which
	// parses and comes back unchanged here, into "%21", and the next pass then refuses the
	// escape. (FuzzNormalizePublicBaseURL found that within a second.) Concatenating the three
	// parts that were just checked keeps the operator's own spelling, which is the whole point,
	// and url.Parse reads the result back as the same scheme, host and path: the host cannot hold
	// a slash, a question mark or a hash, since url.Parse would have split the URL there, and a
	// path that is not absolute and clean was refused above.
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
