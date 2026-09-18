package outbox_test

// The interleavings that timing alone cannot produce: each test here holds one transaction at a
// chosen point while another one runs.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/sluiceway/internal/outbox"
)

// adminConn is a superuser session that stays open for the whole test.
func (e *env) adminConn() *pgx.Conn {
	e.t.Helper()
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		e.t.Fatalf("admin connect: %v", err)
	}
	e.t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// waitForLockWaiter returns once some session of this database is waiting for an advisory lock.
// early reports that the operation expected to wait has finished instead, which fails the test.
func (e *env) waitForLockWaiter(admin *pgx.Conn, early func() bool, what string) {
	e.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		var waiting int
		err := admin.QueryRow(e.ctx, `
			SELECT count(*) FROM pg_locks
			 WHERE locktype = 'advisory' AND NOT granted
			   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&waiting)
		if err != nil {
			e.t.Fatalf("pg_locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		if early() {
			e.t.Fatalf("%s finished without waiting", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("%s never started waiting", what)
}

const gateLock = 7001

// gateClaimOf makes any Claim stop at the moment it leases row id: after the statement has taken
// its snapshot and chosen its heads, and before it locks the rows that sort after id. That is the
// window in which another transaction can change a chosen row, and timing alone cannot hit it.
// The returned function lets the claim go on.
//
// It relies on the claim locking its rows one at a time, in seq order, as it updates them. A test
// that uses it must check that the row it changes in the window was in fact not locked yet.
func (e *env) gateClaimOf(id string) (admin *pgx.Conn, open func()) {
	e.t.Helper()
	e.admin(fmt.Sprintf(`
		CREATE FUNCTION sluiceway.test_gate() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		  IF NEW.id = TG_ARGV[0] AND NEW.lease_token IS NOT NULL
		     AND NEW.lease_token IS DISTINCT FROM OLD.lease_token THEN
		    PERFORM pg_advisory_xact_lock(%d);
		  END IF;
		  RETURN NEW;
		END $$;
		CREATE TRIGGER test_gate BEFORE UPDATE ON sluiceway.outbox
		  FOR EACH ROW EXECUTE FUNCTION sluiceway.test_gate('%s');`, gateLock, id))
	admin = e.adminConn()
	if _, err := admin.Exec(e.ctx, "SELECT pg_advisory_lock($1)", gateLock); err != nil {
		e.t.Fatalf("close the gate: %v", err)
	}
	return admin, func() {
		e.t.Helper()
		if _, err := admin.Exec(e.ctx, "SELECT pg_advisory_unlock($1)", gateLock); err != nil {
			e.t.Fatalf("open the gate: %v", err)
		}
	}
}

// claimInBackground runs one Claim that is expected to stop at the gate.
func (e *env) claimInBackground() (result <-chan []outbox.Claimed, finished func() bool) {
	ch := make(chan []outbox.Claimed, 1)
	var done atomic.Bool
	go func() {
		got, err := e.ob.Claim(e.ctx, 10, lease)
		if err != nil {
			e.t.Errorf("gated Claim: %v", err)
		}
		done.Store(true)
		ch <- got
	}()
	return ch, done.Load
}

// acceptAndHold accepts a delivery for tenantA in a transaction that stays open, holding the
// ordering key's lock, until commit is called.
func (e *env) acceptAndHold(key string, version int) (id string, commit func()) {
	e.t.Helper()
	inserted := make(chan string, 1)
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	e.t.Cleanup(release) // a failing test must not leave it open, or closing the pool would hang
	done := make(chan error, 1)
	go func() {
		done <- e.db.TenantTx(e.ctx, tenantA, func(tx pgx.Tx) error {
			id, _, err := outbox.AcceptIn(e.ctx, tx, tenantA, outbox.Delivery{
				Provider: "fake", OrderingKey: key, RawBody: fmt.Appendf(nil, `{"key":%q,"v":%d}`, key, version),
			})
			inserted <- id
			if err != nil {
				return err
			}
			<-hold
			return nil
		})
	}()
	if id = <-inserted; id == "" {
		e.t.Fatalf("held Accept: %v", <-done)
	}
	return id, func() {
		e.t.Helper()
		release()
		if err := <-done; err != nil {
			e.t.Fatalf("held Accept: %v", err)
		}
	}
}

// TestAcceptsOfOneKeyCommitInQueueOrder is the late commit: seq is assigned by the INSERT, not by
// the COMMIT. If v1 could be inserted first and committed last, a claim in between would lease v2,
// and the next one would lease v1 while v2 is still in flight. So the second Accept of a key has
// to wait for the first one's transaction.
func TestAcceptsOfOneKeyCommitInQueueOrder(t *testing.T) {
	e := setup(t)
	admin := e.adminConn()

	// T1 accepts v1 and stays open.
	v1, commitT1 := e.acceptAndHold("task:1", 1)

	// T2 accepts v2 of the same entity. It must not get past T1.
	type accepted struct {
		id  string
		err error
	}
	t2 := make(chan accepted, 1)
	var t2Finished atomic.Bool
	go func() {
		id, _, err := e.ob.Accept(e.ctx, tenantA, outbox.Delivery{
			Provider: "fake", OrderingKey: "task:1", RawBody: []byte(`{"v":2}`),
		})
		t2Finished.Store(true)
		t2 <- accepted{id, err}
	}()
	e.waitForLockWaiter(admin, t2Finished.Load, "Accept of v2 while v1's transaction is open")

	// Another entity is not held up, and it is all a claim can see.
	other := e.accept(tenantA, "task:2", 1)
	e.claimOne(other)
	if t2Finished.Load() {
		t.Fatal("Accept of v2 finished while v1's transaction is open")
	}

	commitT1()
	second := <-t2
	if second.err != nil || second.id == "" {
		t.Fatalf("T2 = %q, %v", second.id, second.err)
	}

	c1 := e.claimOne(v1)
	e.claimNone("v1 is in flight, so v2 must wait behind it")
	if err := e.ob.MarkDelivered(e.ctx, c1); err != nil {
		t.Fatal(err)
	}
	e.claimOne(second.id)
}

// TestReplayWaitsForAnOpenAcceptOfItsKey: a replay assigns a seq too, so it has the same late
// commit to fear. If it could slip in behind an Accept that is still open, the replayed row would
// have the higher seq but be visible first: it would be leased, and then the accepted row would
// appear ahead of it and be leased as well.
func TestReplayWaitsForAnOpenAcceptOfItsKey(t *testing.T) {
	e := setup(t)
	admin := e.adminConn()
	v1 := e.accept(tenantA, "task:1", 1)
	if err := e.ob.MarkDead(e.ctx, e.claimOne(v1), "normalizer", "bad shape"); err != nil {
		t.Fatal(err)
	}

	v2, commitV2 := e.acceptAndHold("task:1", 2)

	replayed := make(chan error, 1)
	var finished atomic.Bool
	go func() {
		err := e.ob.Replay(e.ctx, tenantA, v1)
		finished.Store(true)
		replayed <- err
	}()
	e.waitForLockWaiter(admin, finished.Load, "Replay of v1 while an Accept of its key is open")
	e.claimNone("v2 is not committed and v1 is not replayed yet")

	commitV2()
	if err := <-replayed; err != nil {
		t.Fatal(err)
	}
	c2 := e.claimOne(v2)
	e.claimNone("v2 is in flight, and the replayed v1 is behind it")
	if err := e.ob.MarkDelivered(e.ctx, c2); err != nil {
		t.Fatal(err)
	}
	e.claimOne(v1)
}

// TestAKeyWithAnyLeasedRowIsNotClaimable pins the claim's second line of defense. The row below
// reaches the table the way a late commit would without the ordering-key lock: ahead of a version
// that is already in flight.
func TestAKeyWithAnyLeasedRowIsNotClaimable(t *testing.T) {
	e := setup(t)
	v2 := e.accept(tenantA, "task:1", 2)
	e.claimOne(v2)

	e.admin(`INSERT INTO sluiceway.outbox (id, seq, tenant_id, provider, delivery_id, ordering_key, raw_body)
	         OVERRIDING SYSTEM VALUE VALUES ('late', 0, 'tenant_a', 'fake', 'late', 'task:1', '')`)
	e.claimNone("v2 is in flight, so nothing else of its entity is claimable, not even an earlier row")

	// An expired lease is not a live one, and then the earlier row is simply the head.
	e.admin("UPDATE sluiceway.outbox SET lease_until = now() - interval '1 second' WHERE id = '" + v2 + "'")
	e.claimOne("late")
	e.claimNone("now the earlier row is in flight")
}

// TestClaimRechecksARowFinishedAfterItsSnapshot: v1's lease has run out, and its slow holder is
// about to finish. A claimer's snapshot sees v1 as a head with an expired lease. The holder's
// MarkDelivered commits before the claimer reaches the row lock. The claimer then looks at the
// new version of the row: no lease, due. Only the state says it is finished, and a claim that
// did not look at it again would hand a delivered row to a worker.
func TestClaimRechecksARowFinishedAfterItsSnapshot(t *testing.T) {
	e := setup(t)
	gate := e.accept(tenantA, "gate", 1) // lower seq, so the claim reaches it first
	v1 := e.accept(tenantA, "task:1", 1)

	var slow outbox.Claimed
	for _, c := range e.claim() {
		if c.ID == v1 {
			slow = c
		}
	}
	e.admin("UPDATE sluiceway.outbox SET lease_until = now() - interval '1 second'")

	admin, open := e.gateClaimOf(gate)
	result, finished := e.claimInBackground()
	e.waitForLockWaiter(admin, finished, "the gated Claim")

	// If the claim had locked v1 already, this would wait for it, and the test would prove nothing.
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	if err := e.ob.MarkDelivered(ctx, slow); err != nil {
		open()
		t.Fatalf("the slow holder finishing inside the window: %v (a timeout means the claim no longer locks row by row, and this test has lost its window)", err)
	}

	open()
	got := <-result
	if len(got) != 1 || got[0].ID != gate {
		t.Errorf("Claim = %v, want only %s: v1 was delivered before the claim locked it", claimedIDs(got), gate)
	}
	row, err := e.ob.Get(e.ctx, tenantA, v1)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != outbox.StateDelivered || row.LeaseUntil != nil {
		t.Errorf("v1 = state %q, lease %v, want delivered and unleased", row.State, row.LeaseUntil)
	}
}

// TestClaimRechecksARowReplayedAfterItsSnapshot: a claimer's snapshot sees v1 as the head of its
// key. Before it reaches the row lock, v1 is claimed by someone else and dies, v2 becomes the head
// and goes in flight, and v1 is replayed to the back. The new version of v1 is pending, unleased
// and due, and the head the claimer joined it to is the stale one. Only its seq says it is no
// longer that head.
func TestClaimRechecksARowReplayedAfterItsSnapshot(t *testing.T) {
	e := setup(t)
	gate := e.accept(tenantA, "gate", 1)
	v1 := e.accept(tenantA, "task:1", 1)
	v2 := e.accept(tenantA, "task:1", 2)

	admin, open := e.gateClaimOf(gate)
	result, finished := e.claimInBackground()
	e.waitForLockWaiter(admin, finished, "the gated Claim")

	// The gate row is locked by the paused claim, so these claims skip it. That the first of them
	// gets v1 also shows the paused claim had not locked it yet.
	c1 := e.claimOne(v1)
	if err := e.ob.MarkDead(e.ctx, c1, "normalizer", "bad shape"); err != nil {
		t.Fatal(err)
	}
	c2 := e.claimOne(v2)
	if err := e.ob.Replay(e.ctx, tenantA, v1); err != nil {
		t.Fatal(err)
	}
	// A replay sets next_attempt_at to the start of its own transaction. Put it before the start
	// of the paused claim's, as for a replay that began first and then waited for the key's lock:
	// otherwise the paused claim drops v1 for not being due, and its seq is never looked at.
	e.admin("UPDATE sluiceway.outbox SET next_attempt_at = now() - interval '1 hour' WHERE id = '" + v1 + "'")

	open()
	got := <-result
	if len(got) != 1 || got[0].ID != gate {
		t.Errorf("Claim = %v, want only %s: v1 went to the back of its queue, and v2 is in flight", claimedIDs(got), gate)
	}
	e.claimNone("v2 is in flight")
	if err := e.ob.MarkDelivered(e.ctx, c2); err != nil {
		t.Fatal(err)
	}
	e.claimOne(v1)
}
