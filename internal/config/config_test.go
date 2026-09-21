package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadUnsetEnvironmentIsProduction(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"LAWANG_DATABASE_URL": "postgres://app@db:5432/lawang",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Env != Production {
		t.Errorf("Env = %q, want %q", cfg.Env, Production)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want json", cfg.LogFormat)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
}

func TestLoadProductionRequiresDatabaseURL(t *testing.T) {
	for _, e := range []string{"", "production"} {
		_, err := Load(env(map[string]string{"LAWANG_ENV": e}))
		if err == nil || !strings.Contains(err.Error(), "LAWANG_DATABASE_URL") {
			t.Errorf("LAWANG_ENV=%q: err = %v, want a LAWANG_DATABASE_URL error", e, err)
		}
	}
}

func TestLoadDevelopmentDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"LAWANG_ENV": "development"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Env != Development {
		t.Errorf("Env = %q, want %q", cfg.Env, Development)
	}
	if cfg.DatabaseURL != devDatabaseURL {
		t.Errorf("DatabaseURL = %q, want the development default", cfg.DatabaseURL)
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want text", cfg.LogFormat)
	}
}

func TestLoadUnknownEnvironmentIsAnError(t *testing.T) {
	// "dev" is the likely typo. It must not be read as development, and must not pass silently.
	_, err := Load(env(map[string]string{
		"LAWANG_ENV":          "dev",
		"LAWANG_DATABASE_URL": "postgres://app@db:5432/lawang",
	}))
	if err == nil || !strings.Contains(err.Error(), "LAWANG_ENV") {
		t.Fatalf("err = %v, want a LAWANG_ENV error", err)
	}
}

// secretFragments are the distinctive pieces of the URLs below. None may appear in any error.
var secretFragments = []string{"hunter2", "leakuser", "leakhost", "leakdb"}

func assertNoSecret(t *testing.T, what, got string) {
	t.Helper()
	for _, frag := range secretFragments {
		if strings.Contains(got, frag) {
			t.Errorf("%s leaks %q: %s", what, frag, got)
		}
	}
}

func TestLoadDatabaseURLErrorsNeverEchoTheValue(t *testing.T) {
	// One case per return in checkDatabaseURL. The unparseable one matters most: url.Parse quotes
	// its whole input in the error, and a password with a bare "%" is the ordinary way a Postgres
	// URL fails to parse.
	for name, raw := range map[string]string{
		"unparseable":  "postgres://leakuser:hunter2%zz@leakhost:5432/leakdb",
		"wrong scheme": "mysql://leakuser:hunter2@leakhost:3306/leakdb",
		"missing host": "postgres://leakuser:hunter2@/leakdb",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(map[string]string{"LAWANG_DATABASE_URL": raw}))
			if err == nil {
				t.Fatal("Load accepted a bad database URL")
			}
			if !strings.Contains(err.Error(), "LAWANG_DATABASE_URL") {
				t.Errorf("error does not name the variable: %v", err)
			}
			assertNoSecret(t, "error", err.Error())
		})
	}
}

func TestLoadErrorsNeverEchoAnyValue(t *testing.T) {
	// A manifest with two entries swapped puts the database URL into some other variable. Load
	// cannot know which, so no variable's error may repeat what it received.
	const secret = "postgres://leakuser:hunter2@leakhost:5432/leakdb"
	validated := []string{
		"LAWANG_ENV",
		"LAWANG_LISTEN_ADDR",
		"LAWANG_LOG_LEVEL",
		"LAWANG_LOG_FORMAT",
		"LAWANG_PUBLIC_BASE_URL",
	}

	for _, name := range validated {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(map[string]string{
				"LAWANG_DATABASE_URL": "postgres://app@db:5432/lawang",
				name:                  secret,
			}))
			if err == nil {
				t.Fatalf("Load accepted a database URL as %s", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error does not name %s: %v", name, err)
			}
			assertNoSecret(t, "error", err.Error())
		})
	}

	t.Run("all at once", func(t *testing.T) {
		all := map[string]string{"LAWANG_DATABASE_URL": "mysql://leakuser:hunter2@leakhost:3306/leakdb"}
		for _, name := range validated {
			all[name] = secret
		}
		_, err := Load(env(all))
		if err == nil {
			t.Fatal("Load accepted an invalid environment")
		}
		assertNoSecret(t, "joined error", err.Error())
	})
}

