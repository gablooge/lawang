package hub_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/hub"
	"github.com/gablooge/lawang/internal/ingress"
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

// The secrets the tests sign with. They are test material and nothing else: no provider ever sees
// them, and nothing here reads a credential from the maintainer's machine.
var (
	secretA = []byte("secret-of-tenant-a")
	secretB = []byte("secret-of-tenant-b")
)

type env struct {
	t   *testing.T
	ctx context.Context
	db  *store.DB
	tdb testdb.Database
	sub *hub.Subscriptions
}

// setup gives a test its own migrated database, as the non-superuser application role.
func setup(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	tdb := testdb.New(t)
	db, err := store.Open(ctx, tdb.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Migrate(ctx, discardLogger()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return &env{t: t, ctx: ctx, db: db, tdb: tdb, sub: hub.NewSubscriptions(db)}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// hub builds a hub over p and returns it with the registry entry the edge would hand it.
func (e *env) hub(p provider.Provider, opts hub.Options) (*hub.Hub, provider.Entry) {
	e.t.Helper()
	if opts.Logger == nil {
		opts.Logger = discardLogger()
	}
	reg, err := provider.NewRegistry(p)
	if err != nil {
		e.t.Fatalf("NewRegistry: %v", err)
	}
	h, err := hub.New(e.db, reg, opts)
	if err != nil {
		e.t.Fatalf("hub.New: %v", err)
	}
	entry, ok := reg.Lookup(p.Key())
	if !ok {
		e.t.Fatalf("the registry does not hold %q", p.Key())
	}
	return h, entry
}

// accept runs one delivery through the hub and fails on an error, which is never a sender's doing.
func (e *env) accept(h *hub.Hub, entry provider.Entry, req provider.Request) ingress.Verdict {
	e.t.Helper()
	verdict, err := h.Accept(e.ctx, entry, req)
	if err != nil {
		e.t.Fatalf("Accept: %v", err)
	}
	return verdict
}

// register stores a subscription and returns it as stored.
func (e *env) register(tenant tenancy.ID, resource, workspace, external string, secret []byte) provider.Subscription {
	e.t.Helper()
	stored, err := e.sub.Register(e.ctx, provider.Subscription{
		Tenant:    tenant,
		Provider:  fake.DefaultKey,
		Resource:  resource,
		Workspace: workspace,
		External:  external,
		Secret:    secret,
	})
	if err != nil {
		e.t.Fatalf("Register(%s, %s): %v", tenant, resource, err)
	}
	return stored
}

// delivery is one of the fake provider's event bodies, spelled out rather than marshalled so that
// the bytes a signature covers are the bytes in the test.
func delivery(workspace, subscription, task string) []byte {
	return fmt.Appendf(nil,
		`{"type":"event","workspace":%q,"subscription":%q,"events":[`+
			`{"external_id":"fake:task:%s","op":"upsert","version":"1","container":"L1",`+
			`"title":"hello","occurred_at":"2026-09-21T10:00:00Z"}]}`,
		workspace, subscription, task)
}

// signed is the request the edge would build for body, signed with secret.
func signed(body, secret []byte) provider.Request {
	h := http.Header{}
	if secret != nil {
		h.Set(fake.SignatureHeader, fake.Sign(secret, body))
	}
	return provider.Request{
		Method: http.MethodPost,
		Header: provider.NewHeader(h),
		Body:   body,
	}
}

// outboxRow is what the tests assert about, read as the superuser so that nothing about
// row-level security can make an unexpected row invisible to the assertion.
type outboxRow struct {
	tenant      string
	provider    string
	state       string
	isHead      bool
	deadReason  string
	orderingKey string
	body        string
}

// outboxRows returns every row of the outbox, in queue order, whoever owns it.
func (e *env) outboxRows() []outboxRow {
	e.t.Helper()
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		e.t.Fatalf("connect as the superuser: %v", err)
	}
	defer func() { _ = conn.Close(e.ctx) }()
	rows, err := conn.Query(e.ctx,
		`SELECT tenant_id, provider, state, is_head, dead_reason, ordering_key, convert_from(raw_body, 'UTF8')
		   FROM lawang.outbox ORDER BY seq`)
	if err != nil {
		e.t.Fatalf("read the outbox: %v", err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (outboxRow, error) {
		var o outboxRow
		err := r.Scan(&o.tenant, &o.provider, &o.state, &o.isHead, &o.deadReason, &o.orderingKey, &o.body)
		return o, err
	})
	if err != nil {
		e.t.Fatalf("read the outbox: %v", err)
	}
	return got
}

// wantNothingStored fails when any row reached the outbox.
func (e *env) wantNothingStored(why string) {
	e.t.Helper()
	if got := e.outboxRows(); len(got) != 0 {
		e.t.Fatalf("the outbox holds %d rows (%+v), want none: %s", len(got), got, why)
	}
}

// wantParked fails unless the outbox holds exactly one row, parked under the sentinel tenant for
// the given reason, finished and not claimable.
func (e *env) wantParked(reason, why string) outboxRow {
	e.t.Helper()
	got := e.outboxRows()
	if len(got) != 1 {
		e.t.Fatalf("the outbox holds %d rows (%+v), want exactly one parked row: %s", len(got), got, why)
	}
	row := got[0]
	switch {
	case row.tenant != tenancy.Sentinel.String():
		e.t.Fatalf("the row is owned by %q, want the sentinel tenant %q: %s", row.tenant, tenancy.Sentinel, why)
	case row.state != "dead":
		e.t.Fatalf("the parked row is %q, want dead: a parked row must never be drained", row.state)
	case row.isHead:
		e.t.Fatal("the parked row is the head of its key, so a worker would claim it")
	case row.deadReason != reason:
		e.t.Fatalf("dead_reason = %q, want %q", row.deadReason, reason)
	}
	return row
}

// wantStored fails unless the outbox holds exactly one pending row, owned by tenant.
func (e *env) wantStored(tenant tenancy.ID, why string) outboxRow {
	e.t.Helper()
	got := e.outboxRows()
	if len(got) != 1 {
		e.t.Fatalf("the outbox holds %d rows (%+v), want exactly one: %s", len(got), got, why)
	}
	if got[0].tenant != tenant.String() {
		e.t.Fatalf("the delivery was routed to %q, want %q: %s", got[0].tenant, tenant, why)
	}
	if got[0].state != "pending" {
		e.t.Fatalf("the stored row is %q, want pending", got[0].state)
	}
	return got[0]
}
