package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadUnsetEnvironmentIsProduction(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"SLUICEWAY_DATABASE_URL": "postgres://app@db:5432/sluiceway",
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
		_, err := Load(env(map[string]string{"SLUICEWAY_ENV": e}))
		if err == nil || !strings.Contains(err.Error(), "SLUICEWAY_DATABASE_URL") {
			t.Errorf("SLUICEWAY_ENV=%q: err = %v, want a SLUICEWAY_DATABASE_URL error", e, err)
		}
	}
}

func TestLoadDevelopmentDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"SLUICEWAY_ENV": "development"}))
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
		"SLUICEWAY_ENV":          "dev",
		"SLUICEWAY_DATABASE_URL": "postgres://app@db:5432/sluiceway",
	}))
	if err == nil || !strings.Contains(err.Error(), "SLUICEWAY_ENV") {
		t.Fatalf("err = %v, want a SLUICEWAY_ENV error", err)
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
			_, err := Load(env(map[string]string{"SLUICEWAY_DATABASE_URL": raw}))
			if err == nil {
				t.Fatal("Load accepted a bad database URL")
			}
			if !strings.Contains(err.Error(), "SLUICEWAY_DATABASE_URL") {
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
		"SLUICEWAY_ENV",
		"SLUICEWAY_LISTEN_ADDR",
		"SLUICEWAY_LOG_LEVEL",
		"SLUICEWAY_LOG_FORMAT",
	}

	for _, name := range validated {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(map[string]string{
				"SLUICEWAY_DATABASE_URL": "postgres://app@db:5432/sluiceway",
				name:                     secret,
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
		all := map[string]string{"SLUICEWAY_DATABASE_URL": "mysql://leakuser:hunter2@leakhost:3306/leakdb"}
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
	for _, addr := range []string{"8080", "localhost", "http://localhost:8080"} {
		_, err := Load(env(map[string]string{
			"SLUICEWAY_DATABASE_URL": "postgres://app@db:5432/sluiceway",
			"SLUICEWAY_LISTEN_ADDR":  addr,
		}))
		if err == nil || !strings.Contains(err.Error(), "SLUICEWAY_LISTEN_ADDR") {
			t.Errorf("SLUICEWAY_LISTEN_ADDR=%q: err = %v, want a SLUICEWAY_LISTEN_ADDR error", addr, err)
		}
	}
}

func TestConfigNeverPrintsTheDatabaseURL(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"SLUICEWAY_DATABASE_URL": "postgres://leakuser:hunter2@leakhost:5432/leakdb",
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
		"SLUICEWAY_LOG_LEVEL":  "loud",
		"SLUICEWAY_LOG_FORMAT": "xml",
	}))
	if err == nil {
		t.Fatal("Load accepted an invalid environment")
	}
	for _, want := range []string{"SLUICEWAY_DATABASE_URL", "SLUICEWAY_LOG_LEVEL", "SLUICEWAY_LOG_FORMAT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"SLUICEWAY_DATABASE_URL": "postgresql://app@db/sluiceway",
		"SLUICEWAY_LISTEN_ADDR":  "127.0.0.1:9000",
		"SLUICEWAY_LOG_LEVEL":    "debug",
		"SLUICEWAY_LOG_FORMAT":   "TEXT",
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