func TestLoadRefusesABadListenAddress(t *testing.T) {
	for _, addr := range []string{
		// The wrong shape: no colon, or too many.
		"8080", "localhost", "http://localhost:8080",
		// Exactly one colon, so net.SplitHostPort alone is satisfied. Each of these used to reach
		// the listener, whose error then quoted it: a user:password pair in the wrong variable,
		// and a database URL with no password and no port.
		"leakuser:hunter2",
		"postgres://leakuser@leakhost/leakdb",
		// A port that is not a number from 0 to 65535. Service names are refused on purpose.
		"leakhost:99999", "leakhost:65536", ":http", "localhost:http", "leakhost:",
		":-1", ":+80", ": 80", ":80 ", ":0x50", ":8_0",
	} {
		_, err := Load(env(map[string]string{
			"LAWANG_DATABASE_URL": "postgres://app@db:5432/lawang",
			"LAWANG_LISTEN_ADDR":  addr,
		}))
		if err == nil || !strings.Contains(err.Error(), "LAWANG_LISTEN_ADDR") {
			t.Errorf("LAWANG_LISTEN_ADDR=%q: err = %v, want a LAWANG_LISTEN_ADDR error", addr, err)
			continue
		}
		assertNoSecret(t, "error", err.Error())
		// The port half is just as likely to be the pasted secret as the host half.
		if _, port, ok := strings.Cut(addr, ":"); ok && len(port) > 1 && strings.Contains(err.Error(), port) {
			t.Errorf("LAWANG_LISTEN_ADDR=%q: error repeats the port %q: %v", addr, port, err)
		}
	}
}

func TestLoadAcceptsEveryOrdinaryListenAddress(t *testing.T) {
	// The numeric port rule must not cost any address a deployment would really use.
	for _, addr := range []string{
		":8080", ":0", ":65535", "[::1]:8080", "localhost:8080", "0.0.0.0:0", "[::1%lo0]:0",
		"127.0.0.1:9000", "[::]:8080",
	} {
		cfg, err := Load(env(map[string]string{
			"LAWANG_DATABASE_URL": "postgres://app@db:5432/lawang",
			"LAWANG_LISTEN_ADDR":  addr,
		}))
		if err != nil {
			t.Errorf("LAWANG_LISTEN_ADDR=%q: %v", addr, err)
			continue
		}
		if cfg.ListenAddr != addr {
			t.Errorf("LAWANG_LISTEN_ADDR=%q: ListenAddr = %q", addr, cfg.ListenAddr)
		}
	}
}

