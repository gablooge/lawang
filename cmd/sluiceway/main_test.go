package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func noEnv(string) string { return "" }

func TestRunVersionNeedsNoConfiguration(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"version"}, noEnv, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Error("version printed nothing")
	}
}

func TestRunUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"frobnicate"}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, noEnv, &out, &errOut); code != 2 {
			t.Errorf("args %v: exit = %d, want 2", args, code)
		}
		if !strings.Contains(errOut.String(), "Usage:") {
			t.Errorf("args %v: stderr has no usage: %s", args, errOut.String())
		}
	}
}

func TestRunRefusesToStartWithoutConfiguration(t *testing.T) {
	// An empty environment is production, and production has no database URL to guess.
	for _, cmd := range []string{"serve", "worker", "migrate"} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), []string{cmd}, noEnv, &out, &errOut); code != 1 {
			t.Errorf("%s: exit = %d, want 1", cmd, code)
		}
		if !strings.Contains(errOut.String(), "SLUICEWAY_DATABASE_URL") {
			t.Errorf("%s: stderr does not name the missing variable: %s", cmd, errOut.String())
		}
	}
}

func TestRunStubbedRolesExitNonZero(t *testing.T) {
	dev := func(k string) string {
		if k == "SLUICEWAY_ENV" {
			return "development"
		}
		return ""
	}
	for _, cmd := range []string{"worker"} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), []string{cmd}, dev, &out, &errOut); code != 1 {
			t.Errorf("%s: exit = %d, want 1", cmd, code)
		}
	}
}

func TestMigrateBootstrapPrintsTheScriptWithoutConfiguration(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"migrate", "bootstrap"}, noEnv, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	for _, want := range []string{"CREATE ROLE sluiceway ", "NOBYPASSRLS", "WITH INHERIT FALSE", "CREATE SCHEMA IF NOT EXISTS sluiceway"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("bootstrap script does not contain %q", want)
		}
	}

	out.Reset()
	if code := run(context.Background(), []string{"migrate", "sideways"}, noEnv, &out, &errOut); code != 2 {
		t.Errorf("migrate sideways: exit = %d, want 2", code)
	}
}

func TestServeAnswersHealthzAndShutsDownCleanly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveOn(ctx, ln, slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Errorf("GET /healthz = %d %q", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveOn returned %v after a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveOn did not return after cancel")
	}
}
