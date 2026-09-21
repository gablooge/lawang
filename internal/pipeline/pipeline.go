// Package pipeline is step 5 of the drain path (docs/architecture.md, section 3.2): it turns one
// accepted delivery into the records a sink receives, and it is the one place where a record, a
// tenant and the history of its entity meet.
//
// It runs in two halves, because one of them talks to a provider's API and the other holds a
// database transaction, and those two must never be the same span:
//
//	n, err := p.Normalize(ctx, pipeline.Delivery{...})   // parse, hydrate, normalize, gate, seal
//	err = db.TenantTx(ctx, tenant, func(tx pgx.Tx) error {
//	        out, err := p.Prepare(ctx, tx, tenant, n)     // ledger, supersede, mask
//	        ...                                           // the caller commits, then delivers
//	})
//
// Normalize does the network I/O and touches no transaction. Prepare does the database work and
// makes no call to anything outside this deployment. The caller (the worker, B10) owns the
// transaction, so it can commit the ledger rows together with whatever else it stores for the row
// it is draining, which is what architecture 3.2 step 6 asks for.
//
// # What Prepare guarantees
//
//   - Nothing reaches a sink that was not sealed for the tenant of the outbox row being drained
//     (record.SealedFor). A record that fails that is a bug or an attack, never a retry.
//   - A record id that has already been prepared is never prepared twice, except in the one shape
//     ADR 4 decision 7 refuses to guess about, which is dead-lettered instead of skipped.
//   - A supersede link points forward only: a record supersedes the newest record of its entity
//     at the moment it is prepared, and a record older than that one is held back rather than
//     made the new head.
//   - Every title and text that leaves here has been through the masker.
//
// # What a caller does with what comes back
//
// An error that wraps ErrDeadLetter cannot be fixed by trying again: the caller dead-letters the
// outbox row. Any other error is a failure of something that may work later (a provider's API,
// the database), so the caller lets the retry ladder have it. The counters on Prepared are the
// stage's own account of what it decided, and the caller logs them; the metrics of B25 read the
// same numbers.
package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// ErrDeadLetter marks every failure of this package that retrying cannot fix. The caller matches
// on it with errors.Is and dead-letters the outbox row instead of walking the ladder.
var ErrDeadLetter = errors.New("pipeline: not retryable")

// ErrUnknownProvider reports a delivery stored for a provider the registry does not hold. The
// provider of an outbox row is a constant of this program (the registry hands out its own copy of
// the key), so this is a deployment that dropped a provider while its rows were still queued.
var ErrUnknownProvider = errors.New("pipeline: no such provider")

// ErrNotAWebhookSource reports a stored delivery whose provider cannot parse one. Nothing can read
// those bytes, now or later.
var ErrNotAWebhookSource = errors.New("pipeline: the provider does not receive deliveries")

// ErrNotSealedForTenant reports a record that record.Seal did not mint for the tenant of the
// outbox row being drained, or that was changed after it was sealed.
//
// The tenant is in no field of the envelope, so a record sealed for tenant A marshals identically
// when it is delivered under tenant B and no sink can tell. This is the one place that can, and it
// refuses: a false from SealedFor is a bug of ours or an attack, and never something that comes
// right on the next attempt.
var ErrNotSealedForTenant = errors.New("pipeline: the record was not sealed for this tenant")

// ErrScopeReturned reports the case ADR 4 decision 7 refuses to guess about: a record id that has
// already been prepared arrives again, it is not its entity's newest record, and the newest record
// is in a different scope.
//
// It is either an entity that moved from scope A to B and back to A while the provider's version
// never changed, or a stale record built from an old webhook body in the scope the entity has
// left. The ledger cannot tell which, and the two want opposite treatments, so it does neither:
// the delivery is dead-lettered, with the entity and both scopes named, and an operator looks at
// the entity at the source. ADR 4 says what they do then.
var ErrScopeReturned = errors.New("pipeline: a prepared record id has come back in a different scope")

// ErrExternalIDTooLong reports an entity whose external id does not fit the ledger's chain key.
//
// The record format bounds an external id in characters (1,024) and the ledger has to bound it in
// bytes, because it is a column of a btree index and a btree tuple cannot exceed about 2,700
// bytes. Every external id a provider mints is far below this; one that is not cannot be linked
// into a supersede chain at all, so it is refused here by name rather than in the database as
// "index row size exceeds maximum".
var ErrExternalIDTooLong = errors.New("pipeline: the external id is too long to key a supersede chain")

// MaxExternalIDBytes is the longest external id the ledger can key an entity by. The
// record_ledger.external_id CHECK holds the same number.
const MaxExternalIDBytes = 2048

// Automation is what the gate does with a record whose origin.automation is true, which is to say
// a record the source itself marked as written by a bot or an integration.
type Automation uint8

