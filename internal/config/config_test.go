package config

import (
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

func TestLoadDatabaseURLErrorsNeverEchoTheValue(t *testing.T) {
	_, err := Load(env(map[string]string{
		"SLUICEWAY_DATABASE_URL": "mysql://app:hunter2@db:3306/sluiceway",
	}))
	if err == nil {
		t.Fatal("Load accepted a non-Postgres URL")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks the password: %v", err)
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
