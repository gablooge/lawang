package outbox_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/outbox"
)

// The failures a sink reports about one record.
var (
	refused     = outbox.NewCause(outbox.ClassSinkRejected).WithCode("unsupported_kind")
	wrongTenant = outbox.NewCause(outbox.ClassInternal).WithCode("wrong_tenant")
)

// recordRow is a row of outbox_record as the tests assert about it, read as the superuser so
// that nothing about row-level security can hide a row from an assertion.
type recordRow struct {
	recordID   string
	pos        int
	document   string
	state      string
	deadReason string
	lastError  string
	finished   bool
}

// recordRows is every record of one delivery, in delivery order.
func (e *env) recordRows(outboxID string) []recordRow {
	e.t.Helper()
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		e.t.Fatalf("connect as the superuser: %v", err)
	}
	defer func() { _ = conn.Close(e.ctx) }()
	rows, err := conn.Query(e.ctx, `SELECT record_id, pos, document, state, dead_reason, last_error,
	                                       finished_at IS NOT NULL
	                                  FROM lawang.outbox_record WHERE outbox_id = $1 ORDER BY pos`, outboxID)
	if err != nil {
		e.t.Fatalf("query: %v", err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (recordRow, error) {
		var out recordRow
		var document []byte
		err := r.Scan(&out.recordID, &out.pos, &document, &out.state, &out.deadReason, &out.lastError, &out.finished)
		out.document = string(document)
		return out, err
	})
	if err != nil {
		e.t.Fatalf("read the rows: %v", err)
	}
	return got
}

// mustPreparedRecords is what a claim still has to deliver, as record ids.
func (e *env) mustPreparedRecords(c outbox.Claimed) []string {
	e.t.Helper()
	recs, err := e.ob.PreparedRecords(e.ctx, c)
	if err != nil {
		e.t.Fatalf("PreparedRecords: %v", err)
	}
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.RecordID
	}
	return ids
}

// TestAPreparedDeliveryIsDeliveredFromWhatItStored is step 6 and step 7 of architecture 3.2 as
// one property: what the first commit stored is what the second commit delivers, byte for byte
// and in order, to a worker that never saw the first one.
func TestAPreparedDeliveryIsDeliveredFromWhatItStored(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	first := e.claimOne(id)
	if err := e.prepare(e.ctx, first, doc("rec_b"), doc("rec_a")); err != nil {
		t.Fatal(err)
	}

	// The worker that prepared the row dies here, and another takes the lease over.
	e.admin("UPDATE lawang.outbox SET lease_until = now() - interval '1 second'")
	second := e.claimOne(id)

	got, err := e.ob.PreparedRecords(e.ctx, second)
	if err != nil {
		t.Fatalf("PreparedRecords: %v", err)
	}
	// In the order they were given, which is the order they must be delivered, and not in id
	// order: rec_b was first.
	want := []outbox.PreparedRecord{doc("rec_b"), doc("rec_a")}
	if len(got) != len(want) {
		t.Fatalf("PreparedRecords = %v, want %v", got, want)
	}
	for i := range want {
		// The op and the kind come back too. record.Reopen needs them to check the document,
		// and a round trip that dropped them would fail every record closed rather than be
		// noticed here, so they are asserted where they are stored.
		if got[i].RecordID != want[i].RecordID || string(got[i].Document) != string(want[i].Document) ||
			got[i].Op != want[i].Op || got[i].Kind != want[i].Kind {
			t.Errorf("record %d = %s %q %s/%s, want %s %q %s/%s",
				i, got[i].RecordID, got[i].Document, got[i].Op, got[i].Kind,
				want[i].RecordID, want[i].Document, want[i].Op, want[i].Kind)
		}
	}
	if row, _ := e.ob.Get(e.ctx, tenantA, id); row.State != outbox.StatePrepared {
		t.Errorf("state = %q, want prepared: the second worker must deliver and not prepare again", row.State)
	}
}

