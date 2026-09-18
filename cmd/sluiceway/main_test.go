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
	// An environment with no SLUICEWAY_ENV is production, and production has no database URL to
	// guess.
	//
	// The test has to fail, not hang, if the refusal ever regresses. A role that wrongly starts
	// would otherwise block forever on the real :8080. So the only variable set is a loopback
	// address with an ephemeral port, which cannot collide with anything, and the context has a
	// short deadline: a serve that starts is stopped by it and exits 0, which the assertion below
	// reports as the wrong exit code.
	getenv := func(k string) string {
		if k == "SLUICEWAY_LISTEN_ADDR" {
			return "127.0.0.1:0"
		}
		return ""
	}
	for _, cmd := range []string{"serve", "worker", "migrate"} {
		t.Run(cmd, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			var out, errOut bytes.Buffer
			code := run(ctx, []string{cmd}, getenv, &out, &errOut)
			if code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			if !strings.Contains(errOut.String(), "invalid configuration") ||
				!strings.Contains(errOut.String(), "SLUICEWAY_DATABASE_URL") {
				t.Errorf("stderr is not a configuration refusal naming the missing variable: %s", errOut.String())
			}
			if ctx.Err() != nil {
				t.Error("run was still going at the deadline: the role started instead of refusing")
			}
		})
	}
}

func TestRunNeverPrintsAConfigurationValue(t *testing.T) {
	// The refusal goes to stderr, which is the container log. A database URL pasted into the wrong
	// variable must not reach it.
	const secret = "postgres://leakuser:hunter2@leakhost:5432/leakdb"
	getenv := func(k string) string {
		switch k {
		case "SLUICEWAY_DATABASE_URL":
			return "postgres://leakuser:hunter2%zz@leakhost:5432/leakdb"
		case "SLUICEWAY_ENV", "SLUICEWAY_LISTEN_ADDR", "SLUICEWAY_LOG_LEVEL", "SLUICEWAY_LOG_FORMAT":
			return secret
		}
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"serve"}, getenv, &out, &errOut); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	for _, frag := range []string{"hunter2", "leakuser", "leakhost", "leakdb"} {
		if strings.Contains(out.String()+errOut.String(), frag) {
			t.Errorf("output leaks %q: %s%s", frag, out.String(), errOut.String())
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
	for _, cmd := range []string{"worker", "migrate"} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), []string{cmd}, dev, &out, &errOut); code != 1 {
			t.Errorf("%s: exit = %d, want 1", cmd, code)
		}
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
