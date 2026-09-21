package pipeline

import (
	"cmp"
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
	// admitNone: the ledger could not answer at all, because the database did not. It always
	// comes back with an error, and it is the zero value so that a decision is never counted by
	// accident.
	admitNone admission = iota
	// admitPrepare: a record nobody has seen, linked to its entity's newest record and written to
	// the ledger. It goes to the sink.
	admitPrepare
	// admitSkip: an id the ledger already holds, in a shape that says nothing is wrong.
	admitSkip
	// admitStale: a record older than what the entity already has prepared.
	admitStale
	// admitScopeReturned: the A, B, back to A case of ADR 4 decision 7. It comes back with
	// ErrScopeReturned, and the decision is counted as well as refused because ADR 4 asks for both.
	admitScopeReturned
	// admitVersionUnordered: two versions of one entity that carry no order this stage can read.
	// It comes back with ErrVersionNotComparable, and is counted for the same reason.
	admitVersionUnordered
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
		return record.Record{}, admitNone, err
	}
	known, isHead, err := l.known(ctx, r.ID)
	if err != nil {
		return record.Record{}, admitNone, err
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
			return record.Record{}, admitScopeReturned, fmt.Errorf(
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
	// backwards for one external id (ADR 4). What that promise means for a string the format calls
	// opaque is ADR 12 decision 1, and compareVersions is where it is read: two versions this
	// stage cannot order are refused, not guessed at, because guessing is how an older record
	// takes the head and the scope access is decided on with it.
	//
	// Two records of one entity that carry the SAME version are ordered by arrival instead: that
	// is the move decision 7 is about, and the second of them supersedes the first. Arrival is a
	// weak order (ADR 12 decision 1 says where it is not an order at all), which is why it decides
	// only this one case.
	if haveHead {
		order, ok := compareVersions(r.Version, head.Version)
		if !ok {
			return record.Record{}, admitVersionUnordered, fmt.Errorf(
				"%w: %w: tenant %s, provider %s, entity %s, the incoming version is %q and the entity's newest is %q",
				ErrDeadLetter, ErrVersionNotComparable,
				l.tenant, l.provider, r.ExternalID, r.Version, head.Version)
		}
		if order < 0 {
			return record.Record{}, admitStale, nil
		}
	}
	supersedes := pgtype.Text{}
	if haveHead {
		supersedes = pgtype.Text{String: head.RecordID, Valid: true}
		if err := l.q.DemoteEntityHead(ctx, pipelinedb.DemoteEntityHeadParams{
			TenantID: l.tenant.String(), Provider: l.provider, ExternalID: r.ExternalID,
		}); err != nil {
			return record.Record{}, admitNone, fmt.Errorf("pipeline: demote the entity head: %w", err)
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
		return record.Record{}, admitNone, fmt.Errorf("pipeline: write the ledger row: %w", err)
	}
	return r, admitPrepare, nil
}

// compareVersions orders two versions of one entity, and says whether they can be ordered at all.
//
// A version is opaque to a sink, but not to this stage: ADR 4 makes "monotonic per external_id" a
// promise the provider's normalizer makes to the pipeline, and this is the stage that uses it.
// ADR 12 decision 1 says what a pipeline may read from that promise, and this is the whole of it:
//
//  1. Two runs of decimal digits are ordered by the NUMBER they spell, whatever their length, with
//     leading zeros counting for nothing. That is what an unpadded counter means, and it is the
//     commonest spelling a real provider uses, so reading it as bytes ("9" after "10") would get
//     the common case wrong in the one direction that must never be wrong.
//  2. Two versions of EQUAL LENGTH are ordered by their bytes. Every encoding whose byte order is
//     its value order is fixed width for as long as it is in use: a ULID, an epoch in
//     milliseconds, an RFC 3339 timestamp, a zero-padded counter. For two equal-length runs of
//     digits this is the same answer as rule 1, so the two rules never disagree.
//  3. Anything else carries NO order this stage can read, and it says so (ok is false). The caller
//     dead-letters the delivery with ErrVersionNotComparable.
//
// Rule 3 is the point of the function. Guessing an order for two strings that have none is how an
// older record takes the head, superseding a newer record at the sink and taking the scope that
// access is decided on with it, which is the failure this whole item exists to prevent. The
// refusal fails closed: the head does not move, nothing is delivered, and the operator is told
// which two versions could not be ordered.
//
// The order this returns is total on the strings it accepts, so it cannot disagree with itself
// between two calls: a length comparison and a byte comparison are both total, and rule 1 falls
// back to bytes when two trimmed digit runs are the same length.
func compareVersions(v, head string) (order int, ok bool) {
	if isDecimal(v) && isDecimal(head) {
		return compareDecimal(v, head), true
	}
	if len(v) == len(head) {
		return strings.Compare(v, head), true
	}
	return 0, false
}

// compareDecimal orders two runs of decimal digits by the number they spell. Leading zeros are not
// part of a number, so "009" and "9" are one version, which arrival order then decides between.
func compareDecimal(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return cmp.Compare(len(a), len(b))
	}
	return strings.Compare(a, b)
}

// isDecimal reports whether s is one non-empty run of decimal digits and nothing else.
func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

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
