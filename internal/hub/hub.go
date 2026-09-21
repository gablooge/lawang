// Package hub decides whose data a delivery becomes.
//
// It is the half of the accept path that internal/ingress deliberately does not have
// (architecture 3.1): the edge turns a path segment into a provider and captures the exact bytes,
// and everything that touches a tenant happens here. In order:
//
//  1. Ask the provider for the delivery's own keys, which are unauthenticated text.
//  2. Look up the candidate subscriptions those keys could belong to, across tenants, under the
//     resolver role, in a transaction that does nothing else.
//  3. Verify the exact bytes against each candidate's secret, in constant time, once per
//     candidate.
//  4. Resolve the owner: none verified is 401 and nothing stored; exactly one is the tenant; more
//     than one is refused.
//  5. Accept the delivery into the outbox, bound to that tenant, or park it under the sentinel
//     tenant when nobody can be shown to own it.
//
// # Principle 2 is the whole package
//
// The tenant comes from a row Lawang owns, never from the payload. Nothing a sender writes selects
// a tenant: the delivery's keys only narrow which rows are asked, and a row's tenant counts only
// once that row's secret has verified the exact bytes the sender sent. When more than one
// subscription's secret verifies one delivery, it is parked and not routed, because routing to the
// first match is how data crossed tenants in the Python predecessor. That is why nothing here
// short-circuits on the first candidate that verifies. Two rows of ONE tenant are parked too: the
// ordering key names the subscription, so which of them owns the delivery is a real question (ADR
// 11, decision 5).
//
// # Where this package stops
//
// At a row in the outbox. Parsing the delivery into changes, hydrating, normalizing and delivering
// are the worker's (B08, B09, B10), on the stored bytes, in another process. Nothing here calls a
// provider's API, and nothing here holds a transaction open across anything but database work.
package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/hub/hubdb"
	"github.com/gablooge/lawang/internal/ingress"
	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/store"
	"github.com/gablooge/lawang/internal/tenancy"
)

// DefaultMaxCandidates is how many candidate subscriptions one delivery may have before the hub
// stops trying to tell them apart.
//
// It is a bound on work a sender can ask for: every candidate costs an HMAC over the whole body,
// which at the 1 MiB body cap is about a millisecond, so without a bound one delivery could ask
// for as much verification as there are subscriptions on one workspace. Thirty-two of them is
// about 32 ms of the accept path's 200 ms target, and far more tenants than are expected to share
// one provider workspace (two is the case the design is built for, architecture section 5).
//
// Reaching it is not a truncation. A candidate set that was silently cut short could hide the
// second tenant whose secret verifies, and route a delivery that should have been parked, so one
// more row than this is parked as an ambiguous owner.
const DefaultMaxCandidates = 32

// maxCandidatesCeiling is the most New will take for Options.MaxCandidates.
//
// Past about 200 candidates the verification alone is the whole 200 ms the accept path targets, so
// a larger number does not configure a hub that answers more deliveries, it configures one that
// answers 503 to them. (It also keeps the limit an int32 can carry, which is what the query takes.)
const maxCandidatesCeiling = 1024

// maxDeliveryKeyLen bounds a delivery key, in bytes, before it reaches a query. The key is text a
// stranger sent and the subscriptions table refuses to store one longer than this, so a longer key
// cannot match any row: refusing it here keeps a megabyte of a stranger's text out of a statement
// and out of whatever quotes one in an error. The table's CHECK carries the same number, and
// TestTheDeliveryKeyBoundMatchesTheColumn holds the two together.
const maxDeliveryKeyLen = 256

// Hub resolves the owner of a delivery and stores it. It is safe for concurrent use: everything it
// holds is read-only after New, and every request's state lives on the stack.
type Hub struct {
	db            *store.DB
	ob            *outbox.Outbox
	log           *slog.Logger
	maxCandidates int
}

// Options tunes the hub. The zero Options is the documented default of every field.
type Options struct {
	// PublicBaseURL is LAWANG_PUBLIC_BASE_URL as config.Load normalized it, or empty when the
	// deployment configured none. The hub does not build a URL out of it (the edge does): it
	// needs it to answer one question at start, which is whether a registered provider's
	// signature scheme covers a URL that nothing will supply. See New.
	PublicBaseURL string
	// MaxCandidates is the most candidate subscriptions one delivery may have, and 0 means
	// DefaultMaxCandidates. A value below 1 is refused.
	MaxCandidates int
	// Logger is where the hub logs, and nil means slog.Default.
	Logger *slog.Logger
}

