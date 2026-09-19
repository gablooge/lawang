package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/store"
	"github.com/gablooge/lawang/internal/tenancy"
)

// scratchJobs builds what B04 onwards really build: a tenant table that a helper role may read
// across tenants. The tenant policy is the one every table has; the second one is permissive and
// names the worker role. One row per tenant.
func scratchJobs(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := testCtx(t)
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		for _, stmt := range []string{
			"CREATE TABLE scratch_jobs (tenant tenant_id NOT NULL, note text NOT NULL DEFAULT '')",
			"ALTER TABLE scratch_jobs ENABLE ROW LEVEL SECURITY",
			"ALTER TABLE scratch_jobs FORCE ROW LEVEL SECURITY",
			"CREATE POLICY tenant_isolation ON scratch_jobs USING (tenant = current_tenant()) WITH CHECK (tenant = current_tenant())",
			"CREATE POLICY worker_claims ON scratch_jobs FOR SELECT TO lawang_worker USING (true)",
			"GRANT SELECT ON scratch_jobs TO lawang_worker",
		} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scratch table: %v", err)
	}
	for _, id := range []tenancy.ID{tenantA, tenantB} {
		err := db.TenantTx(ctx, id, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "INSERT INTO scratch_jobs (tenant) VALUES ($1)", id.String())
			return err
		})
		if err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
}

func visibleJobs(ctx context.Context, t *testing.T, tx pgx.Tx) []string {
	t.Helper()
	rows, err := tx.Query(ctx, "SELECT tenant::text FROM scratch_jobs ORDER BY tenant")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return got
}

