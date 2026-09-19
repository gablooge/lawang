package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
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

func TestRunNeverPrintsAnUnknownCommand(t *testing.T) {
	// The first argument is whatever was typed first. An image whose entrypoint is the bare binary,
	// run with the database URL as its only argument, puts the password there, and stderr is the
	// container log. Nothing after it is printed either.
	const secret = "postgres://leakuser:hunter2@leakhost:5432/leakdb"
	for _, args := range [][]string{{secret}, {secret, "serve"}, {"--" + secret}} {
		var out, errOut bytes.Buffer
		if code := run(context.Background(), args, noEnv, &out, &errOut); code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
		if !strings.HasPrefix(errOut.String(), "sluiceway: unknown command\n") || !strings.Contains(errOut.String(), "Usage:") {
			t.Errorf("stderr does not say the command is unknown and how to use the binary: %.60s", errOut.String())
		}
		if out.Len() != 0 {
			t.Errorf("stdout is not empty: %.60s", out.String())
		}
		for _, frag := range []string{"hunter2", "leakuser", "leakhost", "leakdb", "postgres://leak"} {
			if strings.Contains(out.String()+errOut.String(), frag) {
				t.Errorf("output leaks %q from the first argument", frag)
			}
		}
	}
}

func TestRunRefusesExtraArguments(t *testing.T) {
	// No command may ignore an argument it does not take: "serve --listen :9090" would otherwise
	// start on the default port with no warning.
	//
	// The environment is a valid development one, so that the only thing wrong is the argument.
	// A role that wrongly starts must fail this test, not hang it: serve gets a loopback address
	// with an ephemeral port and is stopped by the deadline, which shows up as exit 0.
	getenv := func(k string) string {
		switch k {
		case "SLUICEWAY_ENV":
			return "development"
		case "SLUICEWAY_LISTEN_ADDR":
			return "127.0.0.1:0"
		}
		return ""
	}
	const extra = "--hunter2-marker"
	for _, tc := range []struct {
		args []string
		want string // what the first line of stderr must say
	}{
		{[]string{"serve", extra}, "serve takes no arguments"},
		{[]string{"serve", "--listen", ":9090"}, "serve takes no arguments"},
		{[]string{"worker", extra}, "worker takes no arguments"},
		// A command that grows a subcommand keeps its row and changes only what it says.
		{[]string{"migrate", extra}, "migrate takes no arguments"},
		{[]string{"version", extra}, "version takes no arguments"},
		{[]string{"help", extra}, "help takes no arguments"},
		{[]string{"-h", extra}, "-h takes no arguments"},
		{[]string{"--help", extra}, "--help takes no arguments"},
	} {
		args := tc.args
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			var out, errOut bytes.Buffer
			if code := run(ctx, args, getenv, &out, &errOut); code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			// The usage text is long, so a failure shows the first line only.
			firstLine := func(s string) string { line, _, _ := strings.Cut(s, "\n"); return line }
			if got := firstLine(errOut.String()); !strings.Contains(got, tc.want) {
				t.Errorf("stderr starts %q, want it to say %q", got, tc.want)
			}
			if !strings.Contains(strings.ToLower(errOut.String()), "usage:") {
				t.Errorf("stderr does not show how the command is used: %s", firstLine(errOut.String()))
			}
			if out.Len() != 0 {
				t.Errorf("stdout is not empty: %s", firstLine(out.String()))
			}
			// An argument can be a pasted secret as easily as a variable can.
			if strings.Contains(errOut.String(), "hunter2") || strings.Contains(errOut.String(), "9090") {
				t.Errorf("stderr repeats the argument: %s", firstLine(errOut.String()))
			}
			if ctx.Err() != nil {
				t.Error("run was still going at the deadline: the role started instead of refusing")
			}
		})
	}
}

func TestRunHelp(t *testing.T) {
	for _, spelling := range []string{"help", "-h", "--help"} {
		t.Run(spelling, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(context.Background(), []string{spelling}, noEnv, &out, &errOut); code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if out.String() != usage {
				t.Errorf("stdout is not the usage text: %s", out.String())
			}
			if errOut.Len() != 0 {
				t.Errorf("asking for help is not an error, but stderr has: %s", errOut.String())
			}
		})
	}
}

