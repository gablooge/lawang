// Package store owns the Postgres connection pool, the transaction helpers that make row-level
// security apply, and the embedded migrations.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gablooge/sluiceway/internal/tenancy"
)

// Schema is where every Sluiceway object lives. It is set as the search path on every connection
// explicitly, because the default "$user" entry follows SET ROLE and would stop resolving the
// moment a transaction enters a helper role.
const Schema = "sluiceway"

// minServerVersion is Postgres 16, the first with GRANT ... WITH INHERIT FALSE.
const minServerVersion = 160000

// Role is a helper database role the application role can enter for one transaction.
type Role string

// The helper roles. Their policies arrive with the tables they act on.
const (
	RoleResolver Role = "sluiceway_resolver"
	RoleWorker   Role = "sluiceway_worker"
)

// ErrUnsafeRole reports a database login under which row-level security would not apply, or whose
// helper roles are not set up as docs/architecture.md section 4 requires.
var ErrUnsafeRole = errors.New("store: unsafe database role")

// ErrNotBootstrapped reports a database where the one-time bootstrap script has not been run.
var ErrNotBootstrapped = errors.New(`store: database is not bootstrapped, run "sluiceway migrate bootstrap" and apply its output as an administrator`)

// DB is the connection pool.
type DB struct {
	pool *pgxpool.Pool
}

// Open connects and then refuses to return a pool that is not safe to use: see preflight.
func Open(ctx context.Context, databaseURL string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// Deliberately not wrapped: the underlying error can quote parts of the URL.
		return nil, errors.New("store: invalid database URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = Schema
	cfg.ConnConfig.RuntimeParams["application_name"] = "sluiceway"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db := &DB{pool: pool}
	if err := db.preflight(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return db, nil
}

// Close releases every connection.
func (db *DB) Close() { db.pool.Close() }

// Ping checks that the database is reachable.
func (db *DB) Ping(ctx context.Context) error { return db.pool.Ping(ctx) }

// preflight fails closed on anything that would make tenant isolation silently not apply. Tests
// that only ever connect as a superuser pass without isolation being real, so this runs on every
// start, in every environment.
func (db *DB) preflight(ctx context.Context) error {
	var version int
	if err := db.pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		return fmt.Errorf("store: preflight: %w", err)
	}
	if version < minServerVersion {
		return fmt.Errorf("store: Postgres 16 or newer is required, server reports %d", version)
	}

	var (
		user                     string
		super, bypass, hasSchema bool
		unsafeHelpers, memberOf  int
	)
	err := db.pool.QueryRow(ctx, `
		SELECT r.rolname, r.rolsuper, r.rolbypassrls,
		       EXISTS (SELECT FROM pg_namespace WHERE nspname = $1),
		       (SELECT count(*) FROM pg_roles h
		         WHERE h.rolname = ANY($2) AND (h.rolsuper OR h.rolbypassrls)),
		       (SELECT count(*) FROM pg_auth_members m JOIN pg_roles h ON h.oid = m.roleid
		         WHERE m.member = r.oid AND h.rolname = ANY($2)
		           AND m.set_option AND NOT m.inherit_option)
		  FROM pg_roles r
		 WHERE r.rolname = current_user`,
		Schema, []string{string(RoleResolver), string(RoleWorker)},
	).Scan(&user, &super, &bypass, &hasSchema, &unsafeHelpers, &memberOf)
	if err != nil {
		return fmt.Errorf("store: preflight: %w", err)
	}

	switch {
	case super || bypass:
		return fmt.Errorf("%w: %q is SUPERUSER or BYPASSRLS, so row-level security would not apply to it", ErrUnsafeRole, user)
	case unsafeHelpers > 0:
		return fmt.Errorf("%w: a helper role is SUPERUSER or BYPASSRLS", ErrUnsafeRole)
	case !hasSchema:
		return ErrNotBootstrapped
	case memberOf != 2:
		return fmt.Errorf("%w: %q must be a member of %s and %s WITH INHERIT FALSE, SET TRUE", ErrUnsafeRole, user, RoleResolver, RoleWorker)
	}
	return nil
}

// Tx runs fn in a transaction with no tenant bound. Every tenant-scoped table reads as empty and
// refuses writes. It is for the few things that are not tenant data.
func (db *DB) Tx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, db.pool, fn)
}

// TenantTx runs fn in a transaction scoped to one tenant.
func (db *DB) TenantTx(ctx context.Context, id tenancy.ID, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if err := tenancy.Bind(ctx, tx, id); err != nil {
			return err
		}
		return fn(tx)
	})
}

// RoleTx runs fn in a transaction under a helper role. SET LOCAL ends with the transaction, so the
// wider policies of that role never outlive it on a pooled connection.
//
// Inside fn, tenancy.Bind narrows the transaction to one tenant once the role's cross-tenant step
// (resolving an owner, claiming a row) is done.
func (db *DB) RoleTx(ctx context.Context, role Role, fn func(pgx.Tx) error) error {
	// A role name cannot be a query parameter, so each statement is a literal. Nothing here is
	// ever built from input.
	var stmt string
	switch role {
	case RoleResolver:
		stmt = "SET LOCAL ROLE sluiceway_resolver"
	case RoleWorker:
		stmt = "SET LOCAL ROLE sluiceway_worker"
	default:
		return fmt.Errorf("store: unknown role %q", role)
	}
	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("store: enter role %s: %w", role, err)
		}
		return fn(tx)
	})
}