// TestWorkWithholdsTheBodyOfAPreparedRow pins the one reason Work's projection has a CASE in it.
//
// A prepared row is delivered from the records it stored and its body is never read again
// (architecture 3.2, step 7), so fetching raw_body for one is up to a megabyte of webhook body
// read on every re-drain and on every halt. A halted row is claimed once per halt pause for as
// long as an operator takes, so for a halted tenant with a backlog that is the whole of that
// backlog's bodies, once a minute, for nothing.
//
// Without this test the CASE is held up by nothing: replacing it with a plain raw_body left
// internal/outbox and internal/worker green.
func TestWorkWithholdsTheBodyOfAPreparedRow(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	c := e.claimOne(id)

	pending, err := e.ob.Work(e.ctx, c)
	if err != nil {
		t.Fatalf("Work on a pending row: %v", err)
	}
	if pending.State != outbox.StatePending {
		t.Fatalf("state = %q, want pending: the case below is only about a prepared row", pending.State)
	}
	// The premise: a pending row does get its body, or withholding it from a prepared one
	// would prove nothing.
	if want := `{"key":"task:1","v":1}`; string(pending.Body) != want {
		t.Fatalf("the body of a pending row = %q, want %q", pending.Body, want)
	}
	if pending.Provider != "fake" {
		t.Errorf("provider = %q, want fake", pending.Provider)
	}

	if err := e.prepare(e.ctx, c, doc("rec_a")); err != nil {
		t.Fatal(err)
	}
	prepared, err := e.ob.Work(e.ctx, c)
	if err != nil {
		t.Fatalf("Work on a prepared row: %v", err)
	}
	if prepared.State != outbox.StatePrepared {
		t.Fatalf("state = %q, want prepared", prepared.State)
	}
	if prepared.Body != nil {
		t.Errorf("the body of a prepared row = %q, want none: it is never read again", prepared.Body)
	}
	// And the row still says who to work it as, which is the rest of what Work is for.
	if prepared.Provider != "fake" {
		t.Errorf("provider = %q, want fake", prepared.Provider)
	}
	// The body is withheld and not lost: it is still in the table for an operator.
	var stored string
	err = e.adminConn().QueryRow(e.ctx,
		"SELECT convert_from(raw_body, 'UTF8') FROM lawang.outbox WHERE id = $1", id).Scan(&stored)
	if err != nil {
		t.Fatalf("read raw_body as the superuser: %v", err)
	}
	if want := `{"key":"task:1","v":1}`; stored != want {
		t.Errorf("raw_body in the table = %q, want %q: Work withholds it, nothing deletes it", stored, want)
	}
}

// TestPrepareInStoresNothingWhenTheLeaseIsLost is the other half: the records and the state move
// together or not at all, so a slow worker that comes back cannot add its records to a delivery
// another worker now holds.
func TestPrepareInStoresNothingWhenTheLeaseIsLost(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	crashed := e.claimOne(id)
	e.admin("UPDATE lawang.outbox SET lease_until = now() - interval '1 second'")
	takeover := e.claimOne(id)

	if err := e.prepare(e.ctx, crashed, doc("rec_a")); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Errorf("PrepareIn by the old holder: err = %v, want ErrLeaseLost", err)
	}
	if got := e.recordRows(id); len(got) != 0 {
		t.Errorf("records after a refused PrepareIn = %v, want none", got)
	}
	if err := e.prepare(e.ctx, takeover, doc("rec_a")); err != nil {
		t.Fatalf("the current holder: %v", err)
	}
	// And the same row cannot be prepared twice, whoever asks: the state guard refuses it, so
	// the documents are never inserted on top of themselves.
	if err := e.prepare(e.ctx, takeover, doc("rec_a")); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Errorf("preparing twice: err = %v, want ErrLeaseLost", err)
	}
	if got := e.recordRows(id); len(got) != 1 {
		t.Errorf("records = %v, want exactly one", got)
	}
}