// The hub is what the edge hands a delivery to.
var _ ingress.Hub = (*Hub)(nil)

// New returns a hub on db, or says why this deployment cannot accept deliveries at all.
//
// # Why a registry is a parameter of a thing that never looks a provider up
//
// The hub never resolves a path segment: the edge does that and hands it the entry. The registry is
// here for a start-up check the edge cannot make. A provider whose signature scheme covers the
// request URL (provider.URLSigner, HubSpot v3) can verify nothing when the deployment configured
// no public base URL: provider.Request.URL is empty, the scheme returns false rather than guess,
// and every delivery is answered 401, the status the contract reserves for a forged signature.
// The edge only warns, because it does not know which schemes sign the URL. The hub is handed the
// registry, so it knows, and it refuses to start rather than let a whole provider's traffic fail
// in the one place nobody reads, which is that provider's own dashboard (architecture 4).
func New(db *store.DB, reg *provider.Registry, opts Options) (*Hub, error) {
	if db == nil {
		return nil, errors.New("hub: a database is required")
	}
	if reg == nil {
		return nil, errors.New("hub: a provider registry is required")
	}
	maxCandidates := opts.MaxCandidates
	switch {
	case maxCandidates == 0:
		maxCandidates = DefaultMaxCandidates
	case maxCandidates < 1, maxCandidates > maxCandidatesCeiling:
		return nil, fmt.Errorf("hub: MaxCandidates must be 1 to %d, or 0 for the default of %d",
			maxCandidatesCeiling, DefaultMaxCandidates)
	}
	if opts.PublicBaseURL == "" {
		for _, e := range reg.Entries() {
			source, ok := e.WebhookSource()
			if !ok {
				continue
			}
			if _, signsURL := source.(provider.URLSigner); signsURL {
				return nil, fmt.Errorf(
					"hub: provider %q signs the public URL it was posted to, and LAWANG_PUBLIC_BASE_URL is not set, "+
						"so every one of its deliveries would be answered 401 as if it were forged: set the variable to the "+
						"URL the provider was given", e.Key())
			}
		}
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Hub{db: db, ob: outbox.New(db), log: log, maxCandidates: maxCandidates}, nil
}

// candidate is one subscription that could own a delivery, as the resolver read it. It is not a
// provider.Subscription: the resolver role reads three columns and this is those three.
type candidate struct {
	id     string
	tenant tenancy.ID
	secret []byte
}

// ownersForLog names the subscriptions an ambiguous delivery could have arrived on, and the
// tenants that hold them, so that the park is something an operator can act on. A count says a
// delivery stopped; these two say which rows to compare, and whether the collision is between two
// tenants or inside one.
//
// Both are this program's own identifiers and neither is a sender's text: the subscription id is
// the table's primary key, bounded by maxIDLen and written by Register, and the tenant id has been
// through tenancy.Parse. The secret, the body and the delivery's keys stay out of the line.
//
// The order is the candidate order, which candidates sorts by subscription id, so the same
// collision reads the same way every time and in the same way for anyone comparing two logs.
func ownersForLog(owners []candidate) (subscriptions, tenants string) {
	ids := make([]string, 0, len(owners))
	holders := make([]string, 0, len(owners))
	for _, c := range owners {
		ids = append(ids, c.id)
		holders = append(holders, c.tenant.String())
	}
	return strings.Join(ids, ","), strings.Join(holders, ",")
}

// Accept resolves the delivery's owner and stores it, and returns what the provider is told.
//
// It returns an error only for a failure on our side. Everything a sender can cause is a verdict,
// including a forged signature, a workspace nobody has connected, a body the provider cannot read
// its own keys out of, and poison that can never be stored.
func (h *Hub) Accept(ctx context.Context, e provider.Entry, req provider.Request) (ingress.Verdict, error) {
	source, ok := e.WebhookSource()
	if !ok {
		// The edge answers 404 before it gets here, so this is only ever reachable by another
		// caller. It is our own wiring either way, never a sender, so it is an error and not a
		// verdict.
		return 0, fmt.Errorf("hub: provider %q is not a webhook source", e.Key())
	}

	keys, err := h.deliveryKeys(source, req)
	if err != nil {
		// The provider does not recognize the body as one of its own, or the keys it read cannot
		// identify anything. Nothing can be resolved from it, and it is not a signature failure,
		// so it is parked and answered 2xx like any other delivery nobody owns.
		h.log.Debug("hub: a delivery carried no usable keys", "provider", e.Key(), "error", err)
		return h.park(ctx, e.Key(), req.Body, outbox.ParkUnreadable)
	}

	candidates, err := h.candidates(ctx, e.Key(), keys)
	if err != nil {
		return 0, err
	}
	switch {
	case len(candidates) == 0:
		// An unknown workspace: nobody has connected it, or the connection was deleted and the
		// provider-side webhook stayed behind. Parked, never a 401: there is no signature claim to
		// reject, and a provider must not be made to retry.
		return h.park(ctx, e.Key(), req.Body, outbox.ParkNoOwner)
	case len(candidates) > h.maxCandidates:
		h.log.Warn("hub: a delivery had more candidate subscriptions than the hub will verify, so it is parked",
			"provider", e.Key(), "max_candidates", h.maxCandidates)
		return h.park(ctx, e.Key(), req.Body, outbox.ParkAmbiguousOwner)
	}

	owners, panicked, err := h.verifyAll(ctx, e.Key(), source, req, candidates)
	switch {
	case err != nil:
		return 0, err
	case panicked:
		return h.park(ctx, e.Key(), req.Body, outbox.ParkUnverifiable)
	case len(owners) == 0:
		// The one thing that answers 401, and the one thing that stores nothing.
		return ingress.Unverified, nil
	case len(owners) > 1:
		// Principle 2. More than one subscription's secret verified the same bytes, so the
		// delivery could have arrived on any of them and none can be shown to own it. Routing to
		// the first is the defect this refuses, and it refuses it just as firmly when the rows
		// belong to one tenant: the ordering key names the subscription, so choosing one is
		// choosing a queue on no evidence (ADR 11, decision 5).
		subscriptions, tenants := ownersForLog(owners)
		h.log.Warn("hub: more than one subscription's secret verified one delivery, so it is parked and not routed",
			"provider", e.Key(), "owners", len(owners),
			"subscriptions", subscriptions, "tenants", tenants)
		return h.park(ctx, e.Key(), req.Body, outbox.ParkAmbiguousOwner)
	}

	owner := owners[0]
	_, fresh, err := h.ob.Accept(ctx, owner.tenant, outbox.Delivery{
		// The registry's own constant, never the path segment the sender wrote.
		Provider:    e.Key(),
		OrderingKey: orderingKey(e.Key(), owner.id),
		RawBody:     req.Body,
	})
	switch {
	case errors.Is(err, outbox.ErrBadOrderingKey):
		// Poison: this delivery can never be stored for this tenant, however often it is sent
		// again, so it must not be a status the provider retries. It is reachable only through a
		// subscription row whose id the table would not have got from Lawang.
		h.log.Error("hub: an accepted delivery could not be stored and was parked",
			"provider", e.Key(), "subscription", owner.id, "error", err)
		return h.park(ctx, e.Key(), req.Body, outbox.ParkUnstorable)
	case err != nil:
		return 0, retryable(ctx, err)
	case !fresh:
		// The provider re-sent something this tenant already has. An accept no-op, and a 200.
		return ingress.Duplicate, nil
	}
	return ingress.Stored, nil
}

// errNoDeliveryKeys reports a delivery that carries nothing to look a subscription up by.
var errNoDeliveryKeys = errors.New("the delivery carries no key to resolve an owner by")

// deliveryKeys asks the provider for the delivery's own identifiers and holds them to what a
// lookup can use. They arrive from the public internet, before anything is verified, so they are
// bounded and checked here rather than trusted to be what the provider package promises.
//
// A delivery with no key at all is refused rather than looked up: without one the only way to find
// an owner would be to verify every subscription of the provider, which is work a stranger could
// ask for by sending an empty body.
func (h *Hub) deliveryKeys(source provider.WebhookSource, req provider.Request) (provider.DeliveryKeys, error) {
	keys, err := source.DeliveryKeys(req.Body, req.Header)
	if err != nil {
		return provider.DeliveryKeys{}, err
	}
	if keys.Workspace == "" && keys.Subscription == "" {
		return provider.DeliveryKeys{}, errNoDeliveryKeys
	}
	for _, k := range []struct{ name, key string }{
		{"workspace", keys.Workspace},
		{"subscription", keys.Subscription},
	} {
		name, key := k.name, k.key
		switch {
		case len(key) > maxDeliveryKeyLen:
			// The length, never the key: it is a stranger's text and this goes to a log line.
			return provider.DeliveryKeys{}, fmt.Errorf("the %s key is %d bytes, over the %d the table stores", name, len(key), maxDeliveryKeyLen)
		case !storable(key):
			// Postgres stores neither in a text column and answers SQLSTATE 22021, which would
			// reach the sender as a 500 for a delivery that is simply not ours.
			return provider.DeliveryKeys{}, fmt.Errorf("the %s key is not storable text", name)
		}
	}
	return keys, nil
}

// candidates returns the subscriptions the delivery's keys could belong to, and at most one more
// than the hub will verify per key.
//
// It runs under the resolver role, which sees across tenants because deriving the tenant is the
// one thing that cannot be done inside a tenant. That transaction is cross-tenant for its whole
// life and does this and nothing else: it commits before anything is verified and long before
// anything is written, and the tenant's own work happens in a second transaction under TenantTx
// (architecture 4, and store.RoleTx, whose transaction refuses a bind for this reason).
//
// # Why both queries, and not the one the delivery looks most like
//
// A subscription is registered with a workspace id, with the registration's own id, or with both,
// and which of them a provider can give is the provider's business, not ours: a ClickUp webhook
// names a workspace, a Microsoft Graph notification names the subscription. A delivery that
// carries both keys can therefore belong to a row that recorded only the other one, and asking
// only the query that fits the delivery's shape would leave that row out of the candidate set.
//
// Leaving a row out is not a missed delivery, it is a wrong owner: "exactly one tenant verified"
// is the whole of principle 2, and it cannot be told from "the one tenant we happened to ask
// verified". The first review of this item reproduced exactly that, two tenants whose secrets both
// verify being routed to one of them because only one of the two was ever a candidate.
//
// So each key the delivery carries runs its own equality probe, on its own index, and the union of
// what they return is the candidate set. Each query also filters on the OTHER key, so a key that
// both the delivery and the row carry must agree; a key either of them left empty says nothing
// either way. Two probes rather than one OR keeps both lookups index-probed under the generic plan
// pgx's statement cache ends up with, which a single statement would not: a delivery that carries
// no registration id would probe the external id against the empty string and read every row
// that recorded none.
func (h *Hub) candidates(ctx context.Context, providerKey string, keys provider.DeliveryKeys) ([]candidate, error) {
	// One more than the maximum, so that "too many" is visible rather than a set cut short.
	limit := int32(h.maxCandidates) + 1 //nolint:gosec // bounded by New, which refuses less than 1

	var found []candidate
	err := h.db.RoleTx(ctx, store.RoleResolver, func(tx pgx.Tx) error {
		q := hubdb.New(tx)
		// A row that both queries return is one candidate and one HMAC, not two: a subscription
		// that verified twice would look like two tenants and park a delivery it owns outright.
		seen := make(map[string]struct{})
		var union []candidate
		add := func(cs []candidate) {
			for _, c := range cs {
				if _, dup := seen[c.id]; dup {
					continue
				}
				seen[c.id] = struct{}{}
				union = append(union, c)
			}
		}
		// A delivery with neither key never gets here: deliveryKeys refuses it.
		if keys.Workspace != "" {
			rows, err := q.CandidatesByWorkspace(ctx, hubdb.CandidatesByWorkspaceParams{
				Provider:      providerKey,
				Workspace:     keys.Workspace,
				Subscription:  keys.Subscription,
				MaxCandidates: limit,
			})
			if err != nil {
				return err
			}
			cs, err := candidatesFrom(rows, func(r hubdb.CandidatesByWorkspaceRow) (string, string, []byte) {
				return r.ID, r.TenantID, r.Secret
			})
			if err != nil {
				return err
			}
			add(cs)
		}
		if keys.Subscription != "" {
			rows, err := q.CandidatesBySubscription(ctx, hubdb.CandidatesBySubscriptionParams{
				Provider:      providerKey,
				Workspace:     keys.Workspace,
				Subscription:  keys.Subscription,
				MaxCandidates: limit,
			})
			if err != nil {
				return err
			}
			cs, err := candidatesFrom(rows, func(r hubdb.CandidatesBySubscriptionRow) (string, string, []byte) {
				return r.ID, r.TenantID, r.Secret
			})
			if err != nil {
				return err
			}
			add(cs)
		}
		// Each query orders by id; the union of two of them does not. The order decides which
		// candidate is verified first, and therefore which subscriptions an ambiguous delivery is
		// reported about, because verifyAll stops at the second that verifies and those are the
		// ids the park's log line names. Sorting makes that report the same every time.
		slices.SortFunc(union, func(a, b candidate) int { return strings.Compare(a.id, b.id) })
		found = union
		return nil
	})
	if err != nil {
		return nil, retryable(ctx, fmt.Errorf("hub: candidate subscriptions: %w", err))
	}
	return found, nil
}

// candidatesFrom turns the rows of either candidate query into candidates. The two queries have
// the same three columns and sqlc gives each its own row type, so cols names them once per query
// rather than repeating the loop.
func candidatesFrom[R any](rows []R, cols func(R) (id, tenantID string, secret []byte)) ([]candidate, error) {
	out := make([]candidate, 0, len(rows))
	for _, r := range rows {
		c, err := newCandidate(cols(r))
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// newCandidate turns a resolver row into a candidate, and refuses one that cannot be an owner. A
// tenant id the domain would not have allowed, or a secret that verifies nothing, means a row that
// did not come from this program.
//
// The error it returns fails the whole resolution, and the delivery becomes a 5xx. That is
// deliberate, and it is stronger than dropping the bad row: with the row skipped, a delivery the
// unaccountable row might have owned would be routed to whichever other candidate verified, and
// nothing would ever say that the table holds a row nobody can account for. A candidate set that
// cannot be reasoned about resolves nothing, because the whole of the isolation argument is that
// the tenant comes from a row Lawang owns.
func newCandidate(id, tenantID string, secret []byte) (candidate, error) {
	tenant, err := tenancy.Parse(tenantID)
	if err != nil {
		return candidate{}, fmt.Errorf("hub: a subscription row carries an unusable tenant id: %w", err)
	}
	if len(secret) == 0 {
		return candidate{}, fmt.Errorf("hub: subscription %q has an empty secret", id)
	}
	return candidate{id: id, tenant: tenant, secret: secret}, nil
}

// maxStackInLog bounds the stack of a recovered panic in the log line. A Verify is a few frames
// deep and this is room for dozens of them; the bound is here so that one provider bug cannot
// write an unbounded amount per delivery into a log an operator has to keep and pay for.
//
// It truncates mid line, which costs nothing: the frames that matter are the ones nearest the
// panic, and debug.Stack puts those first.
const maxStackInLog = 8 << 10

// maxTypeNameInLog bounds the type name of a recovered panic value. No type written in source
// comes near it, and the bound is the other half of the check below: a name can pass every
// character test and still be as long as the provider likes, because nesting is free
// ([][][]...int), so one panicking delivery could write as much of it as it wanted into the log.
const maxTypeNameInLog = 128

// refusedTypeName stands in for a panic value whose type name typeNameForLog will not vouch for.
// It says which of the two things happened (the type is unusual, not the log line missing), so an
// operator who meets it can go and look at the provider package.
const refusedTypeName = "(not a plain type name)"

// typeNameForLog is the dynamic type of a recovered value, as %T prints it, when that name is one
// only a plainly written type can have, and refusedTypeName otherwise.
//
// A type name is NOT a compile-time constant, which is the mistake this function exists to undo.
// reflect.StructOf builds a type at run time and takes arbitrary bytes in a struct tag, and %T
// prints the tag, so a Verify holding a tenant's secret can panic with a value whose type name is
// that secret. Dynamic struct libraries do the same with a delivery's own field names.
//
// What is left after this check cannot carry those bytes AS TEXT. Every run-time construction
// that can hold text of its own prints it inside braces and after a space (a struct tag or field
// name in "struct { ... }", a method name in "interface { ... }"), and a func type prints a
// parenthesis; space, brace and parenthesis are all refused here. The names that pass are built
// out of identifiers, package paths, and the six punctuation bytes below, all of which are written
// in a provider package's source.
//
// "As text" is the whole of that promise, and the known residual is a number. An array length is
// digits, and digits are allowed: reflect.PointerTo(reflect.ArrayOf(n, byteType)) prints
// "*[n]uint8", and n can be a secret's bytes read as a base-256 integer, which reflect's
// address-space check caps at roughly 47 bits, about six bytes per panicking delivery. Banning
// digits would not close that, only narrow it again: the channel is the provider package's own
// choice of what to panic with, not the character set of a type name.
//
// This narrows and does not close. A provider package that panics is already holding the secret
// in its own process and could log it directly, so the type name is trusted exactly as far as the
// provider package is, and no further; what this refuses is the trivial way to smuggle it into
// OUR log line, at error level, in every backup of it.
//
// There is no empty-name case to guard: %T is "<nil>" for a nil value, which the character check
// refuses, and a non-empty name for everything else.
func typeNameForLog(v any) string {
	name := fmt.Sprintf("%T", v)
	if len(name) > maxTypeNameInLog {
		return refusedTypeName
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == '*', c == '[', c == ']', c == '/':
		default:
			return refusedTypeName
		}
	}
	return name
}

// crash is what a recovered panic leaves behind for the log.
//
// It deliberately does NOT carry the recovered value. A panic value is whatever the provider
// package passed to panic, which can be a string or an error it built out of the delivery, and a
// provider that panics is by definition one whose promises are not being kept: it could carry a
// body, a delivery key or the secret it was handed into a log line that is then in every backup.
// What is kept is ours and is what actually finds the bug.
type crash struct {
	// kind is the dynamic type of the recovered value, as typeNameForLog vouched for it. A type
	// name is not a compile-time constant, so it is not trusted on that ground: see the function.
	kind string
	// stack is the goroutine's stack at the point of the panic: function names, files, lines and
	// argument words. Argument words are pointers and lengths, never the bytes they point at.
	stack []byte
}

// verifyAll runs the provider's verification once per candidate and returns every candidate whose
// secret verified the exact bytes.
//
// It does not stop at the first one that verifies, and that is the point: "exactly one" cannot be
// told from "the first of two" without asking them all. It stops at the second, because by then
// the delivery is already unattributable, and at the first panic, for the same reason.
func (h *Hub) verifyAll(ctx context.Context, providerKey string, source provider.WebhookSource, req provider.Request, candidates []candidate) (owners []candidate, panicked bool, err error) {
	for _, c := range candidates {
		verified, crashed, verifyErr := verifyOne(ctx, source, req, c.secret)
		switch {
		case verifyErr != nil:
			return nil, false, verifyErr
		case crashed != nil:
			// Architecture 7: Verify never panics, and nothing can enforce it. One that does has
			// answered nothing, so the candidates that did answer cannot settle the owner either.
			// This is a bug in a provider package, hence the error level, and the stack is the
			// whole reason the level is useful: without it the line says the provider's name and
			// nothing else, and there is nowhere to start looking.
			h.log.Error("hub: a provider's Verify panicked, so the delivery is parked instead of routed",
				"provider", providerKey, "panic_type", crashed.kind, "stack", string(crashed.stack))
			return nil, true, nil
		case verified:
			owners = append(owners, c)
			if len(owners) > 1 {
				return owners, false, nil
			}
		}
	}
	return owners, false, nil
}

// verifyOne is one call to a provider's Verify, defended against the two things the interface
// promises and the type system cannot enforce (architecture 7).
//
// A panic is recovered. Left alone, net/http recovers it per connection and the provider sees a
// dropped response rather than a status, which for a webhook means a retry storm against an
// endpoint that will drop it again.
//
// A Verify that does not return is given up on when ctx runs out, so a provider bug cannot hold a
// request past the accept path's bound: the caller then answers 503 with a Retry-After, which
// every provider understands, instead of nothing at all. The goroutine that was left behind stays
// until Verify returns, which for a genuinely stuck implementation is forever; that is the price
// of not blocking, and it is why the body it holds is a copy and not the request's own buffer.
//
// The copy is the other half. provider.Request.Body is documented as a rule and not a guarantee:
// the slice aliases the edge's own capture buffer and the same Request goes to every candidate, so
// an implementation that normalized the bytes in place (trimming a byte order mark, lower casing,
// blanking a field before hashing) would change what every later candidate verifies AND what is
// then stored in the outbox. Copying per candidate makes it a guarantee for 0.9 microseconds at
// 8 KiB and 57 at the 1 MiB cap, against a 200 ms target, and it is also what keeps a Verify that
// was given up on from racing with the INSERT of the same bytes.
func verifyOne(ctx context.Context, source provider.WebhookSource, req provider.Request, secret []byte) (ok bool, panicked *crash, err error) {
	req.Body = bytes.Clone(req.Body)

	type answer struct {
		ok      bool
		crashed *crash
	}
	// Buffered, so the goroutine finishes even when nobody is listening any more.
	done := make(chan answer, 1)
	go func() {
		var a answer
		defer func() {
			if r := recover(); r != nil {
				// Inside the deferred call of a panicking goroutine, so the frames that panicked
				// are still on the stack and this is the trace that names the line.
				stack := debug.Stack()
				if len(stack) > maxStackInLog {
					stack = stack[:maxStackInLog]
				}
				a = answer{crashed: &crash{kind: typeNameForLog(r), stack: stack}}
			}
			done <- a
		}()
		a.ok = source.Verify(req, secret)
	}()

	select {
	case a := <-done:
		return a.ok, a.crashed, nil
	case <-ctx.Done():
		return false, nil, fmt.Errorf("hub: verification did not finish: %w", ctx.Err())
	}
}

// park stores a delivery nobody can be shown to own under the sentinel tenant, and answers 2xx.
//
// Parking cannot reach a real tenant: outbox.Park takes no tenant at all, so there is no argument
// here for a crafted delivery to influence. What a sender contributes is the raw body, which the
// outbox keeps as it arrived for the reasons a sweep can settle later (B25) and replaces with a
// short note for the one it cannot: see outbox.Park. That matters here because ParkUnreadable
// takes no credential and no knowledge of any tenant to produce, and no later registration can
// ever make those bytes resolvable, so keeping them would be cost with no use. ParkNoOwner is as
// cheap for a stranger to produce and does keep its bytes, because B25 re-resolves rows from
// exactly those bytes; what that costs is #25's to bound.
func (h *Hub) park(ctx context.Context, providerKey string, body []byte, reason outbox.ParkReason) (ingress.Verdict, error) {
	_, fresh, err := h.ob.Park(ctx, providerKey, body, reason)
	if err != nil {
		return 0, retryable(ctx, err)
	}
	h.log.Debug("hub: parked a delivery under the sentinel tenant",
		"provider", providerKey, "reason", reason.String(), "fresh", fresh)
	// A re-send of a parked delivery is a Parked verdict too, not a Duplicate: both answer 200,
	// and "this was parked" is the true thing to log and to count (B25).
	return ingress.Parked, nil
}

// orderingKey is the queue an accepted delivery joins: the provider key and the id of the
// subscription that owns it, which are both this program's own strings.
//
// It is the subscription and not the entity because the hub does not parse a delivery, and must
// not: Parse runs in the worker, on verified bytes. So the finest grouping the accept path can
// name is the registration a delivery arrived on, which is coarser than one entity and therefore
// safe (the guarantee is that two versions of one entity are never in flight together, and a
// coarser queue keeps it). The cost is that one subscription's deliveries drain one at a time.
// A provider that can name its entity cheaply, from bytes it has already looked at, could narrow
// this later; that belongs with the pipeline (B08) and the first real provider (B11).
func orderingKey(providerKey, subscriptionID string) string {
	return providerKey + ":" + subscriptionID
}

// retryable makes an error the edge can answer honestly when the accept path ran out of time.
//
// The edge answers 503 with a Retry-After for errors.Is(err, context.DeadlineExceeded), which
// covers both shapes pgx produces for a saturated connection pool. It does not cover a statement
// the server cancelled, which comes back as a *pgconn.PgError with SQLSTATE 57014 and no context
// error anywhere in its chain: that would be answered 500, where "try again in a moment" is the
// truth. So whenever the context is already done, the context's error is put in front of whatever
// came back, and the wrapped error is kept for the log.
func retryable(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return fmt.Errorf("%w: %w", ctxErr, err)
	}
	return err
}
