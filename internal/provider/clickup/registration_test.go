package clickup_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/hub"
	"github.com/gablooge/lawang/internal/ingress"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/clickup"
	"github.com/gablooge/lawang/internal/store"
	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/testdb"
)

// The workspace, the webhooks and the secrets of these tests. None of them is real, and no file
// under testdata carries any of them.
const (
	workspace = "9000000001"
	webhook1  = "7fa3ec74-0000-4000-8000-000000000001"
	webhook2  = "7fa3ec74-0000-4000-8000-000000000002"
)

var (
	tenantA = tenancy.ID("tenant_a")
	tenantB = tenancy.ID("tenant_b")
	secretA = []byte("secret-of-tenant-a")
	secretB = []byte("secret-of-tenant-b")
)

type regEnv struct {
	t    *testing.T
	ctx  context.Context
	db   *store.DB
	tdb  testdb.Database
	subs *hub.Subscriptions
	h    *hub.Hub
	e    provider.Entry
}

func setupRegistration(t *testing.T) *regEnv {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	tdb := testdb.New(t)
	db, err := store.Open(ctx, tdb.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := db.Migrate(ctx, discard); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	reg, err := provider.NewRegistry(newProvider(t))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	h, err := hub.New(db, reg, hub.Options{Logger: discard})
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}
	entry, ok := reg.Lookup(clickup.Key)
	if !ok {
		t.Fatal("the registry does not hold the clickup provider")
	}
	return &regEnv{t: t, ctx: ctx, db: db, tdb: tdb, subs: hub.NewSubscriptions(db), h: h, e: entry}
}

func (r *regEnv) register(t tenancy.ID, workspaceID, webhookID string, secret []byte) provider.Subscription {
	r.t.Helper()
	sub, err := clickup.SubscriptionFor(t, workspaceID, webhookID, secret)
	if err != nil {
		r.t.Fatalf("SubscriptionFor: %v", err)
	}
	stored, err := r.subs.Register(r.ctx, sub)
	if err != nil {
		r.t.Fatalf("Register: %v", err)
	}
	return stored
}

// deliver runs one delivery through the hub, signed with secret, and returns what the provider
// would be told.
func (r *regEnv) deliver(webhookID string, secret []byte) ingress.Verdict {
	r.t.Helper()
	body := []byte(`{"event":"taskUpdated","task_id":"86a1b2","webhook_id":"` + webhookID + `",` +
		`"history_items":[{"id":"1","date":"1791100000000","field":"name","parent_id":"901100"}]}`)
	h := http.Header{}
	if secret != nil {
		h.Set(clickup.SignatureHeader, sign(secret, body))
	}
	verdict, err := r.h.Accept(r.ctx, r.e, provider.Request{
		Method: http.MethodPost,
		Header: provider.NewHeader(h),
		Body:   body,
	})
	if err != nil {
		r.t.Fatalf("Accept: %v", err)
	}
	return verdict
}

// rows reads the subscriptions table as the superuser, which is the only way to see every
// tenant's rows at once.
func (r *regEnv) rows() []struct {
	Tenant, Resource, Workspace, External string
} {
	r.t.Helper()
	conn, err := pgx.Connect(r.ctx, r.tdb.AdminURL)
	if err != nil {
		r.t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(r.ctx) }()
	q, err := conn.Query(r.ctx,
		`SELECT tenant_id, resource, workspace_id, external_id FROM lawang.subscriptions ORDER BY tenant_id, resource`)
	if err != nil {
		r.t.Fatalf("read the subscriptions: %v", err)
	}
	got, err := pgx.CollectRows(q, func(row pgx.CollectableRow) (struct {
		Tenant, Resource, Workspace, External string
	}, error) {
		var s struct{ Tenant, Resource, Workspace, External string }
		err := row.Scan(&s.Tenant, &s.Resource, &s.Workspace, &s.External)
		return s, err
	})
	if err != nil {
		r.t.Fatalf("read the subscriptions: %v", err)
	}
	return got
}

