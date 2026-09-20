// Package provider holds the interfaces a SaaS integration implements and the registry that wires
// them up once at startup. Nothing here knows about any particular provider: each one lives in a
// package of its own under this one, and the rest of Lawang reaches it only through these
// interfaces (docs/architecture.md, section 7).
//
// Provider is what every integration implements. The others are optional capabilities, discovered
// with a type assertion rather than stubbed out, so a provider that has no webhook does not carry
// a method that returns "not supported".
package provider

import (
	"context"
	"net/http"
	"slices"

	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// Provider is one SaaS integration.
type Provider interface {
	// Key is the internal provider key ("slack", "ms_graph"): a lowercase letter followed by up
	// to 31 of a-z, 0-9 and underscore, and never a hyphen (record.ValidProviderKey, ADR 3). It
	// is a constant of the program, never a name a sink or a request chooses: it is the first
	// segment of every scope id and of every external id, and it is hashed into every record id,
	// so changing it re-keys everything the provider has ever delivered. A Registry calls Key
	// once, at registration, and uses its own copy of the result from then on.
	Key() string

	// Hydrate fetches the full object a Change is about. It is the only place a provider talks
	// to its API, it runs in the worker and never on the accept path, and it must honour ctx.
	// The pipeline degrades to a minimal record when it fails, so an error here is not a lost
	// change (architecture 3.2, step 4).
	Hydrate(ctx context.Context, t tenancy.ID, c Change) (Hydrated, error)

	// Normalize turns what Hydrate returned into the records of one change: usually one, and
	// more when a delivery carries an entity and its parent. The records are complete but
	// unsealed, because sealing needs the tenant and is the pipeline's step: Normalize fills
	// everything record.Seal then checks. It does no I/O.
	Normalize(h Hydrated, c Change) ([]record.Record, error)
}

// Change is one thing that happened at a provider, as its webhook body or a reconciliation page
// reported it. The same type carries the live path and the backfill path, so both end up in the
// same pipeline (architecture 3.2 and 3.3).
type Change struct {
	// ExternalID identifies the entity that changed, in the record format's namespaced form
	// ("clickup:task:86a1b2"): the provider key, a colon, and the provider's own id. It becomes
	// Record.ExternalID, and the outbox orders by the entity, so two versions of one entity must
	// give the same string.
	ExternalID string
	// Op is what happened to the entity.
	Op record.Op
	// Payload is the part of the delivery this change was read from, exactly as the provider
	// wrote it. It is what a record degraded from the webhook body is built out of, so a
	// provider keeps here what it would need if its own API were unreachable.
	Payload []byte
}

// Hydrated is the full object a provider fetched for a Change. It is deliberately opaque: only
// the provider that produced it reads it again, in its own Normalize, and no two providers'
// objects have a field in common, so an interface with methods here would be a shape every
// provider bends to and nothing else uses.
type Hydrated any

// WebhookSource is the optional capability of a provider that receives deliveries at
// /ingress/{provider}. Its four methods are the accept path in order: handshake, delivery keys,
// verification, parse (architecture 3.1).
//
// Everything they are handed arrives from the public internet, signed or not. body is the exact
// bytes of the request, and no method may assume it is JSON, or UTF-8, or anything else.
type WebhookSource interface {
	// Handshake answers a challenge the provider sends to prove the endpoint is ours, and
	// reports whether it did. It runs before any tenant is resolved and before anything is
	// verified or stored, because a challenge arrives when no subscription exists yet.
	//
	// The reply is bytes and a content type, not JSON: Slack echoes its challenge inside a JSON
	// object and Microsoft Graph echoes a validationToken as text/plain, so the interface cannot
	// assume a shape. Returning false means "this is not a handshake", and the delivery goes on
	// down the accept path.
	//
	// r is the request with its body already read and replaced by http.NoBody: read body, never
	// r.Body. r is there for the URL and the headers, which is where a challenge often is.
	Handshake(r *http.Request, body []byte) (Reply, bool)

	// DeliveryKeys reads the provider's own identifiers out of a delivery, which are what the
	// hub looks subscriptions up by. They are untrusted: they say which rows are candidates,
	// never which tenant this is. The tenant comes from the candidate whose secret verifies the
	// body (principle 2).
	//
	// It takes the body and the headers rather than a Request, because the identifiers are in the
	// delivery's own content: no provider puts them somewhere only a Request would carry, and a
	// lookup key read from configuration rather than from the delivery would select candidates
	// for the wrong delivery.
	DeliveryKeys(body []byte, h Header) (DeliveryKeys, error)

	// Verify reports whether the delivery is signed with secret, compared in constant time. It
	// never errors and never panics: a missing secret, a missing or malformed signature, a
	// missing r.URL that this scheme needs, and a body that is not what the provider sends are
	// all a plain false (principle 1). It is called once per candidate subscription, so it does
	// no I/O.
	Verify(r Request, secret []byte) bool

	// Parse turns a verified delivery into its changes. It runs in the worker, on the stored
	// bytes, never on the accept path.
	Parse(body []byte) ([]Change, error)
}

// Request is one delivery as a signature scheme sees it: the parts of the HTTP request a
// provider's signing scheme can cover. It is a struct rather than a longer parameter list because
// the schemes disagree about what a signature is over, and the next one to need a field it does
// not have must not break every implementation that came before it. ClickUp signs the body alone,
// Slack v0 signs a timestamp header and the body, and HubSpot v3 signs the method, the full
// request URL, the body and a timestamp header, so the union is what is here.
//
// Everything in it arrived from the public internet except URL, which is configuration.
type Request struct {
	// Method is the request method, and it is always "POST": ingress.Pattern fixes the method, so
	// the mux answers anything else itself and no other method reaches a provider. It is carried
	// because HubSpot v3 puts the method in its base string, and an implementation that builds
	// that string from this field rather than from a literal keeps saying the truth if the edge
	// ever routes a second method. There is nothing here to branch on.
	Method string

	// URL is the absolute public URL the provider posted to: the deployment's configured public
	// base URL, then this request's own escaped path, then its raw query if it has one.
	//
	// It is deliberately not built from Host, X-Forwarded-Host or X-Forwarded-Proto. Every one of
	// those is chosen by whoever sent the request (a tunnel or a reverse proxy passes them
	// through), and a sender that chooses part of its own signed input can make a signature
	// verify over content it picked, which is not a signature check at all.
	//
	// It is the empty string when the deployment set no public base URL. A scheme that signs the
	// URL must then return false rather than guess one, because a signature verified against a
	// URL Lawang invented proves nothing (fail closed). A scheme that does not sign the URL, which
	// is most of them, ignores this field and is unaffected.
	URL string

	// Header is the request's header fields. A scheme's timestamp and its signature are here, and
	// a header the sender did not send is the empty string, never an error.
	Header Header

	// Body is the exact bytes of the request, the ones a signature is over. An implementation
	// must not re-serialize them, and must not modify the slice, which is not copied.
	Body []byte
}

// Header is the header fields of one delivery, as a provider sees them: readable, and with no way
// to change what anything else will read.
//
// It is not an http.Header, and that is the point. The hub hands one Request to Verify once per
// candidate subscription (B07), so with a map an implementation that normalized a header in place
// (a Set or a Del on the way to building a base string) would change what every later candidate
// sees, and the symptom would be a signature that fails only for the second candidate and only
// when a tenant has more than one. A comment asking implementations not to do that is not a
// guard, and cloning the map per request does not help either, since every candidate is handed
// the same Request. A type with no mutating method takes the mistake off the table and copies
// nothing: the edge wraps the request's own map once, and reading through the wrapper costs a
// method call that inlines away.
type Header struct {
	h http.Header
}

// NewHeader wraps h, which the caller must not write to afterwards. The edge passes the request's
// own map, which net/http does not touch once the handler has been called.
func NewHeader(h http.Header) Header { return Header{h: h} }

// Get returns the first value of the named field, matching the name case-insensitively the way
// http.Header.Get does, and the empty string when the sender sent no such field. The zero Header
// has no fields, so Get on it is the empty string rather than a panic.
func (h Header) Get(name string) string { return h.h.Get(name) }

// Values returns every value of the named field, in the order the sender sent them, and nil when
// there is none. A scheme that refuses a delivery carrying two signature headers, which is two
// claims where the protocol allows one, needs the count and not just the first value.
//
// The slice is a copy, so writing to it changes nothing another candidate will read.
func (h Header) Values(name string) []string { return slices.Clone(h.h.Values(name)) }

// Reply is a handshake answer. The zero Reply is an empty 200.
type Reply struct {
	// Status is the HTTP status, and 0 means 200. The edge accepts a 2xx and a 4xx and treats
	// anything else as a bug in the provider: a handshake is not a delivery, so a 4xx for a
	// malformed challenge is a legitimate answer, while a redirect (which would let a provider
	// point a stranger somewhere) and a 5xx (which asks for a retry) are not.
	Status int
	// ContentType is the Content-Type header, and empty means text/plain; charset=utf-8. The edge
	// refuses anything outside a closed allowlist (text/plain and application/json, optionally
	// with charset=utf-8), because a handshake echoes a stranger's text and a provider that could
	// name the type could make this origin serve something a browser executes.
	ContentType string
	// Body is written as it stands. A provider that echoes a challenge is echoing text a
	// stranger sent, so it checks that text before putting it here.
	Body []byte
}

// DeliveryKeys are the provider's own identifiers on a delivery that say which subscriptions
// could own it. Both fields are optional, because providers differ: Slack sends a team id and
// ClickUp a webhook id, Microsoft Graph sends a subscription id per notification, and HubSpot
// sends a portal id. A key the provider did not send is the empty string, and the hub narrows
// candidates by the keys it has.
//
// Nothing in here establishes a tenant. It is text from a stranger until a candidate's secret
// verifies the body.
type DeliveryKeys struct {
	// Workspace is the provider's id of the workspace, team, portal or account.
	Workspace string
	// Subscription is the provider's id of the webhook registration itself.
	Subscription string
}
