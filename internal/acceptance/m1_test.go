package acceptance

import (
	"bytes"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/provider/clickup"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// This file is the five lines of M1's "Done when" (docs/roadmap.md), one test each, in order.
//
// Each test also asks which layer produced the outcome, and not only what the outcome was. That is
// deliberate: a status code, an empty table or an empty sink can each be produced by several
// layers, and a test that watches one of those alone cannot tell the layer it names from any other
// layer that answers the same way. So the forgery test has the honest control beside it, the
// re-send test says whether the outbox or the ledger refused, the crash test counts the calls to
// the provider's API, and the parked test shows the same bytes being routed once a subscription
// records them.
//
// Nothing here uses t.Run for a case that calls a helper on the environment: the helpers fail
// through the environment's own *testing.T, and calling FailNow on it from a subtest's goroutine
// is not something the testing package allows. The cases are loops, and each message names its
// case.

// Clause 1. A comment on a ClickUp task produces exactly two records at the sink: the comment and
// its re-hydrated parent task (architecture section 7, and ADR 15).
func TestACommentOnATaskProducesExactlyTwoRecordsAtTheSink(t *testing.T) {
	e := setup(t)
	body := fixture(t, "webhook_comment_posted.json")

	if got := e.deliverSigned(body, secretA); got != http.StatusAccepted {
		t.Fatalf("the edge answered %d, want %d: the delivery was not stored", got, http.StatusAccepted)
	}
	stop := e.run()
	defer stop()
	// The row reaching its final state is what makes "exactly two" a complete count rather than a
	// count taken while the delivery was still in flight. The wait ends on either outcome, the
	// right one or the wrong one, so that a delivery which dead-letters is named by the assertion
	// below rather than being left to time out with nothing said about why.
	e.wait("the delivery to reach a final state", e.oneRowDone)
	if rows := e.outboxRows(); len(rows) != 1 || rows[0].State != outbox.StateDelivered {
		t.Fatalf("the outbox holds %+v, want one delivered row: nothing reached the sink, so the counts below "+
			"would be about a delivery that failed rather than about one that landed", rows)
	}

	docs := e.docsAt(tenantA)
	if len(docs) != 2 {
		t.Fatalf("got %d documents at the sink, want exactly 2: %+v", len(docs), docs)
	}

	// The parent task first, so that a sink following edges.reply_parent meets the task before the
	// comment that names it.
	task, comment := docs[0], docs[1]
	wantScope, err := record.ScopeID(clickup.Key, clickup.ContainerKindList, wantList)
	if err != nil {
		t.Fatalf("ScopeID: %v", err)
	}
	for _, c := range []struct{ what, got, want string }{
		{"the first document's kind", task.Kind, string(record.KindTask)},
		{"the first document's external id", task.ExternalID, clickup.Key + ":task:" + wantTask},
		{"the first document's scope", task.Visibility.Scope, wantScope},
		{"the first document's source", task.Source, clickup.Key},
		{"the second document's kind", comment.Kind, string(record.KindMessage)},
		{"the second document's external id", comment.ExternalID, clickup.Key + ":comment:" + wantComment},
		{"the second document's scope", comment.Visibility.Scope, wantScope},
		{"the comment's reply parent", comment.Edges.ReplyParent, clickup.Key + ":task:" + wantTask},
		{"the comment's container", comment.Container.ID, wantTask},
	} {
		if c.got != c.want {
			t.Errorf("%s is %q, want %q", c.what, c.got, c.want)
		}
	}

	// The masker ran on the way out. The fixtures put an address in the task's text and a
	// telephone number in the comment's on purpose, and the redaction map is what tells "the
	// masker removed them" apart from "nothing ever looked", which is how the loop below would
	// otherwise pass for the wrong reason.
	if n := e.redactionCount(); n == 0 {
		t.Error("the redaction map is empty, so this delivery was never masked and the next assertions prove nothing")
	}
	for i, d := range docs {
		for _, planted := range []string{"@example.invalid", "555 0100"} {
			if strings.Contains(d.Text, planted) || strings.Contains(d.Title, planted) {
				t.Errorf("document %d reached the sink carrying %q", i, planted)
			}
		}
	}

	// The whole pipeline really did run: two calls to the provider's API, the task and its
	// comments, which is hydration having happened rather than a record built out of the body.
	if n, paths := e.api.seen(); n != 2 {
		t.Errorf("the provider made %d calls to the ClickUp API (%v), want 2: the task and its comments", n, paths)
	}
	if n := e.ledgerCount(); n != 2 {
		t.Errorf("the ledger holds %d records, want 2", n)
	}
}

// Clause 2. Re-sending the same webhook produces no new records.
//
// It is two cases, because two different layers refuse a re-send and only one of them is reached by
// sending the same bytes again. The accept path dedupes on the delivery id, which is a hash of the
// body (ids.DeliveryID), so a re-send spelled differently is a NEW row that the accept path cannot
// see as a repeat, and what refuses it then is the ledger. A test that only sent the same bytes
// twice would stay green with the ledger's skip taken out altogether.
//
// "No new records" is counted at the outbox and at the sink call, and deliberately not at the
// documents the sink holds. A Sink is idempotent on the record id and the strict stub answers a
// repeat whose content matches with a silent no-op, so the document count is the same whether the
// ledger held the records back or offered all of them again, and a mutation of the ledger's skip
// survived a first version of this test that counted only documents. What cannot be the same
// either way is how many records were prepared and whether a sink was called at all.
func TestResendingTheSameWebhookProducesNoNewRecords(t *testing.T) {
	e := setup(t)
	body := fixture(t, "webhook_comment_posted.json")

	if got := e.deliverSigned(body, secretA); got != http.StatusAccepted {
		t.Fatalf("the first delivery answered %d, want %d", got, http.StatusAccepted)
	}
	stop := e.run()
	defer stop()
	// On either outcome, so that a first delivery which dead-letters is named here instead of
	// timing out before the cases this clause is about have been reached.
	e.wait("the first delivery to reach a final state", e.oneRowDone)
	if rows := e.outboxRows(); len(rows) != 1 || rows[0].State != outbox.StateDelivered {
		t.Fatalf("the outbox holds %+v, want one delivered row: there is no first delivery for a re-send to "+
			"be a repeat of", rows)
	}
	callsAfterFirst := e.apiCalls()
	sinkCallsAfterFirst := e.sinks.deliverCalls()
	if got := e.preparedRecordCount(); got != 2 {
		t.Fatalf("the first delivery prepared %d records, want 2", got)
	}

	// Case one: the same bytes. 200 and not 202, because the outbox's unique delivery id per
	// tenant turned the insert into a no-op, which is the Duplicate verdict; a provider must not
	// be made to retry.
	if got := e.deliverSigned(body, secretA); got != http.StatusOK {
		t.Errorf("the identical re-send answered %d, want %d (a duplicate, stored once)", got, http.StatusOK)
	}
	if rows := e.outboxRows(); len(rows) != 1 {
		t.Errorf("after the identical re-send the outbox holds %d rows, want 1: %+v", len(rows), rows)
	}
	if got := e.apiCalls(); got != callsAfterFirst {
		t.Errorf("the identical re-send reached the provider's API (%d calls, was %d): it was not refused at the accept path",
			got, callsAfterFirst)
	}

	// Case two: the same JSON object, one byte of whitespace longer. ClickUp's delivery of one
	// event is not byte-stable in general, and the accept path hashes bytes, so this is the
	// re-send the accept path cannot catch.
	respelled := append(bytes.Clone(body), '\n')
	if bytes.Equal(respelled, body) {
		t.Fatal("the respelled body is the same bytes, so the second case proves nothing")
	}
	if got := e.deliverSigned(respelled, secretA); got != http.StatusAccepted {
		t.Fatalf("the respelled delivery answered %d, want %d: it is a new row by construction", got, http.StatusAccepted)
	}
	// On either outcome. The wrong ones are listed first and on purpose: a defect that lets the
	// respelled delivery prepare its own records, or offer them to a sink, or that stops its row
	// reaching a final state at all, is what the assertions below are written to name, and a wait
	// that could only be satisfied by the right outcome would time out before they run.
	e.wait("the respelled delivery to be drained", func() bool {
		if e.preparedRecordCount() > 2 || e.sinks.deliverCalls() > sinkCallsAfterFirst {
			return true
		}
		rows := e.outboxRows()
		if len(rows) != 2 {
			return len(rows) > 2
		}
		for _, r := range rows {
			if !done(r) {
				return false
			}
		}
		return true
	})
	// Which of the two outcomes it was. A respelled delivery the ledger refuses is still a
	// delivered row: its records are skipped, nothing is left to offer, and the row is finished
	// with. A row that is instead retried, or dead-lettered, means the re-send was refused
	// somewhere that is not the ledger, and saying so here is what keeps the three assertions
	// below from being read as the ledger's answer when they are not.
	for i, r := range e.outboxRows() {
		if r.State != outbox.StateDelivered || r.Attempts > 1 {
			t.Errorf("outbox row %d is %+v, want a delivered row on its first attempt: the respelled delivery "+
				"was not refused by the ledger and drained, so the counts below are about something else", i, r)
		}
	}
	// It was hydrated and normalized, so the ledger and nothing earlier is what refused its
	// records.
	if got := e.apiCalls(); got <= callsAfterFirst {
		t.Errorf("the ClickUp API was called %d times in all, and %d after the first delivery: the respelled "+
			"delivery was never hydrated, so the ledger is not what this case is testing", got, callsAfterFirst)
	}
	// And it produced no record: none prepared, none offered to a sink. Both halves matter. The
	// first says the ledger held them back, and the second says they were held back BEFORE a
	// receiver was asked to be idempotent about them.
	if got := e.preparedRecordCount(); got != 2 {
		t.Errorf("%d records have been prepared in all, want the 2 of the first delivery: the re-send prepared its own", got)
	}
	if got := e.sinks.deliverCalls(); got != sinkCallsAfterFirst {
		t.Errorf("the sink was called %d times, and %d after the first delivery: the re-send's records were offered "+
			"again and only the sink's idempotence hid it", got, sinkCallsAfterFirst)
	}
	if got := len(e.documents(tenantA)); got != 2 {
		t.Errorf("the sink holds %d documents, want 2: a re-send produced new records", got)
	}
	if got := e.ledgerCount(); got != 2 {
		t.Errorf("the ledger holds %d records, want 2", got)
	}
}

// Clause 3. A crash between prepare and deliver re-drains into "already delivered" rather than a
// duplicate (ADR 14, and architecture 3.2 step 7).
//
// The crash is the sink call dying, which is what a worker killed between its two commits looks
// like from the outside: the records and the ledger rows are committed, the row is prepared, and
// nothing has been delivered. The question the test has to answer is not "did two documents turn
// up" but where they came from, because a re-drain that derived them again would have prepared the
// delivery twice. The calls to the provider's API are the only thing that can tell a delivery read
// out of the store from one normalized a second time.
func TestACrashBetweenPrepareAndDeliverReDrainsIntoAlreadyDelivered(t *testing.T) {
	e := setup(t)
	body := fixture(t, "webhook_comment_posted.json")
	e.sinks.dieOnce()

	if got := e.deliverSigned(body, secretA); got != http.StatusAccepted {
		t.Fatalf("the edge answered %d, want %d", got, http.StatusAccepted)
	}
	stop := e.run()
	defer stop()

	// The crash: prepared, nothing at the sink, and the row still holding the lease of the worker
	// that died.
	e.wait("the delivery to be prepared and the sink call to die", func() bool {
		rows := e.outboxRows()
		return len(rows) == 1 && rows[0].State == outbox.StatePrepared && e.sinks.deliverCalls() == 1
	})
	if got := len(e.documents(tenantA)); got != 0 {
		t.Fatalf("%d documents reached the sink before the crash, want none: the crash did not happen where this test needs it", got)
	}
	if got := e.ledgerCount(); got != 2 {
		t.Fatalf("the ledger holds %d records after the prepare, want 2", got)
	}
	callsAtCrash := e.apiCalls()

	// The dead worker's lease runs out, and another worker takes the row over.
	//
	// The wait ends on either outcome, the right one or the wrong one, so that a re-drain which
	// re-derives the delivery is named by the assertions below rather than being left to time out
	// with nothing said about why.
	e.leaseExpired()
	e.wait("the row to be claimed again and leave the prepared state", func() bool {
		rows := e.outboxRows()
		return e.apiCalls() > callsAtCrash || (len(rows) == 1 && rows[0].State != outbox.StatePrepared)
	})
	if rows := e.outboxRows(); len(rows) != 1 || rows[0].State != outbox.StateDelivered {
		t.Errorf("the row is %+v after the re-drain, want one delivered row: a prepared delivery that is "+
			"re-derived instead of being read back cannot be delivered at all, because the stored body is not "+
			"fetched for a row that is past pending (outbox.Work)", rows)
	}

	if got := len(e.documents(tenantA)); got != 2 {
		t.Errorf("the sink holds %d documents, want 2, the two the first commit stored: more means the re-drain "+
			"duplicated the delivery, fewer means it could not deliver what was stored", got)
	}
	if got := e.ledgerCount(); got != 2 {
		t.Errorf("the ledger holds %d records, want 2: the re-drain prepared the delivery a second time", got)
	}
	// The assertion that says the records came out of the store. A re-drain that normalized again
	// would have asked ClickUp for the task and its comments a second time, and would then have
	// been skipped by its own ledger rows and delivered nothing at all.
	if got := e.apiCalls(); got != callsAtCrash {
		t.Errorf("the ClickUp API was called %d times, and %d before the re-drain: the re-drain re-hydrated "+
			"instead of delivering what the first commit stored", got, callsAtCrash)
	}
	if got := e.sinks.deliverCalls(); got != 2 {
		t.Errorf("the sink was called %d times, want 2: one that died and one that delivered", got)
	}
	// What the re-drain OFFERED, which is the only place a duplicate is visible: the sink is
	// idempotent on the record id, so a batch carrying one id twice leaves the receiver holding
	// exactly what one copy would, and the document count cannot tell the two apart.
	batches := e.sinks.batches()
	if len(batches) != 2 {
		t.Fatalf("the sink was offered %d batches, want 2", len(batches))
	}
	if len(batches[1]) != 2 {
		t.Errorf("the re-drain offered %d records (%v), want exactly the 2 the first commit stored", len(batches[1]), batches[1])
	}
	if batches[1][0] == batches[1][1] {
		t.Errorf("the re-drain offered one record id twice: %v", batches[1])
	}
	if !slices.Equal(batches[0], batches[1]) {
		t.Errorf("the re-drain offered %v and the attempt that died offered %v: a re-drain delivers what the "+
			"first commit decided, not a fresh derivation of it", batches[1], batches[0])
	}
}

// Clause 4. A forged signature gets 401 and stores nothing.
//
// The control case is half of this test. A 401 with an empty outbox is also what an unregistered
// provider, a path the mux redirected, a subscription that was never stored and a hub that refuses
// everything would produce. So the forgeries are shown beside the one delivery that differs from
// them by the signature alone, and that one has to be accepted and routed.
func TestAForgedSignatureGets401AndStoresNothing(t *testing.T) {
	e := setup(t)
	body := fixture(t, "webhook_comment_posted.json")

	for _, tc := range []struct {
		name      string
		signature string
	}{
		{"a signature over the right bytes with the wrong secret", sign(secretB, body)},
		// The B06 lesson, applied to ClickUp: the signature is over the bytes that arrived and
		// nothing may re-serialize them, so a signature over a body spelled differently is a
		// forgery even though the two parse to the same event.
		{"a signature over the same event spelled differently", sign(secretA, append(bytes.Clone(body), '\n'))},
		{"no signature at all", ""},
		{"a signature that is not hex", strings.Repeat("z", 64)},
		// hex.DecodeString hands back the bytes it decoded BEFORE its error, so a correct digest
		// followed by anything that is not hex decodes to exactly the right 32 bytes.
		{"a correct signature with trailing rubbish", sign(secretA, body) + "zz"},
		{"a correct signature of the right length but half a byte short", sign(secretA, body)[:63] + "0"},
	} {
		if got := e.deliver(body, tc.signature); got != http.StatusUnauthorized {
			t.Errorf("%s answered %d, want %d", tc.name, got, http.StatusUnauthorized)
		}
		e.wantNoRows("a delivery nobody can be shown to have signed stores nothing at all: " + tc.name)
		e.wantNoDocuments("nothing was stored, so nothing can be delivered: " + tc.name)
	}

	// Two signature headers, which is two claims where the scheme allows one, and the one case
	// that needs the edge to have passed the header fields through as they arrived. One of the two
	// is correct, so a scheme that took the first, or the last, or any that matched would accept
	// it.
	right := sign(secretA, body)
	for _, tc := range []struct {
		name       string
		signatures []string
	}{
		{"a correct signature and a forged one", []string{right, sign(secretB, body)}},
		{"a forged signature and a correct one", []string{sign(secretB, body), right}},
		{"the correct signature twice", []string{right, right}},
	} {
		if got := e.deliverWithSignatures(body, tc.signatures...); got != http.StatusUnauthorized {
			t.Errorf("%s answered %d, want %d", tc.name, got, http.StatusUnauthorized)
		}
		e.wantNoRows("two signature headers are two claims, and neither may be chosen: " + tc.name)
	}

	// The control. Every case above differs from this one by the signature alone, so a 401 here
	// would mean they were refused by something other than the signature check.
	if got := e.deliverSigned(body, secretA); got != http.StatusAccepted {
		t.Fatalf("the honest control answered %d, want %d: the refusals above were not about the signature",
			got, http.StatusAccepted)
	}
	rows := e.outboxRows()
	if len(rows) != 1 || rows[0].Tenant != tenantA.String() {
		t.Fatalf("the honest delivery produced %+v, want one row owned by %s", rows, tenantA)
	}
}

// Clause 5. A webhook for an unknown workspace is parked, never routed.
//
// "Unknown workspace" for ClickUp is a webhook id no subscription records, because a ClickUp
// delivery carries no workspace or team id at all and the registration id is its only key (ADR 15
// decision 3, and DeliveryKeys). The delivery is correctly signed, with a secret that is real to
// whoever sent it: that is the point, because there is no signature claim to reject when no row
// exists to check one against.
func TestAWebhookForAnUnknownWorkspaceIsParkedAndNeverRouted(t *testing.T) {
	const unknownWebhook = "7fa3ec74-0000-4000-8000-00000000beef"
	e := setup(t)
	registered := fixture(t, "webhook_comment_posted.json")
	unknown := bytes.ReplaceAll(registered, []byte(fixtureWebhook(t)), []byte(unknownWebhook))
	if bytes.Equal(unknown, registered) {
		t.Fatal("the unknown delivery is the registered one, so this test proves nothing")
	}

	// 200 and never an error status: a provider must not be made to retry a delivery nobody owns.
	if got := e.deliverSigned(unknown, secretA); got != http.StatusOK {
		t.Fatalf("the edge answered %d, want %d", got, http.StatusOK)
	}
	rows := e.outboxRows()
	if len(rows) != 1 {
		t.Fatalf("the outbox holds %d rows, want exactly one parked row: %+v", len(rows), rows)
	}
	for _, c := range []struct{ what, got, want string }{
		{"the parked row's tenant", rows[0].Tenant, tenancy.Sentinel.String()},
		{"the parked row's state", rows[0].State, outbox.StateDead},
		{"the parked row's reason", rows[0].DeadReason, outbox.ParkNoOwner.String()},
	} {
		if c.got != c.want {
			t.Errorf("%s is %q, want %q", c.what, c.got, c.want)
		}
	}
	if rows[0].IsHead {
		t.Error("the parked row is the head of its key, so a worker would claim it")
	}

	// Never routed, and never drained. The drain runs beside the parked row and carries a
	// legitimate delivery all the way to the sink, which is what proves it was polling and still
	// left the parked row alone: "the sink is empty" on its own is also what a drain that never
	// started would produce.
	stop := e.run()
	defer stop()
	if got := e.deliverSigned(registered, secretA); got != http.StatusAccepted {
		t.Fatalf("the registered delivery answered %d, want %d", got, http.StatusAccepted)
	}
	// On either outcome. The parked row is dead from the moment it was stored, so "a dead row" is
	// the normal state of affairs here and cannot be the signal: what is watched is the row of the
	// tenant whose delivery this is, so that a legitimate delivery the drain fails to carry is
	// named by the assertion below instead of timing out.
	e.wait("the drain to carry a legitimate delivery past the parked row", func() bool {
		return len(e.documents(tenantA)) >= 2 || e.doneFor(tenantA)
	})
	if got := len(e.documents(tenantA)); got != 2 {
		t.Fatalf("the legitimate delivery put %d documents at the sink, want 2: the drain did not carry it past "+
			"the parked row, so \"the parked row was left alone\" below would be what a drain that never ran "+
			"looks like. outbox: %+v", got, e.outboxRows())
	}
	if got := len(e.documents(tenancy.Sentinel)); got != 0 {
		t.Errorf("%d documents were delivered for the sentinel tenant, want 0: a parked delivery was drained", got)
	}
	parked := e.outboxRows()
	if len(parked) != 2 {
		t.Fatalf("the outbox holds %d rows, want 2 (one parked, one delivered): %+v", len(parked), parked)
	}
	if parked[0].Tenant != tenancy.Sentinel.String() || parked[0].State != outbox.StateDead {
		t.Errorf("the parked row is now %+v: the drain moved it", parked[0])
	}

	// The control: the same bytes, routed, once a subscription records that webhook id. It is what
	// tells "parked because nobody owns it" apart from "parked because this provider could not
	// read its own body", which is a different reason in a different column.
	sub, err := clickup.SubscriptionFor(tenantB, fixtureWorkspace(t), unknownWebhook, secretB)
	if err != nil {
		t.Fatalf("SubscriptionFor: %v", err)
	}
	if _, err := e.subscriptions().Register(e.ctx, sub); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := e.deliverSigned(unknown, secretB); got != http.StatusAccepted {
		t.Fatalf("the same delivery answered %d once its subscription existed, want %d: it had been parked for "+
			"some other reason than having no owner", got, http.StatusAccepted)
	}
	// On either outcome, for the same reason: tenant B's row reaching a final state without two
	// documents is the control failing, and the assertion below says so.
	e.wait("the once-unowned delivery to be delivered to its owner", func() bool {
		return len(e.documents(tenantB)) >= 2 || e.doneFor(tenantB)
	})
	if got := len(e.documents(tenantB)); got != 2 {
		t.Fatalf("the once-owned delivery put %d documents at tenant B's sink, want 2: the control cannot tell "+
			"\"parked because nobody owns it\" from \"parked for some other reason\". outbox: %+v",
			got, e.outboxRows())
	}
	if got := len(e.documents(tenantA)); got != 2 {
		t.Errorf("tenant A holds %d documents, want the 2 of its own delivery: a delivery crossed tenants", got)
	}
}