// One ClickUp workspace is one subscription row per tenant, however many times it is registered.
//
// This is ADR 11 decision 5's requirement on a registration design, and the hub has no defence
// of its own: two rows of one tenant that both verify one delivery are parked, for ever, with no
// way out but a human. The row's resource is the workspace id, and hub.Subscriptions.Register
// upserts on (tenant, provider, resource), so the second registration replaces the first.
func TestOneWorkspaceIsOneSubscriptionRow(t *testing.T) {
	t.Parallel()
	e := setupRegistration(t)
	first := e.register(tenantA, workspace, webhook1, secretA)
	second := e.register(tenantA, workspace, webhook2, secretA)

	if first.ID != second.ID {
		t.Errorf("re-registering the workspace made a new row (%s then %s)", first.ID, second.ID)
	}
	rows := e.rows()
	if len(rows) != 1 {
		t.Fatalf("the table holds %d rows for one workspace, want 1: %+v", len(rows), rows)
	}
	if rows[0].Resource != workspace {
		t.Errorf("resource = %q, want the workspace id: it is what the upsert keys on", rows[0].Resource)
	}
	if rows[0].External != webhook2 {
		t.Errorf("external id = %q, want the newest webhook %q", rows[0].External, webhook2)
	}
	if rows[0].Workspace != "" {
		t.Errorf("workspace id = %q, want empty: a ClickUp delivery carries none", rows[0].Workspace)
	}

	// The whole point: the delivery resolves to one owner and is stored, not parked.
	if got := e.deliver(webhook2, secretA); got != ingress.Stored {
		t.Errorf("verdict = %s, want stored", got)
	}
}

// A subscription that records the workspace instead of the webhook id is selected by no delivery
// at all. This is the failure a reader of ADR 11 decision 5 would not predict, because that
// decision assumed a ClickUp delivery carries a workspace id; the documentation says it carries
// only webhook_id, so the row that would be ambiguous is instead a row that is never found.
func TestASubscriptionWithoutTheWebhookIDIsNeverFound(t *testing.T) {
	t.Parallel()
	e := setupRegistration(t)
	// Registered the wrong way round, on purpose, which SubscriptionFor cannot express.
	if _, err := e.subs.Register(e.ctx, provider.Subscription{
		Tenant: tenantA, Provider: clickup.Key, Resource: workspace,
		Workspace: workspace, External: "", Secret: secretA,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := e.deliver(webhook1, secretA); got != ingress.Parked {
		t.Errorf("verdict = %s, want parked: no row records the webhook id the delivery carries", got)
	}
}

// Two tenants on two workspaces resolve to themselves, and a delivery signed with the other
// tenant's secret is a 401 rather than somebody else's record.
func TestTwoTenantsResolveToThemselves(t *testing.T) {
	t.Parallel()
	e := setupRegistration(t)
	e.register(tenantA, workspace, webhook1, secretA)
	e.register(tenantB, "9000000002", webhook2, secretB)

	if got := e.deliver(webhook1, secretA); got != ingress.Stored {
		t.Errorf("tenant A's delivery: %s, want stored", got)
	}
	if got := e.deliver(webhook2, secretB); got != ingress.Stored {
		t.Errorf("tenant B's delivery: %s, want stored", got)
	}
	if got := e.deliver(webhook1, secretB); got != ingress.Unverified {
		t.Errorf("tenant A's webhook signed with tenant B's secret: %s, want unverified (401)", got)
	}
	rows := e.rows()
	if len(rows) != 2 {
		t.Fatalf("the table holds %d rows, want 2: %+v", len(rows), rows)
	}
}

// SubscriptionFor refuses a registration that could not resolve a delivery.
//
// It cannot refuse a registration that would be a second row for one workspace, or a second row
// for one webhook id: it is given one registration and cannot see the others. The table is what
// refuses those, and TestOneWorkspaceIsOneSubscriptionRow and
// TestTwoWorkspacesOfOneTenantCannotShareAWebhookID are where they are held.
func TestSubscriptionForRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                   string
		tenant                 tenancy.ID
		workspaceID, webhookID string
		secret                 []byte
	}{
		{"no tenant", "", workspace, webhook1, secretA},
		{"no workspace", tenantA, "", webhook1, secretA},
		{"no webhook", tenantA, workspace, "", secretA},
		{"no secret", tenantA, workspace, webhook1, nil},
		{"an empty secret", tenantA, workspace, webhook1, []byte{}},
		{"a workspace that is not an identifier", tenantA, "9000/../1", webhook1, secretA},
		{"a webhook that is not an identifier", tenantA, workspace, "w h", secretA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sub, err := clickup.SubscriptionFor(tc.tenant, tc.workspaceID, tc.webhookID, tc.secret)
			if err == nil {
				t.Fatalf("accepted it: %+v", sub)
			}
			if !errors.Is(err, clickup.ErrBadRegistration) {
				t.Errorf("error is not an ErrBadRegistration: %v", err)
			}
			if sub.Secret != nil {
				t.Error("returned a subscription carrying the secret beside the error")
			}
		})
	}
}

