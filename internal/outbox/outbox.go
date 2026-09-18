// Package outbox is Sluiceway's only queue: a Postgres table whose row states are the retry and
// dead-letter machinery.
//
// A row moves pending -> prepared -> delivered, or to dead. A claim is a lease on the HEAD of an
// ordering key (the unfinished row with the lowest seq for one source entity), so the versions of
// an entity deliver one at a time, in queue order, while different entities proceed in parallel.
//
// Queue order is arrival order, with one exception: a replayed dead letter goes to the back. Every
// writer that gives a row its place (Accept, Replay) first takes a transaction-scoped advisory
// lock on the ordering key, so within one key a lower seq always commits first and a claim can
// never see a row before the rows ahead of it.
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

// ErrLeaseLost reports a transition by a worker that no longer holds the row: its lease ran out
// and another worker took over, or the row already finished. The caller must stop working on it.
var ErrLeaseLost = errors.New("outbox: lease lost")

// ErrNotFound reports a row that does not exist for this tenant.
var ErrNotFound = errors.New("outbox: not found")

// Outbox reads and writes the outbox table.
type Outbox struct {
	db *store.DB
}

// New returns an Outbox on db.
func New(db *store.DB) *Outbox { return &Outbox{db: db} }

// Delivery is one accepted webhook, or one change synthesized by reconciliation.
type Delivery struct {
	Provider string
	// OrderingKey names the source entity. Rows sharing a key deliver in arrival order.
	OrderingKey string
	// RawBody is stored, and hashed into the delivery id, exactly as received.
	RawBody []byte
}

// Claimed is a leased row. It deliberately carries no payload: claiming runs across tenants, and
// the payload is only ever read bound to Tenant.
type Claimed struct {
	ID     string
	Tenant tenancy.ID
	// Attempt counts claims of this row, starting at 1.
	Attempt int
	token   string
}

// Row is a full outbox row, as its own tenant sees it.
type Row = outboxdb.GetRow

// Accept stores a delivery for the tenant that owns the verified subscription. fresh is false for
// a delivery this tenant already has, which makes a provider's re-send a no-op.
//
// Two Accepts for one ordering key run one after the other: the second waits until the first has
// committed. See LockOrderingKey in queries.sql.
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
	if d.OrderingKey == "" {
		return "", false, errors.New("outbox: empty ordering key")
	}
	deliveryID, err := ids.DeliveryID(d.Provider, d.RawBody)
	if err != nil {
		return "", false, err
	}
	q := outboxdb.New(tx)
	// Before the INSERT, which is what assigns seq, and as a statement of its own.
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
			BatchSize:    int32(min(batch, 1000)), //nolint:gosec // bounded just here
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
			claimed = append(claimed, Claimed{ID: r.ID, Tenant: tenant, Attempt: int(r.Attempts), token: token})
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
	return o.transition(ctx, c, func(q *outboxdb.Queries) (int64, error) {
		return q.MarkPrepared(ctx, outboxdb.MarkPreparedParams{ID: c.ID, LeaseToken: c.token})
	})
}

// MarkDelivered finishes the row.
func (o *Outbox) MarkDelivered(ctx context.Context, c Claimed) error {
	return o.transition(ctx, c, func(q *outboxdb.Queries) (int64, error) {
		return q.MarkDelivered(ctx, outboxdb.MarkDeliveredParams{ID: c.ID, LeaseToken: c.token})
	})
}

// Fail handles a retryable failure: it schedules the next attempt from the ladder, or parks the
// row as a dead letter once the ladder is used up. cause must never contain token material.
func (o *Outbox) Fail(ctx context.Context, c Claimed, ladder Ladder, cause string) error {
	delay, ok := ladder.Next(c.Attempt)
	if !ok {
		return o.MarkDead(ctx, c, "retries exhausted", cause)
	}
	return o.transition(ctx, c, func(q *outboxdb.Queries) (int64, error) {
		return q.Retry(ctx, outboxdb.RetryParams{
			ID: c.ID, LeaseToken: c.token, DelaySeconds: delay.Seconds(), LastError: clip(cause),
		})
	})
}

// MarkDead parks the row as a dead letter, for failures that retrying cannot fix. A dead row no
// longer holds back newer versions of its entity.
func (o *Outbox) MarkDead(ctx context.Context, c Claimed, reason, cause string) error {
	return o.transition(ctx, c, func(q *outboxdb.Queries) (int64, error) {
		return q.MarkDead(ctx, outboxdb.MarkDeadParams{
			ID: c.ID, LeaseToken: c.token, DeadReason: clip(reason), LastError: clip(cause),
		})
	})
}

// Replay makes a dead letter claimable again, at the BACK of its entity's queue: a newer version
// may be in flight right now, and the replayed row must not be leased alongside it. It is
// delivered after every version accepted before the replay, as a late arrival of an old version.
// A row that died after it was prepared comes back prepared, and is not prepared again.
func (o *Outbox) Replay(ctx context.Context, tenant tenancy.ID, id string) error {
	var n int64
	err := o.db.TenantTx(ctx, tenant, func(tx pgx.Tx) (err error) {
		q := outboxdb.New(tx)
		// Before the UPDATE, which is what assigns the new seq, and as a statement of its own.
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

// transition runs one lease-guarded state change bound to the row's own tenant.
func (o *Outbox) transition(ctx context.Context, c Claimed, change func(*outboxdb.Queries) (int64, error)) error {
	if c.token == "" {
		return ErrLeaseLost
	}
	var n int64
	err := o.db.TenantTx(ctx, c.Tenant, func(tx pgx.Tx) (err error) {
		n, err = change(outboxdb.New(tx))
		return err
	})
	if err != nil {
		return fmt.Errorf("outbox: %w", err)
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
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
