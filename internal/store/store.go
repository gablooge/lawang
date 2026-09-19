// Package store owns the Postgres connection pool, the transaction helpers that make row-level
// security apply, and the embedded migrations.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// ErrSchemaNotOwned reports a schema of the right name that the login role does not own, so it is
// not the one the bootstrap script creates. It is a sentinel of its own because neither of the
// others tells the operator the truth: tenant isolation is not at stake, and applying the
// bootstrap script again does not change who owns a schema that already exists.
var ErrSchemaNotOwned = errors.New(`store: schema "sluiceway" is not owned by the login role`)

// DB is the connection pool.
type DB struct {
	pool *pgxpool.Pool
}

// defaultConnectTimeout bounds one connection attempt when the URL has no connect_timeout of its
// own. Without it a host that drops packets (a firewall or a security group, the usual mistake on
// managed Postgres) holds Open for the TCP timeout of the operating system, 75 seconds or more,
// with nothing logged. An operator overrides it with connect_timeout=N (seconds) in the URL, for
// N of 1 or more.
//
// connect_timeout=0 does NOT lift the bound, and that is deliberate. In libpq 0 means "wait for as
// long as it takes", but pgx parses an explicit 0 to the same zero value as a missing parameter, so
// the two cannot be told apart here, and of the two readings the bounded one is the safe one: a
// start that hangs with nothing logged is the failure this constant exists to prevent. An operator
// who needs a long wait (a slow failover proxy, a debugger on the server) writes a large number.
const defaultConnectTimeout = 10 * time.Second

// Open connects and then refuses to return a pool that is not safe to use: see preflight.
//
// No error it returns repeats any part of databaseURL: not the password, and not the user, host,
// port or database either. See connError.
func Open(ctx context.Context, databaseURL string) (*DB, error) {
	cfg, err := poolConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	// The pool connects lazily, so an unreachable or refusing server is first met by preflight.
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, connError(ctx, err)
	}
	db := &DB{pool: pool}
	if err := db.preflight(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return db, nil
}

// poolConfig parses the URL and applies what every Sluiceway connection needs.
func poolConfig(databaseURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// Deliberately not wrapped: pgx's parse error repeats the URL with only the password masked.
		return nil, errors.New("SLUICEWAY_DATABASE_URL: not a valid Postgres connection URL")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = Schema
	cfg.ConnConfig.RuntimeParams["application_name"] = "sluiceway"
	// Zero is "not set" and also an explicit connect_timeout=0: see defaultConnectTimeout.
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = defaultConnectTimeout
	}
	return cfg, nil
}

// checkVersion refuses a server older than Postgres 16. version is server_version_num.
func checkVersion(version int) error {
	if version < minServerVersion {
		return fmt.Errorf("store: Postgres 16 or newer is required, server reports %d", version)
	}
	return nil
}

// Close releases every connection.
func (db *DB) Close() { db.pool.Close() }

// preflight fails closed on anything that would make tenant isolation silently not apply. Tests
// that only ever connect as a superuser pass without isolation being real, so this runs on every
// start, in every environment. It runs once per process: a membership changed by an administrator
// afterwards is not seen until the next start.
//
// Its refusals never name the login role: that name is the user half of the database URL.
func (db *DB) preflight(ctx context.Context) error {
	var version int
	if err := db.pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		return connError(ctx, err)
	}
	if err := checkVersion(version); err != nil {
		return err
	}

	var super, bypass, hasSchema, ownSchema bool
	err := db.pool.QueryRow(ctx, `
		SELECT r.rolsuper, r.rolbypassrls,
		       EXISTS (SELECT FROM pg_namespace WHERE nspname = $1),
		       EXISTS (SELECT FROM pg_namespace WHERE nspname = $1 AND nspowner = r.oid)
		  FROM pg_roles r
		 WHERE r.rolname = current_user`, Schema,
	).Scan(&super, &bypass, &hasSchema, &ownSchema)
	if err != nil {
		return connError(ctx, err)
	}
	if super || bypass {
		return fmt.Errorf("%w: the login role is SUPERUSER or BYPASSRLS, so row-level security would not apply to it", ErrUnsafeRole)
	}

	helpers, err := db.helperStates(ctx)
	if err != nil {
		return err
	}
	for _, h := range helpers {
		if h.exists && h.unsafe {
			return fmt.Errorf("%w: helper role %s is SUPERUSER or BYPASSRLS", ErrUnsafeRole, h.name)
		}
	}
	if !hasSchema {
		return ErrNotBootstrapped
	}
	// Ownership itself, not membership in the owner: the bootstrap creates the schema for exactly
	// this role, and migrations create and alter everything in it as this role.
	if !ownSchema {
		return fmt.Errorf("%w: it was not created by the bootstrap script; an administrator can hand it over with ALTER SCHEMA %s OWNER TO the login role, or use another database",
			ErrSchemaNotOwned, Schema)
	}
	for _, h := range helpers {
		switch {
		case !h.exists:
			return fmt.Errorf("%w: helper role %s does not exist, apply the bootstrap script again", ErrUnsafeRole, h.name)
		case h.inherited:
			return fmt.Errorf("%w: the login role inherits %s, directly or through another role, so its cross-tenant policies would apply without SET ROLE; every membership on the path must be WITH INHERIT FALSE",
				ErrUnsafeRole, h.name)
		case h.administers:
			return fmt.Errorf("%w: the login role holds ADMIN OPTION on %s, directly or through another role, so it could grant itself that role WITH INHERIT TRUE while running; revoke the ADMIN OPTION",
				ErrUnsafeRole, h.name)
		case !h.canSet:
			return fmt.Errorf("%w: the login role cannot SET ROLE %s, it must be a member WITH INHERIT FALSE, SET TRUE", ErrUnsafeRole, h.name)
		}
	}
	return nil
}

