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

// TestAnAnswerAboutARecordThatWasNotOfferedIsUnreadable. A DeliveryResult says "these were
// refused and every other record of the batch was taken", so an id that was never offered, or
// one named twice, makes the whole sentence unreadable. Architecture section 11 has it as a
// retry with nothing marked delivered: a sink that is idempotent on the record id loses nothing
// by a repeat, and a guess here loses a record.
func TestAnAnswerAboutARecordThatWasNotOfferedIsUnreadable(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{})
	id := e.accept(tenantA, "fake:S1", ev(entity, "1", listA))
	e.sinks.mu.Lock()
	e.sinks.extra = []string{"rec_00000000000000000000000000000000"}
	e.sinks.mu.Unlock()

	e.drainOnce()

	row := e.row(tenantA, id)
	if row.State != outbox.StatePrepared || row.Attempts != 1 {
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

	// The same answer with the record of this delivery named twice.
	e.sinks.mu.Lock()
	e.sinks.extra = nil
	for recordID := range e.recordStates(id) {
		e.sinks.extra = append(e.sinks.extra, recordID) // named once by reject, once by extra
		e.sinks.reject = map[string]bool{recordID: true}
	}
	e.sinks.mu.Unlock()
	e.due()
	e.drainOnce()

	if got := e.row(tenantA, id); got.State != outbox.StatePrepared || got.LastError != "the sink's answer could not be read" {
		t.Errorf("row after an id named twice = state %q, last_error %q", got.State, got.LastError)
	}
	for recordID, state := range e.recordStates(id) {
		if state != "prepared" {
			t.Errorf("record %s is %q, want prepared", recordID, state)
		}
	}
}
