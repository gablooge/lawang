package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gablooge/sluiceway/internal/outbox"
	"github.com/gablooge/sluiceway/internal/store"
	"github.com/gablooge/sluiceway/internal/tenancy"
	"github.com/gablooge/sluiceway/internal/testdb"
)

const (
	tenantA = tenancy.ID("tenant_a")
	tenantB = tenancy.ID("tenant_b")
	lease   = time.Minute
)

type env struct {
	t   *testing.T
	ctx context.Context
	db  *store.DB
	ob  *outbox.Outbox
	tdb testdb.Database
}

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
	return &env{t: t, ctx: ctx, db: db, ob: outbox.New(db), tdb: tdb}
}

// accept stores a delivery whose body is unique to (key, version).
func (e *env) accept(tenant tenancy.ID, key string, version int) string {
	e.t.Helper()
	id, fresh, err := e.ob.Accept(e.ctx, tenant, outbox.Delivery{
		Provider: "fake", OrderingKey: key, RawBody: fmt.Appendf(nil, `{"key":%q,"v":%d}`, key, version),
	})
	if err != nil || !fresh {
		e.t.Fatalf("Accept(%s, %s, v%d) = fresh %v, err %v", tenant, key, version, fresh, err)
	}
	return id
}

func (e *env) claim() []outbox.Claimed {
	e.t.Helper()
	got, err := e.ob.Claim(e.ctx, 10, lease)
	if err != nil {
		e.t.Fatalf("Claim: %v", err)
	}
	return got
}

func (e *env) claimOne(wantID string) outbox.Claimed {
	e.t.Helper()
	got := e.claim()
	if len(got) != 1 || got[0].ID != wantID {
		e.t.Fatalf("Claim = %v, want exactly row %s", claimedIDs(got), wantID)
	}
	return got[0]
}

func (e *env) claimNone(why string) {
	e.t.Helper()
	if got := e.claim(); len(got) != 0 {
		e.t.Fatalf("Claim = %v, want nothing: %s", claimedIDs(got), why)
	}
}

// admin changes rows behind the application's back, to move time along.
func (e *env) admin(sql string) { testdb.Exec(e.t, e.tdb.AdminURL, sql) }

func claimedIDs(cs []outbox.Claimed) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func TestAcceptIsIdempotentPerTenant(t *testing.T) {
	e := setup(t)
	d := outbox.Delivery{Provider: "fake", OrderingKey: "task:1", RawBody: []byte(`{"a":1}`)}

	id, fresh, err := e.ob.Accept(e.ctx, tenantA, d)
	if err != nil || !fresh || id == "" {
		t.Fatalf("first Accept = %q, %v, %v", id, fresh, err)
	}
	again, fresh, err := e.ob.Accept(e.ctx, tenantA, d)
	if err != nil || fresh || again != "" {
		t.Fatalf("re-sent Accept = %q, fresh %v, err %v, want a no-op", again, fresh, err)
	}

	// Two tenants may connect the same provider workspace. The second one's identical delivery is
	// its own, not a duplicate of the first tenant's.
	if _, fresh, err := e.ob.Accept(e.ctx, tenantB, d); err != nil || !fresh {
		t.Fatalf("same bytes for another tenant: fresh %v, err %v, want accepted", fresh, err)
	}

	if got := e.claim(); len(got) != 2 {
		t.Errorf("claimable rows = %d, want 2 (one per tenant)", len(got))
	}
}