func TestConfigNeverPrintsTheDatabaseURL(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"LAWANG_DATABASE_URL": "postgres://leakuser:hunter2@leakhost:5432/leakdb",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, verb := range []string{"%v", "%+v", "%s", "%#v"} {
		assertNoSecret(t, verb, fmt.Sprintf(verb, cfg))
		assertNoSecret(t, verb+" of a pointer", fmt.Sprintf(verb, &cfg))
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("config", "cfg", cfg, "ptr", &cfg)
	assertNoSecret(t, "slog output", buf.String())
	if !strings.Contains(buf.String(), `"listen_addr":":8080"`) {
		t.Errorf("slog output lost the fields that are safe to print: %s", buf.String())
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Load(env(map[string]string{
		"LAWANG_LOG_LEVEL":  "loud",
		"LAWANG_LOG_FORMAT": "xml",
	}))
	if err == nil {
		t.Fatal("Load accepted an invalid environment")
	}
	for _, want := range []string{"LAWANG_DATABASE_URL", "LAWANG_LOG_LEVEL", "LAWANG_LOG_FORMAT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"LAWANG_DATABASE_URL": "postgresql://app@db/lawang",
		"LAWANG_LISTEN_ADDR":  "127.0.0.1:9000",
		"LAWANG_LOG_LEVEL":    "debug",
		"LAWANG_LOG_FORMAT":   "TEXT",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9000" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want text", cfg.LogFormat)
	}
}

// readBy records every name Load asks getenv for, under base.
func readBy(base map[string]string) map[string]bool {
	read := make(map[string]bool)
	_, _ = Load(func(k string) string {
		read[k] = true
		return base[k]
	})
	return read
}

// validSamples are values Load accepts for the variables that are not enumerations. An enumerated
// variable needs none: its states come from Variable.Values.
var validSamples = map[string][]string{
	"LAWANG_DATABASE_URL":    {"postgres://app@db/lawang", "postgresql://app@db:5432/lawang"},
	"LAWANG_LISTEN_ADDR":     {":9000", "127.0.0.1:9000"},
	"LAWANG_PUBLIC_BASE_URL": {"https://lawang.example.test", "https://lawang.example.test/prefix"},
}

// recordingBases is the full cross product of the states of every documented variable: unset,
// each accepted value (or each valid sample), and one refused value. It is a product and not a
// hand-picked list because a hand-picked list is exactly where a read behind two conditions hid
// (development together with an explicit database URL).
func recordingBases(t *testing.T) []map[string]string {
	t.Helper()
	bases := []map[string]string{{}}
	for _, v := range Variables() {
		accepted := v.Values
		if accepted == nil {
			accepted = validSamples[v.Name]
		}
		if len(accepted) == 0 {
			t.Fatalf("%s has neither Values nor an entry in validSamples, so no base would ever set it to something Load accepts", v.Name)
		}
		// A state only drives the path it is named for if Load really takes it that way. A sample
		// that has rotted into a refused value would silently leave the accepted path undriven.
		for _, state := range accepted {
			if _, err := Load(env(map[string]string{"LAWANG_DATABASE_URL": "postgres://app@db/lawang", v.Name: state})); err != nil {
				t.Fatalf("%s: Load refuses the state %q that stands for an accepted value: %v", v.Name, state, err)
			}
		}
		if _, err := Load(env(map[string]string{"LAWANG_DATABASE_URL": "postgres://app@db/lawang", v.Name: "x"})); err == nil {
			t.Fatalf("%s: Load accepts \"x\", which stands for a refused value", v.Name)
		}
		states := append([]string{"", "x"}, accepted...)

		var next []map[string]string
		for _, base := range bases {
			for _, state := range states {
				m := maps.Clone(base)
				if state != "" {
					m[v.Name] = state
				}
				next = append(next, m)
			}
		}
		bases = next
	}
	return bases
}

func TestVariablesAreExactlyWhatLoadReads(t *testing.T) {
	// Variables is the operator's documentation, by way of the usage text. A variable Load reads
	// and Variables omits is undocumented; one Variables lists and Load ignores is a promise the
	// binary does not keep.
	//
	// Recording can only see the paths it drives. It drives every combination of {unset, each
	// accepted value, one refused value} over the documented variables, so a read behind any
	// conjunction of those states is seen. What it cannot see: a read behind a condition that is
	// none of those states, such as one particular host in the database URL, or a second variable
	// that is itself undocumented and so is never set here. A new kind of branch in Load needs a
	// new state in recordingBases.
	bases := recordingBases(t)
	if len(bases) < 1000 {
		t.Fatalf("only %d bases, the cross product has collapsed", len(bases))
	}
	read := make(map[string]bool)
	for _, base := range bases {
		for k := range readBy(base) {
			read[k] = true
		}
	}

	documented := make(map[string]bool)
	for _, v := range Variables() {
		if documented[v.Name] {
			t.Errorf("%s is listed twice", v.Name)
		}
		documented[v.Name] = true
		if !read[v.Name] {
			t.Errorf("%s is documented, but Load never reads it", v.Name)
		}
		if v.Doc == "" || v.Default == "" {
			t.Errorf("%s has no description or no default: %+v", v.Name, v)
		}
	}
	for k := range read {
		if !documented[k] {
			t.Errorf("Load reads %s, but Variables does not document it", k)
		}
	}
}

func TestVariablesStateTheDefaultsLoadApplies(t *testing.T) {
	// The documented default is compared with what Load does with the variable unset, so the two
	// cannot drift. The database URL is the only thing set, because production has no default for it.
	doc := make(map[string]string)
	for _, v := range Variables() {
		doc[v.Name] = v.Default
	}

	prod, err := Load(env(map[string]string{"LAWANG_DATABASE_URL": "postgres://app@db/lawang"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dev, err := Load(env(map[string]string{"LAWANG_ENV": "development"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := doc["LAWANG_ENV"]; got != string(prod.Env) {
		t.Errorf("LAWANG_ENV: documented default %q, Load applies %q", got, prod.Env)
	}
	if got := doc["LAWANG_LISTEN_ADDR"]; got != prod.ListenAddr {
		t.Errorf("LAWANG_LISTEN_ADDR: documented default %q, Load applies %q", got, prod.ListenAddr)
	}
	if got, want := doc["LAWANG_LOG_LEVEL"], strings.ToLower(prod.LogLevel.String()); got != want {
		t.Errorf("LAWANG_LOG_LEVEL: documented default %q, Load applies %q", got, want)
	}
	wantFormat := prod.LogFormat + " in production, " + dev.LogFormat + " in development"
	if got := doc["LAWANG_LOG_FORMAT"]; got != wantFormat {
		t.Errorf("LAWANG_LOG_FORMAT: documented default %q, Load applies %q", got, wantFormat)
	}
	if got := doc["LAWANG_DATABASE_URL"]; !strings.Contains(got, "required in production") {
		t.Errorf("LAWANG_DATABASE_URL: the documented default %q does not say production has none", got)
	}
	// The public base URL has no default on purpose, and the documented default says what an
	// unset one costs rather than naming a value. Guessing one would put a URL Lawang invented
	// into a signature base string, which is a check that proves nothing.
	if prod.PublicBaseURL != "" || dev.PublicBaseURL != "" {
		t.Errorf("LAWANG_PUBLIC_BASE_URL unset gave %q in production and %q in development, want empty in both",
			prod.PublicBaseURL, dev.PublicBaseURL)
	}
	if got := doc["LAWANG_PUBLIC_BASE_URL"]; !strings.Contains(got, "none") || !strings.Contains(got, "refuses") {
		t.Errorf("LAWANG_PUBLIC_BASE_URL: the documented default %q does not say there is none and what that costs", got)
	}

	// The development fallback is a URL with a password in it. It is described, never printed.
	// The description names where the process will connect, which is not secret, and nothing else.
	u, err := url.Parse(dev.DatabaseURL)
	if err != nil {
		t.Fatal("the development database URL does not parse")
	}
	password, _ := u.User.Password()
	if password == "" {
		t.Fatal("the development database URL has no password, so the check below proves nothing")
	}
	where := u.Host + ", database " + strings.TrimPrefix(u.Path, "/")
	if got := doc["LAWANG_DATABASE_URL"]; !strings.Contains(got, where) {
		t.Errorf("LAWANG_DATABASE_URL: the documented default %q does not say development connects to %q", got, where)
	}
	for _, v := range Variables() {
		if strings.Contains(v.Doc+v.Default, dev.DatabaseURL) || strings.Contains(v.Doc+v.Default, "@") ||
			strings.Contains(v.Doc+v.Default, u.User.String()) {
			t.Errorf("%s: the documentation prints a URL with credentials: %+v", v.Name, v)
		}
	}
}

func TestVariablesStateTheValuesLoadAccepts(t *testing.T) {
	// Values is the very slice Load validates against, so the two agree by construction. This
	// holds the construction in place from the outside: every listed value is accepted, the
	// refusal lists exactly the listed values, and a handful of plausible unlisted ones are
	// refused. The last part is a sample: no test can try every string, so a value that Load
	// accepts by some route other than the slice, and that nobody thought to list below, would
	// still get through.
	const dbURL = "postgres://app@db/lawang"
	unlisted := []string{"x", "dev", "staging", "test", "prod", "trace", "fatal", "info+2", "logfmt", "console", "pretty"}

	enumerated := make(map[string]bool)
	for _, v := range Variables() {
		if v.Values == nil {
			continue
		}
		enumerated[v.Name] = true
		for _, value := range v.Values {
			if _, err := Load(env(map[string]string{"LAWANG_DATABASE_URL": dbURL, v.Name: value})); err != nil {
				t.Errorf("%s=%s is documented as accepted, but Load refuses it: %v", v.Name, value, err)
			}
		}
		for _, value := range unlisted {
			_, err := Load(env(map[string]string{"LAWANG_DATABASE_URL": dbURL, v.Name: value}))
			if err == nil {
				t.Errorf("%s=%s is not documented, but Load accepts it", v.Name, value)
				continue
			}
			if want := v.Name + ": must be one of: " + strings.Join(v.Values, ", "); err.Error() != want {
				t.Errorf("%s=%s: the refusal is %q, want %q", v.Name, value, err, want)
			}
		}
	}
	for _, name := range []string{"LAWANG_ENV", "LAWANG_LOG_LEVEL", "LAWANG_LOG_FORMAT"} {
		if !enumerated[name] {
			t.Errorf("%s is an enumeration in Load, but Variables does not list its values", name)
		}
	}

	// Variables hands out copies: a caller that edits one must not change what Load accepts.
	// The edit is undone at the end, so that a failure here does not spill into the other tests.
	values := Variables()[0].Values
	original := values[0]
	values[0] = "edited"
	defer func() { values[0] = original }()
	if _, err := Load(env(map[string]string{"LAWANG_DATABASE_URL": dbURL, "LAWANG_ENV": "edited"})); err == nil {
		t.Error("editing the result of Variables changed what Load accepts")
	}
}

// TestThePublicBaseURLIsNormalizedAndNeverGuessed covers the variable a signature base string is
// built from. The edge concatenates a request path onto this string and a provider's HMAC is
// taken over the result, so an accepted value must come back in exactly one spelling, and
// anything the edge could not concatenate onto safely must be refused at start rather than turned
// into a signature that never verifies.
// acceptedBaseURLs is what NormalizePublicBaseURL takes, and the one spelling it returns.
//
// It is a package-level table because two tests read it: the one below, which checks each answer,
// and TestNormalizingTheResultAgainChangesNothing, which checks the property that would have
// caught the shapes nobody thought to put in a table.
var acceptedBaseURLs = map[string]string{
	"https://lawang.example.test":         "https://lawang.example.test",
	"https://lawang.example.test/":        "https://lawang.example.test",
	"https://lawang.example.test:8443":    "https://lawang.example.test:8443",
	"http://localhost:8080":               "http://localhost:8080",
	"https://lawang.example.test/lawang":  "https://lawang.example.test/lawang",
	"https://lawang.example.test/lawang/": "https://lawang.example.test/lawang",
	"https://lawang.example.test/a/b":     "https://lawang.example.test/a/b",
	"HTTPS://lawang.example.test":         "https://lawang.example.test",
	// A default port and the host's case are deliberately kept: this string has to match the URL
	// the operator registered with the provider, which they copied from its dashboard.
	"https://EXAMPLE.com:443": "https://EXAMPLE.com:443",
	"https://[::1]:8443/":     "https://[::1]:8443",
	// An internationalized host, in the one spelling a provider's dashboard holds. The kanji
	// spelling of the same name is refused, so that an operator hears about it at start rather
	// than as a 401 per delivery.
	"https://xn--80ak6aa92e.com":       "https://xn--80ak6aa92e.com",
	"https://xn--caf-dma.example/a/b/": "https://xn--caf-dma.example/a/b",
	// Trailing slashes, however many. TrimSuffix would leave "https://x//" as "https://x/", which
	// keeps the trailing slash the function promises to remove, and the second pass then removes
	// it: Load and ingress.New would hold two different strings for one variable and every
	// signature over the URL would fail.
	"https://lawang.example.test//":         "https://lawang.example.test",
	"https://lawang.example.test///":        "https://lawang.example.test",
	"https://lawang.example.test/lawang//":  "https://lawang.example.test/lawang",
	"https://lawang.example.test:8443//":    "https://lawang.example.test:8443",
	"HTTPS://lawang.example.test/lawang///": "https://lawang.example.test/lawang",
	// A sub-delimiter url.Parse leaves alone in a path. Rebuilding the answer with
	// url.URL.String escaped it to %21, which the next pass refused: the fuzzer found this in
	// under a second, and testdata/fuzz keeps it as a seed.
	"https://lawang.example.test/a!b": "https://lawang.example.test/a!b",
}

// refusedBaseURLs is what NormalizePublicBaseURL refuses, by the name of the shape.
var refusedBaseURLs = map[string]string{
	"no scheme":                "lawang.example.test",
	"a scheme-relative URL":    "//lawang.example.test",
	"a path only":              "/ingress",
	"an unsupported scheme":    "ftp://lawang.example.test",
	"a postgres URL":           "postgres://app:hunter2@db:5432/lawang",
	"no host":                  "https://",
	"credentials":              "https://user:pass@lawang.example.test",
	"a query":                  "https://lawang.example.test?x=1",
	"a bare question mark":     "https://lawang.example.test?",
	"a fragment":               "https://lawang.example.test#f",
	"a percent-encoded prefix": "https://lawang.example.test/a%2Fb",
	"a relative segment":       "https://lawang.example.test/a/../b",
	"a doubled slash":          "https://lawang.example.test/a//b",
	"a leading doubled slash":  "https://lawang.example.test//a",
	"a control character":      "https://lawang.example.test/\x00",
	"not a URL at all":         "://",
	"just a word":              "x",
	// The escapes Go re-spells to themselves, which a RawPath check does not see. Each one used
	// to be accepted and decoded, and the decoded value was then refused by the next pass: an
	// escaped space came back with a literal space in it, an escaped hash became a fragment and
	// an escaped question mark became a query.
	"an escaped space":         "https://lawang.example.test/a%20b",
	"an escaped hash":          "https://lawang.example.test/a%23b",
	"an escaped question mark": "https://lawang.example.test/a%3Fb",
	"an escaped dot":           "https://lawang.example.test/%2e",
	"a literal space":          "https://lawang.example.test/a b",
	"a prefix outside ASCII":   "https://lawang.example.test/café",
	"a single dot segment":     "https://lawang.example.test/.",
	"a parent segment":         "https://lawang.example.test/..",
	// url.Parse decodes escapes in the host, so this one parses with a host of "%" and the
	// answer was a string no second pass could read back. The fuzzer found it, and
	// testdata/fuzz keeps it as a seed.
	"a percent-escape in the host": "http://%25",
	"an IPv6 zone id":              "http://[fe80::1%25eth0]:8080",
	// A URL that names a port and no machine. u.Host is ":8080", which is not empty, so the
	// check that catches "https://" does not catch this one, and neither can the idempotence
	// property: it parses, and it is its own normal form. LAWANG_LISTEN_ADDR takes exactly this
	// spelling, which is how it gets written here.
	"a port and no host":    "http://:8080",
	"a colon and no host":   "http://:",
	"an empty port":         "http://lawang.example.test:",
	"a host outside ASCII":  "https://café.example",
	"an IDN host in kanji":  "http://例え.jp",
	"an IDN host with path": "https://café.example/lawang",
}

func TestThePublicBaseURLIsNormalizedAndNeverGuessed(t *testing.T) {
	t.Parallel()

	accepted := acceptedBaseURLs
	for raw, want := range accepted {
		t.Run("accepts "+raw, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizePublicBaseURL(raw)
			if err != nil {
				t.Fatalf("NormalizePublicBaseURL(%q): %v", raw, err)
			}
			if got != want {
				t.Fatalf("NormalizePublicBaseURL(%q) = %q, want %q", raw, got, want)
			}
		})
	}

	refused := refusedBaseURLs
	for name, raw := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizePublicBaseURL(raw)
			if err == nil {
				t.Fatalf("NormalizePublicBaseURL(%q) = %q, want a refusal", raw, got)
			}
			if got != "" {
				t.Fatalf("a refusal returned %q as well", got)
			}
			// No refusal echoes the value: Load cannot know which variable a secret was pasted
			// into, and the refusal is printed to a container log.
			if strings.Contains(err.Error(), raw) {
				t.Fatalf("the refusal quotes what it was given: %v", err)
			}
		})
	}
}

// baseURLCorpus is what the idempotence property is checked over: every input either table
// already holds, plus every combination of a scheme, a host and a path shape that has ever been
// interesting here. Two more tables would only cover what somebody thought of; this covers the
// product, which is where "https://x//" was hiding.
func baseURLCorpus() []string {
	var corpus []string
	for raw := range acceptedBaseURLs {
		corpus = append(corpus, raw)
	}
	for _, raw := range refusedBaseURLs {
		corpus = append(corpus, raw)
	}
	schemes := []string{"http://", "https://", "HTTPS://", "ftp://", ""}
	hosts := []string{"x", "x:443", "x:8443", "EXAMPLE.com", "[::1]:8443", "localhost", ""}
	paths := []string{
		"", "/", "//", "///", "/a", "/a/", "/a//", "//a", "/a//b", "/a/b", "/a/b/",
		"/a/../b", "/a/./b", "/.", "/..", "/a%20b", "/a%2Fb", "/a%2e", "/a b", "/café",
		"/a%23b", "/a%3Fb", "/a\x00", "/a?b", "/a#b", "/A/B",
	}
	for _, s := range schemes {
		for _, h := range hosts {
			for _, p := range paths {
				corpus = append(corpus, s+h+p)
			}
		}
	}
	return corpus
}

// TestNormalizingTheResultAgainChangesNothing is the property the tables cannot state. The same
// value is normalized more than once in a running deployment (Load normalizes the variable,
// ingress.New normalizes what Load stored, and a Registrar registers what Load stored), and the
// spellings meet inside an HMAC base string. A rule that moves a value on the second pass makes
// the edge sign one string while the provider signed another, which is a 401 on every delivery
// and reads as a forgery.
func TestNormalizingTheResultAgainChangesNothing(t *testing.T) {
	t.Parallel()

	accepted := 0
	for _, raw := range baseURLCorpus() {
		got, err := NormalizePublicBaseURL(raw)
		if err != nil {
			// A refusal is a fixed point of nothing: a refused value never becomes an output.
			continue
		}
		accepted++
		checkNormalizedFixedPoint(t, raw, got)
	}
	// A guard against the corpus quietly becoming all refusals, which would make this test pass
	// while proving nothing.
	if accepted < 50 {
		t.Fatalf("only %d of the corpus was accepted: the property is not being exercised", accepted)
	}
}

// checkNormalizedFixedPoint holds one accepted answer to everything the doc comment promises:
// a second pass returns it unchanged, and the result really is the shape the edge concatenates
// onto.
func checkNormalizedFixedPoint(t *testing.T, raw, got string) {
	t.Helper()

	again, err := NormalizePublicBaseURL(got)
	if err != nil {
		t.Errorf("NormalizePublicBaseURL(%q) = %q, and that answer is then refused: %v", raw, got, err)
		return
	}
	if again != got {
		t.Errorf("not idempotent: %q normalizes to %q, which normalizes to %q", raw, got, again)
		return
	}
	if strings.HasSuffix(got, "/") {
		t.Errorf("NormalizePublicBaseURL(%q) = %q, which keeps a trailing slash", raw, got)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Errorf("NormalizePublicBaseURL(%q) = %q, which is not a URL: %v", raw, got, err)
		return
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		t.Errorf("NormalizePublicBaseURL(%q) = %q, which has no host or no usable scheme", raw, got)
	}
	if u.EscapedPath() != u.Path {
		t.Errorf("NormalizePublicBaseURL(%q) = %q, whose path is not its own escaped form", raw, got)
	}
	// The premise of the function dropping its old "must be absolute" check on the path: with a
	// host present, url.Parse never produces a relative path. If this ever fires, that check has
	// to come back, because the edge concatenates a request path straight onto this string.
	if u.Path != "" && !strings.HasPrefix(u.Path, "/") {
		t.Errorf("NormalizePublicBaseURL(%q) = %q, whose path is relative", raw, got)
	}
}

// FuzzNormalizePublicBaseURL keeps the property open-ended: go test runs the seeds, and go test
// -fuzz runs whatever the fuzzer invents. Nothing here asserts what the answer should be, only
// that an accepted answer is one this function would return again unchanged.
func FuzzNormalizePublicBaseURL(f *testing.F) {
	for _, raw := range baseURLCorpus() {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := NormalizePublicBaseURL(raw)
		if err != nil {
			if got != "" {
				t.Errorf("a refusal of %q returned %q as well", raw, got)
			}
			return
		}
		checkNormalizedFixedPoint(t, raw, got)
	})
}

// TestLoadCarriesThePublicBaseURLThrough holds Load to the same rules, since the edge is handed
// Config.PublicBaseURL and not the raw variable.
func TestLoadCarriesThePublicBaseURLThrough(t *testing.T) {
	t.Parallel()

	const dbURL = "postgres://app@db/lawang"
	cfg, err := Load(env(map[string]string{
		"LAWANG_DATABASE_URL":    dbURL,
		"LAWANG_PUBLIC_BASE_URL": "https://lawang.example.test/",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PublicBaseURL != "https://lawang.example.test" {
		t.Fatalf("PublicBaseURL = %q, want the normalized form", cfg.PublicBaseURL)
	}

	_, err = Load(env(map[string]string{
		"LAWANG_DATABASE_URL":    dbURL,
		"LAWANG_PUBLIC_BASE_URL": "postgres://app:hunter2@db:5432/lawang",
	}))
	if err == nil {
		t.Fatal("Load accepted a public base URL that is not one")
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "db:5432") {
		t.Fatalf("the refusal echoes the value, which may be a database URL pasted into the wrong variable: %v", err)
	}
	if !strings.Contains(err.Error(), "LAWANG_PUBLIC_BASE_URL") {
		t.Fatalf("the refusal does not name the variable: %v", err)
	}
}
