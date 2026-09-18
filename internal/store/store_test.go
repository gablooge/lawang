package store_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gablooge/sluiceway/internal/store"
	"github.com/gablooge/sluiceway/internal/tenancy"
	"github.com/gablooge/sluiceway/internal/testdb"
	"github.com/gablooge/sluiceway/migrations"
)

const (
	tenantA = tenancy.ID("tenant_a")
	tenantB = tenancy.ID("tenant_b")
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// open connects as the application role and migrates. A pooled connection that carried a tenant
// or a role into its next transaction would be a leak, so tests that look for one pass
// "&pool_max_conns=1" to guarantee the next transaction reuses the same connection.
func open(t *testing.T, params string) *store.DB {
	t.Helper()
	ctx := testCtx(t)
	tdb := testdb.New(t)
	db, err := store.Open(ctx, tdb.URL+params)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Migrate(ctx, quiet); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func seedTenants(t *testing.T, db *store.DB) {
	t.Helper()
	for _, id := range []tenancy.ID{tenantA, tenantB} {
		err := db.TenantTx(testCtx(t), id, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), "INSERT INTO tenants (id, name) VALUES ($1, $2)", id.String(), id.String())
			return err
		})
		if err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
}

func visibleTenants(t *testing.T, tx pgx.Tx) []string {
	t.Helper()
	rows, err := tx.Query(context.Background(), "SELECT id FROM tenants ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return got
}

func TestMigrationsRunAndRerunAsTheNonSuperuserRole(t *testing.T) {
	ctx := testCtx(t)
	tdb := testdb.New(t)
	db, err := store.Open(ctx, tdb.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Prove the premise, so this test cannot quietly turn into a superuser test.
	err = db.Tx(ctx, func(tx pgx.Tx) error {
		var user string
		var super, bypass bool
		if err := tx.QueryRow(ctx, `SELECT rolname, rolsuper, rolbypassrls FROM pg_roles
			WHERE rolname = current_user`).Scan(&user, &super, &bypass); err != nil {
			return err
		}
		if user != "sluiceway" || super || bypass {
			t.Fatalf("connected as %q (super=%v bypassrls=%v), want the plain application role", user, super, bypass)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := db.Migrate(ctx, quiet)
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if first == 0 {
		t.Fatal("first Migrate applied nothing")
	}
	second, err := db.Migrate(ctx, quiet)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if second != 0 {
		t.Errorf("second Migrate applied %d migrations, want 0", second)
	}
	if err := db.Ping(ctx); err != nil {
		t.Errorf("pool unusable after Migrate: %v", err)
	}
}

func TestConcurrentMigrateIsSerialized(t *testing.T) {
	ctx := testCtx(t)
	tdb := testdb.New(t)
	db, err := store.Open(ctx, tdb.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var wg sync.WaitGroup
	errs := make([]error, 4)
	counts := make([]int, 4)
	for i := range errs {
		wg.Go(func() { counts[i], errs[i] = db.Migrate(ctx, quiet) })
	}
	wg.Wait()

	total := 0
	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d: %v", i, err)
		}
		total += counts[i]
	}
	var want int
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			want++
		}
	}
	if total != want {
		t.Errorf("replicas applied %d migrations between them, want each of the %d applied exactly once", total, want)
	}
}

func TestNoTenantBoundReturnsZeroRows(t *testing.T) {
	db := open(t, "")
	seedTenants(t, db)

	err := db.Tx(testCtx(t), func(tx pgx.Tx) error {
		if got := visibleTenants(t, tx); len(got) != 0 {
			t.Errorf("with no tenant bound, saw %v, want nothing", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNoTenantBoundRefusesWrites(t *testing.T) {
	db := open(t, "")
	err := db.Tx(testCtx(t), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "INSERT INTO tenants (id) VALUES ('tenant_x')")
		return err
	})
	assertRLSViolation(t, err)
}

func TestTenantCannotReadOrWriteAnotherTenant(t *testing.T) {
	db := open(t, "")
	seedTenants(t, db)
	ctx := testCtx(t)

	err := db.TenantTx(ctx, tenantA, func(tx pgx.Tx) error {
		if got := visibleTenants(t, tx); len(got) != 1 || got[0] != tenantA.String() {
			t.Errorf("tenant_a saw %v, want only itself", got)
		}
		for _, stmt := range []string{
			"UPDATE tenants SET name = 'stolen' WHERE id = 'tenant_b'",
			"DELETE FROM tenants WHERE id = 'tenant_b'",
		} {
			tag, err := tx.Exec(ctx, stmt)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("%q touched %d rows of another tenant", stmt, tag.RowsAffected())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Writing a row that belongs to someone else is an error, not a silent no-op.
	err = db.TenantTx(ctx, tenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO tenants (id) VALUES ('tenant_c')")
		return err
	})
	assertRLSViolation(t, err)
	err = db.TenantTx(ctx, tenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE tenants SET id = 'tenant_c' WHERE id = 'tenant_a'")
		return err
	})
	assertRLSViolation(t, err)

	// And tenant_b's row is exactly as it was.
	err = db.TenantTx(ctx, tenantB, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(ctx, "SELECT name FROM tenants").Scan(&name); err != nil {
			return err
		}
		if name != "tenant_b" {
			t.Errorf("tenant_b name = %q", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTenantBindingDoesNotOutliveItsTransaction(t *testing.T) {
	db := open(t, "&pool_max_conns=1")
	seedTenants(t, db)
	ctx := testCtx(t)

	// Commit and rollback both have to clear it.
	_ = db.TenantTx(ctx, tenantA, func(pgx.Tx) error { return nil })
	_ = db.TenantTx(ctx, tenantA, func(pgx.Tx) error { return errors.New("roll back") })

	err := db.Tx(ctx, func(tx pgx.Tx) error {
		// The setting now reads back as '', not NULL. No row may match that either.
		var raw *string
		if err := tx.QueryRow(ctx, "SELECT current_setting('sluiceway.tenant', true)").Scan(&raw); err != nil {
			return err
		}
		if raw != nil && *raw != "" {
			t.Errorf("tenant setting leaked into the next transaction: %q", *raw)
		}
		if got := visibleTenants(t, tx); len(got) != 0 {
			t.Errorf("the next transaction on the same connection saw %v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRebindingMovesTheTransactionToAnotherTenant(t *testing.T) {
	db := open(t, "")
	seedTenants(t, db)
	ctx := testCtx(t)

	err := db.TenantTx(ctx, tenantA, func(tx pgx.Tx) error {
		if err := tenancy.Bind(ctx, tx, tenantB); err != nil {
			return err
		}
		if got := visibleTenants(t, tx); len(got) != 1 || got[0] != tenantB.String() {
			t.Errorf("after re-binding to tenant_b saw %v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHelperRolesAreEnteredExplicitlyAndLeftAtCommit(t *testing.T) {
	db := open(t, "&pool_max_conns=1")
	seedTenants(t, db)
	ctx := testCtx(t)

	currentUser := func(tx pgx.Tx) string {
		var u string
		if err := tx.QueryRow(ctx, "SELECT current_user").Scan(&u); err != nil {
			t.Fatal(err)
		}
		return u
	}

	for _, role := range []store.Role{store.RoleResolver, store.RoleWorker} {
		err := db.RoleTx(ctx, role, func(tx pgx.Tx) error {
			if got := currentUser(tx); got != string(role) {
				t.Errorf("inside RoleTx(%s) current_user = %q", role, got)
			}
			// Nothing has been granted to the helper roles yet, so they cannot even read.
			if got := readTenantsOutcome(ctx, tx); got != "permission denied" {
				t.Errorf("%s reading tenants: %s, want permission denied", role, got)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		err = db.Tx(ctx, func(tx pgx.Tx) error {
			if got := currentUser(tx); got != "sluiceway" {
				t.Errorf("after RoleTx(%s), same connection, current_user = %q", role, got)
			}
			// INHERIT FALSE: membership grants the right to SET ROLE, and nothing implicitly.
			var inherits, canSet bool
			if err := tx.QueryRow(ctx, "SELECT pg_has_role($1, 'USAGE'), pg_has_role($1, 'SET')", string(role)).
				Scan(&inherits, &canSet); err != nil {
				return err
			}
			if inherits || !canSet {
				t.Errorf("%s: inherits=%v canSet=%v, want false and true", role, inherits, canSet)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := db.RoleTx(ctx, store.Role("postgres"), func(pgx.Tx) error { return nil }); err == nil {
		t.Error("RoleTx accepted a role that is not a helper role")
	}
}

// readTenantsOutcome reports how a read of tenants went, for callers that expect it to fail. It
// runs inside a savepoint, because a failed statement would otherwise abort the caller's
// transaction.
func readTenantsOutcome(ctx context.Context, tx pgx.Tx) string {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err.Error()
	}
	defer func() { _ = sp.Rollback(ctx) }()

	var n int
	err = sp.QueryRow(ctx, "SELECT count(*) FROM tenants").Scan(&n)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "42501":
		return "permission denied"
	case err != nil:
		return err.Error()
	}
	return "readable"
}

func TestOpenRefusesASuperuser(t *testing.T) {
	tdb := testdb.New(t)
	db, err := store.Open(testCtx(t), tdb.AdminURL)
	if !errors.Is(err, store.ErrUnsafeRole) {
		if db != nil {
			db.Close()
		}
		t.Fatalf("Open as superuser: err = %v, want ErrUnsafeRole", err)
	}
}

func TestOpenRefusesABypassRLSRole(t *testing.T) {
	tdb := testdb.New(t)
	testdb.Exec(t, tdb.AdminURL, "CREATE ROLE sneaky LOGIN BYPASSRLS PASSWORD 'sneaky'")
	t.Cleanup(func() { testdb.Exec(t, tdb.AdminURL, "DROP ROLE sneaky") })

	url := strings.Replace(tdb.URL, "sluiceway:sluiceway_test@", "sneaky:sneaky@", 1)
	db, err := store.Open(testCtx(t), url)
	if !errors.Is(err, store.ErrUnsafeRole) {
		if db != nil {
			db.Close()
		}
		t.Fatalf("Open as a BYPASSRLS role: err = %v, want ErrUnsafeRole", err)
	}
}

func TestOpenRefusesADatabaseThatIsNotBootstrapped(t *testing.T) {
	// The roles exist cluster-wide once any test has bootstrapped, so make sure they do, then use
	// a database that has never seen the script.
	testdb.New(t)
	raw := testdb.NewRaw(t)
	db, err := store.Open(testCtx(t), raw.URL)
	if !errors.Is(err, store.ErrNotBootstrapped) {
		if db != nil {
			db.Close()
		}
		t.Fatalf("Open on a bare database: err = %v, want ErrNotBootstrapped", err)
	}
}

// A schema called "sluiceway" that someone else created is not the one the bootstrap makes. The
// script's CREATE SCHEMA IF NOT EXISTS used to accept it silently, Open passed, and the first sign
// of trouble was Migrate failing with "no schema has been selected to create in".
func TestASchemaOwnedByAnotherRoleIsRefused(t *testing.T) {
	testdb.New(t) // the roles exist cluster-wide once any database is bootstrapped
	raw := testdb.NewRaw(t)
	testdb.Exec(t, raw.AdminURL, "CREATE SCHEMA sluiceway")

	db, err := store.Open(testCtx(t), raw.URL)
	if db != nil {
		db.Close()
	}
	if !errors.Is(err, store.ErrSchemaNotOwned) {
		t.Errorf("Open: err = %v, want ErrSchemaNotOwned", err)
	}

	// The script says so as well, instead of leaving the schema as it found it and reporting
	// success.
	var pgErr *pgconn.PgError
	err = testdb.TryBootstrap(t, raw.AdminURL)
	if !errors.As(err, &pgErr) || pgErr.Code != "55000" || !strings.Contains(pgErr.Message, `owned by "postgres"`) {
		t.Fatalf("bootstrap over a foreign schema: err = %v, want a refusal (55000) that names the owner", err)
	}

	// Handing the schema over is the way out, and then both are content.
	testdb.Exec(t, raw.AdminURL, "ALTER SCHEMA sluiceway OWNER TO sluiceway")
	testdb.Bootstrap(t, raw.AdminURL)
	db, err = store.Open(testCtx(t), raw.URL)
	if err != nil {
		t.Fatalf("Open once the schema belongs to the application role: %v", err)
	}
	db.Close()
}

func TestOpenNeverEchoesTheURL(t *testing.T) {
	_, err := store.Open(testCtx(t), "postgres://app:hunter2@db:5432/x?pool_max_conns=banana")
	if err == nil {
		t.Fatal("Open accepted a malformed URL")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestBootstrapIsRerunnable(t *testing.T) {
	tdb := testdb.New(t) // first run
	testdb.Bootstrap(t, tdb.AdminURL)
	db, err := store.Open(testCtx(t), tdb.URL)
	if err != nil {
		t.Fatalf("Open after a second bootstrap: %v", err)
	}
	db.Close()
}

func TestTenantIDDomainRejectsMalformedIDs(t *testing.T) {
	db := open(t, "")
	ctx := testCtx(t)
	// Bind would refuse these first, so go around it and let the database be the last line.
	for _, bad := range []string{"", "tenant a", strings.Repeat("x", 65)} {
		err := db.Tx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT set_config('sluiceway.tenant', $1, true)", bad); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO tenants (id) VALUES ($1)", bad)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code != "23514" && pgErr.Code != "42501") {
			t.Errorf("inserting tenant id %q: err = %v, want a check or policy violation", bad, err)
		}
	}
}

// TestEveryTableForcesRowLevelSecurity guards every future migration: a new table either forces
// row-level security or is named here with a reason. A relation that holds rows but cannot have
// row-level security at all (a materialized view, a foreign table) is refused outright unless it
// is named here: a materialized view over a tenant table would hand every tenant's rows to
// whoever may read it.
func TestEveryTableForcesRowLevelSecurity(t *testing.T) {
	notTenantScoped := map[string]string{
		"goose_db_version": "migration bookkeeping, no tenant data",
	}

	db := open(t, "")
	ctx := testCtx(t)
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		// Every kind of relation that stores or serves rows of its own: tables, partitioned
		// tables, materialized views, foreign tables. Views are left out: they hold nothing, and
		// one owned by the application role reads its tables under that role's forced policies.
		rows, err := tx.Query(ctx, `
			SELECT c.relname, c.relkind::text, c.relrowsecurity, c.relforcerowsecurity,
			       (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid)
			  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1 AND c.relkind IN ('r', 'p', 'm', 'f')`, store.Schema)
		if err != nil {
			return err
		}
		defer rows.Close()
		seen := 0
		for rows.Next() {
			var name, kind string
			var enabled, forced bool
			var policies int
			if err := rows.Scan(&name, &kind, &enabled, &forced, &policies); err != nil {
				return err
			}
			seen++
			if _, ok := notTenantScoped[name]; ok {
				continue
			}
			if kind != "r" && kind != "p" {
				t.Errorf("%s (relkind %q) holds rows but cannot have row-level security: keep tenant data in tables, or name it in notTenantScoped with a reason",
					name, kind)
				continue
			}
			if !enabled || !forced || policies == 0 {
				t.Errorf("table %s: rls enabled=%v forced=%v policies=%d, want enabled, forced, and at least one policy",
					name, enabled, forced, policies)
			}
		}
		if seen < 2 {
			t.Errorf("found %d tables in schema %s, the query is not seeing the schema", seen, store.Schema)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertRLSViolation(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("err = %v, want a row-level security violation (42501)", err)
	}
}
