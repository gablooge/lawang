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

// The advisory locks of the gates below. gateLock is held by the test while a gate is closed.
// firstLock is taken by the first statement that reaches the claim's gate, so that only that one
// stops, and the claims a test makes inside the window pass through.
const (
	gateLock  = 7001
	firstLock = 7002
)

// Which advisory lock a session is waiting for. A lock taken with one bigint shows up in pg_locks
// split into classid (the high half) and objid (the low half). The lock of an ordering key is a
// 64-bit hash, and none of the keys used here hashes to the gate's number.
const (
	onTheGate       = "(classid = 0 AND objid = 7001)"
	onAnOrderingKey = "NOT (classid = 0 AND objid = 7001)"
)

// waitsOn reports whether an operation ends up waiting for an advisory lock of the given kind
// (true), or finishes without doing so (false).
func (e *env) waitsOn(admin *pgx.Conn, which string, finished func() bool) bool {
	e.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		var waiting int
		err := admin.QueryRow(e.ctx, `
			SELECT count(*) FROM pg_locks
			 WHERE locktype = 'advisory' AND NOT granted AND `+which+`
			   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&waiting)
		if err != nil {
			e.t.Fatalf("pg_locks: %v", err)
		}
		if waiting > 0 {
			return true
		}
		if finished() {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("after a minute, neither waiting for an advisory lock nor finished")
	return false
}

// waitForLockWaiter returns once the operation is waiting for an advisory lock of the given kind.
// If it finishes instead, the test fails.
func (e *env) waitForLockWaiter(admin *pgx.Conn, which string, finished func() bool, what string) {
	e.t.Helper()
	if !e.waitsOn(admin, which, finished) {
		e.t.Fatalf("%s finished without waiting", what)
	}
}

// closeGate takes gateLock in a superuser session of its own. The returned function opens it. A
// test that fails with the gate closed does not hang: closing the session opens it too.
func (e *env) closeGate() (admin *pgx.Conn, open func()) {
	e.t.Helper()
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

// gateClaimOn makes the next Claim stop when it first reads row id: after the statement has taken
// its snapshot, and before it has locked any row. That is the window in which another transaction
// can change a row the snapshot has chosen, and timing alone cannot hit it. The returned function
// lets the claim go on.
//
// The pause is a test-only RESTRICTIVE select policy for the worker role. Its function is always
// true, and waits for the gate when it is shown the gate row. A policy is checked where the table
// is scanned, and the claim's row locks are taken above its scans and joins, so under any plan the
// gate row is read before the first row is locked. The tests that use this run under several
// planner settings (see planners), and each of them checks that the row it changes in the window
// was in fact not locked.
//
// Every claim reads the gate row, so only the first one to get there stops (firstLock). The gate
// row comes from acceptGateRow.
func (e *env) gateClaimOn(id string) (admin *pgx.Conn, open func()) {
	e.t.Helper()
	e.admin(fmt.Sprintf(`
		CREATE FUNCTION sluiceway.test_gate(row_id text) RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$
		BEGIN
		  IF row_id = '%s' AND pg_try_advisory_xact_lock(%d) THEN
		    PERFORM pg_advisory_xact_lock(%d);
		  END IF;
		  RETURN true;
		END $$;
		CREATE POLICY test_gate ON sluiceway.outbox AS RESTRICTIVE FOR SELECT TO sluiceway_worker
		  USING (sluiceway.test_gate(id));`, id, firstLock, gateLock))
	return e.closeGate()
}

// acceptGateRow accepts the row for gateClaimOn. It is unfinished, so the claim's search for the
// heads is sure to read it, whatever else the plan filters first. It is never due, so no claim
// leases it, and every claim in the test returns only the rows the test is about.
func (e *env) acceptGateRow() string {
	e.t.Helper()
	id := e.accept(tenantA, "gate", 1)
	e.admin("UPDATE sluiceway.outbox SET next_attempt_at = now() + interval '1 day' WHERE id = '" + id + "'")
	return id
}

// planners are the planner settings the gated claim tests run under: each entry lists what is
// turned off. The claim's re-checks have to hold under any plan. With the default settings the
// claim locks and updates its rows one at a time (the top of the plan is a nested loop with the
// locked subquery on its outer side). Without nested loops, or without index scans, that join
// becomes a hash or merge join, and every chosen row is locked before the first one is updated.
var planners = map[string][]string{
	"default":               nil,
	"no nested loop":        {"enable_nestloop"},
	"no index scan":         {"enable_indexscan", "enable_indexonlyscan", "enable_bitmapscan"},
	"no hash or merge join": {"enable_hashjoin", "enable_mergejoin"},
	"no seq scan or sort":   {"enable_seqscan", "enable_sort"},
}

// gateInsertOf makes the INSERT of the row with this body stop in a BEFORE INSERT row trigger:
// after the row has been given its seq (column defaults are computed before the row triggers
// run), and before the statement ends. The returned function lets it go on.
func (e *env) gateInsertOf(body []byte) (admin *pgx.Conn, open func()) {
	e.t.Helper()
	e.admin(fmt.Sprintf(`
		CREATE FUNCTION sluiceway.test_insert_gate() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		  IF NEW.raw_body = decode(TG_ARGV[0], 'hex') THEN
		    PERFORM pg_advisory_xact_lock(%d);
		  END IF;
		  RETURN NEW;
		END $$;
		CREATE TRIGGER test_insert_gate BEFORE INSERT ON sluiceway.outbox
		  FOR EACH ROW EXECUTE FUNCTION sluiceway.test_insert_gate('%x');`, gateLock, body))
	return e.closeGate()
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

func bodyOf(key string, version int) []byte {
	return fmt.Appendf(nil, `{"key":%q,"v":%d}`, key, version)
}

type accepted struct {
	id  string
	err error
}

// acceptInBackground runs one Accept for tenantA that is expected to wait for something.
func (e *env) acceptInBackground(key string, version int) (result <-chan accepted, finished func() bool) {
	ch := make(chan accepted, 1)
	var done atomic.Bool
	go func() {
		id, _, err := e.ob.Accept(e.ctx, tenantA, outbox.Delivery{
			Provider: "fake", OrderingKey: key, RawBody: bodyOf(key, version),
		})
		done.Store(true)
		ch <- accepted{id, err}
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
				Provider: "fake", OrderingKey: key, RawBody: bodyOf(key, version),
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

func (e *env) seqOf(id string) int64 {
	e.t.Helper()
	row, err := e.ob.Get(e.ctx, tenantA, id)
	if err != nil {
		e.t.Fatalf("Get(%s): %v", id, err)
	}
	return row.Seq
}

// TestAcceptsOfOneKeyCommitInQueueOrder is the late commit: seq is assigned by the INSERT, not by
// the COMMIT. If v1 could be inserted first and committed last, a claim in between would lease v2,
// and the next one would lease v1 while v2 is still in flight. So the second Accept of a key has
// to wait for the first one's transaction, and it has to wait before it takes its seq.
func TestAcceptsOfOneKeyCommitInQueueOrder(t *testing.T) {
	e := setup(t)
	admin := e.adminConn()

	// T1 accepts v1 and stays open.
	v1, commitT1 := e.acceptAndHold("task:1", 1)

	// T2 accepts v2 of the same entity. It must not get past T1.
	t2, t2Finished := e.acceptInBackground("task:1", 2)
	e.waitForLockWaiter(admin, onAnOrderingKey, t2Finished, "Accept of v2 while v1's transaction is open")

	// Another entity is not held up, and it is all a claim can see.
	other := e.accept(tenantA, "task:2", 1)
	e.claimOne(other)
	if t2Finished() {
		t.Fatal("Accept of v2 finished while v1's transaction is open")
	}

	commitT1()
	second := <-t2
	if second.err != nil || second.id == "" {
		t.Fatalf("T2 = %q, %v", second.id, second.err)
	}
	// The other entity was accepted while T2 was waiting. T2 waits before its INSERT, so it cannot
	// have a seq yet at that point. An Accept that took its seq first and the lock second would
	// wait just the same, and be below the other entity here.
	if v2Seq, otherSeq := e.seqOf(second.id), e.seqOf(other); v2Seq <= otherSeq {
		t.Errorf("v2 has seq %d, and a row accepted while v2 was waiting has %d: v2 took its seq before it took the lock",
			v2Seq, otherSeq)
	}

	c1 := e.claimOne(v1)
	e.claimNone("v1 is in flight, so v2 must wait behind it")
	if err := e.ob.MarkDelivered(e.ctx, c1); err != nil {
		t.Fatal(err)
	}
	e.claimOne(second.id)
}

// TestNoVersionIsClaimableWhileAnEarlierOneIsStillToCommit is the property the ordering key's lock
// exists for, in the one interleaving where the order of the lock and the INSERT decides it. T1's
// INSERT of v1 is stopped after v1 has its seq. With the lock taken first, T1 holds it by then,
// and T2's Accept of v2 waits. With the lock taken after the INSERT, nobody holds it: T2 inserts
// v2 behind v1, locks, commits, and v2 is leased while v1, ahead of it in the queue, has yet to
// appear. When it does, two versions of the entity are unfinished at once, the later one first.
func TestNoVersionIsClaimableWhileAnEarlierOneIsStillToCommit(t *testing.T) {
	e := setup(t)
	admin, open := e.gateInsertOf(bodyOf("task:1", 1))

	t1, t1Finished := e.acceptInBackground("task:1", 1)
	e.waitForLockWaiter(admin, onTheGate, t1Finished, "the gated Accept of v1")

	// v1 has its seq and is not committed. Whatever the Accept of v2 does now, no claim may return
	// v2 before v1 is there to be seen.
	t2, t2Finished := e.acceptInBackground("task:1", 2)
	waited := e.waitsOn(admin, onAnOrderingKey, t2Finished)
	if got := e.claim(); len(got) != 0 {
		t.Fatalf("Claim = %v while v1, which has the lower seq, is still to commit", claimedIDs(got))
	}
	if !waited {
		t.Fatal("Accept of v2 finished while the Accept of v1 is open and already has its seq")
	}

	open()
	first, second := <-t1, <-t2
	if first.err != nil || first.id == "" || second.err != nil || second.id == "" {
		t.Fatalf("T1 = %q, %v, T2 = %q, %v", first.id, first.err, second.id, second.err)
	}
	if v1Seq, v2Seq := e.seqOf(first.id), e.seqOf(second.id); v1Seq >= v2Seq {
		t.Errorf("v1 has seq %d and v2 has seq %d, want v1 first", v1Seq, v2Seq)
	}
	c1 := e.claimOne(first.id)
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
	e.waitForLockWaiter(admin, onAnOrderingKey, finished.Load, "Replay of v1 while an Accept of its key is open")
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
	for name, off := range planners {
		t.Run(name, func(t *testing.T) {
			e := setup(t, off...)
			gate := e.acceptGateRow()
			v1 := e.accept(tenantA, "task:1", 1)
			slow := e.claimOne(v1)
			e.admin("UPDATE sluiceway.outbox SET lease_until = now() - interval '1 second' WHERE id = '" + v1 + "'")

			admin, open := e.gateClaimOn(gate)
			result, finished := e.claimInBackground()
			e.waitForLockWaiter(admin, onTheGate, finished, "the gated Claim")

			// If the claim had locked v1 already, this would wait for it, and the test would prove
			// nothing.
			ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
			defer cancel()
			if err := e.ob.MarkDelivered(ctx, slow); err != nil {
				open()
				t.Fatalf("the slow holder finishing inside the window: %v (a timeout means the paused claim already holds v1's row lock, and this test has lost its window)", err)
			}

			open()
			if got := <-result; len(got) != 0 {
				t.Errorf("Claim = %v, want nothing: v1 was delivered before the claim locked it", claimedIDs(got))
			}
			row, err := e.ob.Get(e.ctx, tenantA, v1)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != outbox.StateDelivered || row.LeaseUntil != nil {
				t.Errorf("v1 = state %q, lease %v, want delivered and unleased", row.State, row.LeaseUntil)
			}
		})
	}
}

// TestClaimRechecksARowReplayedAfterItsSnapshot: a claimer's snapshot sees v1 as the head of its
// key. Before it reaches the row lock, v1 is claimed by someone else and dies, v2 becomes the head
// and goes in flight, and v1 is replayed to the back. The new version of v1 is pending, unleased
// and due, and the head the claimer joined it to is the stale one. Only its seq says it is no
// longer that head.
func TestClaimRechecksARowReplayedAfterItsSnapshot(t *testing.T) {
	for name, off := range planners {
		t.Run(name, func(t *testing.T) {
			e := setup(t, off...)
			gate := e.acceptGateRow()
			v1 := e.accept(tenantA, "task:1", 1)
			v2 := e.accept(tenantA, "task:1", 2)

			admin, open := e.gateClaimOn(gate)
			result, finished := e.claimInBackground()
			e.waitForLockWaiter(admin, onTheGate, finished, "the gated Claim")

			// These claims read the gate row too, and pass: only the first claim stops there. That
			// the first of them gets v1 also shows the paused claim had not locked it.
			c1 := e.claimOne(v1)
			if err := e.ob.MarkDead(e.ctx, c1, "normalizer", "bad shape"); err != nil {
				t.Fatal(err)
			}
			c2 := e.claimOne(v2)
			if err := e.ob.Replay(e.ctx, tenantA, v1); err != nil {
				t.Fatal(err)
			}
			// A replay sets next_attempt_at to the start of its own transaction. Put it before the
			// start of the paused claim's, as for a replay that began first and then waited for
			// the key's lock: otherwise the paused claim drops v1 for not being due, and its seq
			// is never looked at.
			e.admin("UPDATE sluiceway.outbox SET next_attempt_at = now() - interval '1 hour' WHERE id = '" + v1 + "'")

			open()
			if got := <-result; len(got) != 0 {
				t.Errorf("Claim = %v, want nothing: v1 went to the back of its queue, and v2 is in flight", claimedIDs(got))
			}
			e.claimNone("v2 is in flight")
			if err := e.ob.MarkDelivered(e.ctx, c2); err != nil {
				t.Fatal(err)
			}
			e.claimOne(v1)
		})
	}
}
