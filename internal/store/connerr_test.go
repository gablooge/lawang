package store

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// marker stands in for a part of the database URL, carried the way the real errors carry it.
const marker = "leakvalue"

// markerAddr stands in for the address a net.OpError carries and prints.
type markerAddr string

func (markerAddr) Network() string  { return "tcp" }
func (a markerAddr) String() string { return string(a) }

// timeoutErr is a net.Error that reports a timeout and quotes the marker.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "dial tcp " + marker + ": i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

func TestConnErrorKeepsOnlyTheClassification(t *testing.T) {
	bg := context.Background()
	cancelled, cancel := context.WithCancel(bg)
	cancel()

	dial := func(inner error) error {
		return fmt.Errorf("failed to connect to `user=%s database=%s`: %w", marker, marker,
			&net.OpError{Op: "dial", Net: "tcp", Addr: markerAddr(marker), Err: inner})
	}
	server := func(code, msg string) error {
		return fmt.Errorf("failed to connect to `user=%s`: server error: %w", marker,
			&pgconn.PgError{Severity: "FATAL", Code: code, Message: msg, Detail: marker, Hint: marker})
	}

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"wrong password", bg, server("28P01", `password authentication failed for user "`+marker+`"`), "authentication failed, check the user, the password and pg_hba.conf (SQLSTATE 28P01)"},
		{"no pg_hba entry", bg, server("28000", `no pg_hba.conf entry for host "`+marker+`"`), "authentication failed, check the user, the password and pg_hba.conf (SQLSTATE 28000)"},
		{"missing database", bg, server("3D000", `database "`+marker+`" does not exist`), "the database does not exist (SQLSTATE 3D000)"},
		{"no CONNECT privilege", bg, server("42501", `permission denied for database "`+marker+`"`), "may not connect to the database (SQLSTATE 42501)"},
		{"too many connections", bg, server("53300", `too many connections for role "`+marker+`"`), "no connection slot left (SQLSTATE 53300)"},
		{"starting up", bg, server("57P03", "the database system is starting up "+marker), "not accepting connections yet (SQLSTATE 57P03)"},
		{"any other SQLSTATE", bg, server("XX000", marker), "the server refused (SQLSTATE XX000)"},
		{"a SQLSTATE that is not one", bg, server(marker, marker), "no usable SQLSTATE"},
		{"a SQLSTATE of the right length that is not one", bg, server("leak!", marker), "no usable SQLSTATE"},
		// The server's verdict wins over the network error pgx joins to it under sslmode=prefer.
		{"server verdict joined to a TLS refusal", bg, errors.Join(errors.New("server refused TLS connection"), server("28P01", marker)), "SQLSTATE 28P01"},
		{"refused", bg, dial(os.NewSyscallError("connect", syscall.ECONNREFUSED)), "connection refused"},
		{"unreachable", bg, dial(os.NewSyscallError("connect", syscall.ENETUNREACH)), "network is unreachable"},
		{"no such host", bg, dial(&net.DNSError{Err: "no such host", Name: marker, IsNotFound: true}), "the host name does not resolve"},
		{"lookup timeout", bg, dial(&net.DNSError{Err: "i/o timeout", Name: marker, IsTimeout: true}), "the host name lookup timed out"},
		{"lookup failure", bg, dial(&net.DNSError{Err: "server misbehaving", Name: marker, Server: marker}), "the host name lookup failed"},
		{"connect timeout", bg, dial(timeoutErr{}), "timed out"},
		{"deadline of pgx's connect_timeout", bg, fmt.Errorf("%s: dial error: %w", marker, context.DeadlineExceeded), "timed out"},
		{"cancelled", cancelled, dial(errors.New("operation was canceled " + marker)), "context canceled"},
		{"certificate not trusted", bg, fmt.Errorf("%s: %w", marker, x509.UnknownAuthorityError{}), "TLS negotiation failed"},
		{"certificate for another host", bg, fmt.Errorf("%s: %w", marker, x509.HostnameError{Certificate: &x509.Certificate{}, Host: marker}), "TLS negotiation failed"},
		{"server declines TLS", bg, fmt.Errorf("%s: %w", marker, errors.New("server refused TLS connection")), "TLS negotiation failed"},
		{"handshake failure as pgx words it", bg, fmt.Errorf("%s: tls error: %w", marker, errors.New("EOF")), "TLS negotiation failed"},
		{"anything else", bg, errors.New("unexpected " + marker), "the connection failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.err.Error(), marker) {
				t.Fatalf("the input does not carry the marker, so the case proves nothing: %v", tc.err)
			}
			got := connError(tc.ctx, tc.err).Error()
			if !strings.HasPrefix(got, "LAWANG_DATABASE_URL: ") || !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want it to name the variable and say %q", got, tc.want)
			}
			if strings.Contains(got, marker) {
				t.Errorf("error leaks %q: %s", marker, got)
			}
		})
	}
}