func TestAcceptRefusesBadInput(t *testing.T) {
	e := setup(t)
	for name, d := range map[string]outbox.Delivery{
		"no provider":     {OrderingKey: "k", RawBody: []byte("x")},
		"no ordering key": {Provider: "fake", RawBody: []byte("x")},
	} {
		if _, _, err := e.ob.Accept(e.ctx, tenantA, d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, _, err := e.ob.Accept(e.ctx, tenancy.ID(""), outbox.Delivery{Provider: "fake", OrderingKey: "k"}); err == nil {
		t.Error("accepted a delivery with no tenant")
	}
}

func TestSecondVersionIsNotClaimableWhileTheFirstIsInFlight(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)
	v2 := e.accept(tenantA, "task:1", 2)

	c1 := e.claimOne(v1)
	e.claimNone("v1 is leased, so v2 must wait behind it")

	if err := e.ob.MarkPrepared(e.ctx, c1); err != nil {
		t.Fatal(err)
	}
	e.claimNone("v1 is prepared but not delivered, so v2 must still wait")

	if err := e.ob.MarkDelivered(e.ctx, c1); err != nil {
		t.Fatal(err)
	}
	c2 := e.claimOne(v2)
	if err := e.ob.MarkDelivered(e.ctx, c2); err != nil {
		t.Fatal(err)
	}
	e.claimNone("everything is delivered")
}

func TestOrderingKeysAreIndependentAcrossEntitiesAndTenants(t *testing.T) {
	e := setup(t)
	e.accept(tenantA, "task:1", 1)
	e.accept(tenantA, "task:1", 2) // behind the head
	e.accept(tenantA, "task:2", 1)
	e.accept(tenantB, "task:1", 1) // same key, other tenant: its own queue

	if got := e.claim(); len(got) != 3 {
		t.Errorf("claimed %d rows, want the 3 heads", len(got))
	}
}

func TestBackoffHoldsTheWholeKey(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)
	e.accept(tenantA, "task:1", 2)

	c1 := e.claimOne(v1)
	if err := e.ob.Fail(e.ctx, c1, outbox.Ladder{time.Hour}, "sink 503"); err != nil {
		t.Fatal(err)
	}
	e.claimNone("v1 is waiting out its backoff, and v2 may not overtake it")

	e.admin("UPDATE sluiceway.outbox SET next_attempt_at = now() - interval '1 second'")
	c1 = e.claimOne(v1)
	if c1.Attempt != 2 {
		t.Errorf("Attempt = %d, want 2", c1.Attempt)
	}
	row, err := e.ob.Get(e.ctx, tenantA, v1)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastError != "sink 503" || row.State != outbox.StatePending {
		t.Errorf("row = state %q, last_error %q", row.State, row.LastError)
	}
}

func TestRetryKeepsThePreparedState(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)
	c := e.claimOne(v1)
	if err := e.ob.MarkPrepared(e.ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := e.ob.Fail(e.ctx, c, outbox.Ladder{time.Hour}, "sink timeout"); err != nil {
		t.Fatal(err)
	}
	row, _ := e.ob.Get(e.ctx, tenantA, v1)
	if row.State != outbox.StatePrepared {
		t.Errorf("state after a delivery retry = %q, want prepared (it must not prepare twice)", row.State)
	}
	e.admin("UPDATE sluiceway.outbox SET next_attempt_at = now() - interval '1 second'")
	e.claimOne(v1)
}

func TestExhaustedLadderParksTheRowAndReplayRevivesIt(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)
	v2 := e.accept(tenantA, "task:1", 2)

	c1 := e.claimOne(v1)
	if err := e.ob.Fail(e.ctx, c1, outbox.Ladder{}, "sink 503"); err != nil {
		t.Fatal(err)
	}
	row, _ := e.ob.Get(e.ctx, tenantA, v1)
	if row.State != outbox.StateDead || row.DeadReason == "" || row.FinishedAt == nil {
		t.Errorf("row = state %q, reason %q, finished %v, want a dead letter", row.State, row.DeadReason, row.FinishedAt)
	}

	// A dead letter does not hold its entity hostage.
	c2 := e.claimOne(v2)
	if err := e.ob.MarkDelivered(e.ctx, c2); err != nil {
		t.Fatal(err)
	}

	if err := e.ob.Replay(e.ctx, tenantB, v1); !errors.Is(err, outbox.ErrNotFound) {
		t.Errorf("another tenant replaying the row: err = %v, want ErrNotFound", err)
	}
	if err := e.ob.Replay(e.ctx, tenantA, v1); err != nil {
		t.Fatal(err)
	}
	c1 = e.claimOne(v1)
	if c1.Attempt != 1 {
		t.Errorf("Attempt after replay = %d, want 1", c1.Attempt)
	}
	if err := e.ob.Replay(e.ctx, tenantA, v1); !errors.Is(err, outbox.ErrNotFound) {
		t.Errorf("replaying a row that is not dead: err = %v, want ErrNotFound", err)
	}
}

