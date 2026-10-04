package worker_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/sink"
	"github.com/gablooge/lawang/internal/store"
	"github.com/gablooge/lawang/internal/worker"
)

// TestADeliveryIsDrainedToTheSink is the whole path once: claim, read, normalize, prepare,
// deliver, finish.
//
// It also pins the thing the claim is split in two transactions for. The worker role is granted
// six scheduling columns and may not read a payload at all, so the premise is asserted first:
// the very statement the drain would run if it read the body under its claim is refused. The
// drain then works the row, which it could not do if it tried.
func TestADeliveryIsDrainedToTheSink(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{})

	// The premise, in two halves. Nothing else in this test could tell a drain that reads the
	// payload in the worker-role transaction from one that does not.
	err := e.db.RoleTx(e.ctx, store.RoleWorker, func(tx pgx.Tx) error {
		var n int
		return tx.QueryRow(e.ctx, "SELECT count(id) FROM outbox").Scan(&n)
	})
	if err != nil {
		t.Fatalf("the worker role cannot read the scheduling columns either, so the half below proves nothing: %v", err)
	}
	err = e.db.RoleTx(e.ctx, store.RoleWorker, func(tx pgx.Tx) error {
		var body []byte
		return tx.QueryRow(e.ctx, "SELECT raw_body FROM outbox LIMIT 1").Scan(&body)
	})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("reading a payload as the worker role gave %v, want a refusal", err)
	}

	id := e.accept(tenantA, "fake:S1", ev(entity, "1", listA))
	if n := e.drainOnce(); n != 1 {
		t.Fatalf("the drain claimed %d rows, want 1", n)
	}

	docs := e.documents(tenantA)
	if len(docs) != 1 {
		t.Fatalf("the stub holds %d documents, want 1", len(docs))
	}
	var got struct {
		Source     string `json:"source"`
		ExternalID string `json:"external_id"`
		Meta       struct {
			Delivery string `json:"delivery"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(docs[0], &got); err != nil {
		t.Fatalf("the document the sink received: %v", err)
	}
	if got.ExternalID != entity || got.Source != "fake" || got.Meta.Delivery != id {
		t.Errorf("the document = %+v, want the entity, the provider key and the delivery's id", got)
	}
	e.wantState(id, outbox.StateDelivered, "")
	if states := e.recordStates(id); len(states) != 1 {
		t.Errorf("the delivery's records = %v, want one", states)
	} else {
		for _, state := range states {
			if state != "delivered" {
				t.Errorf("the record is %q, want delivered", state)
			}
		}
	}
	// Nothing of another tenant's, and nothing for a tenant that has no rows.
	if docs := e.documents(tenantB); len(docs) != 0 {
		t.Errorf("tenant B received %d documents", len(docs))
	}
}

// TestTwoTenantsOfOneWorkspaceAreDrainedApart. Two tenants may connect the same provider
// workspace, and then the same bytes arrive for both: the claim runs across tenants, so this is
// the path where one tenant's delivery could be worked under the other's. Each record is salted
// with its tenant (principle 3), each sink delivery carries its own tenant, and the stub files
// them apart.
func TestTwoTenantsOfOneWorkspaceAreDrainedApart(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{})
	sameEvent := ev(entity, "1", listA)
	forA := e.accept(tenantA, "fake:S1", sameEvent)
	forB := e.accept(tenantB, "fake:S1", sameEvent)

	for e.drainOnce() > 0 { //nolint:revive // drain until there is nothing claimable left
	}

	e.wantState(forA, outbox.StateDelivered, "")
	if row := e.row(tenantB, forB); row.State != outbox.StateDelivered {
		t.Errorf("tenant B's row is %q, want delivered", row.State)
	}
	docsA, docsB := e.documents(tenantA), e.documents(tenantB)
	if len(docsA) != 1 || len(docsB) != 1 {
		t.Fatalf("the stub holds %d documents for A and %d for B, want one each", len(docsA), len(docsB))
	}
	idOf := func(doc []byte) string {
		var got struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(doc, &got); err != nil {
			t.Fatalf("the document: %v", err)
		}
		return got.ID
	}
	if idOf(docsA[0]) == idOf(docsB[0]) {
		t.Errorf("both tenants received the record id %s, so one tenant's record would overwrite the other's", idOf(docsA[0]))
	}
}

// TestACrashBetweenPrepareAndDeliverRedeliversWhatWasStored is B10's first acceptance line.
//
// The worker dies between its two commits. The ledger already holds the record, so deriving it
// again would skip it and the delivery would be marked delivered with nothing sent: the second
// attempt has to deliver what the first one stored. Exactly one record reaches the stub,
// whatever the attempt did.
func TestACrashBetweenPrepareAndDeliverRedeliversWhatWasStored(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{})
	id := e.accept(tenantA, "fake:S1", ev(entity, "1", listA))

	// The first attempt dies in the sink call, which is after the prepare commit and before
	// anything is marked delivered.
	e.sinks.mu.Lock()
	e.sinks.panics = 1
	e.sinks.mu.Unlock()
	e.drainOnce()

	e.wantState(id, outbox.StatePrepared, "")
	if n := e.ledgerCount(); n != 1 {
		t.Fatalf("the ledger holds %d rows after the crash, want the one the prepare wrote", n)
	}
	if got := e.documents(tenantA); len(got) != 0 {
		t.Fatalf("the stub holds %d documents, want none: the sink call never finished", len(got))
	}

	// The dead worker's lease runs out and another takes the row over.
	e.leaseExpired()
	if n := e.drainOnce(); n != 1 {
		t.Fatalf("the second attempt claimed %d rows, want 1", n)
	}

	if got := e.documents(tenantA); len(got) != 1 {
		t.Fatalf("the stub holds %d documents, want exactly 1", len(got))
	}
	if n := e.ledgerCount(); n != 1 {
		t.Errorf("the ledger holds %d rows, want 1: a prepared row is never prepared again", n)
	}
	e.wantState(id, outbox.StateDelivered, "")
	if calls := e.sinks.callCount(); calls != 2 {
		t.Errorf("the sink was called %d times, want 2: one that died and one that delivered", calls)
	}
}

// TestAFailingSinkWalksTheLadderAndParks is B10's second acceptance line, and
// TestReplayingTheDeadLetterDeliversIt below is its third. They are separate tests over the same
// setup because the second is only meaningful from the state the first leaves.
func TestAFailingSinkWalksTheLadderAndParks(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{})
	id := e.accept(tenantA, "fake:S1", ev(entity, "1", listA))
	e.sinks.breakWith(&sink.Fault{
		Action: sink.ActionRetry,
		Cause:  outbox.NewCause(outbox.ClassSinkUnavailable).WithStatus(503),
		Detail: "the connection failed",
	})

	// Two rungs, so two attempts are scheduled and the third is the one that parks the row.
	for attempt := 1; attempt <= len(shortLadder); attempt++ {
		e.drainOnce()
		row := e.row(tenantA, id)
		if row.State != outbox.StatePrepared || row.Attempts != int32(attempt) {
			t.Fatalf("attempt %d: state %q, attempts %d, want prepared and %d", attempt, row.State, row.Attempts, attempt)
		}
		if row.LastError != "sink unavailable (status 503)" {
			t.Errorf("attempt %d: last_error = %q", attempt, row.LastError)
		}
		if row.NextAttemptAt.Before(time.Now()) {
			t.Errorf("attempt %d: the row is due again at %v, want a backoff", attempt, row.NextAttemptAt)
		}
		e.due()
	}

	e.drainOnce()
	e.wantState(id, outbox.StateDead, "retries exhausted")
	if got := e.documents(tenantA); len(got) != 0 {
		t.Errorf("the stub holds %d documents, want none: every attempt failed", len(got))
	}
	// The records of the parked delivery are still waiting, which is what makes the replay
	// below deliver anything at all.
	for recordID, state := range e.recordStates(id) {
		if state != "prepared" {
			t.Errorf("record %s is %q, want prepared: nothing was delivered and nothing was refused", recordID, state)
		}
	}
	e.drainOnce()
	if n := e.drainOnce(); n != 0 {
		t.Errorf("a dead row was claimed %d times, want never", n)
	}
}

// TestReplayingTheDeadLetterDeliversIt is B10's third acceptance line.
func TestReplayingTheDeadLetterDeliversIt(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{})
	id := e.accept(tenantA, "fake:S1", ev(entity, "1", listA))
	e.sinks.breakWith(&sink.Fault{Action: sink.ActionRetry, Cause: outbox.NewCause(outbox.ClassSinkUnavailable)})
	for range len(shortLadder) + 1 {
		e.drainOnce()
		e.due()
	}
	e.wantState(id, outbox.StateDead, "retries exhausted")

	// The operator fixes the sink and replays the dead letter.
	e.sinks.fix()
	if err := e.ob.Replay(e.ctx, tenantA, id); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if n := e.drainOnce(); n != 1 {
		t.Fatalf("the replayed row was claimed %d times, want once", n)
	}

	if got := e.documents(tenantA); len(got) != 1 {
		t.Fatalf("the stub holds %d documents, want the replayed record", len(got))
	}
	e.wantState(id, outbox.StateDelivered, "")
	if n := e.ledgerCount(); n != 1 {
		t.Errorf("the ledger holds %d rows, want 1: a replayed prepared row is delivered, not prepared again", n)
	}
}

// TestARefusedRecordDiesWhileTheRestOfTheBatchLands is the per-record dead letter end to end:
// the sink refuses one record of a delivery that carries three, the other two land, and the
// delivery is finished rather than killed.
func TestARefusedRecordDiesWhileTheRestOfTheBatchLands(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{})
	id := e.accept(tenantA, "fake:S1",
		ev("fake:task:1", "1", listA), ev("fake:task:2", "1", listA), ev("fake:task:3", "1", listA))

	// Prepare the delivery without delivering it, so the test can name a record id the way a
	// receiver would: by the id it was offered.
	e.sinks.mu.Lock()
	e.sinks.panics = 1
	e.sinks.mu.Unlock()
	e.drainOnce()
	e.leaseExpired()

	states := e.recordStates(id)
	if len(states) != 3 {
		t.Fatalf("the delivery prepared %d records, want 3", len(states))
	}
	refused := ""
	for recordID := range states {
		if refused == "" || recordID < refused {
			refused = recordID // whichever, deterministically
		}
	}
	e.sinks.mu.Lock()
	e.sinks.panics, e.sinks.reject = 0, map[string]bool{refused: true}
	e.sinks.mu.Unlock()

	e.drainOnce()

	if got := len(e.documents(tenantA)); got != 2 {
		t.Errorf("the stub holds %d documents, want the two the receiver took", got)
	}
	e.wantState(id, outbox.StateDelivered, "")
	for recordID, state := range e.recordStates(id) {
		want := "delivered"
		if recordID == refused {
			want = "dead"
		}
		if state != want {
			t.Errorf("record %s is %q, want %q", recordID, state, want)
		}
	}

	// And the dead letter is replayable on its own: the row comes back, offers the one record,
	// and the two that landed are not sent again.
	e.sinks.mu.Lock()
	e.sinks.reject = nil
	e.sinks.mu.Unlock()
	if err := e.ob.Replay(e.ctx, tenantA, id); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	e.drainOnce()
	if got := len(e.documents(tenantA)); got != 3 {
		t.Errorf("the stub holds %d documents, want all three once the refused one was replayed", got)
	}
	for recordID, state := range e.recordStates(id) {
		if state != "delivered" {
			t.Errorf("record %s is %q, want delivered", recordID, state)
		}
	}
}

// TestADeliveryWhoseRecordsTheLedgerAlreadyHoldsIsFinished. A provider that sends one change
// twice, in two bodies that are not byte-identical, is two outbox rows and one record: the
// ledger holds the record id from the first, so the second prepares nothing at all. There is
// nothing to offer a sink, and the row has to be finished rather than left for the ladder,
// which would park a delivery that was never wrong.
func TestADeliveryWhoseRecordsTheLedgerAlreadyHoldsIsFinished(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{})
	first := ev(entity, "1", listA)
	again := ev(entity, "1", listA)
	again.Title = "the same version in another body" // a new delivery id, the same record id
	firstID := e.accept(tenantA, "fake:S1", first)
	againID := e.accept(tenantA, "fake:S1", again)

	for e.drainOnce() > 0 { //nolint:revive // drain until there is nothing claimable left
	}

	e.wantState(firstID, outbox.StateDelivered, "")
	e.wantState(againID, outbox.StateDelivered, "")
	if got := len(e.documents(tenantA)); got != 1 {
		t.Errorf("the stub holds %d documents, want 1: the second delivery carried a record the ledger held", got)
	}
	if got := e.recordStates(againID); len(got) != 0 {
		t.Errorf("the second delivery stored %v, want nothing: it prepared no records", got)
	}
	if n := e.ledgerCount(); n != 1 {
		t.Errorf("the ledger holds %d rows, want 1", n)
	}
	if calls := e.sinks.callCount(); calls != 1 {
		t.Errorf("the sink was called %d times, want once: an empty batch is not offered", calls)
	}
}

// TestARowNobodyReportsOnIsParkedAndItsEntityMovesOn. A worker that dies on every attempt
// records nothing, so the row's lease expires and it is claimed again forever, holding every
// later version of its entity behind it. The drain looks at the attempt count before it does
// any work and parks the row instead.
func TestARowNobodyReportsOnIsParkedAndItsEntityMovesOn(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{})
	first := e.accept(tenantA, "fake:S1", ev(entity, "1", listA))
	second := e.accept(tenantA, "fake:S1", ev(entity, "2", listA))

	e.sinks.mu.Lock()
	e.sinks.panics = 100 // every attempt dies
	e.sinks.mu.Unlock()

	// One claim per attempt until the drain gives up on it. The ladder has two rungs, so the
	// attempts it can account for are 1, 2 and 3, and the fourth claim is the one that parks.
	for range len(shortLadder) + 2 {
		e.drainOnce()
		e.leaseExpired()
		e.due()
	}

	e.wantState(first, outbox.StateDead, "attempts exhausted without a recorded failure")
	if got := e.row(tenantA, first).LastError; got != "no attempt on this row reported an outcome" {
		t.Errorf("last_error = %q", got)
	}
	// The point of parking it: the next version of the entity is no longer stuck behind it.
	e.sinks.fix()
	e.due()
	e.drainOnce()
	e.wantState(second, outbox.StateDelivered, "")
	if got := len(e.documents(tenantA)); got != 1 {
		t.Errorf("the stub holds %d documents, want the second version only", got)
	}
}

// TestANormalizerThatCannotReadTheBodyIsParkedAtOnce. A dead letter from the pipeline is not
// retried at all: the same bytes produce the same refusal, so the ladder would only put the
// answer off.
func TestANormalizerThatCannotReadTheBodyIsParkedAtOnce(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{})
	id, fresh, err := e.ob.Accept(e.ctx, tenantA, outbox.Delivery{
		Provider: "fake", OrderingKey: "fake:S1", RawBody: []byte(`{"type":"event","nonsense":true}`),
	})
	if err != nil || !fresh {
		t.Fatalf("Accept: %v", err)
	}

	e.drainOnce()

	e.wantState(id, outbox.StateDead, "not retryable")
	if got := e.row(tenantA, id).LastError; got != "normalizer failed" {
		t.Errorf("last_error = %q, want the normalizer class and no text from the body", got)
	}
	if got := e.row(tenantA, id).Attempts; got != 1 {
		t.Errorf("attempts = %d, want 1: a dead letter does not walk the ladder", got)
	}
	if n := e.ledgerCount(); n != 0 {
		t.Errorf("the ledger holds %d rows, want none", n)
	}
}

// TestAProviderThatIsUnreachableIsRetriedAndNotKilled. Hydration that fails is the provider's
// API, which comes back; the record is not lost over it, and the delivery is not parked while
// the ladder still has rungs.
func TestAProviderThatIsUnreachableIsRetriedAndNotKilled(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{})
	down := ev(entity, "1", fake.UnknownContainer)
	down.Hydrate = fake.HydrateFail
	id := e.accept(tenantA, "fake:S1", down)

	e.drainOnce()

	row := e.row(tenantA, id)
	if row.State != outbox.StatePending || row.LastError != "provider unavailable" {
		t.Errorf("row = state %q, last_error %q, want pending and the provider class", row.State, row.LastError)
	}
	if n := e.ledgerCount(); n != 0 {
		t.Errorf("the ledger holds %d rows, want none: nothing was prepared", n)
	}
}

// TestNewDrainRefusesAConfigurationThatCannotWork.
func TestNewDrainRefusesAConfigurationThatCannotWork(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{})
	conns := e.db.MaxConns()

	for name, opts := range map[string]worker.Options{
		"a pool that leaves the database nothing": {Pool: conns},
		"a pool larger than the database pool":    {Pool: conns + 1},
		"a negative pool":                         {Pool: -1},
		"a row that may outlive its lease":        {Lease: time.Minute, RowTimeout: time.Minute},
	} {
		if _, err := worker.NewDrain(e.db, drainPipeline(t), e.sinks, opts); err == nil {
			t.Errorf("%s: err = nil, want a refusal", name)
		}
	}
	// The premise: the same options without the one bad field are accepted, so each refusal
	// above is about the field it names.
	if _, err := worker.NewDrain(e.db, drainPipeline(t), e.sinks, worker.Options{Pool: conns - 1}); err != nil {
		t.Errorf("a pool of %d with %d connections: %v", conns-1, conns, err)
	}
	if _, err := worker.NewDrain(e.db, drainPipeline(t), e.sinks, worker.Options{Lease: time.Minute, RowTimeout: time.Second}); err != nil {
		t.Errorf("a row timeout under the lease: %v", err)
	}
	// And nothing is defaulted into existence.
	for name, build := range map[string]func() (*worker.Drain, error){
		"no database": func() (*worker.Drain, error) {
			return worker.NewDrain(nil, drainPipeline(t), e.sinks, worker.Options{})
		},
		"no pipeline": func() (*worker.Drain, error) { return worker.NewDrain(e.db, nil, e.sinks, worker.Options{}) },
		"no sinks":    func() (*worker.Drain, error) { return worker.NewDrain(e.db, drainPipeline(t), nil, worker.Options{}) },
	} {
		if _, err := build(); err == nil {
			t.Errorf("%s: err = nil, want a refusal", name)
		}
	}
}

// TestRunStopsClaimingAndFinishesWhatItHolds is the graceful shutdown of architecture section 9:
// cancel the context, stop claiming, let the row in flight finish.
//
// The row in flight is in its sink call when the cancel arrives, which is the point in the path
// where stopping would leave a lease on a row nobody is working and the records already
// committed.
func TestRunStopsClaimingAndFinishesWhatItHolds(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{Pool: 1, Batch: 1})
	id := e.accept(tenantA, "fake:S1", ev(entity, "1", listA))

	inSink := make(chan struct{}, 1)
	release := make(chan struct{})
	e.sinks.before = func() {
		select {
		case inSink <- struct{}{}:
		default:
		}
		<-release
	}

	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- e.d.Run(ctx) }()

	select {
	case <-inSink:
	case <-time.After(30 * time.Second):
		t.Fatal("the drain never reached the sink")
	}
	cancel() // the shutdown arrives while the delivery is in flight
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	// The row finished, although its context had been cancelled: the delivery was made and
	// recording it must not be what a shutdown interrupts.
	e.wantState(id, outbox.StateDelivered, "")
	if got := len(e.documents(tenantA)); got != 1 {
		t.Errorf("the stub holds %d documents, want 1", got)
	}
}

// TestShutdownGivesBackTheRowsItWillNotReach. A claimed row that the drain never starts on is
// not "in flight": leaving it to its lease stalls its entity for the length of the lease and
// spends an attempt on it, and enough restarts would park a row that nothing was ever wrong
// with.
func TestShutdownGivesBackTheRowsItWillNotReach(t *testing.T) {
	t.Parallel()
	e := setup(t, worker.Options{Pool: 1, Batch: 5})
	// Five entities, so one claim leases all five and they are worked one after another.
	var ids []string
	for _, key := range []string{"a", "b", "c", "d", "e"} {
		ids = append(ids, e.accept(tenantA, "fake:S1:"+key, ev("fake:task:"+key, "1", listA)))
	}

	inSink := make(chan struct{}, 1)
	release := make(chan struct{})
	e.sinks.before = func() {
		select {
		case inSink <- struct{}{}:
		default:
		}
		<-release
	}

	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	go func() { done <- e.d.Run(ctx) }()
	select {
	case <-inSink:
	case <-time.After(30 * time.Second):
		t.Fatal("the drain never reached the sink")
	}
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
	}

	var delivered, waiting int
	for _, id := range ids {
		row := e.row(tenantA, id)
		switch row.State {
		case outbox.StateDelivered:
			delivered++
		case outbox.StatePending, outbox.StatePrepared:
			waiting++
			if row.LeaseUntil != nil {
				t.Errorf("row %s is still leased after the shutdown, so nothing may claim it for the length of the lease", id)
			}
			if row.Attempts != 0 {
				t.Errorf("row %s was charged %d attempts for work that never started", id, row.Attempts)
			}
		default:
			t.Errorf("row %s is %q after a shutdown", id, row.State)
		}
	}
	if delivered == 0 || waiting == 0 {
		t.Fatalf("%d rows delivered and %d given back, want some of each: the test needs a shutdown in the middle of a batch", delivered, waiting)
	}
}
