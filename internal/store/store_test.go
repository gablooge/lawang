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

	"github.com/gablooge/lawang/internal/store"
	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/testdb"
	"github.com/gablooge/lawang/migrations"
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
	t.Cleanup(func() { closeWithin(t, db, 10*time.Second) })
	if _, err := db.Migrate(ctx, quiet); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

// closeWithin closes the pool, which waits for every connection to come back. A test that finds a
// connection that was never given back (an open transaction left behind by a panic, for example)
// would otherwise hang here for as long as go test allows, after it had already failed. A test
// that is expected to fail has to fail fast.
func closeWithin(t *testing.T, db *store.DB, limit time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		db.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Errorf("Close was still waiting after %s: a connection was never given back to the pool", limit)
	}
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
		if user != "lawang" || super || bypass {
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
	// Migrate closes the database/sql handle it borrowed the pool through. The pool must survive it.
	if err := db.Tx(ctx, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, "SELECT 1"); return err }); err != nil {
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
		if err := tx.QueryRow(ctx, "SELECT current_setting('lawang.tenant', true)").Scan(&raw); err != nil {
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
			if got := currentUser(tx); got != "lawang" {
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

// TestEveryTransactionIsReadCommittedWhateverTheDefault: callers rely on a statement that follows a
// lock wait seeing what the transaction it waited for committed (the outbox's head marker is the
// first, docs/adr/0010-outbox-head-marker.md), and that is READ COMMITTED only. The default can be
// changed on the database or on the role by an administrator, so the helpers ask for the level by
// name. Both settings here are scoped to the test's own database.
func TestEveryTransactionIsReadCommittedWhateverTheDefault(t *testing.T) {
	for name, alter := range map[string]string{
		"set on the database": "ALTER DATABASE %I SET default_transaction_isolation = %L",
		"set on the role":     "ALTER ROLE lawang IN DATABASE %I SET default_transaction_isolation = %L",
	} {
		for _, level := range []string{"repeatable read", "serializable"} {
			t.Run(name+", "+level, func(t *testing.T) {
				ctx := testCtx(t)
				tdb := testdb.New(t)
				testdb.Exec(t, tdb.AdminURL,
					"DO $do$ BEGIN EXECUTE format($f$"+alter+"$f$, current_database(), '"+level+"'); END $do$")
				db, err := store.Open(ctx, tdb.URL)
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				t.Cleanup(func() { closeWithin(t, db, 10*time.Second) })
				if _, err := db.Migrate(ctx, quiet); err != nil {
					t.Fatalf("Migrate under a default of %s: %v", level, err)
				}

				check := func(tx pgx.Tx) error {
					var session, got string
					err := tx.QueryRow(ctx, "SELECT current_setting('default_transaction_isolation'), current_setting('transaction_isolation')").
						Scan(&session, &got)
					if err != nil {
						return err
					}
					if session != level {
						t.Fatalf("the session's default is %q, want %q: the test has not set up what it is about", session, level)
					}
					if got != "read committed" {
						t.Errorf("the transaction runs at %q, want read committed", got)
					}
					return nil
				}
				for helper, err := range map[string]error{
					"Tx":               db.Tx(ctx, check),
					"TenantTx":         db.TenantTx(ctx, tenantA, check),
					"RoleTx(worker)":   db.RoleTx(ctx, store.RoleWorker, check),
					"RoleTx(resolver)": db.RoleTx(ctx, store.RoleResolver, check),
				} {
					if err != nil {
						t.Errorf("%s: %v", helper, err)
					}
				}
			})
		}
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

	url := strings.Replace(tdb.URL, "lawang:lawang_test@", "sneaky:sneaky@", 1)
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

// A schema called "lawang" that someone else created is not the one the bootstrap makes. The
// script's CREATE SCHEMA IF NOT EXISTS used to accept it silently, Open passed, and the first sign
// of trouble was Migrate failing with "no schema has been selected to create in".
func TestASchemaOwnedByAnotherRoleIsRefused(t *testing.T) {
	testdb.New(t) // the roles exist cluster-wide once any database is bootstrapped
	raw := testdb.NewRaw(t)
	testdb.Exec(t, raw.AdminURL, "CREATE SCHEMA lawang")

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
	testdb.Exec(t, raw.AdminURL, "ALTER SCHEMA lawang OWNER TO lawang")
	testdb.Bootstrap(t, raw.AdminURL)
	db, err = store.Open(testCtx(t), raw.URL)
	if err != nil {
		t.Fatalf("Open once the schema belongs to the application role: %v", err)
	}
	db.Close()
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

// domainVerdict asks the tenant_id domain, and nothing else, about one value: a cast, as the
// application role, with no table and so no policy anywhere near it. It returns the SQLSTATE, or
// "" when the domain accepts the value.
func domainVerdict(t *testing.T, db *store.DB, value string) string {
	t.Helper()
	ctx := testCtx(t)
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		var got string
		if err := tx.QueryRow(ctx, "SELECT ($1::text::tenant_id)::text", value).Scan(&got); err != nil {
			return err
		}
		if got != value {
			t.Errorf("the domain accepted %q and handed back %q", value, got)
		}
		return nil
	})
	if err == nil {
		return ""
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("casting %q to tenant_id: %v, want an answer from the server", value, err)
	}
	return pgErr.Code
}

func TestTenantIDDomainRejectsMalformedIDs(t *testing.T) {
	db := open(t, "")
	ctx := testCtx(t)
	bad := []string{"", "tenant a", strings.Repeat("x", 65)}

	// The domain by itself. An insert cannot show this for the empty id: with '' bound,
	// current_tenant() is NULL and the policy refuses the row (42501) whatever the domain thinks
	// of it. 23514 is a check violation, and the domain's check is the only one in reach.
	for _, id := range bad {
		if got := domainVerdict(t, db, id); got != "23514" {
			t.Errorf("casting %q to tenant_id: SQLSTATE %q, want 23514 from the domain's check", id, got)
		}
	}

	// And the column really is of that domain. Bind would refuse these ids first, so go around it;
	// the policy is content (the id equals the bound tenant), which leaves the domain as the only
	// thing that can refuse. The empty id is left out for the reason above.
	for _, id := range bad[1:] {
		err := db.Tx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT set_config('lawang.tenant', $1, true)", id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO tenants (id) VALUES ($1)", id)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("inserting tenant id %q: err = %v, want a check violation (23514)", id, err)
		}
	}
}

// TestTheGoRuleAndTheDomainAgree ties tenancy.Parse to the tenant_id domain. The rule is written
// twice, as a byte loop in Go and as a regular expression in SQL, and the places where those two
// dialects usually part ways are the cases here. If this fails after a change to one of them, the
// other one needs the same change.
func TestTheGoRuleAndTheDomainAgree(t *testing.T) {
	db := open(t, "")
	cases := []struct {
		name, id string
		// notText: Postgres cannot hold the value in a text at all (22021), so it never reaches
		// the domain. That is a refusal too, and the only other one allowed.
		notText bool
	}{
		{name: "plain", id: "tenant_a"},
		{name: "a ULID", id: "01JZXA8Q2KTENANT0000000000"},
		{name: "one character", id: "a"},
		{name: "every class", id: "A-b_9"},
		{name: "only a hyphen", id: "-"},
		{name: "only an underscore", id: "_"},
		{name: "64 characters", id: strings.Repeat("x", 64)},
		{name: "65 characters", id: strings.Repeat("x", 65)},
		{name: "empty", id: ""},
		{name: "space", id: " "},
		{name: "inner space", id: "tenant a"},
		{name: "trailing newline", id: "tenant_a\n"},
		{name: "leading newline", id: "\ntenant_a"},
		{name: "only a newline", id: "\n"},
		{name: "carriage return", id: "tenant\ra"},
		{name: "tab", id: "tenant\ta"},
		{name: "the separator byte of the id recipes", id: "tenant\x1fa"},
		{name: "dot", id: "a.b"},
		{name: "semicolon", id: "a;b"},
		{name: "quote", id: "tenant'a"},
		{name: "backslash", id: `a\b`},
		{name: "accented letter", id: "ténant"},
		{name: "fullwidth letters", id: "ｔｅｎａｎｔ"},
		{name: "64 characters that are 128 bytes", id: strings.Repeat("é", 64)},
		{name: "a digit from another script", id: "tenant٣"},
		{name: "invalid UTF-8", id: "tenant\xff", notText: true},
		{name: "NUL", id: "tenant\x00a", notText: true},
	}
	accepted := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, goErr := tenancy.Parse(tc.id)
			verdict := domainVerdict(t, db, tc.id)

			switch verdict {
			case "", "23514":
			case "22021":
				if !tc.notText {
					t.Fatalf("%q: Postgres refused it as text (22021), which only the cases marked notText may be", tc.id)
				}
			default:
				t.Fatalf("%q: SQLSTATE %s, want the domain's verdict", tc.id, verdict)
			}
			if goAccepts, sqlAccepts := goErr == nil, verdict == ""; goAccepts != sqlAccepts {
				t.Errorf("%q: tenancy.Parse accepts=%v, the tenant_id domain accepts=%v (SQLSTATE %q)", tc.id, goAccepts, sqlAccepts, verdict)
			}
			if verdict == "" {
				accepted++
			}
		})
	}
	// Guards the test itself: a domainVerdict that always refused would agree with nothing valid.
	if accepted != 7 {
		t.Errorf("the domain accepted %d of the cases, want exactly the 7 valid ones", accepted)
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

// policy is what one row-level security policy says, as the catalog spells it.
type policy struct {
	cmd, roles, using, check, why string
}

// TestEveryPolicySaysWhatTheDesignSaysItSays is the predicate half of the test above.
//
// TestEveryTableForcesRowLevelSecurity counts policies and never reads one, so a policy that
// isolates nothing passes it: `USING (true)` on redaction_map, which holds by construction the
// personal data the masker took out of records, was green across the whole repository in review
// round 2. What isolation rested on then was every query in every package remembering
// `tenant_id = @tenant_id`, and on nothing else, so the first query written without that predicate
// would have been a cross-tenant read of personal data with every test still passing.
//
// So the predicates themselves are listed here, for EVERY table that has one and not only for the
// two that were new at the time. A migration that adds, widens or drops a policy has to say so in
// this table, where a reviewer sees it. The two `true` policies are the deliberate ones: they are
// granted to a single helper role each, they are read as the design's own exception, and
// TestHelperRolesHoldOnlyTheGrantsTheDesignNames is what keeps those roles' reach to one table.
//
// The behaviour behind the predicate is pinned separately, because a predicate that reads right
// and does nothing would pass this: TestTenantCannotReadOrWriteAnotherTenant for tenants, and
// TestAnotherTenantsLedgerAndRedactionMapAreInvisible in internal/pipeline for the two tables of
// B08.
func TestEveryPolicySaysWhatTheDesignSaysItSays(t *testing.T) {
	// The catalog renders the predicate from the parse tree, so tenant_id's domain shows as a
	// cast. Matching the rendered text is the point: it is what the database will actually apply.
	const (
		byTenantID = "((tenant_id)::text = current_tenant())"
		byID       = "((id)::text = current_tenant())"
	)
	want := map[string]policy{
		"tenants/tenant_isolation":       {"*", "public", byID, byID, "a tenant sees its own row (migration 00001)"},
		"outbox/tenant_isolation":        {"*", "public", byTenantID, byTenantID, "migration 00002"},
		"outbox/worker_claim_select":     {"r", "lawang_worker", "true", "", "the worker claims across tenants, which is its whole job (migration 00002)"},
		"outbox/worker_claim_update":     {"w", "lawang_worker", "true", "true", "attempts, lease_until and lease_token (migration 00002)"},
		"subscriptions/tenant_isolation": {"*", "public", byTenantID, byTenantID, "migration 00003"},
		"subscriptions/resolver_read":    {"r", "lawang_resolver", "true", "", "deriving the tenant is the resolver's whole job (migration 00003)"},
		"record_ledger/tenant_isolation": {"*", "public", byTenantID, byTenantID, "the supersede chain is per tenant (migration 00004)"},
		"redaction_map/tenant_isolation": {"*", "public", byTenantID, byTenantID, "personal data by construction (migration 00004)"},
	}

	db := open(t, "")
	ctx := testCtx(t)
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.relname, p.polname, p.polcmd::text, p.polpermissive,
			       coalesce((SELECT string_agg(r.rolname, ',' ORDER BY r.rolname)
			                   FROM pg_roles r WHERE r.oid = ANY(p.polroles)), 'public'),
			       coalesce(pg_get_expr(p.polqual, p.polrelid), ''),
			       coalesce(pg_get_expr(p.polwithcheck, p.polrelid), '')
			  FROM pg_policy p
			  JOIN pg_class c ON c.oid = p.polrelid
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1`, store.Schema)
		if err != nil {
			return err
		}
		defer rows.Close()
		seen := map[string]bool{}
		for rows.Next() {
			var rel, name, cmd, roles, using, check string
			var permissive bool
			if err := rows.Scan(&rel, &name, &cmd, &permissive, &roles, &using, &check); err != nil {
				return err
			}
			key := rel + "/" + name
			seen[key] = true
			// A restrictive policy narrows rather than grants, and reading one as though it
			// granted would read the whole set wrongly. None exists today; if one is added, this
			// test has to learn how to combine them before it can go on meaning anything.
			if !permissive {
				t.Errorf("policy %s is RESTRICTIVE, which this test does not know how to read: teach it before adding one", key)
				continue
			}
			got := policy{cmd: cmd, roles: roles, using: using, check: check}
			exp, ok := want[key]
			if !ok {
				t.Errorf("policy %s exists and nothing in the design names it: %+v", key, got)
				continue
			}
			exp.why = ""
			if got != exp {
				t.Errorf("policy %s is %+v, want %+v (%s): a policy that does not say what the design "+
					"says it says is a table whose isolation rests on every query remembering to filter",
					key, got, exp, want[key].why)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// The other direction, so that a migration which quietly drops a policy fails here rather
		// than at the first cross-tenant read, and so that a query returning nothing cannot pass.
		for key, p := range want {
			if !seen[key] {
				t.Errorf("policy %s is gone: %s", key, p.why)
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestHelperRolesHoldOnlyTheGrantsTheDesignNames is the grant half of the test above, and it
// guards every future migration the same way.
//
// Row-level security decides which rows a role may touch. It decides nothing about which TABLES a
// role may touch, and that is a grant, which no test watched until now. The two helper roles exist
// to do one narrow thing each with one table each: lawang_resolver reads six columns of
// subscriptions across tenants, because deriving the tenant is its whole job, and lawang_worker
// reads and updates a few columns of outbox. Every other table in the schema must be closed to
// both of them, and redaction_map is the sharpest case: it holds, by construction, the personal
// data that masking took out of records, and one grant on it would hand whoever holds a helper
// role every tenant's values at once.
//
// So the whole privilege surface of both roles is listed here, and anything that is not on the
// list fails. A migration that grants something new has to say so in this table, where a reviewer
// sees it, rather than passing every test in the repository in silence.
func TestHelperRolesHoldOnlyTheGrantsTheDesignNames(t *testing.T) {
	// role, table, privilege. Both grants in the schema today are column-level, and
	// has_any_column_privilege is what sees one, so a grant cannot hide behind a column list.
	allowed := map[string]string{
		"lawang_resolver/subscriptions/SELECT": "deriving the tenant is the resolver's whole job, six columns, read only (migration 00003)",
		"lawang_worker/outbox/SELECT":          "the worker claims and leases rows (migration 00002)",
		"lawang_worker/outbox/UPDATE":          "attempts, lease_until and lease_token (migration 00002)",
	}

	db := open(t, "")
	ctx := testCtx(t)
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT r.rolname, c.relname, p.priv
			  FROM pg_class c
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 CROSS JOIN (VALUES ('lawang_resolver'), ('lawang_worker')) AS r(rolname)
			 CROSS JOIN (VALUES ('SELECT'), ('INSERT'), ('UPDATE'), ('DELETE'),
			                    ('TRUNCATE'), ('REFERENCES'), ('TRIGGER')) AS p(priv)
			 WHERE n.nspname = $1
			   AND c.relkind IN ('r', 'p')
			   -- DELETE, TRUNCATE and TRIGGER exist only on a whole table, which is why the two
			   -- tests are separate rather than one.
			   AND (has_table_privilege(r.rolname, c.oid, p.priv)
			        OR (p.priv IN ('SELECT', 'INSERT', 'UPDATE', 'REFERENCES')
			            AND has_any_column_privilege(r.rolname, c.oid, p.priv)))
			 ORDER BY r.rolname, c.relname, p.priv`, store.Schema)
		if err != nil {
			return err
		}
		defer rows.Close()
		held := map[string]bool{}
		for rows.Next() {
			var role, table, priv string
			if err := rows.Scan(&role, &table, &priv); err != nil {
				return err
			}
			key := role + "/" + table + "/" + priv
			held[key] = true
			if _, ok := allowed[key]; !ok {
				t.Errorf("%s may %s %s, and nothing in the design says it should: either take the grant "+
					"away or add it to this test with the reason", role, priv, table)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// The other direction, so that a migration which quietly drops a grant the program needs
		// is caught here rather than at runtime, and so that a query returning nothing at all
		// cannot pass this test.
		for key, why := range allowed {
			if !held[key] {
				t.Errorf("%s is not granted: %s", key, why)
			}
		}
		return nil
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