func TestAnExpiredLeaseIsTakenOverAndTheOldHolderIsShutOut(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)

	crashed := e.claimOne(v1)
	e.claimNone("the lease is still running")

	e.admin("UPDATE sluiceway.outbox SET lease_until = now() - interval '1 second'")
	takeover := e.claimOne(v1)
	if takeover.Attempt != 2 {
		t.Errorf("Attempt = %d, want 2", takeover.Attempt)
	}

	// The first worker was only slow, not dead, and now comes back.
	for name, err := range map[string]error{
		"MarkPrepared":  e.ob.MarkPrepared(e.ctx, crashed),
		"MarkDelivered": e.ob.MarkDelivered(e.ctx, crashed),
		"Fail":          e.ob.Fail(e.ctx, crashed, outbox.DefaultLadder, "late"),
		"MarkDead":      e.ob.MarkDead(e.ctx, crashed, "late", "late"),
	} {
		if !errors.Is(err, outbox.ErrLeaseLost) {
			t.Errorf("%s by the old holder: err = %v, want ErrLeaseLost", name, err)
		}
	}
	if err := e.ob.MarkDelivered(e.ctx, takeover); err != nil {
		t.Errorf("the current holder: %v", err)
	}
	if err := e.ob.MarkDelivered(e.ctx, takeover); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Errorf("delivering twice: err = %v, want ErrLeaseLost", err)
	}
	if err := e.ob.MarkDelivered(e.ctx, outbox.Claimed{ID: v1, Tenant: tenantA}); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Errorf("a Claimed that never came from Claim: err = %v, want ErrLeaseLost", err)
	}
}

