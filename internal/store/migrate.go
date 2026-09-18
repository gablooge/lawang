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

// Migrate applies every pending migration and returns how many it applied. It runs as the same
// non-superuser role as everything else (Open has already refused anything stronger), so the
// tables end up owned by the role that will use them.
//
// A session-level advisory lock serializes concurrent callers, so several replicas may run
// "sluiceway migrate" at once.
func (db *DB) Migrate(ctx context.Context, logger *slog.Logger) (int, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return 0, fmt.Errorf("store: migrate: %w", err)
	}

	// Shares the pool, and so its search path. Closing sqlDB leaves the pool open.
	sqlDB := stdlib.OpenDBFromPool(db.pool)
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS,
		goose.WithSessionLocker(locker),
		goose.WithSlog(logger),
	)
	if err != nil {
		return 0, fmt.Errorf("store: migrate: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return len(results), fmt.Errorf("store: migrate: %w", err)
	}
	return len(results), nil
}