// TestAHelperRoleTransactionIsCrossTenantWhateverIsBound pins what Postgres does, because it is
// not what one would guess. Permissive policies are ORed together: once the worker role's
// "USING (true)" applies, a bound tenant takes nothing away. So per-tenant work may never happen
// inside RoleTx, and RoleTx makes that hard to do by accident: tenancy.Bind refuses its
// transaction.
func TestAHelperRoleTransactionIsCrossTenantWhateverIsBound(t *testing.T) {
	db := open(t, "")
	scratchJobs(t, db)
	ctx := testCtx(t)

	err := db.RoleTx(ctx, store.RoleWorker, func(tx pgx.Tx) error {
		if got := visibleJobs(ctx, t, tx); len(got) != 2 {
			t.Fatalf("the worker role saw %v, want the rows of both tenants: the case does not build the state it names", got)
		}

		if err := tenancy.Bind(ctx, tx, tenantA); !errors.Is(err, tenancy.ErrCrossTenantTx) {
			t.Errorf("Bind inside RoleTx: err = %v, want ErrCrossTenantTx", err)
		}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		if err := tenancy.Bind(ctx, sp, tenantA); !errors.Is(err, tenancy.ErrCrossTenantTx) {
			t.Errorf("Bind inside a savepoint of RoleTx: err = %v, want ErrCrossTenantTx", err)
		}
		if err := sp.Rollback(ctx); err != nil {
			return err
		}

		// The reason for the refusal, shown by going around it: with tenant_a bound the hard way,
		// tenant_b's row is still there.
		if _, err := tx.Exec(ctx, "SELECT set_config('lawang.tenant', $1, true)", tenantA.String()); err != nil {
			return err
		}
		if got := visibleJobs(ctx, t, tx); len(got) != 2 {
			t.Errorf("under the worker role with tenant_a bound, saw %v. Postgres used to show both tenants here; "+
				"if that has changed, the comments on store.RoleTx and tenancy.Bind are out of date", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The second transaction, as the application role, is the one that is narrow.
	err = db.TenantTx(ctx, tenantA, func(tx pgx.Tx) error {
		if got := visibleJobs(ctx, t, tx); len(got) != 1 || got[0] != tenantA.String() {
			t.Errorf("TenantTx(tenant_a) saw %v, want only its own row", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// assertCleanConnection runs the next transaction on a pool of one connection and fails if it
// carries a role or a tenant. It has a deadline of its own: a connection that was never given back
// makes this wait, and it must fail instead of stalling the package.
func assertCleanConnection(t *testing.T, db *store.DB, after string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := db.Tx(ctx, func(tx pgx.Tx) error {
		var user string
		var tenant *string
		if err := tx.QueryRow(ctx, "SELECT current_user, current_setting('lawang.tenant', true)").Scan(&user, &tenant); err != nil {
			return err
		}
		if user != "lawang" {
			t.Errorf("after %s, the next transaction runs as %q", after, user)
		}
		if tenant != nil && *tenant != "" {
			t.Errorf("after %s, the next transaction has tenant %q bound", after, *tenant)
		}
		if got := visibleJobs(ctx, t, tx); len(got) != 0 {
			t.Errorf("after %s, the next transaction saw %v", after, got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("after %s, the next transaction failed: %v", after, err)
	}
}

// bindAround binds a tenant inside RoleTx the way tenancy.Bind refuses to, so that the leak tests
// below leave both a role and a tenant behind for the next transaction to find.
func bindAround(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "SELECT set_config('lawang.tenant', $1, true)", tenantA.String())
	return err
}

// The three tests below pin a property that rests on pgx internals: the deferred rollback in
// pgx.BeginFunc, and the pool destroying a connection that comes back in the middle of something.
// A pgx upgrade, or a move away from BeginFunc, must not be able to leave a role or a tenant on a
// pooled connection with the rest of the suite still green.
//
// They do not have the same teeth, and the difference is worth knowing before trusting them:
//
//   - The panic test can be failed from this package: a begin written by hand without the deferred
//     rollback fails it.
//   - The two cancellation tests CANNOT be failed by any mutation of this package. What they pin
//     belongs to pgxpool, not to store: a connection that is released in the middle of an operation
//     (a transaction still open, a statement interrupted by the cancel) is destroyed and never
//     handed out again, so no SET LOCAL or set_config can ride along, whatever begin does. Nothing
//     here implements that and nothing here can break it. They exist for one reason: to notice the
//     pgx upgrade that changes it. If one of them goes red, read the pgx changelog first.

func TestAPanicLeavesNoRoleOrTenantOnTheConnection(t *testing.T) {
	db := open(t, "&pool_max_conns=1")
	scratchJobs(t, db)
	ctx := testCtx(t)

	panics := func(name string, run func() error) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: the panic did not reach the caller", name)
			}
			assertCleanConnection(t, db, "a panic inside "+name)
		}()
		_ = run()
	}
	panics("Tx", func() error {
		return db.Tx(ctx, func(tx pgx.Tx) error {
			if err := bindAround(ctx, tx); err != nil {
				t.Fatal(err)
			}
			panic("boom")
		})
	})
	panics("TenantTx", func() error {
		return db.TenantTx(ctx, tenantA, func(pgx.Tx) error { panic("boom") })
	})
	panics("RoleTx", func() error {
		return db.RoleTx(ctx, store.RoleWorker, func(tx pgx.Tx) error {
			if err := bindAround(ctx, tx); err != nil {
				t.Fatal(err)
			}
			panic("boom")
		})
	})
}

// Pins pgxpool behaviour, not code of this package (see the comment above the panic test): no
// mutation of store fails it. It is here to catch a pgx upgrade.
func TestACancelledContextLeavesNoRoleOrTenantOnTheConnection(t *testing.T) {
	db := open(t, "&pool_max_conns=1")
	scratchJobs(t, db)

	// Cancelled after the work and before the commit.
	for name, run := range map[string]func(context.Context, context.CancelFunc) error{
		"TenantTx": func(ctx context.Context, cancel context.CancelFunc) error {
			return db.TenantTx(ctx, tenantA, func(pgx.Tx) error { cancel(); return nil })
		},
		"RoleTx": func(ctx context.Context, cancel context.CancelFunc) error {
			return db.RoleTx(ctx, store.RoleWorker, func(tx pgx.Tx) error {
				err := bindAround(ctx, tx)
				cancel()
				return err
			})
		},
	} {
		ctx, cancel := context.WithCancel(testCtx(t))
		if err := run(ctx, cancel); !errors.Is(err, context.Canceled) {
			t.Errorf("%s cancelled before commit: err = %v, want context.Canceled", name, err)
		}
		cancel()
		assertCleanConnection(t, db, name+" cancelled before its commit")
	}
}

// Like the test above it, this pins pgxpool behaviour (a connection released while a statement was
// being interrupted is destroyed), which no mutation of store can break. It is here to catch a pgx
// upgrade.
func TestACancelInFlightLeavesNoRoleOrTenantOnTheConnection(t *testing.T) {
	db := open(t, "&pool_max_conns=1")
	scratchJobs(t, db)

	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	timer := time.AfterFunc(300*time.Millisecond, cancel)
	defer timer.Stop()

	start := time.Now()
	err := db.RoleTx(ctx, store.RoleWorker, func(tx pgx.Tx) error {
		if err := bindAround(ctx, tx); err != nil {
			return err
		}
		// Bounded by the server as well, so a cancel that never arrives fails the test in seconds.
		_, err := tx.Exec(ctx, "SELECT pg_sleep(10)")
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("RoleTx cancelled while a statement was running: err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("RoleTx returned %s after the cancel, the statement was not interrupted", elapsed.Round(time.Millisecond))
	}
	assertCleanConnection(t, db, "RoleTx cancelled while a statement was running")
}
