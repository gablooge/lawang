package worker_test

import (
	"errors"
	"testing"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/sink"
	"github.com/gablooge/lawang/internal/worker"
)

// TestAHaltDeliversNothingKillsNothingAndSpendsNoAttempt. Architecture section 11 says a sink
// that refuses the request itself leaves the row prepared, marks nothing delivered and
// dead-letters nothing. The attempt matters as much as the state: Fail indexes the ladder by
// the attempt count, so a halt that counted as an attempt would walk a row to its dead letter
// through a failure that says nothing about the row at all.
func TestAHaltDeliversNothingKillsNothingAndSpendsNoAttempt(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{})
	id := e.accept(tenantA, "fake:S1", ev(entity, "1", listA))
	e.sinks.breakWith(&sink.Fault{
		Action: sink.ActionHalt,
		Cause:  outbox.NewCause(outbox.ClassSinkUnauthorized).WithStatus(401),
	})

	// More halts than the ladder has rungs, and more than the abandonment allows. Neither may
	// fire: an operator has not fixed the credential yet, and that is not the row's fault.
	for range len(shortLadder) + 4 {
		e.drainOnce()
		e.due()
	}

	row := e.row(tenantA, id)
	if row.State != outbox.StatePrepared {
		t.Errorf("state = %q, want prepared: a halt delivers nothing and kills nothing", row.State)
	}
	if row.LastError != "sink refused the credential (status 401)" {
		t.Errorf("last_error = %q", row.LastError)
	}
	if row.Attempts != 0 {
		t.Errorf("attempts = %d, want 0: a halted attempt is given back", row.Attempts)
	}
	for recordID, state := range e.recordStates(id) {
		if state != "prepared" {
			t.Errorf("record %s is %q, want prepared: a halt kills nothing", recordID, state)
		}
	}
	if got := len(e.documents(tenantA)); got != 0 {
		t.Errorf("the stub holds %d documents, want none", got)
	}

	// The operator fixes the credential. The next claim delivers, and the records are the ones
	// the first attempt prepared.
	e.sinks.fix()
	e.due()
	e.drainOnce()
	e.wantState(id, outbox.StateDelivered, "")
	if got := len(e.documents(tenantA)); got != 1 {
		t.Errorf("the stub holds %d documents, want 1", got)
	}
	if n := e.ledgerCount(); n != 1 {
		t.Errorf("the ledger holds %d rows, want 1: the halted row was prepared once", n)
	}
}

// TestASinkThatCannotBeBuiltHalts. A tenant nobody has configured a sink for and a vault that
// cannot be reached look the same from here, and both want the same answer: deliver nothing,
// kill nothing, and come back later without spending the ladder.
func TestASinkThatCannotBeBuiltHalts(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{})
	id := e.accept(tenantA, "fake:S1", ev(entity, "1", listA))
	e.sinks.mu.Lock()
	e.sinks.err = errNoSink
	e.sinks.mu.Unlock()

	e.drainOnce()

	row := e.row(tenantA, id)
	if row.State != outbox.StatePrepared || row.Attempts != 0 {
		t.Errorf("row = state %q, attempts %d, want prepared and no attempt charged", row.State, row.Attempts)
	}
	if row.LastError != "internal error (code no_sink)" {
		t.Errorf("last_error = %q, want the class and a code, and no text from the error", row.LastError)
	}
	if calls := e.sinks.callCount(); calls != 0 {
		t.Errorf("the sink was called %d times, want never: there was no sink", calls)
	}
}