// The gate's two settings. The zero value drops, because the stage is called a noise gate and a
// deployment that has not thought about it wants less noise in its memory, not more.
const (
	// DropAutomation drops such a record before it is sealed. It never reaches the ledger, so a
	// deployment that later changes its mind receives those entities again from the next change
	// onwards, and not the history in between.
	DropAutomation Automation = iota
	// KeepAutomation passes it on. origin.automation travels with the record either way, so a
	// sink can still tell.
	KeepAutomation
)

// Options configure a Pipeline.
type Options struct {
	// Automation is the noise gate's setting. The zero value drops.
	Automation Automation
}

// Pipeline is the stage. It is built once and is safe for concurrent use: it holds a registry,
// which never changes after startup, and a setting.
type Pipeline struct {
	reg  *provider.Registry
	opts Options
}

// New returns a Pipeline over reg. A nil registry is refused rather than defaulted: a pipeline
// that can look nothing up would dead-letter every delivery it was given.
func New(reg *provider.Registry, opts Options) (*Pipeline, error) {
	if reg == nil {
		return nil, errors.New("pipeline: New needs a registry")
	}
	return &Pipeline{reg: reg, opts: opts}, nil
}

// Delivery is one outbox row to prepare: the row's tenant, the provider key it was accepted under,
// the row's own id and the bytes as they arrived.
type Delivery struct {
	// Tenant owns the row. It comes from the claimed row, never from the delivery's content.
	Tenant tenancy.ID
	// Provider is the registry's key, which the accept path took from the registry itself.
	Provider string
	// ID is the outbox row's id, which travels to the sink as meta.delivery so that a support
	// question about one record can be traced back to one delivery.
	ID string
	// Body is the stored raw body, exactly as it was received.
	Body []byte
}

// Normalized is one delivery turned into sealed records, before anything has touched the database.
// Build it with Normalize and hand it to Prepare.
type Normalized struct {
	provider string
	records  []record.Record

	// Automation counts the records the noise gate dropped.
	Automation int
	// Degraded counts the records built from the webhook body because hydration failed. They are
	// ordinary records in every other way: in particular their scope is the one the hydrated
	// record would have carried, which is what keeps one version of one entity to one id.
	Degraded int
}

// Prepared is what Prepare decided. Records is what the caller hands to the sink, in order; the
// counters are everything else that happened.
type Prepared struct {
	// Records are sealed, linked and masked, in the order they must be delivered.
	Records []record.Record
	// Skipped counts the records whose id the ledger already held: a re-drain, a backfill that
	// overlaps the live feed, or a provider that sent the same version twice.
	Skipped int
	// Stale counts the records held back because the entity already has a newer version prepared.
	// A record that arrives after a newer one can never be linked (links point forward only), and
	// delivering it unlinked would leave a sink holding two live versions of one entity. The
	// commonest cause is a replayed dead letter, which architecture 3.2 sends to the back of the
	// queue on purpose.
	Stale int
	// Automation and Degraded are carried over from Normalize, so that one struct is the whole
	// account of one delivery and the caller logs one thing.
	Automation int
	Degraded   int
}

// Normalize is the hand-off: it parses the stored body into changes, hydrates each one, asks the
// provider to normalize it, drops automation noise and seals what is left.
//
// It talks to the provider's API, so it must not run inside a transaction.
//
// Hydration that fails does not lose the change. A provider that implements provider.Degrader is
// asked to build the record from the webhook body instead, and the record it builds derives its
// scope from the same inputs through the same function the hydrated one would have used (ADR 4,
// decision 7): otherwise one version of one entity would get two ids, be delivered twice and look
// like a move. A provider that cannot degrade, or that says this particular body does not carry
// what the scope is made of (provider.ErrCannotDegrade), gets the hydration error back as a
// retryable failure. A guessed scope is never an option.
func (p *Pipeline) Normalize(ctx context.Context, d Delivery) (Normalized, error) {
	if _, err := tenancy.Parse(d.Tenant.String()); err != nil {
		// Fail closed. A record sealed for no tenant is a record no sink can be given safely.
		return Normalized{}, fmt.Errorf("%w: %w", ErrDeadLetter, err)
	}
	entry, ok := p.reg.Lookup(d.Provider)
	if !ok {
		return Normalized{}, fmt.Errorf("%w: %w: %q", ErrDeadLetter, ErrUnknownProvider, d.Provider)
	}
	src, ok := entry.WebhookSource()
	if !ok {
		return Normalized{}, fmt.Errorf("%w: %w: %q", ErrDeadLetter, ErrNotAWebhookSource, d.Provider)
	}
	changes, err := src.Parse(d.Body)
	if err != nil {
		// Parse read bytes that are already stored, so a second attempt reads the same bytes and
		// fails the same way.
		return Normalized{}, fmt.Errorf("%w: parse: %w", ErrDeadLetter, err)
	}
	out := Normalized{provider: entry.Key()}
	prov := entry.Provider()
	for _, c := range changes {
		recs, degraded, err := hydrateAndNormalize(ctx, prov, d.Tenant, c)
		if err != nil {
			return Normalized{}, err
		}
		if degraded {
			out.Degraded += len(recs)
		}
		for _, r := range recs {
			if r.Origin.Automation && p.opts.Automation == DropAutomation {
				out.Automation++
				continue
			}
			r.Meta.Delivery = d.ID
			sealed, err := r.Seal(entry.Key(), d.Tenant)
			if err != nil {
				// A normalizer that hands over a record the format refuses is a bug in the
				// provider package, and the same bytes will produce the same record next time.
				return Normalized{}, fmt.Errorf("%w: seal: %w", ErrDeadLetter, err)
			}
			out.records = append(out.records, sealed)
		}
	}
	return out, nil
}

