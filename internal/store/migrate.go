package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/gablooge/sluiceway/migrations"
)

// How a Migrate that finds the advisory lock taken waits for it: one attempt per second, 300
// times, so the ceiling stays at the five minutes of the goose default.
const (
	lockRetrySeconds = 1
	lockRetries      = 300
)

// provider builds the goose provider over the pool, and so over its search path. The returned
// function closes the database/sql handle the provider borrowed the pool through, which leaves the
// pool itself open.
func (db *DB) provider(logger *slog.Logger) (*goose.Provider, func(), error) {
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(lockRetrySeconds, lockRetries))
	if err != nil {
		return nil, nil, err
	}
	sqlDB := stdlib.OpenDBFromPool(db.pool)
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS,
		goose.WithSessionLocker(locker),
		goose.WithSlog(logger),
	)
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, err
	}
	return provider, func() { _ = sqlDB.Close() }, nil
}

// Migrate applies every pending migration and returns how many it applied. It runs as the same
// non-superuser role as everything else (Open has already refused anything stronger), so the
// tables end up owned by the role that will use them.
//
// A session-level advisory lock serializes concurrent callers, so several replicas may run
// "sluiceway migrate" at once. A caller that finds the lock taken asks again every second for up to
// five minutes. The goose default asks every five seconds, so the k-th of N replicas started
// together slept about 5(k-1) seconds, usually for a lock that was released within milliseconds
// because there was nothing left to apply. One second is the shortest period goose accepts.
func (db *DB) Migrate(ctx context.Context, logger *slog.Logger) (int, error) {
	provider, closeProvider, err := db.provider(logger)
	if err != nil {
		return 0, fmt.Errorf("store: migrate: %w", scrub(ctx, err))
	}
	defer closeProvider()

	results, err := provider.Up(ctx)
	if err != nil {
		// A migration that fails keeps the server's message: that is what the operator needs. A
		// connection that fails does not, because it would repeat the database URL.
		return len(results), fmt.Errorf("store: migrate: %w", scrub(ctx, err))
	}
	return len(results), nil
}
