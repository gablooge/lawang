package store_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/sluiceway/internal/store"
	"github.com/gablooge/sluiceway/internal/testdb"
)

// forwarder is a TCP listener on the loopback that relays every connection to target. It stands
// between a pool and the test database so that a test can take the database away after Open has
// succeeded, which is what a restart or a failover looks like from inside the process.
type forwarder struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	conns []net.Conn
	gone  bool

	wg      sync.WaitGroup
	cutOnce sync.Once
}

func newForwarder(t *testing.T, target string) *forwarder {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &forwarder{ln: ln, target: target}
	f.wg.Add(1)
	go f.accept()
	t.Cleanup(func() { f.cut(t) })
	return f
}

func (f *forwarder) addr() string { return f.ln.Addr().String() }

// track remembers a connection so that cut can close it, and refuses it once cut has run.
func (f *forwarder) track(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone {
		_ = c.Close()
		return false
	}
	f.conns = append(f.conns, c)
	return true
}

func (f *forwarder) accept() {
	defer f.wg.Done()
	for {
		client, err := f.ln.Accept()
		if err != nil {
			return // the listener was closed
		}
		if !f.track(client) {
			continue
		}
		server, err := net.DialTimeout("tcp", f.target, 10*time.Second)
		if err != nil {
			_ = client.Close()
			continue
		}
		if !f.track(server) {
			_ = client.Close()
			continue
		}
		f.wg.Add(2)
		go f.relay(server, client)
		go f.relay(client, server)
	}
}

func (f *forwarder) relay(dst, src net.Conn) {
	defer f.wg.Done()
	_, _ = io.Copy(dst, src)
	// One direction ending ends the pair, which is what unblocks the other relay.
	_ = dst.Close()
	_ = src.Close()
}

// cut stops listening and closes every relayed connection, both halves, then waits (bounded) for
// the relays to end. From then on a connect to addr is refused, and a connection the pool still
// holds is dead. It is safe to call twice.
func (f *forwarder) cut(t *testing.T) {
	t.Helper()
	f.cutOnce.Do(func() {
		_ = f.ln.Close()
		f.mu.Lock()
		f.gone = true
		for _, c := range f.conns {
			_ = c.Close()
		}
		f.mu.Unlock()

		done := make(chan struct{})
		go func() {
			f.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the forwarder's relays were still running 10s after the cut")
		}
	})
}

// errorTexts walks err to every depth, through Unwrap() error and Unwrap() []error, and returns
// each node printed the two ways a log line prints an error. A marker that survives only on an
// inner error is still one errors.Unwrap away from a log. %#v is left out on purpose: it prints
// pointer values, and a port number can turn up inside one by chance.
func errorTexts(err error) []string {
	var texts []string
	var walk func(error, int)
	walk = func(e error, depth int) {
		if e == nil || depth > 32 {
			return
		}
		texts = append(texts, fmt.Sprintf("%v", e), fmt.Sprintf("%+v", e))
		switch u := e.(type) { //nolint:errorlint // the walk needs the node itself, not a match somewhere below it
		case interface{ Unwrap() error }:
			walk(u.Unwrap(), depth+1)
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				walk(inner, depth+1)
			}
		}
	}
	walk(err, 0)
	return texts
}

func assertNoLeakAtAnyDepth(t *testing.T, name string, err error, markers ...string) {
	t.Helper()
	var leaked []string
	for _, m := range markers {
		if m == "" {
			continue
		}
		for _, text := range errorTexts(err) {
			if strings.Contains(text, m) {
				leaked = append(leaked, m)
				break
			}
		}
	}
	if len(leaked) > 0 {
		// One line per error, not one per marker and depth: a leak trips nearly all of them.
		t.Errorf("%s: the error, or one it wraps, leaks %q: %v", name, leaked, err)
	}
}