// What SubscriptionFor builds is what the accept path needs, field by field. The three fields
// are the whole of the registration constraint, so they are asserted here and not only through
// the database tests above, which would still pass if two of them were swapped.
func TestSubscriptionForBuildsTheRowTheAcceptPathNeeds(t *testing.T) {
	t.Parallel()
	sub, err := clickup.SubscriptionFor(tenantA, workspace, webhook1, secretA)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case sub.Resource != workspace:
		t.Errorf("resource = %q, want the workspace id", sub.Resource)
	case sub.External != webhook1:
		t.Errorf("external = %q, want the webhook id", sub.External)
	case sub.Workspace != "":
		t.Errorf("workspace = %q, want empty", sub.Workspace)
	case sub.Provider != clickup.Key:
		t.Errorf("provider = %q", sub.Provider)
	case sub.Tenant != tenantA:
		t.Errorf("tenant = %q", sub.Tenant)
	}
	// The delivery key the hub will look this row up by is the one DeliveryKeys reads off a
	// delivery. If these two ever disagree, every delivery of the workspace is parked.
	keys, err := newProvider(t).DeliveryKeys(
		[]byte(`{"event":"taskUpdated","task_id":"a","webhook_id":"`+webhook1+`"}`), provider.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if keys.Subscription != sub.External {
		t.Errorf("a delivery is looked up by %q and the row records %q", keys.Subscription, sub.External)
	}
	if keys.Workspace != sub.Workspace {
		t.Errorf("a delivery carries workspace %q and the row records %q", keys.Workspace, sub.Workspace)
	}
}

// Two rows of one tenant cannot carry one webhook id, because both would be candidates for every
// delivery that names it, both would verify, and the hub would park the delivery for ever.
//
// This is the shape ADR 11 decision 5 feared, reached by the one route ClickUp leaves open. Not
// two rows for one workspace: the upsert on (tenant, provider, resource) makes that impossible.
// Two workspaces recorded against one webhook, which a registrar produces by recreating a webhook
// before the row it replaced was written, or by being handed the wrong workspace id.
// `subscriptions_one_registration_per_tenant` refuses it at the table, so the mistake is an error
// the registrar sees at once rather than a workspace that silently receives nothing for ever.
func TestTwoWorkspacesOfOneTenantCannotShareAWebhookID(t *testing.T) {
	t.Parallel()
	e := setupRegistration(t)
	e.register(tenantA, workspace, webhook1, secretA)

	second, err := clickup.SubscriptionFor(tenantA, "9000000002", webhook1, secretA)
	if err != nil {
		t.Fatalf("SubscriptionFor: %v", err)
	}
	if stored, err := e.subs.Register(e.ctx, second); err == nil {
		t.Fatalf("stored a second row for one webhook id: %+v", stored)
	}

	rows := e.rows()
	if len(rows) != 1 {
		t.Fatalf("the table holds %d rows, want 1: %+v", len(rows), rows)
	}
	// The point of refusing it: the delivery still resolves to its one owner.
	if got := e.deliver(webhook1, secretA); got != ingress.Stored {
		t.Errorf("verdict = %s, want stored", got)
	}
}

// Two tenants may still name one webhook id, and that is not Lawang's to rule out: ClickUp's id
// space is not this deployment's to make unique across accounts, and the hub already decides such
// a delivery on the secrets, routing it when one verifies and parking it when both do. The
// constraint above is per tenant for exactly that reason.
func TestTwoTenantsMayNameOneWebhookID(t *testing.T) {
	t.Parallel()
	e := setupRegistration(t)
	e.register(tenantA, workspace, webhook1, secretA)
	e.register(tenantB, "9000000002", webhook1, secretB)

	if got := e.deliver(webhook1, secretA); got != ingress.Stored {
		t.Errorf("tenant A's delivery: %s, want stored", got)
	}
	if got := e.deliver(webhook1, secretB); got != ingress.Stored {
		t.Errorf("tenant B's delivery: %s, want stored", got)
	}
}
