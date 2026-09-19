// Package outbox is Sluiceway's only queue: a Postgres table whose row states are the retry and
// dead-letter machinery.
//
// A row moves pending -> prepared -> delivered, or to dead. A claim is a lease on the HEAD of an
// ordering key (the unfinished row with the lowest seq for one source entity), so the versions of
// an entity deliver one at a time, in queue order, while different entities proceed in parallel.
//
// Being the head is a stored column, is_head, and not something the claim works out: the claim
// walks an index of due heads and stops at its batch, so a poll costs what it returns, however
// much is waiting behind the heads, in backoff, or leased to other workers. The record of that
// decision, and the full correctness argument, is docs/adr/0010-outbox-head-marker.md. In brief:
//
//   - Safety is the database's. A unique index allows one head per key, a CHECK says a head is
//     unfinished, and the claim re-checks is_head on the row it has locked. An unfinished row keeps
//     the marker until it finishes. So two rows of one key are never in flight together, whatever
//     the callers do.
//   - Liveness and order are the writers'. Every writer that changes which rows of a key are
//     unfinished (Accept, Replay, MarkDelivered, MarkDead) first takes a transaction-scoped
//     advisory lock on the key. Within one key they therefore run one after the other: a lower seq
//     always commits first, a new row knows whether it is the head, and the row that finishes the
//     head hands the marker to the next one, with no window in which the two can miss each other.
//
// Queue order is arrival order, with one exception: a replayed dead letter goes to the back.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/sluiceway/internal/ids"
	"github.com/gablooge/sluiceway/internal/outbox/outboxdb"
	"github.com/gablooge/sluiceway/internal/store"
	"github.com/gablooge/sluiceway/internal/tenancy"
)

// Row states.
const (
	StatePending   = "pending"
	StatePrepared  = "prepared"
	StateDelivered = "delivered"
	StateDead      = "dead"
)

// maxErrorLen bounds what is kept of an error message.
const maxErrorLen = 1000

// MaxOrderingKeyLen is the longest ordering key Accept takes, in bytes. An entity id is a few dozen
// bytes. The bound exists because the key is a column of two indexes, and a btree tuple cannot
// exceed about 2700 bytes: without it, whether a long key can be stored depends on how well it
// compresses. The table has a CHECK for the same number.
const MaxOrderingKeyLen = 512

// MaxBatch is the most rows one Claim leases, whatever it is asked for.
const MaxBatch = 1000

// ErrLeaseLost reports a transition by a worker that no longer holds the row: its lease ran out
// and another worker took over, or the row already finished. The caller must stop working on it.
var ErrLeaseLost = errors.New("outbox: lease lost")

// ErrNotFound reports a row that does not exist for this tenant.
var ErrNotFound = errors.New("outbox: not found")

// ErrBadOrderingKey reports a delivery whose ordering key is not 1 to MaxOrderingKeyLen bytes of
// valid UTF-8 with no NUL byte. (Postgres stores neither a NUL nor invalid UTF-8 in text, and a NUL
// is one JSON escape away from any sender: encoding/json decodes the escape for U+0000 inside an
// entity id into that byte.) Such a key can never be stored, however often it is sent again, so a
// caller that answers a provider must treat it as poison (park it, answer 2xx) and not as a
// failure to retry.
var ErrBadOrderingKey = errors.New("outbox: ordering key must be 1 to 512 bytes of valid UTF-8 with no NUL")

// errBadProvider is deliberately not exported: the provider is the connector's own name for
// itself, a constant of this program and nothing a sender chooses, so a bad one is a bug to fix
// and not a case for a caller to handle. It is still refused here, by name, because the
// alternative is the database's "invalid byte sequence", which names nothing.
var errBadProvider = errors.New("outbox: provider must be valid UTF-8 with no NUL")

// Outbox reads and writes the outbox table.
type Outbox struct {
	db *store.DB
}

// New returns an Outbox on db.
func New(db *store.DB) *Outbox { return &Outbox{db: db} }

// Delivery is one accepted webhook, or one change synthesized by reconciliation.
type Delivery struct {
	// Provider is the internal provider key, and it must be a constant of the program: a key
	// from the provider registry, never text taken from a request (not a path segment, a header
	// or a payload field). Accept refuses a provider that cannot be stored with an error that is
	// deliberately NOT a sentinel, because a bad provider is a bug in the caller, not poison from
	// a sender. A caller that passed request text here would turn a crafted request into a
	// server error, and a provider into a retry storm. Look the provider up in the registry
	// first, and answer an unknown one before reaching the outbox.
	Provider string
	// OrderingKey names the source entity. Rows sharing a key deliver in arrival order.
	OrderingKey string
	// RawBody is stored, and hashed into the delivery id, exactly as received.
	RawBody []byte
}

