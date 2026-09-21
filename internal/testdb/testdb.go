// Package testdb gives integration tests a real Postgres.
//
// One container is started per test process. Each test gets its own bootstrapped database inside
// it, and connects as the non-superuser application role, because tests that run as a superuser
// pass without row-level security ever applying (docs/architecture.md, principle 12).
//
// Roles, unlike databases, are cluster-wide. A test that changes a role or a membership would
// change it for every other test in the process, so it takes a Postgres of its own from
// NewRawCluster instead of the shared one.
package testdb

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/gablooge/lawang/internal/ids"
	"github.com/gablooge/lawang/migrations"
)

const (
	image       = "postgres:17-alpine"
	appPassword = "lawang_test" //nolint:gosec // a throwaway container that lives as long as the test process
)

var (
	once     sync.Once
	adminURL *url.URL
	startErr error

	// Roles are cluster-wide, so two databases bootstrapping at once would race on CREATE ROLE.
	bootstrapMu sync.Mutex
)

// Database is one test's database.
type Database struct {
	// URL connects as the application role. Use this one.
	URL string
	// AdminURL connects as the superuser, for setup and for proving what a superuser is refused.
	AdminURL string
}

// New returns a fresh database with the bootstrap script applied and no migrations run.
func New(t testing.TB) Database {
	t.Helper()
	db := NewRaw(t)
	Bootstrap(t, db.AdminURL)
	return db
}

// Bootstrap applies the bootstrap script over connURL, as whichever administrator that URL logs
// in as, and gives the application role the password that Database.URL uses.
func Bootstrap(t testing.TB, connURL string) {
	t.Helper()
	if err := TryBootstrap(t, connURL); err != nil {
		t.Fatalf("testdb: bootstrap: %v", err)
	}
	Exec(t, connURL, fmt.Sprintf("ALTER ROLE lawang PASSWORD '%s'", appPassword))
}

// TryBootstrap applies the bootstrap script and returns the server's verdict, for tests that
// expect a refusal.
//
// It uses the extended protocol, which takes exactly one statement. The script promises to apply
// completely or not at all under any client, psql included, which sends statements one by one
// and carries on after an error. That only holds while the script is a single statement, and the
// simple protocol would hide a second one by wrapping the lot in one implicit transaction.
func TryBootstrap(t testing.TB, connURL string) error {
	t.Helper()
	bootstrapMu.Lock()
	defer bootstrapMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, connURL)
	if err != nil {
		t.Fatalf("testdb: connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.PgConn().ExecParams(ctx, migrations.Bootstrap, nil, nil, nil, nil).Close()
	return err
}

// NewRaw returns a fresh database with nothing in it, not even the bootstrap.
func NewRaw(t testing.TB) Database {
	t.Helper()
	once.Do(start)
	if startErr != nil {
		// A developer without Docker can still run the unit tests. CI may never skip these.
		if os.Getenv("CI") == "" {
			t.Skipf("no Postgres container (is Docker running?): %v", startErr)
		}
		t.Fatalf("no Postgres container: %v", startErr)
	}

	return newDatabase(t, adminURL)
}

// NewRawCluster returns a fresh database, with nothing in it, in a Postgres container of its own
// that is removed when the test ends. It costs a container start, so it is only for tests that
// change cluster-wide state: role attributes, memberships, or who runs the bootstrap. No role
// exists in it yet, not even the application role, until the test calls Bootstrap.
func NewRawCluster(t testing.TB) Database {
	t.Helper()
	once.Do(start)
	if startErr != nil {
		// Same rule as NewRaw. Asking the shared container first keeps the skip in one place.
		if os.Getenv("CI") == "" {
			t.Skipf("no Postgres container (is Docker running?): %v", startErr)
		}
		t.Fatalf("no Postgres container: %v", startErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, clusterURL, err := run(ctx)
	if c != nil {
		t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
	}
	if err != nil {
		t.Fatalf("testdb: private Postgres container: %v", err)
	}
	return newDatabase(t, clusterURL)
}

func newDatabase(t testing.TB, cluster *url.URL) Database {
	t.Helper()
	name := "t_" + strings.ToLower(ids.New())
	Exec(t, cluster.String(), "CREATE DATABASE "+name)

	admin := *cluster
	admin.Path = "/" + name
	app := admin
	app.User = url.UserPassword("lawang", appPassword)
	return Database{URL: app.String(), AdminURL: admin.String()}
}

// As returns connURL with the login replaced, for tests that connect as a role of their own.
func As(t testing.TB, connURL, user, password string) string {
	t.Helper()
	u, err := url.Parse(connURL)
	if err != nil {
		// Not wrapped: the error would quote the URL.
		t.Fatal("testdb: invalid connection URL")
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

// Exec runs SQL, which may hold several statements, over a one-off connection.
func Exec(t testing.TB, connURL, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, connURL)
	if err != nil {
		t.Fatalf("testdb: connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	// The simple protocol, because the extended one takes a single statement.
	if _, err := conn.PgConn().Exec(ctx, sql).ReadAll(); err != nil {
		t.Fatalf("testdb: exec: %v", err)
	}
}

func start() {
	defer func() {
		// testcontainers panics, rather than returning an error, when it finds no Docker host.
		if r := recover(); r != nil {
			startErr = fmt.Errorf("%v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Not terminated here: the testcontainers reaper removes it when the test process exits.
	_, adminURL, startErr = run(ctx)
}

// run starts one Postgres container and returns the superuser URL of its "postgres" database.
func run(ctx context.Context) (testcontainers.Container, *url.URL, error) {
	c, err := postgres.Run(ctx, image,
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		// A container that started but never became ready is still returned, so it can be removed.
		if c == nil {
			return nil, nil, err
		}
		return c, nil, err
	}
	raw, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return c, nil, err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return c, nil, errors.New("testdb: the container returned an unparsable connection string")
	}
	return c, u, nil
}