// hydrateAndNormalize turns one change into its unsealed records, through the degraded path when
// hydration fails.
func hydrateAndNormalize(ctx context.Context, prov provider.Provider, t tenancy.ID, c provider.Change) (recs []record.Record, degraded bool, err error) {
	h, hydrateErr := prov.Hydrate(ctx, t, c)
	if hydrateErr == nil {
		recs, err := prov.Normalize(h, c)
		if err != nil {
			return nil, false, fmt.Errorf("%w: normalize: %w", ErrDeadLetter, err)
		}
		return recs, false, nil
	}
	deg, ok := prov.(provider.Degrader)
	if !ok {
		// Nothing to degrade to, so the change waits for the provider's API to come back. The
		// retry ladder dead-letters it when the attempts run out (B10).
		return nil, false, fmt.Errorf("hydrate: %w", hydrateErr)
	}
	recs, degradeErr := deg.Degrade(c)
	switch {
	case degradeErr == nil:
		return recs, true, nil
	case errors.Is(degradeErr, provider.ErrCannotDegrade):
		// The body does not carry what the scope is made of, and a guessed scope would give one
		// version of one entity a second id. So this waits for hydration, exactly as a provider
		// with no degraded path does.
		return nil, false, fmt.Errorf("hydrate: %w (and the body cannot be degraded: %w)", hydrateErr, degradeErr)
	default:
		return nil, false, fmt.Errorf("%w: degrade: %w", ErrDeadLetter, degradeErr)
	}
}

// Prepare is the database half: it checks every record against the tenant of the row being
// drained, decides what the ledger says about each one, links the supersede chain forward only,
// writes the ledger rows, and masks what is left.
//
// tx must be bound to tenant (store.TenantTx). tenant is the tenant of the OUTBOX ROW being
// drained, and it is the one thing here that does not come from the delivery's content.
//
// Nothing is delivered from inside this function: the caller commits the transaction first, so a
// crash can only ever repeat a delivery and never lose one.
func (p *Pipeline) Prepare(ctx context.Context, tx pgx.Tx, tenant tenancy.ID, n Normalized) (Prepared, error) {
	if tx == nil {
		return Prepared{}, errors.New("pipeline: Prepare needs a transaction")
	}
	if _, err := tenancy.Parse(tenant.String()); err != nil {
		return Prepared{}, fmt.Errorf("%w: %w", ErrDeadLetter, err)
	}
	for _, r := range n.records {
		// The tenant and the record meet again here for the first time since Seal, and this is
		// the only check in the program that can notice a record going out under the wrong one.
		if !r.SealedFor(tenant) {
			return Prepared{}, fmt.Errorf("%w: %w: record %s", ErrDeadLetter, ErrNotSealedForTenant, r.ID)
		}
		if len(r.ExternalID) > MaxExternalIDBytes {
			return Prepared{}, fmt.Errorf("%w: %w: %d bytes", ErrDeadLetter, ErrExternalIDTooLong, len(r.ExternalID))
		}
	}
	l := ledger{q: queriesOn(tx), tenant: tenant, provider: n.provider}
	if err := l.lockEntities(ctx, n.records); err != nil {
		return Prepared{}, err
	}
	out := Prepared{Automation: n.Automation, Degraded: n.Degraded}
	for _, r := range n.records {
		linked, what, err := l.admit(ctx, r)
		if err != nil {
			return Prepared{}, err
		}
		switch what {
		case admitPrepare:
			out.Records = append(out.Records, linked)
		case admitSkip:
			out.Skipped++
		case admitStale:
			out.Stale++
		}
	}
	if err := maskRecords(ctx, queriesOn(tx), tenant, out.Records); err != nil {
		// Masking is the last thing that happens to a record, and everything that can go wrong in
		// it is about the record and not about the database: a field that no longer fits once the
		// placeholders are in, or a value the map could not take. Retrying the same bytes gives
		// the same answer, so this is a dead letter and the fix is on the normalizer.
		if errors.Is(err, ErrMaskedTooLong) {
			return Prepared{}, fmt.Errorf("%w: %w", ErrDeadLetter, err)
		}
		return Prepared{}, err
	}
	return out, nil
}