// Claimed is a leased row, and the only thing that can move it on. It deliberately carries no
// payload: claiming runs across tenants, and the payload is only ever read bound to Tenant.
//
// It can be read and not changed or built: only Claim makes one that any transition accepts. The
// tenant in particular is the one the row was claimed under, because every transition binds
// row-level security to it. (A Claimed aimed at another tenant would change nothing, since that
// tenant cannot see the row. It should not be possible to write one by accident either.)
type Claimed struct {
	id      string
	tenant  tenancy.ID
	attempt int
	token   string
}

// ID is the row's id.
func (c Claimed) ID() string { return c.id }

// Tenant is the tenant that owns the row. Bind to it before reading the payload.
func (c Claimed) Tenant() tenancy.ID { return c.tenant }

// Attempt counts the claims of this row, starting at 1.
func (c Claimed) Attempt() int { return c.attempt }

// Row is a full outbox row, as its own tenant sees it.
type Row = outboxdb.GetRow

// Accept stores a delivery for the tenant that owns the verified subscription. fresh is false for
// a delivery this tenant already has, which makes a provider's re-send a no-op.
//
// Two Accepts for one ordering key run one after the other: the second waits until the first has
// committed, and so does an Accept that meets a MarkDelivered, MarkDead or Replay of its key. See
// LockOrderingKey in queries.sql. The new row is claimable at once if its entity has nothing
// unfinished, and otherwise when the rows ahead of it have finished.
func (o *Outbox) Accept(ctx context.Context, tenant tenancy.ID, d Delivery) (id string, fresh bool, err error) {
	err = o.db.TenantTx(ctx, tenant, func(tx pgx.Tx) (err error) {
		id, fresh, err = acceptIn(ctx, tx, tenant, d)
		return err
	})
	if err != nil {
		return "", false, err
	}
	return id, fresh, nil
}

// acceptIn is Accept inside a transaction already bound to tenant.
func acceptIn(ctx context.Context, tx pgx.Tx, tenant tenancy.ID, d Delivery) (id string, fresh bool, err error) {
	if d.OrderingKey == "" || len(d.OrderingKey) > MaxOrderingKeyLen || !storable(d.OrderingKey) {
		// The length is reported, the key is not, and neither is the byte that is wrong with it:
		// all of it is derived from what a sender sent.
		return "", false, fmt.Errorf("%w, got %d bytes", ErrBadOrderingKey, len(d.OrderingKey))
	}
	deliveryID, err := ids.DeliveryID(d.Provider, d.RawBody) // refuses an empty provider
	if err != nil {
		return "", false, err
	}
	if !storable(d.Provider) {
		return "", false, errBadProvider
	}
	q := outboxdb.New(tx)
	// Before the INSERT, which assigns seq and decides whether the row is the head of its key, and
	// as a statement of its own.
	err = q.LockOrderingKey(ctx, outboxdb.LockOrderingKeyParams{TenantID: tenant.String(), OrderingKey: d.OrderingKey})
	if err != nil {
		return "", false, fmt.Errorf("outbox: accept: %w", err)
	}
	id = ids.New()
	_, err = q.Accept(ctx, outboxdb.AcceptParams{
		ID:          id,
		TenantID:    tenant.String(),
		Provider:    d.Provider,
		DeliveryID:  deliveryID,
		OrderingKey: d.OrderingKey,
		RawBody:     d.RawBody,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows): // ON CONFLICT DO NOTHING returned nothing
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("outbox: accept: %w", err)
	}
	return id, true, nil
}