// TestALostDatabaseNeverEchoesTheURL pins that every entry point that can be the first to meet a
// database that has gone away (Tx, TenantTx, RoleTx and Migrate) replaces the driver's error.
//
// The leak tests of Open cannot see this. Open fails in its preflight, before begin or Migrate is
// ever reached, and the pool connects lazily: after a restart of the database it is the next
// transaction, not Open, that gets pgx's "failed to connect to `user=... database=...`: dial tcp
// host:port". A begin rewritten without its scrub, or a new entry point that forgets it, would
// put that line into the worker's log with every other test green.
func TestALostDatabaseNeverEchoesTheURL(t *testing.T) {
	tdb := testdb.New(t)
	target, err := url.Parse(tdb.URL)
	if err != nil {
		t.Fatal("the test database URL does not parse")
	}
	fwd := newForwarder(t, target.Host)
	fwdHost, fwdPort, err := net.SplitHostPort(fwd.addr())
	if err != nil {
		t.Fatal(err)
	}

	through := *target
	through.Host = fwd.addr()
	query := through.Query()
	// One connection, so that one failed call is enough to empty the pool after the cut. A short
	// connect timeout, so that a connect that hangs where it should be refused fails the test.
	query.Set("pool_max_conns", "1")
	query.Set("connect_timeout", "5")
	through.RawQuery = query.Encode()

	openCtx, cancelOpen := context.WithTimeout(context.Background(), time.Minute)
	defer cancelOpen()
	db, err := store.Open(openCtx, through.String())
	if err != nil {
		t.Fatalf("Open through the forwarder: %v", err)
	}
	t.Cleanup(func() { closeWithin(t, db, 10*time.Second) })

	// Everything the pool was told, and everything it could learn: pgx quotes the user and the
	// database, the dialer quotes the address. The variable name is upper case, so the role name
	// in lower case is a marker too, as are the labels pgx puts in front of the values.
	password, _ := target.User.Password()
	markers := []string{
		target.User.Username(), password, strings.TrimPrefix(target.Path, "/"),
		fwd.addr(), fwdHost, fwdPort, target.Host, target.Port(),
		"user=", "database=", "dial tcp",
	}

	fwd.cut(t)

	mustNotRun := func(name string) func(pgx.Tx) error {
		return func(pgx.Tx) error {
			t.Errorf("%s ran its function with no database behind the pool", name)
			return nil
		}
	}
	call := func(run func(context.Context) error) error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := run(ctx)
		if ctx.Err() != nil {
			t.Fatalf("the call was still going at its 20s deadline: %v", err)
		}
		return err
	}

	// The pool still holds the connection Open used, and that connection is dead. Whatever the
	// call that finds it returns (a reset, an EOF, a broken pipe, each of them in the middle of a
	// transaction) must not leak either. That call also removes the connection from the pool, so
	// the calls after it have to connect, and are refused. Bounded, and no sleep: a pool of one
	// connection needs one call.
	drained := false
	for attempt := 1; attempt <= 5 && !drained; attempt++ {
		err := call(func(ctx context.Context) error { return db.Tx(ctx, mustNotRun("Tx")) })
		if err == nil {
			t.Fatal("Tx succeeded with no database behind the pool")
		}
		assertNoLeakAtAnyDepth(t, fmt.Sprintf("Tx on the dead connection (attempt %d)", attempt), err, markers...)
		drained = errors.Is(err, syscall.ECONNREFUSED)
	}
	if !drained {
		t.Fatal("after 5 calls the pool was still not trying to connect, so the refused connect was never exercised")
	}

	for _, entry := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"Tx", func(ctx context.Context) error { return db.Tx(ctx, mustNotRun("Tx")) }},
		{"TenantTx", func(ctx context.Context) error { return db.TenantTx(ctx, tenantA, mustNotRun("TenantTx")) }},
		{"RoleTx", func(ctx context.Context) error { return db.RoleTx(ctx, store.RoleWorker, mustNotRun("RoleTx")) }},
		{"Migrate", func(ctx context.Context) error {
			_, err := db.Migrate(ctx, quiet)
			return err
		}},
	} {
		err := call(entry.run)
		if err == nil {
			t.Errorf("%s succeeded with no database behind the pool", entry.name)
			continue
		}
		assertNoLeakAtAnyDepth(t, entry.name, err, markers...)
		if !strings.Contains(err.Error(), "SLUICEWAY_DATABASE_URL") || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: the error does not name the variable and the cause: %v", entry.name, err)
		}
		// What a caller may match on has to survive: a retry loop tells "refused" from the rest.
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Errorf("%s: err = %v, want ECONNREFUSED to survive the replacement", entry.name, err)
		}
	}
}

// TestANetErrorFromTheTransactionFunctionIsReplacedToo documents a decision, it does not defend a
// nicety. The replacement judges the error and not where it came from (see begin), so a network
// error of the function's own comes back as a database connection error and loses what was wrapped
// around it. Network I/O inside a transaction is forbidden anyway. Whoever changes this has to
// keep the other half true: a statement that fails inside the function on a dead connection is
// returned BY the function, carries the address, and must still be replaced.
func TestANetErrorFromTheTransactionFunctionIsReplacedToo(t *testing.T) {
	db := open(t, "")
	ctx := testCtx(t)

	const farEnd = "provider.leakhost.example:443"
	sentinel := errors.New("a sentinel of the caller")
	ownNetErr := fmt.Errorf("%w: %w", sentinel, &net.OpError{
		Op: "read", Net: "tcp", Addr: addrOf(farEnd), Err: os.NewSyscallError("read", syscall.ECONNRESET),
	})

	for name, run := range map[string]func(func(pgx.Tx) error) error{
		"Tx":       func(fn func(pgx.Tx) error) error { return db.Tx(ctx, fn) },
		"TenantTx": func(fn func(pgx.Tx) error) error { return db.TenantTx(ctx, tenantA, fn) },
		"RoleTx":   func(fn func(pgx.Tx) error) error { return db.RoleTx(ctx, store.RoleWorker, fn) },
	} {
		err := run(func(pgx.Tx) error { return ownNetErr })
		if err == nil || !strings.HasPrefix(err.Error(), "SLUICEWAY_DATABASE_URL: ") {
			t.Errorf("%s: err = %v, want the net error replaced", name, err)
			continue
		}
		if errors.Is(err, sentinel) {
			t.Errorf("%s: the caller's sentinel survived. If that is now intended, update the doc comment of begin and docs/architecture.md section 4", name)
		}
		if !errors.Is(err, syscall.ECONNRESET) {
			t.Errorf("%s: err = %v, want the errno to survive", name, err)
		}
		assertNoLeakAtAnyDepth(t, name, err, farEnd, "leakhost")

		// The contrast: an error that is not a net error is the caller's, sentinel and text.
		plain := fmt.Errorf("lookup failed: %w", sentinel)
		if err := run(func(pgx.Tx) error { return plain }); !errors.Is(err, sentinel) || err.Error() != plain.Error() {
			t.Errorf("%s: a plain error came back as %v, want it untouched", name, err)
		}
	}
}

// addrOf is a net.Addr with a chosen text.
type addrOf string

func (addrOf) Network() string  { return "tcp" }
func (a addrOf) String() string { return string(a) }