func TestUsageDocumentsTheConfigurationAndTheExitCodes(t *testing.T) {
	// The usage text is the only documentation an operator is sure to have. Every variable
	// config.Load reads has to be in it, found here by watching what Load asks for and not by
	// trusting a list. This recording drives the two environments only. The thorough one, over
	// every combination of states, is TestVariablesAreExactlyWhatLoadReads in the config package,
	// and the loop over config.Variables below carries its result into the usage text.
	read := make(map[string]bool)
	for _, envName := range []string{"", "development"} {
		_, _ = config.Load(func(k string) string {
			read[k] = true
			if k == "SLUICEWAY_ENV" {
				return envName
			}
			return ""
		})
	}
	if len(read) == 0 {
		t.Fatal("config.Load read nothing, so this test proves nothing")
	}
	for name := range read {
		if !strings.Contains(usage, name) {
			t.Errorf("config.Load reads %s, which the usage text does not mention", name)
		}
	}
	for _, v := range config.Variables() {
		if !strings.Contains(usage, v.Doc) || !strings.Contains(usage, "default: "+v.Default) {
			t.Errorf("the usage text does not carry the description and default of %s", v.Name)
		}
		if len(v.Values) > 0 && !strings.Contains(usage, "values: "+strings.Join(v.Values, ", ")+"\n") {
			t.Errorf("the usage text does not list the values %s accepts", v.Name)
		}
	}
	if !strings.Contains(usage, "\n  help ") {
		t.Error("the usage text does not list help among the commands")
	}

	for _, line := range []string{
		"Exit codes:",
		"  0   ",
		"  1   ",
		"  2   ",
	} {
		if !strings.Contains(usage, "\n"+line) {
			t.Errorf("the usage text has no line starting %q", line)
		}
	}
	// Still one screen, and still the commands first.
	if !strings.HasPrefix(usage, "Usage: sluiceway <command>") {
		t.Errorf("the usage text does not start with the synopsis: %.40s", usage)
	}
	if strings.Contains(usage, "@") || strings.Contains(usage, "sluiceway:sluiceway") {
		t.Error("the usage text prints a URL with credentials")
	}
}

// lockedBuffer is a bytes.Buffer that a running role can log to while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunServeExitsZeroAfterACleanShutdown(t *testing.T) {
	// The whole path an operator sees: run serve, answer a request, take the signal (here the
	// cancel that main wires to SIGINT and SIGTERM), exit 0. Nothing else proves that a clean
	// shutdown is not reported as a failure.
	//
	// It cannot collide (loopback, ephemeral port) and it cannot hang: every wait below is bounded,
	// and the cleanup cancels the role whatever happened.
	getenv := func(k string) string {
		switch k {
		case "SLUICEWAY_ENV":
			return "development"
		case "SLUICEWAY_LISTEN_ADDR":
			return "127.0.0.1:0"
		case "SLUICEWAY_LOG_FORMAT":
			return "json"
		}
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	var out, errOut lockedBuffer
	exit := make(chan int, 1)
	go func() { exit <- run(ctx, []string{"serve"}, getenv, &out, &errOut) }()

	// The port is ephemeral, so the only place to learn it is the "listening" log line.
	var addr string
	for deadline := time.Now().Add(5 * time.Second); addr == "" && time.Now().Before(deadline); {
		for line := range strings.SplitSeq(errOut.String(), "\n") {
			var entry struct{ Msg, Addr string }
			if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "listening" {
				addr = entry.Addr
			}
		}
		select {
		case code := <-exit:
			t.Fatalf("run serve returned %d before it was asked to stop: %s", code, errOut.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if addr == "" {
		t.Fatalf("serve never logged that it was listening: %s", errOut.String())
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}
	client.CloseIdleConnections()

	cancel()
	select {
	case code := <-exit:
		if code != 0 {
			t.Errorf("exit = %d after a clean shutdown, want 0: %s", code, errOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("run serve did not return after the cancel: %s", errOut.String())
	}
	if strings.Contains(errOut.String(), `"level":"ERROR"`) {
		t.Errorf("a clean shutdown logged an error: %s", errOut.String())
	}
	if out.String() != "" {
		t.Errorf("serve wrote to stdout: %s", out.String())
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
	for _, want := range []string{"CREATE ROLE sluiceway ", "NOBYPASSRLS", "WITH INHERIT FALSE", "CREATE SCHEMA sluiceway AUTHORIZATION sluiceway"} {
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
