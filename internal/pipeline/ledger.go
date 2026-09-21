package pipeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/gablooge/lawang/internal/pipeline/pipelinedb"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// queriesOn is the typed queries over one transaction.
func queriesOn(tx pgx.Tx) *pipelinedb.Queries { return pipelinedb.New(tx) }

// admission is what the ledger decided about one record.
type admission uint8

const (
	// admitPrepare: a record nobody has seen, linked to its entity's newest record and written to
	// the ledger. It goes to the sink.
	admitPrepare admission = iota
	// admitSkip: an id the ledger already holds, in a shape that says nothing is wrong.
	admitSkip
	// admitStale: a record older than what the entity already has prepared.
	admitStale
)

// ledger is the ledger stage for one tenant and one provider, over one transaction.
type ledger struct {
	q        *pipelinedb.Queries
	tenant   tenancy.ID
	provider string
}

// lockEntities takes the advisory lock of every entity in the delivery, once each, in a sorted
// order.
//
// Sorted, because a delivery can carry several entities (a comment and the task it hangs under),
// and two transactions that took the same two locks in opposite orders would deadlock. Sorting
// gives every transaction in the program one order, so they can only ever queue.
func (l ledger) lockEntities(ctx context.Context, recs []record.Record) error {
	for _, k := range entityKeys(recs) {
		if err := l.q.LockEntity(ctx, pipelinedb.LockEntityParams{
			TenantID: l.tenant.String(), Provider: l.provider, ExternalID: k,
		}); err != nil {
			return fmt.Errorf("pipeline: lock the entity: %w", err)
		}
	}
	return nil
}

