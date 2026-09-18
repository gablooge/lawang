// Package testdb gives integration tests a real Postgres.
//
// One container is started per test process. Each test gets its own bootstrapped database inside
// it, and connects as the non-superuser application role, because tests that run as a superuser
// pass without row-level security ever applying (docs/architecture.md, principle 12).
package testdb

import (
	"context"
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

	"github.com/gablooge/sluiceway/internal/ids"
	"github.com/gablooge/sluiceway/migrations"
)

const (
	image       = "postgres:17-alpine"
	appPassword = "sluiceway_test" //nolint:gosec // a throwaway container that lives as long as the test process
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
	bootstrapMu.Lock()
	defer bootstrapMu.Unlock()
	Exec(t, db.AdminURL, migrations.Bootstrap)
	Exec(t, db.AdminURL, fmt.Sprintf("ALTER ROLE sluiceway PASSWORD '%s'", appPassword))
	return db
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

	name := "t_" + strings.ToLower(ids.New())
	Exec(t, adminURL.String(), "CREATE DATABASE "+name)

	admin := *adminURL
	admin.Path = "/" + name
	app := admin
	app.User = url.UserPassword("sluiceway", appPassword)
	return Database{URL: app.String(), AdminURL: admin.String()}
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
	c, err := postgres.Run(ctx, image,
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		startErr = err
		return
	}
	raw, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		startErr = err
		return
	}
	adminURL, startErr = url.Parse(raw)
}
