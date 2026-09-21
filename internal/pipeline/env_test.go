package pipeline_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/ids"
	"github.com/gablooge/lawang/internal/pipeline"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/store"
	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/testdb"
)

const (
	tenantA = tenancy.ID("tenant_a")
	tenantB = tenancy.ID("tenant_b")
)

// occurred is the one instant every fixture event happened at. It is fixed so that nothing in a
// test depends on the clock, and so that two events differ only in what the test is about.
var occurred = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

type env struct {
	t   *testing.T
	ctx context.Context
	db  *store.DB
	tdb testdb.Database
	p   *pipeline.Pipeline
}

// setup gives a test its own migrated database, as the non-superuser application role, and a
// pipeline over one provider, which is the ordinary fake unless the test names another.
func setup(t *testing.T, opts pipeline.Options, providers ...provider.Provider) *env {
	t.Helper()
	if len(providers) == 0 {
		providers = []provider.Provider{fake.New(fake.DefaultKey)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	tdb := testdb.New(t)
	db, err := store.Open(ctx, tdb.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Migrate(ctx, testLogger()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return &env{t: t, ctx: ctx, db: db, tdb: tdb, p: newPipeline(t, opts, providers...)}
}

func newPipeline(t *testing.T, opts pipeline.Options, providers ...provider.Provider) *pipeline.Pipeline {
	t.Helper()
	reg, err := provider.NewRegistry(providers...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	p, err := pipeline.New(reg, opts)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	return p
}

// ev is one event of the fake provider, with the fields every test needs and nothing else.
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
func body(t *testing.T, events ...fake.Event) []byte {
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

// drain is one outbox row going through both halves of the pipeline, the way the worker will:
// Normalize outside any transaction, Prepare inside one bound to the row's tenant.
//
// It is the whole seam in five lines, which is the point: a caller cannot hold a transaction open
// across the provider's API by accident.
func (e *env) drain(tenant tenancy.ID, events ...fake.Event) (pipeline.Prepared, error) {
	e.t.Helper()
	n, err := e.p.Normalize(e.ctx, pipeline.Delivery{
		Tenant: tenant, Provider: fake.DefaultKey, ID: ids.New(), Body: body(e.t, events...),
	})
	if err != nil {
		return pipeline.Prepared{}, err
	}
	return e.prepare(tenant, n)
}

// prepare is the second half on its own, for the tests that build a Normalized themselves or
// prepare one under a tenant it was not sealed for.
func (e *env) prepare(tenant tenancy.ID, n pipeline.Normalized) (pipeline.Prepared, error) {
	e.t.Helper()
	var out pipeline.Prepared
	err := e.db.TenantTx(e.ctx, tenant, func(tx pgx.Tx) error {
		var err error
		out, err = e.p.Prepare(e.ctx, tx, tenant, n)
		return err
	})
	return out, err
}

// mustDrain fails the test when a delivery does not go through.
func (e *env) mustDrain(tenant tenancy.ID, events ...fake.Event) pipeline.Prepared {
	e.t.Helper()
	out, err := e.drain(tenant, events...)
	if err != nil {
		e.t.Fatalf("drain: %v", err)
	}
	return out
}

// ledgerRow is a row of record_ledger as the tests assert about it, read as the superuser so that
// nothing about row-level security can hide a row from an assertion.
type ledgerRow struct {
	recordID   string
	provider   string
	externalID string
	version    string
	scope      string
	supersedes string // "" for a record that replaced nothing
	isHead     bool
}

// ledgerRows returns the whole ledger, whoever owns it, in a stable order.
func (e *env) ledgerRows() []ledgerRow {
	e.t.Helper()
	return collect(e, `SELECT record_id, provider, external_id, version, scope,
	                          coalesce(supersedes, ''), is_head
	                     FROM lawang.record_ledger ORDER BY prepared_at, record_id`,
		func(r pgx.CollectableRow) (ledgerRow, error) {
			var l ledgerRow
			err := r.Scan(&l.recordID, &l.provider, &l.externalID, &l.version, &l.scope, &l.supersedes, &l.isHead)
			return l, err
		})
}

// redactionRow is a row of redaction_map.
//
// lastSeen is here because it is the only observable difference between "the masker did not run"
// and "the masker ran and found a value it had already mapped": the row count is the same either
// way, since one value is one row forever.
type redactionRow struct {
	tenant   string
	token    string
	kind     string
	value    string
	lastSeen time.Time
}

func (e *env) redactionRows() []redactionRow {
	e.t.Helper()
	return collect(e, `SELECT tenant_id, token, kind, value, last_seen_at
	                     FROM lawang.redaction_map ORDER BY value`,
		func(r pgx.CollectableRow) (redactionRow, error) {
			var m redactionRow
			err := r.Scan(&m.tenant, &m.token, &m.kind, &m.value, &m.lastSeen)
			return m, err
		})
}

func collect[T any](e *env, sql string, scan func(pgx.CollectableRow) (T, error)) []T {
	e.t.Helper()
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		e.t.Fatalf("connect as the superuser: %v", err)
	}
	defer func() { _ = conn.Close(e.ctx) }()
	rows, err := conn.Query(e.ctx, sql)
	if err != nil {
		e.t.Fatalf("query: %v", err)
	}
	got, err := pgx.CollectRows(rows, scan)
	if err != nil {
		e.t.Fatalf("read the rows: %v", err)
	}
	return got
}

// exec runs one statement as the superuser. It is how a test stands in for something another item
// owns, such as the retention sweep of B25.
func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		e.t.Fatalf("connect as the superuser: %v", err)
	}
	defer func() { _ = conn.Close(e.ctx) }()
	if _, err := conn.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("exec: %v", err)
	}
}