// entityKeys is the entities of a delivery, each once, in the order their locks are taken.
//
// The order is what matters and it is why this is a function of its own: two transactions that
// took the locks of the same two entities in opposite orders would deadlock, and one sorted order
// for every caller in the program is what turns that into queueing. Sorting also makes
// slices.Compact drop every repeat, so a delivery that carries two versions of one entity, or an
// entity twice, locks it once.
func entityKeys(recs []record.Record) []string {
	keys := make([]string, 0, len(recs))
	for _, r := range recs {
		keys = append(keys, r.ExternalID)
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// admit decides what happens to one record, and writes the ledger row when it is to be delivered.
//
// The order of the questions is ADR 4, decision 7, and each one rules out a shape the next must
// not be asked about:
//
//  1. Is this id already prepared? If not, it is a new record and only the chain's direction is
//     left to settle.
//  2. Is it still its entity's newest record? A re-drain of the newest record, a backfill that
//     overlaps the live feed and a provider that sent one version twice all stop here, as a skip.
//  3. Is the newest record in a different scope? A late re-send of an old version is not, because
//     a record hydrated at drain time carries the scope the entity is in now. What is left is an
//     entity that moved back into a scope it already had, or a stale record built from a webhook
//     body in the scope the entity has left. Those want opposite treatments and the ledger cannot
//     tell them apart, so it dead-letters rather than guess, which is the whole point of the rule:
//     the alternative is skipping, and skipping leaves the record at the sink in the scope the
//     entity has left, with nothing reported.
//
// The rule also catches A, B, C, B. It cannot see delete-then-restore with an unchanged version
// (the head's scope is the same, so question 3 says no), which fails closed and which only the
// normalizer's rule protects; v0.1 sends no deletes, and the item that ships them settles it.
func (l ledger) admit(ctx context.Context, r record.Record) (record.Record, admission, error) {
	head, haveHead, err := l.head(ctx, r.ExternalID)
	if err != nil {
		return record.Record{}, 0, err
	}
	known, isHead, err := l.known(ctx, r.ID)
	if err != nil {
		return record.Record{}, 0, err
	}
	if known {
		// ADR 4 decision 7's three conditions, written as the ADR states them so that the two can
		// be read against each other. "known" is the first.
		//
		// The second, !isHead, is EQUIVALENT to what follows it and is here for the reading. When
		// this record is its entity's head, the head row and this row are the same row, so the
		// third condition compares a scope with itself and is false anyway: a record id hashes the
		// external id, so a ledger row with this id can belong to no other entity. Taking !isHead
		// out changes no answer, and a mutation of it survives every test in this package. It stays
		// because the rule it implements is stated as three conditions and a reader has to be able
		// to check the code against the document line by line.
		if !isHead && haveHead && head.Scope != r.Visibility.Scope {
			return record.Record{}, 0, fmt.Errorf(
				"%w: %w: tenant %s, provider %s, entity %s, prepared in scope %s while the entity's newest record is %s in scope %s",
				ErrDeadLetter, ErrScopeReturned,
				l.tenant, l.provider, r.ExternalID, r.Visibility.Scope, head.RecordID, head.Scope)
		}
		// A re-drain of the newest record, a backfill that overlaps the live feed, or a late
		// re-send of an older version whose entity has not moved. The sink already has this record,
		// under this very id.
		return record.Record{}, admitSkip, nil
	}

	// A record nobody has prepared. The only question left is which way the link points.
	//
	// The chain is ordered by the provider's version, which a normalizer promises never goes
	// backwards for one external id (ADR 4). Two records of one entity that carry the SAME
	// version are ordered by arrival instead, and arrival order is a usable order because the
	// outbox delivers one entity's versions one at a time, in queue order (ADR 10, ADR 11): the
	// second of them is the move that decision 7 is about, and it supersedes the first.
	if haveHead && versionIsOlder(r.Version, head.Version) {
		return record.Record{}, admitStale, nil
	}
	supersedes := pgtype.Text{}
	if haveHead {
		supersedes = pgtype.Text{String: head.RecordID, Valid: true}
		if err := l.q.DemoteEntityHead(ctx, pipelinedb.DemoteEntityHeadParams{
			TenantID: l.tenant.String(), Provider: l.provider, ExternalID: r.ExternalID,
		}); err != nil {
			return record.Record{}, 0, fmt.Errorf("pipeline: demote the entity head: %w", err)
		}
		r.Supersedes = record.Ref(head.RecordID)
	}
	if err := l.q.InsertLedgerEntry(ctx, pipelinedb.InsertLedgerEntryParams{
		TenantID:   l.tenant.String(),
		RecordID:   r.ID,
		Provider:   l.provider,
		ExternalID: r.ExternalID,
		Version:    r.Version,
		Scope:      r.Visibility.Scope,
		Supersedes: supersedes,
	}); err != nil {
		return record.Record{}, 0, fmt.Errorf("pipeline: write the ledger row: %w", err)
	}
	return r, admitPrepare, nil
}

// versionIsOlder reports whether v is behind the entity's newest prepared version.
//
// A version is opaque to a sink, but not to this stage: ADR 4 makes "monotonic per external_id" a
// promise the provider's normalizer makes to the pipeline, and this is the stage that uses it.
// What "monotonic" means for a string the format calls opaque is decided in ADR 12: byte order,
// which is the only order an opaque string has. A normalizer therefore spells a version so that
// byte order is version order (a fixed-width counter, an epoch in milliseconds, an RFC 3339
// timestamp), and never as a bare decimal counter, where "10" sorts before "9".
//
// The cost of being wrong in each direction is why the comparison is here at all. If a late old
// version were allowed to take the head, it would supersede a newer record at the sink, and with
// it the scope that access is decided on, which is the failure this whole item exists to prevent.
// If a normalizer's versions are not in byte order, a genuinely newer record is held back instead,
// which shows up as a Stale count that is not zero while the sink stays behind. That is the
// residual risk, and it is recorded in ADR 12.
func versionIsOlder(v, head string) bool { return strings.Compare(v, head) < 0 }

// head is the entity's newest prepared record, if it has one.
func (l ledger) head(ctx context.Context, externalID string) (pipelinedb.EntityHeadRow, bool, error) {
	row, err := l.q.EntityHead(ctx, pipelinedb.EntityHeadParams{
		TenantID: l.tenant.String(), Provider: l.provider, ExternalID: externalID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return pipelinedb.EntityHeadRow{}, false, nil
	case err != nil:
		return pipelinedb.EntityHeadRow{}, false, fmt.Errorf("pipeline: read the entity head: %w", err)
	}
	return row, true, nil
}

// known reports whether the ledger already holds this record id, and whether that row is still its
// entity's newest.
func (l ledger) known(ctx context.Context, recordID string) (known, isHead bool, err error) {
	row, err := l.q.LedgerEntry(ctx, pipelinedb.LedgerEntryParams{
		TenantID: l.tenant.String(), RecordID: recordID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, false, nil
	case err != nil:
		return false, false, fmt.Errorf("pipeline: read the ledger: %w", err)
	}
	return true, row.IsHead, nil
}
