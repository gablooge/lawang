package vault_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gablooge/lawang/internal/store"
	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/testdb"
	"github.com/gablooge/lawang/internal/vault"
)

// env is one test's migrated database and a local vault on it, under keyA.
type env struct {
	t   *testing.T
	ctx context.Context
	db  *store.DB
	tdb testdb.Database
	v   *vault.Local
}

// setup gives a test its own database, migrated, connected as the non-superuser application role
// (CLAUDE.md: never as the superuser, or row-level security would never apply).
func setup(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	tdb := testdb.New(t)
	db, err := store.Open(ctx, tdb.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Migrate(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	e := &env{t: t, ctx: ctx, db: db, tdb: tdb}
	e.v = e.vaultUnder(keyA)
	return e
}

// vaultUnder returns a local vault on this test's database, sealing with key.
func (e *env) vaultUnder(key []byte) *vault.Local {
	e.t.Helper()
	v, err := vault.NewLocal(e.db, vault.LocalOptions{Key: key})
	if err != nil {
		e.t.Fatalf("NewLocal: %v", err)
	}
	return v
}

// store stores a credential and fails the test on a refusal.
func (e *env) store(t tenancy.ID, providerKey string, fields map[string]string) vault.Credential {
	e.t.Helper()
	cred, err := vault.NewCredential(fields)
	if err != nil {
		e.t.Fatalf("NewCredential: %v", err)
	}
	if err := e.v.Store(e.ctx, t, providerKey, cred); err != nil {
		e.t.Fatalf("Store(%s, %s): %v", t, providerKey, err)
	}
	return cred
}

// storeMarkerA stores what most of these tests start from: the marker, as tenant A's token for
// providerKey.
func (e *env) storeMarkerA() {
	e.t.Helper()
	e.store(tenantA, providerKey, map[string]string{"token": marker})
}

// wantFetchedA fails unless v hands tenant A out a credential whose token is want.
func (e *env) wantFetchedA(v *vault.Local, want, why string) {
	e.t.Helper()
	cred, err := v.Fetch(e.ctx, tenantA, providerKey)
	if err != nil {
		e.t.Fatalf("Fetch(%s, %s): %v: %s", tenantA, providerKey, err, why)
	}
	got, err := cred.Field("token")
	if err != nil {
		e.t.Fatalf("Field: %v: %s", err, why)
	}
	if got != want {
		e.t.Fatalf("the fetched token is not the one stored: %s", why)
	}
}

// admin runs SQL as the superuser, which is how these tests see rows that row-level security
// hides from the application role, and how they do the things an operator, a restore or a buggy
// future version could do to a row.
//
// Nothing the vault itself does goes through this connection. A test that asserted about the
// table through a superuser only would pass with the policies gone.
func (e *env) admin(sql string, args ...any) {
	e.t.Helper()
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		e.t.Fatalf("connect as the superuser: %v", err)
	}
	defer func() { _ = conn.Close(e.ctx) }()
	if _, err := conn.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("as the superuser: %s: %v", sql, err)
	}
}

// tryAdmin is admin for the statements a test expects the server to refuse.
func (e *env) tryAdmin(sql string, args ...any) error {
	e.t.Helper()
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		e.t.Fatalf("connect as the superuser: %v", err)
	}
	defer func() { _ = conn.Close(e.ctx) }()
	_, err = conn.Exec(e.ctx, sql, args...)
	return err
}

// queryAdmin reads one value as the superuser.
func queryAdmin[T any](e *env, sql string, args ...any) T {
	e.t.Helper()
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		e.t.Fatalf("connect as the superuser: %v", err)
	}
	defer func() { _ = conn.Close(e.ctx) }()
	var out T
	if err := conn.QueryRow(e.ctx, sql, args...).Scan(&out); err != nil {
		e.t.Fatalf("as the superuser: %s: %v", sql, err)
	}
	return out
}

// rowCount is how many rows the vault table holds, whoever owns them.
func (e *env) rowCount() int {
	e.t.Helper()
	return queryAdmin[int](e, "SELECT count(*)::int FROM lawang.vault_credentials")
}

// ciphertext is one row's stored envelope for providerKey, read as the superuser.
func (e *env) ciphertext(t tenancy.ID) []byte {
	e.t.Helper()
	return queryAdmin[[]byte](e,
		"SELECT ciphertext FROM lawang.vault_credentials WHERE tenant_id = $1 AND provider = $2",
		t.String(), providerKey)
}

// isCheckViolation reports the server refusing a row because a CHECK said no, and names the
// constraint.
func isCheckViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == constraint
}
