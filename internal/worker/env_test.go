package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/pipeline"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/sink"
	"github.com/gablooge/lawang/internal/store"
	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/testdb"
	"github.com/gablooge/lawang/internal/worker"
)

const (
	tenantA = tenancy.ID("tenant_a")
	tenantB = tenancy.ID("tenant_b")
	listA   = "L1"
	entity  = "fake:task:1"
)

// occurred is the one instant every fixture event happened at, so that nothing depends on the
// clock and two events differ only in what a test is about.
var occurred = time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)

// shortLadder is the retry schedule the tests walk: two rungs, so a row reaches its dead letter
// on the third attempt.
//
// The waits are an hour because a test never waits one. It moves the row's due time itself
// (env.due), which is what the end of a backoff looks like from the outside, and an hour is
// long enough that a test can also assert the backoff was really scheduled. A ladder of
// milliseconds would make that assertion pass whether or not anything was scheduled.
var shortLadder = outbox.Ladder{time.Hour, time.Hour}

type env struct {
	t     *testing.T
	ctx   context.Context
	db    *store.DB
	tdb   testdb.Database
	ob    *outbox.Outbox
	stub  *sink.Stub
	sinks *sinks
	d     *worker.Drain
}

// setup gives a test its own migrated database, as the non-superuser application role, a drain
// over the fake provider, and a strict stub sink behind a Sinks the test can break.
//
// providers replaces the fake provider, for the one test that needs a provider whose API fails
// in a way of its own. It keeps the same key, so nothing else about a test changes.
func setup(t *testing.T, opts worker.Options, providers ...provider.Provider) *env {
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

	if len(providers) == 0 {
		providers = []provider.Provider{fake.New(fake.DefaultKey)}
	}
	reg, err := provider.NewRegistry(providers...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	p, err := pipeline.New(reg, pipeline.Options{})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	stub, err := sink.NewStub(sink.StubConfig{})
	if err != nil {
		t.Fatalf("NewStub: %v", err)
	}
	s := &sinks{inner: stub, test: t}
	if opts.Ladder == nil {
		opts.Ladder = shortLadder
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	d, err := worker.NewDrain(db, p, s, opts)
	if err != nil {
		t.Fatalf("NewDrain: %v", err)
	}
	return &env{t: t, ctx: ctx, db: db, tdb: tdb, ob: outbox.New(db), stub: stub, sinks: s, d: d}
}

// sinks is the Sinks the drain is given: the strict stub, with the failures a test asks for in
// front of it. Everything it does not break goes through to the stub, so a test never asserts
// against a lenient double (principle 5).
type sinks struct {
	inner *sink.Stub
	// test is the test this double runs in, so that a refusal by the strict stub fails it
	// where it happens rather than looking like a delivery that did not arrive.
	test testing.TB

	mu sync.Mutex
	// err is what Sink answers instead of a sink.
	err error
	// panics is how many more Deliver calls panic, which is a worker that dies mid-row.
	panics int
	// fault is returned by Deliver instead of delivering, while it is set.
	fault *sink.Fault
	// plainErr is returned by Deliver instead of a *sink.Fault, which no sink may do.
	plainErr error
	// reject names record ids the receiver refuses, and extra names ids it refuses that were
	// never offered to it. rejectDetail is the Detail each Rejection carries, and rejectCode
	// is the code in its Cause (the default below when it is empty). A sink this repository
	// did not write chooses both freely.
	reject map[string]bool
	extra  []string
	// rejectAll refuses every record offered, for a test that does not know the record ids.
	rejectAll    bool
	rejectDetail string
	rejectCode   string
	// before runs at the start of every Deliver, for the tests that need to be inside the sink
	// call when something else happens (a shutdown).
	before func()
	// calls counts Deliver calls, and delivered counts the records that reached the stub.
	calls     int
	delivered int
}

func (s *sinks) Sink(context.Context, tenancy.ID) (sink.Sink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return s, nil
}

func (s *sinks) Deliver(ctx context.Context, t tenancy.ID, recs []record.Record) (sink.DeliveryResult, error) {
	s.mu.Lock()
	before := s.before
	s.mu.Unlock()
	if before != nil {
		before()
	}
	s.mu.Lock()
	s.calls++
	switch {
	case s.panics > 0:
		s.panics--
		s.mu.Unlock()
		panic("the sink call died, as a worker that is killed mid-delivery does")
	case s.plainErr != nil:
		err := s.plainErr
		s.mu.Unlock()
		return sink.DeliveryResult{}, err
	case s.fault != nil:
		fault := s.fault
		s.mu.Unlock()
		return sink.DeliveryResult{}, fault
	}
	reject, extra, detail, all, code := s.reject, s.extra, s.rejectDetail, s.rejectAll, s.rejectCode
	s.mu.Unlock()
	if code == "" {
		code = "unsupported_kind"
	}

	var result sink.DeliveryResult
	var taken []record.Record
	for _, r := range recs {
		if all || reject[r.ID] {
			result.Rejected = append(result.Rejected, sink.Rejection{
				ID:     r.ID,
				Cause:  outbox.NewCause(outbox.ClassSinkRejected).WithCode(code),
				Detail: detail,
			})
			continue
		}
		taken = append(taken, r)
	}
	for _, id := range extra {
		result.Rejected = append(result.Rejected, sink.Rejection{
			ID:     id,
			Cause:  outbox.NewCause(outbox.ClassSinkRejected).WithCode("invented"),
			Detail: detail,
		})
	}
	// Everything the receiver did not refuse goes to the strict stub, which is what actually
	// decides whether the records are acceptable.
	inner, err := s.inner.Deliver(ctx, t, taken)
	if err != nil {
		return sink.DeliveryResult{}, err
	}
	if len(inner.Rejected) > 0 {
		s.t().Errorf("the strict stub refused %d records the test meant to be delivered: %+v",
			len(inner.Rejected), inner.Rejected)
	}
	s.mu.Lock()
	s.delivered += len(taken)
	s.mu.Unlock()
	return result, nil
}

// t is set by setup so that a refusal inside the stub fails the test it happens in.
func (s *sinks) t() testing.TB { return s.test }

func (s *sinks) breakWith(fault *sink.Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault = fault
}

func (s *sinks) fix() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault, s.plainErr, s.err, s.panics = nil, nil, nil, 0
}

func (s *sinks) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// ev is one event of the fake provider.
func ev(externalID, version, container string) fake.Event {
	return fake.Event{
		ExternalID: externalID,
		Op:         "upsert",
		Version:    version,
		Container:  container,
		Title:      "a task",
		OccurredAt: occurred,
	}
}

// body is one delivery of the fake provider carrying these events.
func body(t testing.TB, events ...fake.Event) []byte {
	t.Helper()
	raw := make([]json.RawMessage, len(events))
	for i, e := range events {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal the event: %v", err)
		}
		raw[i] = b
	}
	out, err := json.Marshal(struct {
		Type         string            `json:"type"`
		Workspace    string            `json:"workspace"`
		Subscription string            `json:"subscription"`
		Events       []json.RawMessage `json:"events"`
	}{"event", "W1", "S1", raw})
	if err != nil {
		t.Fatalf("marshal the delivery: %v", err)
	}
	return out
}