// Claim leases up to batch rows for the given duration, across tenants, as the worker role. Rows
// locked by a concurrent claimer are skipped, never waited for.
//
// It never leases more than MaxBatch rows: a larger batch is cut to that, silently, so a caller
// that sizes anything by its batch should not ask for more. A batch or a lease that is not
// positive is refused. (A lease of zero would hand out rows that are claimable again at once, and
// every poll would use up one attempt of the ladder with no delivery ever tried.)
func (o *Outbox) Claim(ctx context.Context, batch int, lease time.Duration) ([]Claimed, error) {
	if batch <= 0 || lease <= 0 {
		return nil, errors.New("outbox: claim needs a positive batch size and lease")
	}
	token := ids.New()
	var claimed []Claimed
	err := o.db.RoleTx(ctx, store.RoleWorker, func(tx pgx.Tx) error {
		rows, err := outboxdb.New(tx).Claim(ctx, outboxdb.ClaimParams{
			LeaseSeconds: lease.Seconds(),
			LeaseToken:   token,
			BatchSize:    int32(min(batch, MaxBatch)), //nolint:gosec // bounded just here
		})
		if err != nil {
			return err
		}
		claimed = make([]Claimed, 0, len(rows))
		for _, r := range rows {
			tenant, err := tenancy.Parse(r.TenantID)
			if err != nil {
				return err
			}
			claimed = append(claimed, Claimed{id: r.ID, tenant: tenant, attempt: int(r.Attempts), token: token})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("outbox: claim: %w", err)
	}
	return claimed, nil
}

// Get returns a row as its tenant sees it.
func (o *Outbox) Get(ctx context.Context, tenant tenancy.ID, id string) (Row, error) {
	if !storable(id) {
		return Row{}, ErrNotFound // no row has such an id, and the database would not say so
	}
	var row Row
	err := o.db.TenantTx(ctx, tenant, func(tx pgx.Tx) (err error) {
		row, err = outboxdb.New(tx).Get(ctx, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, ErrNotFound
	}
	return row, err
}

// MarkPrepared records that the ledger rows and prepared records are committed. After this, a
// crash re-drains into delivery, not into preparing again.
func (o *Outbox) MarkPrepared(ctx context.Context, c Claimed) error {
	return o.transition(ctx, c, func(tx pgx.Tx) error {
		return held(outboxdb.New(tx).MarkPrepared(ctx, outboxdb.MarkPreparedParams{ID: c.id, LeaseToken: c.token}))
	})
}

// MarkDelivered finishes the row, and makes the next version of its entity claimable.
func (o *Outbox) MarkDelivered(ctx context.Context, c Claimed) error {
	return o.transition(ctx, c, func(tx pgx.Tx) error { return markDeliveredIn(ctx, tx, c) })
}

// markDeliveredIn is MarkDelivered inside a transaction already bound to the row's tenant.
func markDeliveredIn(ctx context.Context, tx pgx.Tx, c Claimed) error {
	return finishIn(ctx, tx, c, func(q *outboxdb.Queries) (string, error) {
		return q.MarkDelivered(ctx, outboxdb.MarkDeliveredParams{ID: c.id, LeaseToken: c.token})
	})
}

// finishIn runs one of the two transitions that finish a row, and hands the head marker on. The
// order is fixed, and each step is a statement of its own:
//
//  1. The ordering key's lock, as in Accept. It comes before any row lock, here as everywhere, so
//     that no two transactions ever wait for each other.
//  2. The lease-guarded state change, which also gives up the marker. No row means the lease was
//     lost: nothing changed, and nothing is promoted.
//  3. The promotion of the key's next unfinished row. Its snapshot is taken after step 1, so it
//     sees every row of the key an earlier lock holder committed, and no Accept or Replay of the
//     key is open while it runs. Without the lock, an Accept that saw this row unfinished would
//     insert a non-head that this statement cannot see yet, and that row would wait forever.
func finishIn(ctx context.Context, tx pgx.Tx, c Claimed, finish func(*outboxdb.Queries) (orderingKey string, err error)) error {
	q := outboxdb.New(tx)
	if err := q.LockOrderingKeyOf(ctx, c.id); err != nil {
		return err
	}
	key, err := finish(q)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrLeaseLost
	case err != nil:
		return err
	}
	return q.PromoteNextHead(ctx, outboxdb.PromoteNextHeadParams{TenantID: c.tenant.String(), OrderingKey: key})
}

// Why a row is dead, as stored in dead_reason. Fixed texts: what went wrong is the Cause.
const (
	reasonRetriesExhausted = "retries exhausted"
	reasonNotRetryable     = "not retryable"
)

// Fail handles a retryable failure: it schedules the next attempt from the ladder, or parks the
// row as a dead letter once the ladder is used up. What went wrong is a Cause and never an error
// string: see Cause for why.
func (o *Outbox) Fail(ctx context.Context, c Claimed, ladder Ladder, cause Cause) error {
	delay, ok := ladder.Next(c.attempt)
	if !ok {
		return o.markDead(ctx, c, reasonRetriesExhausted, cause)
	}
	return o.transition(ctx, c, func(tx pgx.Tx) error {
		return held(outboxdb.New(tx).Retry(ctx, outboxdb.RetryParams{
			ID: c.id, LeaseToken: c.token, DelaySeconds: delay.Seconds(), LastError: clip(cause.String()),
		}))
	})
}

// MarkDead parks the row as a dead letter, for failures that retrying cannot fix. A dead row no
// longer holds back newer versions of its entity: the next one becomes claimable. Like Fail, it
// takes a Cause and no text.
func (o *Outbox) MarkDead(ctx context.Context, c Claimed, cause Cause) error {
	return o.markDead(ctx, c, reasonNotRetryable, cause)
}

func (o *Outbox) markDead(ctx context.Context, c Claimed, reason string, cause Cause) error {
	return o.transition(ctx, c, func(tx pgx.Tx) error {
		return finishIn(ctx, tx, c, func(q *outboxdb.Queries) (string, error) {
			return q.MarkDead(ctx, outboxdb.MarkDeadParams{
				ID: c.id, LeaseToken: c.token, DeadReason: reason, LastError: clip(cause.String()),
			})
		})
	})
}

// Replay makes a dead letter claimable again, at the BACK of its entity's queue: a newer version
// may be in flight right now, and the replayed row must not be leased alongside it. It is
// delivered after every version accepted before the replay, as a late arrival of an old version.
// A row that died after it was prepared comes back prepared, and is not prepared again.
func (o *Outbox) Replay(ctx context.Context, tenant tenancy.ID, id string) error {
	if !storable(id) {
		return ErrNotFound // as in Get
	}
	var n int64
	err := o.db.TenantTx(ctx, tenant, func(tx pgx.Tx) (err error) {
		q := outboxdb.New(tx)
		// Before the UPDATE, which assigns the new seq and decides whether the row is the head of
		// its key, and as a statement of its own.
		if err := q.LockOrderingKeyOf(ctx, id); err != nil {
			return err
		}
		n, err = q.Replay(ctx, id)
		return err
	})
	if err != nil {
		return fmt.Errorf("outbox: replay: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// StrandedKeys returns ordering keys of the tenant that have unfinished rows and no head, in key
// order, at most limit of them and never more than MaxBatch. A limit that is not positive is
// refused. An empty result is the healthy state.
//
// A stranded key is the one way the head marker can be broken without anybody noticing: nothing
// of the key is ever claimed again, and no error is raised anywhere. No writer of this package
// leaves a key like that (docs/adr/0010-outbox-head-marker.md has the argument, and the statement
// that repairs one), so a key returned here was written behind the package's back, or by a bug.
// This is detection only, for a sweep or an operator: nothing in the delivery path calls it. It
// costs the tenant's unfinished rows, not the size of the table (tens of milliseconds for a
// backlog of 100,000), so it is for a sweep that runs now and then, not for every poll.
//
// It is scoped to a tenant because it needs the state and the ordering key of a row, which only
// the row's tenant may read. The worker role, which sees across tenants, sees neither.
func (o *Outbox) StrandedKeys(ctx context.Context, tenant tenancy.ID, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, errors.New("outbox: stranded keys needs a positive limit")
	}
	var keys []string
	err := o.db.TenantTx(ctx, tenant, func(tx pgx.Tx) (err error) {
		keys, err = outboxdb.New(tx).StrandedKeys(ctx, outboxdb.StrandedKeysParams{
			TenantID: tenant.String(),
			MaxKeys:  int32(min(limit, MaxBatch)), //nolint:gosec // bounded just here
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("outbox: stranded keys: %w", err)
	}
	return keys, nil
}

// transition runs one lease-guarded state change bound to the row's own tenant. change returns
// ErrLeaseLost when the guard matched no row, and only then: a database or context error is never
// reported as a lost lease, because the two tell a worker opposite things (stop working on the
// row, or try the transition again while the lease lasts).
func (o *Outbox) transition(ctx context.Context, c Claimed, change func(pgx.Tx) error) error {
	if c.token == "" {
		return ErrLeaseLost
	}
	err := o.db.TenantTx(ctx, c.tenant, change)
	switch {
	case errors.Is(err, ErrLeaseLost):
		return ErrLeaseLost
	case err != nil:
		return fmt.Errorf("outbox: %w", err)
	}
	return nil
}

// held turns the row count of a lease-guarded UPDATE into ErrLeaseLost when it matched nothing.
func held(rows int64, err error) error {
	if err == nil && rows == 0 {
		return ErrLeaseLost
	}
	return err
}

// storable reports whether Postgres takes s as a text value at all: it refuses a NUL byte and
// anything that is not valid UTF-8 with SQLSTATE 22021, which a caller cannot tell from an outage.
// Every text that reaches a statement from outside this package is asked this first.
func storable(s string) bool {
	return utf8.ValidString(s) && strings.IndexByte(s, 0) < 0
}

// clip makes s storable and bounded: valid UTF-8 with no NUL (Postgres rejects both in text, and a
// failure that cannot be recorded is never backed off or parked), and at most maxErrorLen bytes,
// cut between runes.
func clip(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.ReplaceAll(s, "\x00", "�")
	if len(s) <= maxErrorLen {
		return s
	}
	cut := maxErrorLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
