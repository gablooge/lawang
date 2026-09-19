package store_test

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gablooge/sluiceway/internal/store"
	"github.com/gablooge/sluiceway/internal/testdb"
)

// Markers that stand in for the parts of SLUICEWAY_DATABASE_URL. None may reach an error, because
// the binary logs what Open returns.
const (
	leakUser = "leakuser"
	leakPass = "hunter2"
	leakHost = "leakhost"
	leakDB   = "leakdb"
)

// failOpen runs Open, which must fail, under a deadline of its own: a connect that hangs fails the
// test instead of stalling the package.
func failOpen(t *testing.T, databaseURL string, deadline time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	db, err := store.Open(ctx, databaseURL)
	if db != nil {
		db.Close()
	}
	if err == nil {
		t.Fatal("Open succeeded, want a refusal")
	}
	if ctx.Err() != nil {
		t.Fatalf("Open was still going at the %s deadline: %v", deadline, err)
	}
	return err
}

func assertNoLeak(t *testing.T, err error, markers ...string) {
	t.Helper()
	for _, m := range markers {
		if m != "" && strings.Contains(err.Error(), m) {
			t.Errorf("error leaks %q: %v", m, err)
		}
	}
}

func TestOpenNeverEchoesTheURL(t *testing.T) {
	// pgx masks the password in its parse error and nothing else, so the user, the host and the
	// database are what tell a wrapped parse error from a replaced one.
	err := failOpen(t, "postgres://"+leakUser+":"+leakPass+"@"+leakHost+":5432/"+leakDB+"?pool_max_conns=banana", 5*time.Second)
	assertNoLeak(t, err, leakUser, leakPass, leakHost, leakDB, "banana")
	if !strings.Contains(err.Error(), "SLUICEWAY_DATABASE_URL") {
		t.Errorf("error does not name the variable: %v", err)
	}
}

// TestOpenConnectFailuresNeverEchoTheURL drives the failures an operator really meets. pgx and the
// server both quote the connection target in every one of them; Open must keep the classification
// and drop the rest.
func TestOpenConnectFailuresNeverEchoTheURL(t *testing.T) {
	t.Run("host that does not resolve", func(t *testing.T) {
		// ".invalid" is reserved and never resolves (RFC 6761), with or without a network.
		err := failOpen(t, "postgres://"+leakUser+":"+leakPass+"@"+leakHost+".invalid:5432/"+leakDB+"?sslmode=disable&connect_timeout=5", 15*time.Second)
		assertNoLeak(t, err, leakUser, leakPass, leakHost, leakDB, "5432")
		if !strings.Contains(err.Error(), "SLUICEWAY_DATABASE_URL") || !strings.Contains(err.Error(), "host name") {
			t.Errorf("error does not name the variable and the cause: %v", err)
		}
	})

	t.Run("closed local port", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_, port, _ := net.SplitHostPort(addr)
		_ = ln.Close()

		err = failOpen(t, "postgres://"+leakUser+":"+leakPass+"@"+addr+"/"+leakDB+"?sslmode=disable&connect_timeout=5", 15*time.Second)
		assertNoLeak(t, err, leakUser, leakPass, leakDB, port, "127.0.0.1")
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Errorf("err = %v, want ECONNREFUSED to survive the classification", err)
		}
		if !strings.Contains(err.Error(), "SLUICEWAY_DATABASE_URL") || !strings.Contains(err.Error(), "refused") {
			t.Errorf("error does not name the variable and the cause: %v", err)
		}
	})

	tdb := testdb.New(t)
	target, err := url.Parse(tdb.URL)
	if err != nil {
		t.Fatal("the test database URL does not parse")
	}
	realDB := strings.TrimPrefix(target.Path, "/")

	t.Run("wrong password", func(t *testing.T) {
		u := *target
		// A role that does not exist is answered exactly like a wrong password, and lets the user
		// be a marker too.
		u.User = url.UserPassword(leakUser, leakPass)
		err := failOpen(t, u.String(), 15*time.Second)
		assertNoLeak(t, err, leakUser, leakPass, realDB, target.Port(), target.Hostname())
		if !strings.Contains(err.Error(), "SLUICEWAY_DATABASE_URL") || !strings.Contains(err.Error(), "authentication failed") ||
			!strings.Contains(err.Error(), "28P01") {
			t.Errorf("error does not name the variable, the cause and the SQLSTATE: %v", err)
		}
	})

	t.Run("missing database", func(t *testing.T) {
		u := *target
		u.Path = "/" + leakDB
		err := failOpen(t, u.String(), 15*time.Second)
		assertNoLeak(t, err, leakDB, "sluiceway_test", target.Port(), target.Hostname())
		// The user here is "sluiceway", which is also the schema and the variable prefix, so it
		// cannot be searched for. The wrong password case covers the user.
		if !strings.Contains(err.Error(), "SLUICEWAY_DATABASE_URL") || !strings.Contains(err.Error(), "database does not exist") ||
			!strings.Contains(err.Error(), "3D000") {
			t.Errorf("error does not name the variable, the cause and the SQLSTATE: %v", err)
		}
	})
}

// A refusal that is about the login role must not repeat its name either: the name is the user
// half of the URL.
func TestPreflightRefusalsNeverNameTheLogin(t *testing.T) {
	tdb := testdb.New(t)
	testdb.Exec(t, tdb.AdminURL, "CREATE ROLE "+leakUser+" LOGIN BYPASSRLS PASSWORD '"+leakPass+"'")
	t.Cleanup(func() { testdb.Exec(t, tdb.AdminURL, "DROP ROLE "+leakUser) })

	err := failOpen(t, testdb.As(t, tdb.URL, leakUser, leakPass), 15*time.Second)
	if !errors.Is(err, store.ErrUnsafeRole) {
		t.Fatalf("Open as a BYPASSRLS role: err = %v, want ErrUnsafeRole", err)
	}
	assertNoLeak(t, err, leakUser, leakPass)
}

// TestOpenGivesUpOnAHostThatDropsPackets pins that a connect timeout reaches the dialer and comes
// back classified. 192.0.2.0/24 is a documentation range that is routed nowhere (RFC 5737), so a
// SYN sent to it is normally never answered: without a timeout Open would sit there for the TCP
// timeout of the operating system, 75 seconds or more, with nothing logged. That Open applies a
// default when the URL sets none is pinned by TestPoolConfigSetsAConnectTimeout, which needs no
// network; this test uses a short one from the URL so that it takes a second and not ten.
func TestOpenGivesUpOnAHostThatDropsPackets(t *testing.T) {
	start := time.Now()
	err := failOpen(t, "postgres://"+leakUser+":"+leakPass+"@192.0.2.1:5432/"+leakDB+"?sslmode=disable&connect_timeout=1", 8*time.Second)
	elapsed := time.Since(start)

	assertNoLeak(t, err, leakUser, leakPass, leakDB, "192.0.2.1", "5432")
	if !strings.Contains(err.Error(), "timed out") {
		// Some environments answer for the documentation range at once (no route, an unreachable
		// reply from a gateway, a sandbox without a network). Then nothing was dropped and there
		// is no hang to bound. The point here is only that Open does not hang.
		t.Skipf("this environment answered for 192.0.2.1 after %s instead of dropping the packets, so the timeout was not exercised: %v", elapsed.Round(time.Millisecond), err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Open took %s with connect_timeout=1", elapsed.Round(time.Millisecond))
	}
}