// accept stores a delivery for a tenant, the way the hub does, and returns the outbox row's id.
func (e *env) accept(tenant tenancy.ID, key string, events ...fake.Event) string {
	e.t.Helper()
	id, fresh, err := e.ob.Accept(e.ctx, tenant, outbox.Delivery{
		Provider: fake.DefaultKey, OrderingKey: key, RawBody: body(e.t, events...),
	})
	if err != nil || !fresh {
		e.t.Fatalf("Accept: fresh %v, err %v", fresh, err)
	}
	return id
}

// drainOnce is one turn of one drain goroutine: claim a batch and work it.
func (e *env) drainOnce() int {
	e.t.Helper()
	n, err := e.d.Once(e.ctx)
	if err != nil {
		e.t.Fatalf("drain: %v", err)
	}
	return n
}

// row is the outbox row as the tests assert about it.
func (e *env) row(tenant tenancy.ID, id string) outbox.Row {
	e.t.Helper()
	got, err := e.ob.Get(e.ctx, tenant, id)
	if err != nil {
		e.t.Fatalf("Get: %v", err)
	}
	return got
}

// recordStates is the state of each record of a delivery, by record id, read as the superuser so
// that nothing about row-level security can hide a row from an assertion.
func (e *env) recordStates(outboxID string) map[string]string {
	e.t.Helper()
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		e.t.Fatalf("connect as the superuser: %v", err)
	}
	defer func() { _ = conn.Close(e.ctx) }()
	rows, err := conn.Query(e.ctx,
		`SELECT record_id, state FROM lawang.outbox_record WHERE outbox_id = $1`, outboxID)
	if err != nil {
		e.t.Fatalf("query: %v", err)
	}
	out := map[string]string{}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			e.t.Fatalf("scan: %v", err)
		}
		out[id] = state
	}
	if err := rows.Err(); err != nil {
		e.t.Fatalf("read the rows: %v", err)
	}
	return out
}

// ledgerCount is how many records have been prepared, over every tenant. A re-drain that
// prepared a delivery twice would show here, and nowhere else.
func (e *env) ledgerCount() int {
	e.t.Helper()
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		e.t.Fatalf("connect as the superuser: %v", err)
	}
	defer func() { _ = conn.Close(e.ctx) }()
	var n int
	if err := conn.QueryRow(e.ctx, `SELECT count(*) FROM lawang.record_ledger`).Scan(&n); err != nil {
		e.t.Fatalf("count the ledger: %v", err)
	}
	return n
}

// admin changes rows behind the application's back, to move time along.
func (e *env) admin(sql string) { testdb.Exec(e.t, e.tdb.AdminURL, sql) }

// due makes every waiting row claimable now, which is what the end of a backoff or a halt looks
// like from the outside.
func (e *env) due() {
	e.t.Helper()
	e.admin("UPDATE lawang.outbox SET next_attempt_at = now() - interval '1 second'")
}

// leaseExpired is a worker that died: its rows keep their lease until it runs out.
func (e *env) leaseExpired() {
	e.t.Helper()
	e.admin("UPDATE lawang.outbox SET lease_until = now() - interval '1 second' WHERE lease_token IS NOT NULL")
}

// documents is what reached the stub for a tenant.
func (e *env) documents(tenant tenancy.ID) [][]byte { return e.stub.Documents(tenant) }

// wantState fails unless the row is in this state, with this dead reason.
func (e *env) wantState(id, state, deadReason string) {
	e.t.Helper()
	got := e.row(tenantA, id)
	if got.State != state || got.DeadReason != deadReason {
		e.t.Errorf("row = state %q, dead_reason %q, last_error %q, want state %q and reason %q",
			got.State, got.DeadReason, got.LastError, state, deadReason)
	}
}

// errNoSink is what a Sinks answers for a tenant nothing is configured for.
var errNoSink = errors.New("this tenant has no sink configured")

// drainPipeline is a pipeline over the fake provider, for the tests that build a Drain of their
// own rather than using the one setup made.
func drainPipeline(t testing.TB) *pipeline.Pipeline {
	t.Helper()
	reg, err := provider.NewRegistry(fake.New(fake.DefaultKey))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	p, err := pipeline.New(reg, pipeline.Options{})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p
}