// helperState is what the preflight learns about one helper role.
type helperState struct {
	name string
	// exists is false when the role is missing, and then the other fields are all false.
	exists bool
	// unsafe: the role is SUPERUSER or BYPASSRLS.
	unsafe bool
	// inherited: the login holds the role's privileges, and is subject to its policies, WITHOUT
	// SET ROLE. A plain transaction with no tenant bound would then see across tenants.
	inherited bool
	// administers: the login may grant the role, to itself included. That is the one membership
	// state it could turn into inheritance by itself, after this check has run.
	administers bool
	// canSet: the login may SET ROLE to it, which RoleTx depends on.
	canSet bool
}

// helperStates asks Postgres the question Postgres itself answers when it evaluates a policy or
// a privilege, instead of reading pg_auth_members. A membership has one row per grantor, and
// inheritance also arrives through intermediate roles, so no count of rows that look right says
// what the login can actually do. pg_has_role follows every path: 'USAGE' is "without SET ROLE",
// 'SET' is "may SET ROLE", and 'MEMBER WITH ADMIN OPTION' is "may grant this role". Postgres
// answers the last one identically for 'USAGE WITH ADMIN OPTION' and 'SET WITH ADMIN OPTION':
// all three ask is_admin_of_role, which follows every membership whether or not it is inherited.
// That is the right reach, because ADMIN OPTION held by a role the login can only SET ROLE to is
// just as usable. All of these are only meaningful for a login that is not a superuser, which the
// caller has already established.
func (db *DB) helperStates(ctx context.Context) ([]helperState, error) {
	want := []string{string(RoleResolver), string(RoleWorker)}
	rows, err := db.pool.Query(ctx, `
		SELECT n.name, h.oid IS NOT NULL,
		       coalesce(h.rolsuper OR h.rolbypassrls, false),
		       coalesce(pg_has_role(h.oid, 'USAGE'), false),
		       coalesce(pg_has_role(h.oid, 'MEMBER WITH ADMIN OPTION'), false),
		       coalesce(pg_has_role(h.oid, 'SET'), false)
		  FROM unnest($1::text[]) WITH ORDINALITY AS n(name, ord)
		  LEFT JOIN pg_roles h ON h.rolname = n.name
		 ORDER BY n.ord`, want)
	if err != nil {
		return nil, connError(ctx, err)
	}
	states, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (helperState, error) {
		var h helperState
		err := row.Scan(&h.name, &h.exists, &h.unsafe, &h.inherited, &h.administers, &h.canSet)
		return h, err
	})
	if err != nil {
		return nil, connError(ctx, err)
	}
	// Fail closed: an answer that came back short has verified nothing.
	if len(states) != len(want) {
		return nil, fmt.Errorf("%w: preflight saw %d helper roles, want %d", ErrUnsafeRole, len(states), len(want))
	}
	return states, nil
}

// Tx runs fn in a transaction with no tenant bound. Every tenant-scoped table reads as empty and
// refuses writes. It is for the few things that are not tenant data.
//
// fn does database work and nothing else: see begin for what happens to a network error that fn
// returns.
func (db *DB) Tx(ctx context.Context, fn func(pgx.Tx) error) error {
	return db.begin(ctx, fn)
}