func TestConnErrorKeepsWhatCallersMatchOn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := connError(ctx, errors.New(marker)); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled to survive", err)
	}
	refused := &net.OpError{Op: "dial", Net: "tcp", Addr: markerAddr(marker), Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	if err := connError(context.Background(), refused); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("err = %v, want ECONNREFUSED to survive", err)
	}
}

func TestScrubReplacesOnlyWhatCarriesTheConnectionTarget(t *testing.T) {
	ctx := context.Background()
	if err := scrub(ctx, nil); err != nil {
		t.Errorf("scrub(nil) = %v", err)
	}

	// What a transaction body returns has to reach the caller as it is: callers match on the
	// SQLSTATE, and tests on the message.
	violation := fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505", Message: "duplicate key"})
	got := scrub(ctx, violation)
	var pgErr *pgconn.PgError
	if !errors.Is(got, violation) || got.Error() != violation.Error() || !errors.As(got, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("scrub changed a constraint violation into %v", got)
	}
	own := errors.New("store: something of our own")
	if got := scrub(ctx, own); !errors.Is(got, own) || got.Error() != own.Error() {
		t.Errorf("scrub changed a plain error into %v", got)
	}

	// What carries an address does not. A real connect error from pgx, against a port nothing
	// listens on: its cause cannot be set from outside the package.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, connectErr := pgconn.Connect(dialCtx, "postgres://"+marker+"@"+addr+"/"+marker+"?sslmode=disable")
	var asConnect *pgconn.ConnectError
	if !errors.As(connectErr, &asConnect) || !strings.Contains(connectErr.Error(), marker) {
		t.Fatalf("pgconn.Connect to a closed port returned %v, want a ConnectError that carries the marker", connectErr)
	}

	for name, err := range map[string]error{
		"pgx connect error": fmt.Errorf("begin: %w", connectErr),
		"net error mid-transaction": fmt.Errorf("commit: %w",
			&net.OpError{Op: "write", Net: "tcp", Addr: markerAddr(marker), Err: os.NewSyscallError("write", syscall.EPIPE)}),
		"lookup": &net.DNSError{Err: "no such host", Name: marker, IsNotFound: true},
	} {
		got := scrub(ctx, err)
		if got == nil || strings.Contains(got.Error(), marker) || !strings.HasPrefix(got.Error(), "LAWANG_DATABASE_URL: ") {
			t.Errorf("%s: scrub returned %v, want a classified error without %q", name, got, marker)
		}
	}
}

func TestPoolConfigSetsAConnectTimeout(t *testing.T) {
	cfg, err := poolConfig("postgres://app@db.example:5432/lawang")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ConnConfig.ConnectTimeout; got != 10*time.Second {
		t.Errorf("with no connect_timeout in the URL, ConnectTimeout = %s, want 10s", got)
	}
	if got := cfg.ConnConfig.RuntimeParams["search_path"]; got != Schema {
		t.Errorf("search_path = %q, want %q", got, Schema)
	}

	cfg, err = poolConfig("postgres://app@db.example:5432/lawang?connect_timeout=3")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ConnConfig.ConnectTimeout; got != 3*time.Second {
		t.Errorf("with connect_timeout=3 in the URL, ConnectTimeout = %s, want the operator's 3s", got)
	}

	// Deliberate: 0 is "wait forever" in libpq, and here it is the default. pgx parses it to the
	// same zero as a missing parameter, and an unbounded start is what the default exists to
	// prevent. If pgx ever starts telling the two apart, this row is where that gets noticed.
	cfg, err = poolConfig("postgres://app@db.example:5432/lawang?connect_timeout=0")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ConnConfig.ConnectTimeout; got != 10*time.Second {
		t.Errorf("with connect_timeout=0 in the URL, ConnectTimeout = %s, want the 10s default: 0 must not lift the bound", got)
	}
}

func TestCheckVersion(t *testing.T) {
	for _, tc := range []struct {
		version int
		ok      bool
	}{
		{90624, false},
		{150019, false},
		{159999, false},
		{160000, true},
		{160015, true},
		{170002, true},
		{0, false},
	} {
		err := checkVersion(tc.version)
		if (err == nil) != tc.ok {
			t.Errorf("checkVersion(%d) = %v, want ok=%v", tc.version, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "Postgres 16 or newer") {
			t.Errorf("checkVersion(%d): the refusal does not say what is required: %v", tc.version, err)
		}
	}
}