// TestASinkThatBreaksItsContractHalts is the fail-closed default. A sink reports a failed
// delivery as a *sink.Fault with an action this package knows, and in no other way. Anything
// else means nothing is known about what reached the receiver, so the row stalls: retrying
// would walk a ladder that ends in a dead letter, and dead-lettering would destroy records over
// a bug of ours.
func TestASinkThatBreaksItsContractHalts(t *testing.T) {
	t.Parallel()
	for name, broken := range map[string]func(s *sinks){
		"an error that is not a Fault": func(s *sinks) { s.plainErr = errors.New("something went wrong") },
		"a Fault with no action":       func(s *sinks) { s.fault = &sink.Fault{Cause: outbox.NewCause(outbox.ClassSinkUnavailable)} },
		"a Fault with an action from the future": func(s *sinks) {
			s.fault = &sink.Fault{Action: sink.Action(99), Cause: outbox.NewCause(outbox.ClassSinkUnavailable)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := setup(t, worker.Options{})
			id := e.accept(tenantA, "fake:S1", ev(entity, "1", listA))
			e.sinks.mu.Lock()
			broken(e.sinks)
			e.sinks.mu.Unlock()

			e.drainOnce()

			row := e.row(tenantA, id)
			if row.State != outbox.StatePrepared || row.Attempts != 0 {
				t.Errorf("row = state %q, attempts %d, want prepared and no attempt charged", row.State, row.Attempts)
			}
			if row.LastError != "internal error (code sink_contract)" {
				t.Errorf("last_error = %q, want the contract code", row.LastError)
			}
			for recordID, state := range e.recordStates(id) {
				if state != "prepared" {
					t.Errorf("record %s is %q, want prepared", recordID, state)
				}
			}
		})
	}
}

// TestAStoredRecordThatIsNotTheOneThatWasStoredIsParked. The second commit delivers documents
// the first one stored, possibly from another process, so the thing that keeps a sink from
// being handed a record nobody prepared is record.Reopen: it mints the id again from the
// document and holds the document against the columns the row carries beside it.
//
// The row is edited here the way nothing in the program can, because the point is that the
// drain does not trust its own table either. Each case is a single edit the tenant's own SQL
// could make, and each is a row that still decodes and still passes the format, which is why
// none of them can be left to the decoder:
//
//   - the version is hashed into the record id, so the document mints an id that is not its own;
//   - the op and the kind are NOT hashed into it. They are in the record's seal, and a seal is
//     recomputed from the document's own values when the document is decoded, so a document
//     edited in one of them agrees with itself and only the columns beside it disagree. This is
//     the edit that matters most: an upsert turned into a delete is a tombstone at the
//     receiver, and record.OpDelete removes every stored version of the entity;
//   - the record id column is what the outbox keys, leases and dead-letters the delivery on, so
//     a document swapped for another record of the tenant is accounted for under an id that was
//     never offered.
//
// The delivery is parked rather than retried, since the same bytes come back the same on the
// next attempt, and nothing of it is delivered: a delivery that cannot be made whole is not
// made in part.
func TestAStoredRecordThatIsNotTheOneThatWasStoredIsParked(t *testing.T) {
	t.Parallel()
	// Each edit runs against a database of its own: the row ends dead either way, and a case
	// that asserted a last_error an earlier case had left there would prove nothing.
	for name, edit := range map[string]string{
		"the version, which the id hashes": `
			UPDATE lawang.outbox_record
			   SET document = convert_to(replace(convert_from(document, 'UTF8'),
			                                     '"version":"1"', '"version":"9"'), 'UTF8')`,
		// Validate refuses a delete that still carries a title and a text, so the tombstone
		// edit clears them. That is one more field in the same edit, not a second guard.
		"the op, which the seal covers and the id does not": `
			UPDATE lawang.outbox_record
			   SET document = convert_to(
			         regexp_replace(
			           regexp_replace(
			             replace(convert_from(document, 'UTF8'), '"op":"upsert"', '"op":"delete"'),
			             '"title":"[^"]*"', '"title":""'),
			           '"text":"[^"]*"', '"text":""'), 'UTF8')`,
		"the kind, which the seal covers and the id does not": `
			UPDATE lawang.outbox_record
			   SET document = convert_to(replace(convert_from(document, 'UTF8'),
			                                     '"kind":"task"', '"kind":"message"'), 'UTF8')`,
		"the record id the row is keyed on": `
			UPDATE lawang.outbox_record SET record_id = 'rec_nobody_prepared_this'`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := setup(t, worker.Options{})
			id := e.accept(tenantA, "fake:S1", ev(entity, "1", listA))

			e.sinks.mu.Lock()
			e.sinks.panics = 1 // prepare the delivery and stop before it is delivered
			e.sinks.mu.Unlock()
			e.drainOnce()
			e.leaseExpired()
			if got := e.row(tenantA, id).LastError; got != "" {
				t.Fatalf("the row starts with last_error = %q, so what this case asserts is not its own", got)
			}

			e.admin(edit)
			e.drainOnce()

			e.wantState(id, outbox.StateDead, "not retryable")
			if got := e.row(tenantA, id).LastError; got != "internal error (code stored_record)" {
				t.Errorf("last_error = %q, want the class and the code, and nothing from the document", got)
			}
			if got := len(e.documents(tenantA)); got != 0 {
				t.Errorf("the stub holds %d documents, want none", got)
			}
			if calls := e.sinks.callCount(); calls != 1 {
				t.Errorf("the sink was called %d times, want only the one that died: nothing may be offered", calls)
			}
		})
	}
}