// begin is pgx.BeginTxFunc (commit when fn returns nil, roll back on an error or a panic) with a
// fixed isolation level (see the end of this comment) and one addition. The pool connects lazily, so any transaction can be the one that meets an unreachable
// server, and a network error in the middle of one carries addresses too: those errors are
// replaced the way Open replaces them. Every other error, from fn or from the server, reaches the
// caller untouched.
//
// The replacement looks at the error and not at where it came from, ON PURPOSE. An error that fn
// returns is replaced too when it wraps a *net.OpError, a *net.DNSError or a *pgconn.ConnectError,
// and then nothing fn wrapped around it survives: errors.Is(err, aSentinelOfTheCaller) is false
// and the text points at SLUICEWAY_DATABASE_URL. It has to work on what fn returns, because a
// statement that fn runs on a connection that has just died fails with exactly such an error, and
// fn is the one that returns it. Telling that error from a network error of the caller's own would
// mean trusting a side channel (is the connection marked closed?) on every failure path of pgx,
// and a miss there puts the connection target back into the log. The wrong label costs less.
//
// So fn must not do network I/O of its own (a provider call, a sink delivery, a lookup), and must
// not return a net error that is not the database's. That is already the rule for other reasons:
// a transaction that waits on the far end of a network call holds a pooled connection and its row
// locks for as long as that takes (docs/architecture.md section 10, principle 6). Do the call
// before or after the transaction and pass in what it returned.
//
// The transaction is READ COMMITTED, asked for by name and not inherited. Callers depend on it:
// a statement that follows a wait for a lock must see what the transaction it waited for
// committed, and only READ COMMITTED takes a new snapshot per statement. The outbox is the first
// such caller (docs/adr/0010-outbox-head-marker.md): under REPEATABLE READ its writers wait for the
// ordering key's lock and then decide on a snapshot from before the wait, which leaves a row that
// is never delivered, with no error anywhere. default_transaction_isolation can be set on the
// server, the database or the role, by an administrator who has never heard of this package, so
// the default is not something to rely on. This is the only place the application opens a
// transaction. (The preflight runs single statements, for which the levels do not differ, and
// goose opens its own transactions for DDL, which the session lock in Migrate already serializes.)
func (db *DB) begin(ctx context.Context, fn func(pgx.Tx) error) error {
	return scrub(ctx, pgx.BeginTxFunc(ctx, db.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, fn))
}

// TenantTx runs fn in a transaction scoped to one tenant. It is the only way to do a tenant's
// work.
//
// fn does database work and nothing else. No network I/O inside it: a net error that fn returns
// comes back as a database connection error, without whatever fn wrapped around it (see begin).
func (db *DB) TenantTx(ctx context.Context, id tenancy.ID, fn func(pgx.Tx) error) error {
	return db.begin(ctx, func(tx pgx.Tx) error {
		if err := tenancy.Bind(ctx, tx, id); err != nil {
			return err
		}
		return fn(tx)
	})
}

// RoleTx runs fn in a transaction under a helper role. SET LOCAL ends with the transaction, so the
// wider policies of that role never outlive it on a pooled connection.
//
// THE TRANSACTION IS CROSS-TENANT FOR ITS WHOLE LIFE. Binding a tenant inside it does not narrow
// it: Postgres ORs permissive policies together, so a policy of the helper role (for example
// "TO sluiceway_worker USING (true)") keeps admitting every tenant's rows whatever
// sluiceway.tenant says. A bind could only ever add rows, on tables where the role has no policy
// of its own. So fn does the role's cross-tenant step and nothing else (resolve an owner, claim a
// row), returns what it learned, and the work for that tenant happens in a SECOND transaction
// under TenantTx, as the application role.
//
// To keep that from being forgotten, the transaction handed to fn refuses tenancy.Bind with
// tenancy.ErrCrossTenantTx, savepoints included. An item that really wants a helper role and a
// tenant in one transaction has to add that on purpose, with a test of what is visible inside it.
//
// As in Tx and TenantTx, fn does database work and nothing else. No network I/O inside it: a net
// error that fn returns comes back as a database connection error, without whatever fn wrapped
// around it (see begin).
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
	return db.begin(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("store: enter role %s: %w", role, err)
		}
		return fn(crossTenantTx{tx})
	})
}

// crossTenantTx is the transaction RoleTx hands out. It carries the marker tenancy.Bind refuses.
type crossTenantTx struct{ pgx.Tx }

var _ tenancy.CrossTenantTx = crossTenantTx{}

// CrossTenant implements tenancy.CrossTenantTx.
func (crossTenantTx) CrossTenant() {}

// Begin opens a savepoint that is just as cross-tenant as the transaction around it.
func (tx crossTenantTx) Begin(ctx context.Context) (pgx.Tx, error) {
	sp, err := tx.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return crossTenantTx{sp}, nil
}
