package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gablooge/sluiceway/internal/config"
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

func TestRunNeverPrintsTheListenAddress(t *testing.T) {
	// Everything else in the environment is valid, so whatever is printed comes from the listen
	// address alone. Load only vouches for the shape and the port; the rest is up to serve, whose
	// listen failure must not repeat the address the way the error from net does.
	//
	// None of this needs a network. The ".invalid" top level domain is reserved and never resolves
	// (RFC 6761), and 192.0.2.0/24 is a documentation range no interface carries (RFC 5737), so
	// binding to it fails at once. The deadline bounds a resolver that is slow to say no, and a
	// serve that wrongly starts: either way run returns and the assertions do the failing.
	cases := []struct {
		name, addr string
		want       string // what stderr must still say
		markers    []string
	}{
		// Refused by Load: exactly one colon, which net.SplitHostPort alone lets through.
		{"user and password", "leakuser:hunter2", "invalid configuration", []string{"leakuser", "hunter2"}},
		{"passwordless database URL", "postgres://leakuser@leakhost/leakdb", "invalid configuration", []string{"leakuser", "leakhost", "leakdb"}},
		{"port out of range", "leakhost.invalid:99999", "invalid configuration", []string{"leakhost", "99999"}},
		// Accepted by Load, refused by the listener.
		{"host that does not resolve", "leakhost.invalid:8080", "SLUICEWAY_LISTEN_ADDR", []string{"leakhost"}},
		{"address of no local interface", "192.0.2.77:8080", "SLUICEWAY_LISTEN_ADDR", []string{"192.0.2.77"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string {
				switch k {
				case "SLUICEWAY_DATABASE_URL":
					return "postgres://app@db:5432/sluiceway"
				case "SLUICEWAY_LISTEN_ADDR":
					return tc.addr
				}
				return ""
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			var out, errOut bytes.Buffer
			if code := run(ctx, []string{"serve"}, getenv, &out, &errOut); code != 1 {
				t.Errorf("exit = %d, want 1", code)
			}
			printed := out.String() + errOut.String()
			if !strings.Contains(printed, tc.want) {
				t.Errorf("output does not mention %q: %s", tc.want, printed)
			}
			for _, frag := range tc.markers {
				if strings.Contains(printed, frag) {
					t.Errorf("output leaks %q: %s", frag, printed)
				}
			}
		})
	}
}

func TestServeSaysWhenTheAddressIsInUse(t *testing.T) {
	// Keeping the address out of the error must not cost the operator the one distinction that
	// matters most in practice: "something else has the port" against "the value is wrong".
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = serve(ctx, config.Config{ListenAddr: ln.Addr().String()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("serve on a taken port returned %v, want EADDRINUSE", err)
	}
	if !strings.Contains(err.Error(), "SLUICEWAY_LISTEN_ADDR") || !strings.Contains(err.Error(), "in use") {
		t.Errorf("error does not name the variable and the cause: %v", err)
	}
	if strings.Contains(err.Error(), port) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error repeats the address: %v", err)
	}
}

// markerAddr stands in for the address a net.OpError carries and prints.
type markerAddr string

func (markerAddr) Network() string  { return "tcp" }
func (a markerAddr) String() string { return string(a) }

func TestListenErrorKeepsOnlyTheClassification(t *testing.T) {
	// Every shape of error net can hand back, each carrying the marker the way the real ones carry
	// the address. Only the classification may survive.
	const marker = "leakhost"
	bg := context.Background()
	cancelled, cancel := context.WithCancel(bg)
	cancel()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"in use", bg, &net.OpError{Op: "listen", Net: "tcp", Addr: markerAddr(marker), Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}, "in use"},
		{"not permitted", bg, &net.OpError{Op: "listen", Net: "tcp", Addr: markerAddr(marker), Err: os.NewSyscallError("bind", syscall.EACCES)}, "permission denied"},
		{"no such host", bg, &net.OpError{Op: "listen", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: marker, IsNotFound: true}}, "does not resolve"},
		{"lookup timeout", bg, &net.OpError{Op: "listen", Net: "tcp", Err: &net.DNSError{Err: "i/o timeout", Name: marker, IsTimeout: true}}, "timed out"},
		{"lookup failure", bg, &net.OpError{Op: "listen", Net: "tcp", Err: &net.DNSError{Err: "server misbehaving", Name: marker, Server: marker}}, "lookup failed"},
		{"bad address", bg, &net.OpError{Op: "listen", Net: "tcp", Err: &net.AddrError{Err: "invalid port", Addr: marker}}, "not valid"},
		{"cancelled", cancelled, &net.OpError{Op: "listen", Net: "tcp", Err: &net.DNSError{Err: "operation was canceled", Name: marker}}, "context canceled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.err.Error(), marker) {
				t.Fatalf("the input does not carry the marker, so the case proves nothing: %v", tc.err)
			}
			got := listenError(tc.ctx, tc.err).Error()
			if !strings.Contains(got, "SLUICEWAY_LISTEN_ADDR") || !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want it to name the variable and say %q", got, tc.want)
			}
			if strings.Contains(got, marker) {
				t.Errorf("error leaks %q: %s", marker, got)
			}
		})
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
