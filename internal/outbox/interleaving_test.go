package outbox_test

// The interleavings that timing alone cannot produce: each test here holds one transaction at a
// chosen point while another one runs.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gablooge/lawang/internal/outbox"
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
		var elsewhere *string
		// A transaction's lock belongs to no database in pg_locks, so the database comes from the
		// session that waits.
		err := admin.QueryRow(e.ctx, `
			SELECT count(*) FILTER (WHERE l.locktype = 'advisory' AND `+which+`),
			       min(l.locktype) FILTER (WHERE l.locktype <> 'advisory')
			  FROM pg_locks l JOIN pg_stat_activity a USING (pid)
			 WHERE NOT l.granted AND a.datname = current_database()`).Scan(&waiting, &elsewhere)
		if err != nil {
			e.t.Fatalf("pg_locks: %v", err)
		}
		if waiting > 0 {
			return true
		}
		if elsewhere != nil {
			// Waiting, and for the wrong thing: a row or a transaction, which is what happens
			// when two writers of a key meet without the key's lock. Say so now, not in a minute.
			e.t.Fatalf("waiting for a %s lock, not for an advisory lock: two writers of one key got past the key's lock", *elsewhere)
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
// is scanned, and the gate row is the first row any scan of the due heads returns (holdFirstInLine),
// so under any plan it is read before the first row is locked: a plan that walks the index of due
// heads reads it first and only then goes on to the rows the test is about, and a plan that scans
// and sorts reads every row before it locks one. The rows read after the pause are still read on
// the snapshot taken before it, which is the point. The tests that use this run under several
// planner settings (see planners), and each of them checks that the row it changes in the window
// was in fact not locked.
//
// Every claim reads the gate row, so only the first one to get there stops (firstLock).
func (e *env) gateClaimOn(id string) (admin *pgx.Conn, open func()) {
	e.t.Helper()
	e.admin(fmt.Sprintf(`
		CREATE FUNCTION lawang.test_gate(row_id text) RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$
		BEGIN
		  IF row_id = '%s' AND pg_try_advisory_xact_lock(%d) THEN
		    PERFORM pg_advisory_xact_lock(%d);
		  END IF;
		  RETURN true;
		END $$;
		CREATE POLICY test_gate ON lawang.outbox AS RESTRICTIVE FOR SELECT TO lawang_worker
		  USING (lawang.test_gate(id));`, id, firstLock, gateLock))
	e.holdFirstInLine(id)
	return e.closeGate()
}

// acceptGateRow accepts the row for gateClaimOn: the head of a key of its own. Until the gate is
// set up it is not due, so the claims a test makes before that do not lease it.
func (e *env) acceptGateRow() string {
	e.t.Helper()
	id := e.accept(tenantA, "gate", 1)
	e.admin("UPDATE lawang.outbox SET next_attempt_at = now() + interval '1 day' WHERE id = '" + id + "'")
	return id
}

// holdFirstInLine makes the gate row due for a year, so that it is the very first row of the index
// the claim walks, and the first row in the claim's order under any other plan. No claim leases
// it, because this holds its row lock until the test ends and the claim skips locked rows: every
// claim in the test still returns only the rows the test is about. It comes after the gate's DDL,
// which needs the table to itself.
func (e *env) holdFirstInLine(id string) {
	e.t.Helper()
	e.admin("UPDATE lawang.outbox SET next_attempt_at = now() - interval '1 year' WHERE id = '" + id + "'")
	holder := e.adminConn() // closing it, when the test ends, ends the transaction and the lock
	if _, err := holder.Exec(e.ctx, "BEGIN"); err != nil {
		e.t.Fatalf("hold the gate row: %v", err)
	}
	if _, err := holder.Exec(e.ctx, "SELECT id FROM lawang.outbox WHERE id = $1 FOR UPDATE", id); err != nil {
		e.t.Fatalf("hold the gate row: %v", err)
	}
}

// planners are the planner settings the gated claim tests run under: each entry lists what is
// turned off. The claim's re-checks have to hold under any plan. With the default settings on a
// small table the candidates come from a scan and a sort, on a large one from a walk of the index
// of due heads, and the claim locks and updates its rows one at a time (the top of the plan is a
// nested loop with the locked subquery on its outer side). Without nested loops that join becomes
// a hash or merge join, and every chosen row is locked before the first one is updated. With
// sequential scans and sorts off, the candidates come from the index walk even on a small table.
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
		CREATE FUNCTION lawang.test_insert_gate() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		  IF NEW.raw_body = decode(TG_ARGV[0], 'hex') THEN
		    PERFORM pg_advisory_xact_lock(%d);
		  END IF;
		  RETURN NEW;
		END $$;
		CREATE TRIGGER test_insert_gate BEFORE INSERT ON lawang.outbox
		  FOR EACH ROW EXECUTE FUNCTION lawang.test_insert_gate('%x');`, gateLock, body))
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

// deliverAndHold marks a claimed row of tenantA delivered in a transaction that stays open,
// holding the ordering key's lock, until commit is called. The row is finished and the marker
// handed on inside that transaction, and nobody else can see it yet.
func (e *env) deliverAndHold(c outbox.Claimed) (commit func()) {
	e.t.Helper()
	marked := make(chan error, 1)
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	e.t.Cleanup(release) // a failing test must not leave it open, or closing the pool would hang
	done := make(chan error, 1)
	go func() {
		done <- e.db.TenantTx(e.ctx, tenantA, func(tx pgx.Tx) error {
			err := outbox.MarkDeliveredIn(e.ctx, tx, c)
			marked <- err
			if err != nil {
				return err
			}
			<-hold
			return nil
		})
	}()
	if err := <-marked; err != nil {
		e.t.Fatalf("held MarkDelivered: %v", err)
	}
	return func() {
		e.t.Helper()
		release()
		if err := <-done; err != nil {
			e.t.Fatalf("held MarkDelivered: %v", err)
		}
	}
}

// headsBroken is the invariant of the head marker, as one statement, so on one snapshot: every
// key with unfinished rows has exactly one head, that head is its earliest unfinished row, and no
// finished row is a head. Writers of one key run one after the other and each of them keeps it, so
// it holds on every snapshot, also in the middle of a concurrent test. It returns the keys that
// break it.
const headsBroken = `
	SELECT coalesce(string_agg(format('%s/%s: %s heads among %s unfinished rows, the earliest %s a head',
	                                  tenant_id, ordering_key, heads, unfinished,
	                                  CASE WHEN first_is_head THEN 'is' ELSE 'is not' END), '; '), '')
	  FROM (
	        SELECT tenant_id, ordering_key,
	               count(*) FILTER (WHERE state IN ('pending', 'prepared')) AS unfinished,
	               count(*) FILTER (WHERE is_head) AS heads,
	               coalesce((array_agg(is_head ORDER BY seq) FILTER (WHERE state IN ('pending', 'prepared')))[1], false) AS first_is_head,
	               bool_or(is_head AND state NOT IN ('pending', 'prepared')) AS finished_head
	          FROM lawang.outbox
	         GROUP BY tenant_id, ordering_key
	       ) k
	 WHERE finished_head OR (unfinished = 0 AND heads <> 0) OR (unfinished > 0 AND (heads <> 1 OR NOT first_is_head))`

// checkHeads fails the test if the head marker's invariant does not hold right now.
func (e *env) checkHeads(admin *pgx.Conn) {
	e.t.Helper()
	var broken string
	if err := admin.QueryRow(e.ctx, headsBroken).Scan(&broken); err != nil {
		e.t.Fatalf("head invariant: %v", err)
	}
	if broken != "" {
		e.t.Fatalf("head invariant broken: %s", broken)
	}
}

// get reads a row of tenantA.
func (e *env) get(id string) outbox.Row {
	e.t.Helper()
	row, err := e.ob.Get(e.ctx, tenantA, id)
	if err != nil {
		e.t.Fatalf("Get(%s): %v", id, err)
	}
	return row
}

func (e *env) isHead(id string) bool {
	e.t.Helper()
	return e.get(id).IsHead
}

func (e *env) seqOf(id string) int64 {
	e.t.Helper()
	return e.get(id).Seq
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
	if err := e.ob.MarkDead(e.ctx, e.claimOne(v1), badShape); err != nil {
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

// adminErr runs one statement as the superuser and returns the server's verdict.
func (e *env) adminErr(sql string) error {
	e.t.Helper()
	_, err := e.adminConn().Exec(e.ctx, sql)
	return err
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// TestAKeyNeverHasTwoHeads pins the second line of defense, which is the database's and not the
// claim's: one head per key, by a unique index. The rows below reach the table the way a writer
// would that took no ordering-key lock and knew nothing of the marker: ahead of a version that is
// already in flight. (Before the head marker this was a NOT EXISTS in the claim, evaluated on the
// snapshot. The index holds at every moment, and for every writer.)
func TestAKeyNeverHasTwoHeads(t *testing.T) {
	e := setup(t)
	v2 := e.accept(tenantA, "task:1", 2)
	e.claimOne(v2)

	const late = `INSERT INTO lawang.outbox (id, seq, tenant_id, provider, delivery_id, ordering_key, raw_body, is_head)
	              OVERRIDING SYSTEM VALUE VALUES ('%s', 0, 'tenant_a', 'fake', '%[1]s', 'task:1', '', %v)`
	if err := e.adminErr(fmt.Sprintf(late, "second-head", true)); sqlState(err) != "23505" {
		t.Fatalf("a second head for a key whose head is in flight: err = %v, want a unique violation", err)
	}
	// Without the marker the row is stored, and is nothing a claim can see.
	if err := e.adminErr(fmt.Sprintf(late, "late", false)); err != nil {
		t.Fatal(err)
	}
	e.claimNone("v2 is in flight, so nothing else of its entity is claimable, not even an earlier row")

	// When v2's lease runs out it is v2 that is taken over. The earlier row is still not a head.
	e.admin("UPDATE lawang.outbox SET lease_until = now() - interval '1 second' WHERE id = '" + v2 + "'")
	c2 := e.claimOne(v2)
	e.claimNone("v2 is in flight again")

	// Finishing the head hands the marker to the earliest unfinished row of the key.
	if err := e.ob.MarkDelivered(e.ctx, c2); err != nil {
		t.Fatal(err)
	}
	if err := e.ob.MarkDelivered(e.ctx, e.claimOne("late")); err != nil {
		t.Fatal(err)
	}

	// And a finished row can never be a head, even of a key that has none.
	err := e.adminErr("UPDATE lawang.outbox SET is_head = true WHERE id = '" + v2 + "'")
	if sqlState(err) != "23514" {
		t.Errorf("making a delivered row a head: err = %v, want a check violation", err)
	}
}

// TestAFinishedRowIsNeverAHead: the claim asks for is_head and does not look at the state, so
// "a finished row is never leased" rests on the table's CHECK, for every writer there is or will
// be. Neither way of finishing a row while keeping its marker gets past it.
func TestAFinishedRowIsNeverAHead(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)
	for _, state := range []string{outbox.StateDelivered, outbox.StateDead} {
		err := e.adminErr(fmt.Sprintf("UPDATE lawang.outbox SET state = '%s', finished_at = now() WHERE id = '%s'", state, v1))
		if sqlState(err) != "23514" {
			t.Errorf("a head made %s and left a head: err = %v, want a check violation", state, err)
		}
	}
	e.claimOne(v1) // and it is still there to be claimed
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
			e.admin("UPDATE lawang.outbox SET lease_until = now() - interval '1 second' WHERE id = '" + v1 + "'")

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

// TestClaimRechecksARowLeasedAfterItsSnapshot: a claimer's snapshot sees v1 as a due head. Before
// it reaches the row lock, another claimer leases v1 and commits. The latest version of v1 is
// still a head and still unfinished. Only due_at, which now carries the lease, says it is taken,
// and due_at is a generated column: what is re-checked has to be the value stored with the row
// version the other claimer wrote, not the one the snapshot saw. A claim that judged it on the
// snapshot would lease v1 a second time, and two workers would deliver it.
func TestClaimRechecksARowLeasedAfterItsSnapshot(t *testing.T) {
	for name, off := range planners {
		t.Run(name, func(t *testing.T) {
			e := setup(t, off...)
			gate := e.acceptGateRow()
			v1 := e.accept(tenantA, "task:1", 1)

			admin, open := e.gateClaimOn(gate)
			result, finished := e.claimInBackground()
			e.waitForLockWaiter(admin, onTheGate, finished, "the gated Claim")

			// This claim reads the gate row too, and passes: only the first claim stops there. That
			// it gets v1 also shows the paused claim had not locked it.
			first := e.claimOne(v1)

			open()
			if got := <-result; len(got) != 0 {
				t.Errorf("Claim = %v, want nothing: v1 was leased before the claim locked it", claimedIDs(got))
			}
			if row := e.get(v1); row.Attempts != 1 || row.LeaseUntil == nil {
				t.Errorf("v1 = attempts %d, lease %v, want the one lease of the first claimer", row.Attempts, row.LeaseUntil)
			}
			// The first claimer's token is still the row's: nobody wrote over it.
			if err := e.ob.MarkDelivered(e.ctx, first); err != nil {
				t.Errorf("the first claimer delivering v1: %v", err)
			}
		})
	}
}

// TestClaimRechecksARowGivenABackoffAfterItsSnapshot: as above, but the other claimer has also
// failed v1 and given the lease up by the time the paused claim reaches the row. The latest version
// of v1 is a head, unfinished and UNLEASED. Only its next_attempt_at, through due_at, says it is
// not to be tried for an hour. A claim that leased it would skip the backoff and use up an attempt
// of the ladder, on every poll that happens to race a failure.
func TestClaimRechecksARowGivenABackoffAfterItsSnapshot(t *testing.T) {
	for name, off := range planners {
		t.Run(name, func(t *testing.T) {
			e := setup(t, off...)
			gate := e.acceptGateRow()
			v1 := e.accept(tenantA, "task:1", 1)

			admin, open := e.gateClaimOn(gate)
			result, finished := e.claimInBackground()
			e.waitForLockWaiter(admin, onTheGate, finished, "the gated Claim")

			if err := e.ob.Fail(e.ctx, e.claimOne(v1), outbox.Ladder{time.Hour}, sink503); err != nil {
				t.Fatal(err)
			}
			if row := e.get(v1); row.LeaseUntil != nil || !row.IsHead || row.State != outbox.StatePending {
				t.Fatalf("v1 = lease %v, head %v, state %q: the test wants an unleased, unfinished head in the window",
					row.LeaseUntil, row.IsHead, row.State)
			}

			open()
			if got := <-result; len(got) != 0 {
				t.Errorf("Claim = %v, want nothing: v1 was given a backoff before the claim locked it", claimedIDs(got))
			}
			if row := e.get(v1); row.Attempts != 1 || row.LeaseUntil != nil {
				t.Errorf("v1 = attempts %d, lease %v, want one attempt and no lease: it is backing off", row.Attempts, row.LeaseUntil)
			}
			e.claimNone("v1 is backing off")
		})
	}
}

// TestClaimLeasesARowReplayedOntoAnEmptyKeyAfterItsSnapshot is the one change inside the window
// after which the row is still the claim's to take. v1 is claimed by someone else, dies, and is
// replayed, and its key has nothing else unfinished: the latest version of v1 is the head again,
// and due. It is a different row version with a different seq, and the claim leases it all the
// same, because what it asks of the locked row is what makes a row claimable and nothing more.
func TestClaimLeasesARowReplayedOntoAnEmptyKeyAfterItsSnapshot(t *testing.T) {
	for name, off := range planners {
		t.Run(name, func(t *testing.T) {
			e := setup(t, off...)
			gate := e.acceptGateRow()
			v1 := e.accept(tenantA, "task:1", 1)

			admin, open := e.gateClaimOn(gate)
			result, finished := e.claimInBackground()
			e.waitForLockWaiter(admin, onTheGate, finished, "the gated Claim")

			if err := e.ob.MarkDead(e.ctx, e.claimOne(v1), badShape); err != nil {
				t.Fatal(err)
			}
			if err := e.ob.Replay(e.ctx, tenantA, v1); err != nil {
				t.Fatal(err)
			}
			// As in the test below: a replay that began before the paused claim did.
			e.admin("UPDATE lawang.outbox SET next_attempt_at = now() - interval '1 hour' WHERE id = '" + v1 + "'")

			open()
			got := <-result
			if len(got) != 1 || got[0].ID() != v1 || got[0].Attempt() != 1 {
				t.Fatalf("Claim = %v, want v1 on attempt 1: it is the head of its key again, and due", claimedIDs(got))
			}
			e.checkHeads(admin)
			e.claimNone("v1 is in flight")
			if err := e.ob.MarkDelivered(e.ctx, got[0]); err != nil {
				t.Errorf("delivering v1 under the paused claim's lease: %v", err)
			}
		})
	}
}

// TestClaimSkipsARowSomeoneHasLocked: the claim never waits for a row lock. A row that another
// transaction holds (a finish in progress, another claim) is passed over for this poll, and the
// rest of the batch is leased at once.
func TestClaimSkipsARowSomeoneHasLocked(t *testing.T) {
	e := setup(t)
	held := e.accept(tenantA, "task:1", 1)
	free := e.accept(tenantA, "task:2", 1)
	holder := e.adminConn()
	if _, err := holder.Exec(e.ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(e.ctx, "SELECT id FROM lawang.outbox WHERE id = $1 FOR UPDATE", held); err != nil {
		t.Fatal(err)
	}

	// A claim that waited would wait for as long as the holder likes. This one has five seconds.
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	got, err := e.ob.Claim(ctx, 10, lease)
	if err != nil {
		t.Fatalf("Claim while a row is locked: %v (a timeout means it waited for the row)", err)
	}
	if ids := claimedIDs(got); !reflect.DeepEqual(ids, []string{free}) {
		t.Errorf("Claim = %v, want only the row nobody holds (%s)", ids, free)
	}
	if _, err := holder.Exec(e.ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	e.claimOne(held)
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
			if err := e.ob.MarkDead(e.ctx, c1, badShape); err != nil {
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
			e.admin("UPDATE lawang.outbox SET next_attempt_at = now() - interval '1 hour' WHERE id = '" + v1 + "'")

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

// The three tests below are the races the head marker brings with it. In each, two writers of one
// key overlap: one decides from what it can see whether a row is a head, the other changes that
// under it. Whichever comes second has to wait for the first to commit, or a row ends up
// unfinished with no head in front of it, and is never delivered.

// isolationDefaults are the database defaults the three tests run under. Waiting for the key's lock
// is only half of what they pin. The writer that waited must also SEE what the one before it
// committed, and it does because its next statement takes a new snapshot, which is READ COMMITTED.
// Under REPEATABLE READ or SERIALIZABLE the snapshot is the transaction's, taken by its first
// statement (the one that binds the tenant, before the wait): the writer waits, as it should, and
// then decides on a picture from before the commit it waited for. The row it writes has no head in
// front of it, no error is raised, and the key is never claimed again. So store asks for READ
// COMMITTED by name, and a default set on the database (one ALTER DATABASE, and some sites set it
// as policy) cannot change it.
var isolationDefaults = map[string][]string{
	"the server's default":        nil,
	"repeatable read by default":  {"default_transaction_isolation = 'repeatable read'"},
	"serializable by default":     {"default_transaction_isolation = 'serializable'"},
	"read committed, spelled out": {"default_transaction_isolation = 'read committed'"},
}

// TestAFinishWaitsForAnOpenAcceptOfItsKey is the lost promotion. The Accept of v2 sees v1
// unfinished and inserts v2 as a non-head. Were the MarkDelivered of v1 to run now, it would not
// see the uncommitted v2, would promote nothing, and v2 would wait forever.
func TestAFinishWaitsForAnOpenAcceptOfItsKey(t *testing.T) {
	for name, settings := range isolationDefaults {
		t.Run(name, func(t *testing.T) {
			e := setupWith(t, settings...)
			admin := e.adminConn()
			v1 := e.accept(tenantA, "task:1", 1)
			c1 := e.claimOne(v1)

			v2, commitV2 := e.acceptAndHold("task:1", 2)

			delivered := make(chan error, 1)
			var finished atomic.Bool
			go func() {
				err := e.ob.MarkDelivered(e.ctx, c1)
				finished.Store(true)
				delivered <- err
			}()
			waited := e.waitsOn(admin, onAnOrderingKey, finished.Load)

			commitV2()
			if err := <-delivered; err != nil {
				t.Fatal(err)
			}
			if !waited {
				t.Error("MarkDelivered of v1 finished while an Accept of its key was open")
			}
			e.checkHeads(admin)
			e.claimOne(v2) // the property: v2 was handed the marker
		})
	}
}

// TestAnAcceptWaitsForAnOpenFinishOfItsKey is the same race from the other side. The MarkDelivered
// of v1 has found nothing to promote and has not committed. Were the Accept of v2 to run now, it
// would still see v1 unfinished and insert v2 as a non-head, behind a row that is about to be gone.
func TestAnAcceptWaitsForAnOpenFinishOfItsKey(t *testing.T) {
	for name, settings := range isolationDefaults {
		t.Run(name, func(t *testing.T) {
			e := setupWith(t, settings...)
			admin := e.adminConn()
			v1 := e.accept(tenantA, "task:2", 1)
			commitV1 := e.deliverAndHold(e.claimOne(v1))

			t2, t2Finished := e.acceptInBackground("task:2", 2)
			waited := e.waitsOn(admin, onAnOrderingKey, t2Finished)

			commitV1()
			second := <-t2
			if second.err != nil || second.id == "" {
				t.Fatalf("Accept of v2 = %q, %v", second.id, second.err)
			}
			if !waited {
				t.Error("Accept of v2 finished while a MarkDelivered of its key was open")
			}
			e.checkHeads(admin)
			if !e.isHead(second.id) {
				t.Error("v2 is not the head of a key with nothing else unfinished")
			}
			e.claimOne(second.id) // the property: v2 can be delivered
		})
	}
}

// TestAReplayWaitsForAnOpenFinishOfItsKey: a replayed row is a head only if its key has nothing
// unfinished, so it has the same race with a finish that an Accept has.
func TestAReplayWaitsForAnOpenFinishOfItsKey(t *testing.T) {
	for name, settings := range isolationDefaults {
		t.Run(name, func(t *testing.T) {
			e := setupWith(t, settings...)
			admin := e.adminConn()
			v1 := e.accept(tenantA, "task:1", 1)
			v2 := e.accept(tenantA, "task:1", 2)
			if err := e.ob.MarkDead(e.ctx, e.claimOne(v1), badShape); err != nil {
				t.Fatal(err)
			}
			commitV2 := e.deliverAndHold(e.claimOne(v2))

			replayed := make(chan error, 1)
			var finished atomic.Bool
			go func() {
				err := e.ob.Replay(e.ctx, tenantA, v1)
				finished.Store(true)
				replayed <- err
			}()
			waited := e.waitsOn(admin, onAnOrderingKey, finished.Load)

			commitV2()
			if err := <-replayed; err != nil {
				t.Fatal(err)
			}
			if !waited {
				t.Error("Replay of v1 finished while a MarkDelivered of its key was open")
			}
			e.checkHeads(admin)
			e.claimOne(v1)
		})
	}
}

// TestAFailedFinishIsNotALostLease is TestAnErrorIsNotALostLease for the statements INSIDE a
// finishing transition, which that test never reaches (it fails before the transaction begins).
// This is where errors happen in production: a finish waits for an open Accept of its entity, so a
// finish with a deadline runs out of time right on the key's lock. Read as ErrLeaseLost, that would
// make the worker drop a row it still holds and has already delivered to the sink, to be delivered
// again when the lease runs out. Each case makes one of the three statements fail, for a reason
// that has nothing to do with the lease, and wants: an error that is not ErrLeaseLost, a row that
// has not changed (the statements before the failed one are rolled back with it), and the same
// lease finishing the row once the obstacle is gone.
func TestAFailedFinishIsNotALostLease(t *testing.T) {
	finishers := map[string]func(*env, context.Context, outbox.Claimed) error{
		"MarkDelivered": func(e *env, ctx context.Context, c outbox.Claimed) error { return e.ob.MarkDelivered(ctx, c) },
		"MarkDead":      func(e *env, ctx context.Context, c outbox.Claimed) error { return e.ob.MarkDead(ctx, c, badShape) },
		"Fail to dead": func(e *env, ctx context.Context, c outbox.Claimed) error {
			return e.ob.Fail(ctx, c, outbox.Ladder{}, sink503)
		},
	}
	// A trigger that refuses one kind of change, as a stand-in for any error of the statement
	// that makes it.
	const refuse = `
		CREATE FUNCTION lawang.test_refuse() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		  IF %s THEN RAISE EXCEPTION 'test: refused'; END IF;
		  RETURN NEW;
		END $$;
		CREATE TRIGGER test_refuse BEFORE UPDATE ON lawang.outbox
		  FOR EACH ROW EXECUTE FUNCTION lawang.test_refuse();`
	const allow = "DROP TRIGGER test_refuse ON lawang.outbox; DROP FUNCTION lawang.test_refuse();"

	// unchanged runs the finish, which must fail, and reports the error. v1 and v2 must be as before.
	unchanged := func(t *testing.T, e *env, v1, v2 string, finish func() error) error {
		t.Helper()
		before1, before2 := e.get(v1), e.get(v2)
		err := finish()
		if err == nil || errors.Is(err, outbox.ErrLeaseLost) {
			t.Errorf("err = %v, want an error that is not ErrLeaseLost", err)
		}
		if after := e.get(v1); !reflect.DeepEqual(before1, after) {
			t.Errorf("the row changed:\nbefore %+v\nafter  %+v", before1, after)
		}
		if after := e.get(v2); !reflect.DeepEqual(before2, after) {
			t.Errorf("the next row of the key changed:\nbefore %+v\nafter  %+v", before2, after)
		}
		return err
	}
	// afterwards: the lease is as good as it was, and the queue moves on.
	afterwards := func(t *testing.T, e *env, c outbox.Claimed, v2 string, finish func(*env, context.Context, outbox.Claimed) error) {
		t.Helper()
		if err := finish(e, e.ctx, c); err != nil {
			t.Fatalf("the same lease, once the obstacle is gone: %v", err)
		}
		e.checkHeads(e.adminConn())
		e.claimOne(v2)
	}

	for name, finish := range finishers {
		t.Run(name+"/the key's lock", func(t *testing.T) {
			e := setup(t)
			admin := e.adminConn()
			v1 := e.accept(tenantA, "task:7", 1)
			c1 := e.claimOne(v1)
			v2, commitV2 := e.acceptAndHold("task:7", 2)

			// Not a deadline that may or may not fall inside the lock statement: the context ends
			// once the finish is seen waiting for the lock.
			ctx, cancel := context.WithCancel(e.ctx)
			defer cancel()
			before := e.get(v1)
			result := make(chan error, 1)
			var finished atomic.Bool
			go func() {
				err := finish(e, ctx, c1)
				finished.Store(true)
				result <- err
			}()
			e.waitForLockWaiter(admin, onAnOrderingKey, finished.Load, name+" while an Accept of its key is open")
			cancel()
			err := <-result
			if err == nil || errors.Is(err, outbox.ErrLeaseLost) {
				t.Errorf("err = %v, want an error that is not ErrLeaseLost", err)
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want it to wrap context.Canceled", err)
			}
			if after := e.get(v1); !reflect.DeepEqual(before, after) {
				t.Errorf("the row changed:\nbefore %+v\nafter  %+v", before, after)
			}

			commitV2()
			afterwards(t, e, c1, v2, finish)
		})

		t.Run(name+"/the state change", func(t *testing.T) {
			e := setup(t)
			v1 := e.accept(tenantA, "task:1", 1)
			v2 := e.accept(tenantA, "task:1", 2)
			c1 := e.claimOne(v1)
			e.admin(fmt.Sprintf(refuse, "NEW.state IN ('delivered', 'dead')"))
			err := unchanged(t, e, v1, v2, func() error { return finish(e, e.ctx, c1) })
			if sqlState(err) != "P0001" {
				t.Errorf("err = %v, want the server's error, still wrapped", err)
			}
			e.admin(allow)
			afterwards(t, e, c1, v2, finish)
		})

		// The finish itself went through, inside the transaction. It must not outlive the failure
		// of the promotion: a finished head with no successor is a key that is never claimed again.
		t.Run(name+"/the promotion", func(t *testing.T) {
			e := setup(t)
			v1 := e.accept(tenantA, "task:1", 1)
			v2 := e.accept(tenantA, "task:1", 2)
			c1 := e.claimOne(v1)
			e.admin(fmt.Sprintf(refuse, "NEW.is_head AND NOT OLD.is_head"))
			err := unchanged(t, e, v1, v2, func() error { return finish(e, e.ctx, c1) })
			if sqlState(err) != "P0001" {
				t.Errorf("err = %v, want the server's error, still wrapped", err)
			}
			e.admin(allow)
			afterwards(t, e, c1, v2, finish)
		})
	}
}

// TestTheMarkerFollowsTheQueue walks one key through every writer and looks at the marker after
// each: it is always on the earliest unfinished row, and nowhere else.
func TestTheMarkerFollowsTheQueue(t *testing.T) {
	e := setup(t)
	admin := e.adminConn()
	heads := func(step string, want map[string]bool) {
		t.Helper()
		e.checkHeads(admin)
		// The detection query agrees with the invariant at every step: nothing is stranded.
		if got, err := e.ob.StrandedKeys(e.ctx, tenantA, 10); err != nil || len(got) != 0 {
			t.Fatalf("%s: StrandedKeys = %q, %v, want none", step, got, err)
		}
		for id, isHead := range want {
			if got := e.isHead(id); got != isHead {
				t.Fatalf("%s: row %s is a head = %v, want %v", step, id, got, isHead)
			}
		}
	}

	v1 := e.accept(tenantA, "task:1", 1)
	v2 := e.accept(tenantA, "task:1", 2)
	v3 := e.accept(tenantA, "task:1", 3)
	heads("accepted", map[string]bool{v1: true, v2: false, v3: false})

	// A retry keeps the marker: the key waits with its head.
	c1 := e.claimOne(v1)
	if err := e.ob.Fail(e.ctx, c1, outbox.Ladder{time.Hour}, sink503); err != nil {
		t.Fatal(err)
	}
	heads("v1 backing off", map[string]bool{v1: true, v2: false, v3: false})
	e.admin("UPDATE lawang.outbox SET next_attempt_at = now() - interval '1 second' WHERE id = '" + v1 + "'")

	// A dead letter gives it up, to the next row and not to the last.
	if err := e.ob.MarkDead(e.ctx, e.claimOne(v1), badShape); err != nil {
		t.Fatal(err)
	}
	heads("v1 dead", map[string]bool{v1: false, v2: true, v3: false})

	// A replay onto a key with unfinished rows joins the back, without the marker.
	if err := e.ob.Replay(e.ctx, tenantA, v1); err != nil {
		t.Fatal(err)
	}
	heads("v1 replayed behind v2 and v3", map[string]bool{v1: false, v2: true, v3: false})

	c2 := e.claimOne(v2)
	if err := e.ob.MarkPrepared(e.ctx, c2); err != nil {
		t.Fatal(err)
	}
	heads("v2 prepared", map[string]bool{v1: false, v2: true, v3: false})
	if err := e.ob.MarkDelivered(e.ctx, c2); err != nil {
		t.Fatal(err)
	}
	heads("v2 delivered", map[string]bool{v1: false, v2: false, v3: true})

	if err := e.ob.MarkDead(e.ctx, e.claimOne(v3), badShape); err != nil {
		t.Fatal(err)
	}
	heads("v3 dead", map[string]bool{v1: true, v2: false, v3: false})
	if err := e.ob.MarkDelivered(e.ctx, e.claimOne(v1)); err != nil {
		t.Fatal(err)
	}
	heads("v1 delivered, nothing unfinished", map[string]bool{v1: false, v2: false, v3: false})

	// A replay onto a key with nothing unfinished is a head at once.
	if err := e.ob.Replay(e.ctx, tenantA, v3); err != nil {
		t.Fatal(err)
	}
	heads("v3 replayed onto an empty queue", map[string]bool{v1: false, v2: false, v3: true})
	e.claimOne(v3)
}
