package pipeline_test

import (
	"context"
	"net/http"

	"github.com/gablooge/lawang/internal/pipeline"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// normalizeFor is the first half of the pipeline on its own, for the tests that want the records
// before anything touches the database. It seals for tenantA, which is the tenant a test then
// either prepares under or deliberately does not.
func normalizeFor(e *env, events ...fake.Event) (pipeline.Normalized, error) {
	e.t.Helper()
	return e.p.Normalize(e.ctx, pipeline.Delivery{
		Tenant: tenantA, Provider: fake.DefaultKey, ID: "01JDELIVERY0000000000000000",
		Body: body(e.t, events...),
	})
}

// neverDegrades is the fake provider with its Degrade method taken away: a provider that receives
// deliveries and cannot build a record without its API, which is what most providers will be.
//
// Every method is written out rather than embedded, because embedding would promote Degrade and
// the type would be exactly what it is here to not be.
type neverDegrades struct{ p *fake.Provider }

func (n neverDegrades) Key() string { return n.p.Key() }
func (n neverDegrades) Hydrate(ctx context.Context, t tenancy.ID, c provider.Change) (provider.Hydrated, error) {
	return n.p.Hydrate(ctx, t, c)
}

func (n neverDegrades) Normalize(h provider.Hydrated, c provider.Change) ([]record.Record, error) {
	return n.p.Normalize(h, c)
}
func (n neverDegrades) Handshake(r *http.Request, body []byte) (provider.Reply, bool) {
	return n.p.Handshake(r, body)
}

func (n neverDegrades) DeliveryKeys(body []byte, h provider.Header) (provider.DeliveryKeys, error) {
	return n.p.DeliveryKeys(body, h)
}
func (n neverDegrades) Verify(r provider.Request, secret []byte) bool { return n.p.Verify(r, secret) }
func (n neverDegrades) Parse(body []byte) ([]provider.Change, error)  { return n.p.Parse(body) }

// apiOnly is a provider with no webhook at all: it hydrates and normalizes, and nothing can read a
// stored delivery for it. A reconciliation-only provider will look like this.
type apiOnly struct{ p *fake.Provider }

func (a apiOnly) Key() string { return a.p.Key() }
func (a apiOnly) Hydrate(ctx context.Context, t tenancy.ID, c provider.Change) (provider.Hydrated, error) {
	return a.p.Hydrate(ctx, t, c)
}

func (a apiOnly) Normalize(h provider.Hydrated, c provider.Change) ([]record.Record, error) {
	return a.p.Normalize(h, c)
}