// TestOneRefusedRecordDiesAndTheRestOfTheBatchLands is the per-record dead letter of
// architecture section 11, which is the whole reason this table exists. Before it, the two moves
// a worker had for this batch were to mark the row delivered (losing the refusal) or to kill the
// row (losing the two records that landed).
func TestOneRefusedRecordDiesAndTheRestOfTheBatchLands(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	c := e.claimOne(id)
	if err := e.prepare(e.ctx, c, doc("rec_a"), doc("rec_b"), doc("rec_c")); err != nil {
		t.Fatal(err)
	}

	err := e.ob.MarkDelivered(e.ctx, c, []outbox.DeadRecord{{RecordID: "rec_b", Cause: refused}})
	if err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}

	want := []recordRow{
		{recordID: "rec_a", pos: 0, document: `{"id":"rec_a"}`, state: "delivered", finished: true},
		{recordID: "rec_b", pos: 1, document: `{"id":"rec_b"}`, state: "dead", finished: true,
			deadReason: "not retryable", lastError: "sink rejected the record (code unsupported_kind)"},
		{recordID: "rec_c", pos: 2, document: `{"id":"rec_c"}`, state: "delivered", finished: true},
	}
	got := e.recordRows(id)
	if len(got) != len(want) {
		t.Fatalf("records = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	row, _ := e.ob.Get(e.ctx, tenantA, id)
	if row.State != outbox.StateDelivered || row.DeadReason != "" {
		t.Errorf("the row = state %q, dead_reason %q, want delivered with no reason: the delivery itself was made",
			row.State, row.DeadReason)
	}
}

// TestADeadLetteredRecordIsReplayedAndTheDeliveredOnesAreNot is the third acceptance line of
// B10 for the per-record dead letter: replaying it delivers it, and delivers nothing else.
func TestADeadLetteredRecordIsReplayedAndTheDeliveredOnesAreNot(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	c := e.claimOne(id)
	if err := e.prepare(e.ctx, c, doc("rec_a"), doc("rec_b")); err != nil {
		t.Fatal(err)
	}
	if err := e.ob.MarkDelivered(e.ctx, c, []outbox.DeadRecord{{RecordID: "rec_b", Cause: refused}}); err != nil {
		t.Fatal(err)
	}
	e.claimNone("the delivery is finished, even though one of its records is a dead letter")

	if err := e.ob.Replay(e.ctx, tenantA, id); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	again := e.claimOne(id)
	if got := e.mustPreparedRecords(again); len(got) != 1 || got[0] != "rec_b" {
		t.Errorf("what the replay offers = %v, want only the record that was refused", got)
	}
	if row, _ := e.ob.Get(e.ctx, tenantA, id); row.State != outbox.StatePrepared {
		t.Errorf("state = %q, want prepared: the records are stored, so it is never prepared again", row.State)
	}
	if err := e.ob.MarkDelivered(e.ctx, again, nil); err != nil {
		t.Fatalf("delivering the replayed record: %v", err)
	}
	if got := e.recordRows(id); len(got) != 2 || got[0].state != "delivered" || got[1].state != "delivered" {
		t.Errorf("records = %v, want both delivered", got)
	}
}

// TestAFinishedDeliveryWithNothingDeadIsNotReplayable: the clause that makes a delivered row
// replayable is the dead record, and not the state. Without this the replay of a per-record dead
// letter would be a way to deliver any finished delivery again.
func TestAFinishedDeliveryWithNothingDeadIsNotReplayable(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	c := e.claimOne(id)
	if err := e.prepare(e.ctx, c, doc("rec_a")); err != nil {
		t.Fatal(err)
	}
	if err := e.ob.MarkDelivered(e.ctx, c, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.ob.Replay(e.ctx, tenantA, id); !errors.Is(err, outbox.ErrNotFound) {
		t.Errorf("replaying a delivery that kept nothing back: err = %v, want ErrNotFound", err)
	}
	if row, _ := e.ob.Get(e.ctx, tenantA, id); row.State != outbox.StateDelivered {
		t.Errorf("state = %q, want it left alone", row.State)
	}
	e.claimNone("nothing was replayed")
}

// TestADeadRecordThatIsNotWaitingIsRefusedAndNothingIsWritten. A sink that answers about a record
// it was not given has said nothing trustworthy about the ones it was, so the transition refuses
// rather than writing part of it. The caller classifies it; this is the line under the caller.
func TestADeadRecordThatIsNotWaitingIsRefusedAndNothingIsWritten(t *testing.T) {
	e := setup(t)
	other := e.accept(tenantA, "task:2", 1)
	otherClaim := e.claimOne(other)
	if err := e.prepare(e.ctx, otherClaim, doc("rec_other")); err != nil {
		t.Fatal(err)
	}

	id := e.accept(tenantA, "task:1", 1)
	c := e.claimOne(id)
	if err := e.prepare(e.ctx, c, doc("rec_a"), doc("rec_b")); err != nil {
		t.Fatal(err)
	}

	for name, dead := range map[string][]outbox.DeadRecord{
		"an id the sink invented":        {{RecordID: "rec_nowhere", Cause: refused}},
		"an id of another delivery":      {{RecordID: "rec_other", Cause: refused}},
		"an id named twice":              {{RecordID: "rec_a", Cause: refused}, {RecordID: "rec_a", Cause: refused}},
		"a waiting id after an unknown ": {{RecordID: "rec_nowhere", Cause: refused}, {RecordID: "rec_a", Cause: refused}},
	} {
		if err := e.ob.MarkDelivered(e.ctx, c, dead); !errors.Is(err, outbox.ErrNoSuchRecord) {
			t.Errorf("%s: err = %v, want ErrNoSuchRecord", name, err)
		}
		for _, got := range e.recordRows(id) {
			if got.state != "prepared" {
				t.Errorf("%s: record %s = %q, want everything left prepared", name, got.recordID, got.state)
			}
		}
		if row, _ := e.ob.Get(e.ctx, tenantA, id); row.State != outbox.StatePrepared {
			t.Errorf("%s: the row = %q, want it left prepared", name, row.State)
		}
	}
	// The other delivery's record was never touched either.
	if got := e.recordRows(other); len(got) != 1 || got[0].state != "prepared" {
		t.Errorf("the other delivery's records = %v, want one, still prepared", got)
	}
	// And the lease still holds, so the worker can finish properly afterwards.
	if err := e.ob.MarkDelivered(e.ctx, c, []outbox.DeadRecord{{RecordID: "rec_a", Cause: refused}}); err != nil {
		t.Fatalf("MarkDelivered after the refusals: %v", err)
	}
}

// TestADeadRowKeepsItsRecordsForTheReplay. A delivery that died after it was prepared is
// replayed as prepared, so the records it stored are exactly what the replay offers: nothing was
// marked delivered, so nothing is held back.
func TestADeadRowKeepsItsRecordsForTheReplay(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	c := e.claimOne(id)
	if err := e.prepare(e.ctx, c, doc("rec_a"), doc("rec_b")); err != nil {
		t.Fatal(err)
	}
	if err := e.ob.MarkDead(e.ctx, c, sink503); err != nil {
		t.Fatal(err)
	}
	if err := e.ob.Replay(e.ctx, tenantA, id); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	again := e.claimOne(id)
	got := e.mustPreparedRecords(again)
	if len(got) != 2 || got[0] != "rec_a" || got[1] != "rec_b" {
		t.Errorf("what the replay offers = %v, want both records in order", got)
	}
}

// TestAnotherTenantSeesNoneOfADeliverysRecords. The table carries no tenant of its own: its
// policy asks the delivery's row. This is the test that the question is really asked.
func TestAnotherTenantSeesNoneOfADeliverysRecords(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	c := e.claimOne(id)
	if err := e.prepare(e.ctx, c, doc("rec_a")); err != nil {
		t.Fatal(err)
	}
	// The premise: tenant A really did store a document, so an empty answer below is isolation
	// and not an empty table.
	if got := e.mustPreparedRecords(c); len(got) != 1 {
		t.Fatalf("tenant A's own records = %v, want one", got)
	}

	foreign := outbox.WithTenant(c, tenantB)
	if got := e.mustPreparedRecords(foreign); len(got) != 0 {
		t.Errorf("tenant B reading tenant A's documents = %v, want none", got)
	}
	// Nor can a transition under the wrong tenant write a dead letter against them.
	if err := e.ob.MarkDelivered(e.ctx, foreign, []outbox.DeadRecord{{RecordID: "rec_a", Cause: wrongTenant}}); !errors.Is(err, outbox.ErrNoSuchRecord) {
		t.Errorf("tenant B killing tenant A's record: err = %v, want ErrNoSuchRecord", err)
	}
	if got := e.recordRows(id); len(got) != 1 || got[0].state != "prepared" {
		t.Errorf("tenant A's records = %v, want one, untouched", got)
	}
}

// TestHaltLeavesTheRowWhereItIsAndGivesTheAttemptBack. Architecture section 11 says a halt marks
// nothing delivered, kills no record and leaves the row prepared. The attempt matters as much:
// Fail indexes the ladder by the attempt count, so a halt that counted would walk a row to its
// end through a failure that is not the row's fault.
func TestHaltLeavesTheRowWhereItIsAndGivesTheAttemptBack(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	v2 := e.accept(tenantA, "task:1", 2)
	c := e.claimOne(id)
	if err := e.prepare(e.ctx, c, doc("rec_a")); err != nil {
		t.Fatal(err)
	}
	if c.Attempt() != 1 {
		t.Fatalf("Attempt = %d, want 1", c.Attempt())
	}

	noCredential := outbox.NewCause(outbox.ClassSinkUnauthorized).WithStatus(401)
	if err := e.ob.Halt(e.ctx, c, time.Hour, noCredential); err != nil {
		t.Fatalf("Halt: %v", err)
	}

	row, err := e.ob.Get(e.ctx, tenantA, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != outbox.StatePrepared || row.LastError != "sink refused the credential (status 401)" {
		t.Errorf("row = state %q, last_error %q, want prepared and the cause recorded", row.State, row.LastError)
	}
	if row.Attempts != 0 {
		t.Errorf("attempts = %d, want 0: a halted attempt is given back", row.Attempts)
	}
	if got := e.recordRows(id); len(got) != 1 || got[0].state != "prepared" {
		t.Errorf("records = %v, want the one record still waiting", got)
	}
	e.claimNone("the row is halted for an hour, and v2 waits behind it")

	// The operator fixes the credential, which is the pause running out.
	e.admin("UPDATE lawang.outbox SET next_attempt_at = now() - interval '1 second'")
	back := e.claimOne(id)
	if back.Attempt() != 1 {
		t.Errorf("the attempt after a halt = %d, want 1: the halt must not walk the ladder", back.Attempt())
	}
	if err := e.ob.MarkDelivered(e.ctx, back, nil); err != nil {
		t.Fatal(err)
	}
	e.claimOne(v2)
}

// TestHaltIsRefusedWithoutTheLeaseOrAPause.
func TestHaltIsRefusedWithoutTheLeaseOrAPause(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	crashed := e.claimOne(id)
	e.admin("UPDATE lawang.outbox SET lease_until = now() - interval '1 second'")
	takeover := e.claimOne(id)

	if err := e.ob.Halt(e.ctx, crashed, time.Minute, sink503); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Errorf("Halt by the old holder: err = %v, want ErrLeaseLost", err)
	}
	if err := e.ob.Halt(e.ctx, takeover, 0, sink503); err == nil {
		t.Error("Halt with no pause: err = nil, want a refusal (it would be claimable at once, over and over)")
	}
	row, _ := e.ob.Get(e.ctx, tenantA, id)
	if row.Attempts != 2 || row.LastError != "" {
		t.Errorf("row = attempts %d, last_error %q, want nothing written by either refusal", row.Attempts, row.LastError)
	}
}

// TestARowNobodyReportsOnIsAbandonedAndItsEntityMovesOn is the failure the attempt count is the
// only trace of: a worker that dies on every attempt never reaches Fail, so the lease expires
// and the row is taken over forever, holding every later version of its entity behind it.
func TestARowNobodyReportsOnIsAbandonedAndItsEntityMovesOn(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)
	v2 := e.accept(tenantA, "task:1", 2)

	ladder := outbox.Ladder{time.Second}
	var c outbox.Claimed
	for attempt := 1; !ladder.Exhausted(attempt); attempt++ {
		c = e.claimOne(v1)
		if c.Attempt() != attempt {
			t.Fatalf("Attempt = %d, want %d", c.Attempt(), attempt)
		}
		// The worker dies here: no transition at all, and the lease runs out. Only the leased
		// row: the table refuses a lease_until with no token beside it, which is v2.
		e.admin("UPDATE lawang.outbox SET lease_until = now() - interval '1 second' WHERE lease_token IS NOT NULL")
	}
	c = e.claimOne(v1)
	if !ladder.Exhausted(c.Attempt()) {
		t.Fatalf("Attempt = %d, want one the ladder cannot account for", c.Attempt())
	}
	if err := e.ob.MarkAbandoned(e.ctx, c); err != nil {
		t.Fatalf("MarkAbandoned: %v", err)
	}

	row, _ := e.ob.Get(e.ctx, tenantA, v1)
	if row.State != outbox.StateDead ||
		row.DeadReason != "attempts exhausted without a recorded failure" ||
		row.LastError != "no attempt on this row reported an outcome" {
		t.Errorf("row = state %q, dead_reason %q, last_error %q", row.State, row.DeadReason, row.LastError)
	}
	// The point of parking it: the entity is not stuck behind it any more.
	e.claimOne(v2)
}

// TestExhaustedIsOnePastTheLastAttemptTheLadderSchedules pins the boundary against Fail's own,
// because the two have to agree: an attempt Fail would still have scheduled must not be
// abandoned, and the attempt after the one Fail dead-letters must be.
func TestExhaustedIsOnePastTheLastAttemptTheLadderSchedules(t *testing.T) {
	for _, ladder := range []outbox.Ladder{{}, {time.Second}, outbox.DefaultLadder} {
		last := 0 // the highest attempt count the ladder still answers for
		for attempt := 1; attempt <= len(ladder)+5; attempt++ {
			if _, ok := ladder.Next(attempt); ok {
				last = attempt
			}
		}
		for attempt := 1; attempt <= last+1; attempt++ {
			if ladder.Exhausted(attempt) {
				t.Errorf("ladder of %d: Exhausted(%d) = true, want false: Fail still has an answer for it",
					len(ladder), attempt)
			}
		}
		if !ladder.Exhausted(last + 2) {
			t.Errorf("ladder of %d: Exhausted(%d) = false, want true: Fail dead-lettered attempt %d, so nothing may claim it again",
				len(ladder), last+2, last+1)
		}
	}
}

// TestPrepareInRefusesATextPostgresCannotStore. Every text that reaches a statement from
// outside the package is asked this first, because the alternative is SQLSTATE 22021, which a
// caller cannot tell from an outage. The op and the kind are asked as well as the record id:
// they are three columns written from the same caller's values, and an empty op would meet the
// table's CHECK instead, which is no easier to read.
func TestPrepareInRefusesATextPostgresCannotStore(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	c := e.claimOne(id)
	bad := map[string]outbox.PreparedRecord{}
	for name, text := range map[string]string{
		"a NUL byte":    "\x00a",
		"invalid UTF-8": "\xff",
		"empty":         "",
	} {
		r := doc("rec_a")
		r.RecordID = text
		bad["the record id is "+name] = r
		r = doc("rec_a")
		r.Op = text
		bad["the op is "+name] = r
		r = doc("rec_a")
		r.Kind = text
		bad["the kind is "+name] = r
	}
	for name, rec := range bad {
		err := e.db.TenantTx(e.ctx, tenantA, func(tx pgx.Tx) error {
			return e.ob.PrepareIn(e.ctx, tx, c, []outbox.PreparedRecord{rec})
		})
		if err == nil {
			t.Errorf("%s: err = nil, want a refusal", name)
		}
	}
	// The refusal is the record and not the state: the row is still preparable afterwards.
	if err := e.prepare(e.ctx, c, doc("rec_a")); err != nil {
		t.Errorf("PrepareIn after the refusals: %v", err)
	}
}

// TestPreparedRecordsOfAClaimThatNeverCameFromClaim: a zero Claimed names no row and no tenant,
// so it reads nothing rather than everything.
func TestPreparedRecordsOfAClaimThatNeverCameFromClaim(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	c := e.claimOne(id)
	if err := e.prepare(e.ctx, c, doc("rec_a")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ob.PreparedRecords(e.ctx, outbox.Claimed{}); err == nil {
		t.Error("PreparedRecords of a zero Claimed: err = nil, want a refusal (it names no tenant)")
	}
	if err := e.ob.PrepareIn(e.ctx, nil, outbox.Claimed{}, nil); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Errorf("PrepareIn with a zero Claimed: err = %v, want ErrLeaseLost before the transaction is touched", err)
	}
}
