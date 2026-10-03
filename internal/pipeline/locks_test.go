package pipeline_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/pipeline"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/tenancy"
)

// TestTheLocksHeldAreTheKeysOfTheEntities. LockOrder pins the order; this pins what is ordered.
// The two together are the whole argument, because sorting one value and locking another is
// exactly the defect this changed (ADR 12, decision 1): the sort guarantees nothing unless the
// thing sorted IS the thing locked.
//
// It reads the locks the transaction really holds out of pg_locks, inside that transaction,
// rather than trusting the statement that was sent.
func TestTheLocksHeldAreTheKeysOfTheEntities(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	// Two entities, one of them twice, so the test also sees that a repeat is one lock.
	n, err := e.p.Normalize(e.ctx, pipeline.Delivery{
		Tenant: tenantA, Provider: fake.DefaultKey, ID: "01JDELIVERY0000000000000001",
		Body: body(t, ev("fake:task:z", "1", listA), ev("fake:task:a", "1", listA), ev("fake:task:z", "2", listA)),
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	ids := []string{"fake:task:a", "fake:task:z"}
	if got := len(pipeline.RecordsOf(n)); got != 3 {
		t.Fatalf("the delivery carries %d records, want 3 over %d entities", got, len(ids))
	}

	var want, held []int64
	err = e.db.TenantTx(e.ctx, tenantA, func(tx pgx.Tx) error {
		if _, err := e.p.Prepare(e.ctx, tx, tenantA, n); err != nil {
			return err
		}
		// The keys the entities of this delivery hash to, asked for separately from the
		// statement under test.
		rows, err := tx.Query(e.ctx, `SELECT hashtextextended($1 || chr(31) || $2 || chr(31) || id, 0)
		                                FROM unnest($3::text[]) AS id`,
			tenantA.String(), fake.DefaultKey, ids)
		if err != nil {
			return err
		}
		if want, err = pgx.CollectRows(rows, pgx.RowTo[int64]); err != nil {
			return err
		}
		// The advisory locks this transaction is actually holding. classid is the key's high 32
		// bits and objid its low 32, which is how Postgres stores a one-argument advisory key.
		rows, err = tx.Query(e.ctx, `SELECT classid, objid FROM pg_locks
		                              WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid()`)
		if err != nil {
			return err
		}
		held, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (int64, error) {
			var high, low uint32
			err := r.Scan(&high, &low)
			return int64(uint64(high)<<32 | uint64(low)), err //nolint:gosec // a 64 bit key put back together
		})
		return err
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	slices.Sort(want)
	slices.Sort(held)
	if !slices.Equal(held, want) {
		t.Errorf("the advisory locks held are %v, want %v: the keys of this delivery's entities, each once", held, want)
	}
}

// TestPrepareRefusesATransactionBoundToAnotherTenant. B10 is the first caller, and nothing but
// this check makes the transaction and the tenant agree. Without it the disagreement is a
// silence: row-level security hides the tenant's ledger rows and then refuses the insert, which
// reaches the caller as an ordinary database error, walks the retry ladder and dead-letters the
// delivery after every attempt over a wiring mistake no attempt could fix.
func TestPrepareRefusesATransactionBoundToAnotherTenant(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{Automation: pipeline.DropAutomation})
	bot := ev("fake:task:bot", "1", listA)
	bot.Automation = true
	n, err := e.p.Normalize(e.ctx, pipeline.Delivery{
		Tenant: tenantA, Provider: fake.DefaultKey, ID: "01JDELIVERY0000000000000001",
		Body: body(t, ev(entity, "1", listA), bot),
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	for name, run := range map[string]func(func(pgx.Tx) error) error{
		"bound to another tenant": func(fn func(pgx.Tx) error) error { return e.db.TenantTx(e.ctx, tenantB, fn) },
		"bound to no tenant":      func(fn func(pgx.Tx) error) error { return e.db.Tx(e.ctx, fn) },
	} {
		var out pipeline.Prepared
		err := run(func(tx pgx.Tx) error {
			var err error
			out, err = e.p.Prepare(e.ctx, tx, tenantA, n)
			return err
		})
		if !errors.Is(err, pipeline.ErrTxNotBoundToTenant) || !errors.Is(err, pipeline.ErrDeadLetter) {
			t.Errorf("%s: err = %v, want ErrTxNotBoundToTenant and ErrDeadLetter", name, err)
		}
		if out.Records != nil {
			t.Errorf("%s: %d records came back from a refusal", name, len(out.Records))
		}
		// The counters the stage had already reached still come back, which is what the caller
		// logs on the error path.
		if out.Automation != 1 {
			t.Errorf("%s: Automation = %d, want 1: the counters survive the refusal", name, out.Automation)
		}
	}
	// The premise: with the right transaction the same delivery goes through, so the refusals
	// above are about the binding and not about the delivery.
	if _, err := e.prepare(tenantA, n); err != nil {
		t.Errorf("the same delivery under the right tenant: %v", err)
	}
	// And nothing of tenant A was written under tenant B by the refused attempts.
	for _, row := range e.ledgerRows() {
		if row.recordID == "" {
			t.Error("a ledger row with no record id")
		}
	}
	if got := len(e.ledgerRows()); got != 1 {
		t.Errorf("the ledger holds %d rows, want the one the successful Prepare wrote", got)
	}
}

// TestCurrentReportsWhatIsBound is tenancy.Current on its own: it answers what the database
// says, and an unbound transaction is an error and never an empty tenant.
func TestCurrentReportsWhatIsBound(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	if err := e.db.TenantTx(e.ctx, tenantB, func(tx pgx.Tx) error {
		got, err := tenancy.Current(e.ctx, tx)
		if err != nil || got != tenantB {
			t.Errorf("Current = %q, %v, want %q", got, err, tenantB)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Tx(e.ctx, func(tx pgx.Tx) error {
		got, err := tenancy.Current(e.ctx, tx)
		if !errors.Is(err, tenancy.ErrNoTenantBound) || got != "" {
			t.Errorf("Current with nothing bound = %q, %v, want ErrNoTenantBound", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