// TestAnAnswerAboutARecordThatWasNotOfferedIsUnreadable. A DeliveryResult says "these were
// refused and every other record of the batch was taken", so an id that was never offered, or
// one named twice, makes the whole sentence unreadable. Architecture section 11 has it as a
// retry with nothing marked delivered: a sink that is idempotent on the record id loses nothing
// by a repeat, and a guess here loses a record.
func TestAnAnswerAboutARecordThatWasNotOfferedIsUnreadable(t *testing.T) {
	t.Parallel()
	// Each case gets a database of its own and starts from a row with nothing recorded against
	// it. The first version of this test ran the second case after the first, against the same
	// row, and it passed with the duplicate check mutated away: the last_error it asserted had
	// been left there by the case before it.
	cases := map[string]func(e *env, prepared []string){
		"an id that was never offered": func(e *env, _ []string) {
			e.sinks.extra = []string{"rec_00000000000000000000000000000000"}
		},
		"one record refused twice": func(e *env, prepared []string) {
			e.sinks.reject = map[string]bool{prepared[0]: true}
			e.sinks.extra = []string{prepared[0]}
		},
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := setup(t, worker.Options{})
			id := e.accept(tenantA, "fake:S1", ev(entity, "1", listA))

			// Prepare the delivery without recording anything about it, so the row starts this
			// case with an empty last_error and the test can name the record it was offered.
			e.sinks.mu.Lock()
			e.sinks.panics = 1
			e.sinks.mu.Unlock()
			e.drainOnce()
			e.leaseExpired()
			var prepared []string
			for recordID := range e.recordStates(id) {
				prepared = append(prepared, recordID)
			}
			if len(prepared) != 1 {
				t.Fatalf("the delivery prepared %d records, want 1", len(prepared))
			}
			if row := e.row(tenantA, id); row.LastError != "" || row.Attempts != 1 {
				t.Fatalf("the row starts with last_error %q and %d attempts, so this case would assert what the setup left",
					row.LastError, row.Attempts)
			}

			e.sinks.mu.Lock()
			answer(e, prepared)
			e.sinks.mu.Unlock()
			e.drainOnce()

			row := e.row(tenantA, id)
			if row.State != outbox.StatePrepared || row.Attempts != 2 {
				t.Errorf("row = state %q, attempts %d, want prepared with the attempt spent on the ladder", row.State, row.Attempts)
			}
			if row.LastError != "the sink's answer could not be read" {
				t.Errorf("last_error = %q", row.LastError)
			}
			for recordID, state := range e.recordStates(id) {
				if state != "prepared" {
					t.Errorf("record %s is %q, want prepared: nothing may be marked delivered on an unreadable answer", recordID, state)
				}
			}
		})
	}
}