func TestTheWorkerRoleNeverReadsAPayload(t *testing.T) {
	e := setup(t)
	e.accept(tenantA, "task:1", 1)

	for _, stmt := range []string{
		"SELECT raw_body FROM outbox",
		"SELECT * FROM outbox",
		"SELECT provider, delivery_id FROM outbox",
		"UPDATE outbox SET state = 'delivered'",
		"UPDATE outbox SET tenant_id = 'tenant_b'",
		"DELETE FROM outbox",
		"INSERT INTO outbox (id, tenant_id, provider, delivery_id, ordering_key, raw_body) VALUES ('x','tenant_a','p','d','k','')",
	} {
		err := e.db.RoleTx(e.ctx, store.RoleWorker, func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, stmt)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("worker role ran %q: err = %v, want permission denied", stmt, err)
		}
	}

	// What it may see is the scheduling columns, across tenants.
	err := e.db.RoleTx(e.ctx, store.RoleWorker, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(e.ctx, "SELECT count(id) FROM outbox").Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("worker role sees %d rows, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// And the application role with no tenant bound sees nothing at all.
	err = e.db.Tx(e.ctx, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(e.ctx, "SELECT count(*) FROM outbox").Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("unbound application role sees %d rows, want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGetIsTenantScoped(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	if _, err := e.ob.Get(e.ctx, tenantB, id); !errors.Is(err, outbox.ErrNotFound) {
		t.Errorf("another tenant's Get: err = %v, want ErrNotFound", err)
	}
}

// TestConcurrentClaimersNeverShareARow hammers Claim from many goroutines, several times over.
func TestConcurrentClaimersNeverShareARow(t *testing.T) {
	e := setup(t)
	const (
		rounds   = 5
		rows     = 120
		claimers = 8
	)
	for round := range rounds {
		want := make(map[string]bool, rows)
		for i := range rows {
			tenant := tenantA
			if i%2 == 1 {
				tenant = tenantB
			}
			want[e.accept(tenant, fmt.Sprintf("r%d:entity:%d", round, i), 1)] = true
		}

		var mu sync.Mutex
		seen := make(map[string]int)
		var wg sync.WaitGroup
		for range claimers {
			wg.Go(func() {
				for {
					got, err := e.ob.Claim(e.ctx, 7, lease)
					if err != nil {
						t.Errorf("Claim: %v", err)
						return
					}
					if len(got) == 0 {
						return
					}
					mu.Lock()
					for _, c := range got {
						seen[c.ID]++
					}
					mu.Unlock()
				}
			})
		}
		wg.Wait()

		for id, n := range seen {
			if n != 1 {
				t.Errorf("round %d: row %s was claimed %d times", round, id, n)
			}
			if !want[id] {
				t.Errorf("round %d: claimed unexpected row %s", round, id)
			}
		}
		if len(seen) != rows {
			t.Errorf("round %d: %d rows claimed, want %d", round, len(seen), rows)
		}
		e.admin("UPDATE sluiceway.outbox SET state = 'delivered', lease_until = NULL, lease_token = NULL, finished_at = now() WHERE state <> 'delivered'")
	}
}

// TestConcurrentWorkersDeliverEachEntityInOrder is the FIFO guarantee under load: no two versions
// of one entity are ever in flight together, and each entity's versions finish in arrival order.
func TestConcurrentWorkersDeliverEachEntityInOrder(t *testing.T) {
	e := setup(t)
	const (
		entities = 25
		versions = 6
		workers  = 8
	)
	// Versions of one entity sit next to each other in the queue, which is the worst case: a claim
	// that ignored the head-of-key rule would pick up several of them in a single batch.
	for k := range entities {
		tenant := tenantA
		if k%2 == 1 {
			tenant = tenantB
		}
		for v := 1; v <= versions; v++ {
			e.accept(tenant, fmt.Sprintf("entity:%d", k), v)
		}
	}

	var (
		mu        sync.Mutex
		inFlight  = map[string]string{} // tenant/key -> row id
		lastSeq   = map[string]int64{}
		delivered int
		wg        sync.WaitGroup
	)
	for range workers {
		wg.Go(func() {
			idle := 0
			for idle < 20 {
				got, err := e.ob.Claim(e.ctx, 3, lease)
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				if len(got) == 0 {
					// Heads may all be leased by other workers right now. Look again shortly.
					idle++
					time.Sleep(5 * time.Millisecond)
					continue
				}
				idle = 0

				// Everything in the batch is in flight from the moment it is claimed, not from
				// the moment its turn comes.
				batch := make([]outbox.Row, len(got))
				for i, c := range got {
					row, err := e.ob.Get(e.ctx, c.Tenant, c.ID)
					if err != nil {
						t.Errorf("Get: %v", err)
						return
					}
					batch[i] = row
					key := row.TenantID + "/" + row.OrderingKey
					mu.Lock()
					if other, busy := inFlight[key]; busy {
						t.Errorf("%s: row %s claimed while %s is still in flight", key, c.ID, other)
					}
					if row.Seq <= lastSeq[key] {
						t.Errorf("%s: seq %d claimed after seq %d was delivered", key, row.Seq, lastSeq[key])
					}
					inFlight[key] = c.ID
					mu.Unlock()
				}

				for i, c := range got {
					key := batch[i].TenantID + "/" + batch[i].OrderingKey
					time.Sleep(time.Millisecond) // the work
					if err := e.ob.MarkDelivered(e.ctx, c); err != nil {
						t.Errorf("MarkDelivered: %v", err)
						return
					}
					mu.Lock()
					lastSeq[key] = batch[i].Seq
					delete(inFlight, key)
					delivered++
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()

	if delivered != entities*versions {
		t.Errorf("delivered %d rows, want %d", delivered, entities*versions)
	}
}
