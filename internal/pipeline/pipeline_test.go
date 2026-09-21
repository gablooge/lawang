package pipeline_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gablooge/lawang/internal/pipeline"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/record"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The two containers the move tests use. A record's scope is built from its container, so moving
// an entity between them is what "the entity moved to another scope" means here.
const (
	listA = "L1"
	listB = "L2"
	listC = "L3"
)

const entity = "fake:task:1"

// TestALedgeredIDIsSkipped is the third "Done when" line. The same delivery twice is the ordinary
// shape of it: a worker that crashed after preparing and before marking the row delivered, a
// backfill that overlaps the live feed, a provider that sent one version twice.
func TestALedgeredIDIsSkipped(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	first := e.mustDrain(tenantA, ev(entity, "1", listA))
	if len(first.Records) != 1 || first.Skipped != 0 {
		t.Fatalf("the first delivery prepared %d records and skipped %d, want 1 and 0", len(first.Records), first.Skipped)
	}
	again := e.mustDrain(tenantA, ev(entity, "1", listA))
	if len(again.Records) != 0 || again.Skipped != 1 {
		t.Errorf("the repeat prepared %d records and skipped %d, want 0 and 1", len(again.Records), again.Skipped)
	}
	if rows := e.ledgerRows(); len(rows) != 1 {
		t.Errorf("the ledger holds %d rows, want one: %+v", len(rows), rows)
	}
}

// TestTheSameIDForTwoTenantsIsNotASkip. The record id is salted with the tenant, so two tenants
// that connect one workspace have different ids for one entity, and the ledger is per tenant
// besides. Without this a second tenant's first record would look like a repeat.
func TestTheSameIDForTwoTenantsIsNotASkip(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	a := e.mustDrain(tenantA, ev(entity, "1", listA))
	b := e.mustDrain(tenantB, ev(entity, "1", listA))
	if len(a.Records) != 1 || len(b.Records) != 1 {
		t.Fatalf("two tenants prepared %d and %d records, want one each", len(a.Records), len(b.Records))
	}
	if a.Records[0].ID == b.Records[0].ID {
		t.Errorf("two tenants got one record id: %s", a.Records[0].ID)
	}
}

// TestANewVersionSupersedesTheOneBeforeIt is the ordinary case the chain exists for.
func TestANewVersionSupersedesTheOneBeforeIt(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	v1 := e.mustDrain(tenantA, ev(entity, "1", listA)).Records[0]
	v2 := e.mustDrain(tenantA, ev(entity, "2", listA)).Records[0]
	if got := string(v2.Supersedes); got != v1.ID {
		t.Errorf("version 2 supersedes %q, want version 1 (%s)", got, v1.ID)
	}
	if v1.Supersedes != "" {
		t.Errorf("the first record of an entity supersedes %q, want nothing", v1.Supersedes)
	}
	wantHead(t, e.ledgerRows(), v2.ID)
}

// TestAnOldVersionArrivingAfterANewerOneNeverSupersedesIt is the FIRST "Done when" line, in the
// shape where the old version has been prepared before: v1, v2, then v1 again.
//
// The repeat is skipped, and what matters as much is that its ledger row is untouched: it still
// supersedes nothing, so nothing in the chain ever came to point backwards.
func TestAnOldVersionArrivingAfterANewerOneNeverSupersedesIt(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	v1 := e.mustDrain(tenantA, ev(entity, "1", listA)).Records[0]
	v2 := e.mustDrain(tenantA, ev(entity, "2", listA)).Records[0]

	again := e.mustDrain(tenantA, ev(entity, "1", listA))
	if len(again.Records) != 0 || again.Skipped != 1 {
		t.Fatalf("the old version prepared %d records and skipped %d, want 0 and 1", len(again.Records), again.Skipped)
	}
	rows := e.ledgerRows()
	wantHead(t, rows, v2.ID)
	wantForwardOnly(t, rows)
	for _, r := range rows {
		if r.recordID == v1.ID && r.supersedes != "" {
			t.Errorf("the old version now supersedes %q; a link must never be repointed", r.supersedes)
		}
	}
}

// TestAnOlderVersionNeverSeenBeforeIsHeldBack is the FIRST "Done when" line in its other shape,
// and the one the outbox produces on purpose: a dead letter replayed after a newer version has
// already gone out (architecture 3.2, "Replay goes to the back").
//
// The record is new to the ledger, so nothing stops it becoming the head except this rule. If it
// did, it would supersede the newer record at the sink and take the scope of the older one with
// it, which is the failure the whole item is here to prevent.
func TestAnOlderVersionNeverSeenBeforeIsHeldBack(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	v2 := e.mustDrain(tenantA, ev(entity, "2", listA)).Records[0]

	late := e.mustDrain(tenantA, ev(entity, "1", listA))
	if len(late.Records) != 0 || late.Stale != 1 {
		t.Fatalf("the late old version prepared %d records and counted %d stale, want 0 and 1", len(late.Records), late.Stale)
	}
	rows := e.ledgerRows()
	if len(rows) != 1 {
		t.Fatalf("the ledger holds %d rows, want only the newer version: %+v", len(rows), rows)
	}
	wantHead(t, rows, v2.ID)
	// It is held back and NOT ledgered, so the same version arriving legitimately later (after
	// retention has pruned the newer one, say) is not mistaken for something already delivered.
	if rows[0].version != "2" {
		t.Errorf("the ledger holds version %q, want only version 2", rows[0].version)
	}
}

// TestTheVersionOrderWorksInBothDirections is the test the round 1 review asked for, and the
// reason the version rule is not plain byte order any more.
//
// ADR 4 makes "monotonic per external_id" a promise the normalizer makes to this stage, and ADR 12
// decision 1 says what a pipeline may read from that promise. It used to say "byte order", and
// byte order puts "9" AFTER "10", which is the commonest spelling a real provider uses. In the
// forward direction that only holds a newer record back, which is visible and safe. In the other
// direction, the one this test exists for, an OLDER record supersedes a newer one, becomes the
// head, and takes the scope that access is decided on with it, with nothing counted and nothing
// raised. Both directions are here, because only one of them was.
func TestTheVersionOrderWorksInBothDirections(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})

	// Zero padded, where byte order and number order agree.
	e.mustDrain(tenantA, ev(entity, "009", listA))
	if out := e.mustDrain(tenantA, ev(entity, "010", listA)); len(out.Records) != 1 {
		t.Fatalf("a padded version 10 after 9 prepared %d records, want one", len(out.Records))
	}

	// Unpadded, forward: 9 then 10. Byte order calls the newer record stale; a number does not.
	forward := "fake:task:forward"
	e.mustDrain(tenantA, ev(forward, "9", listA))
	up := e.mustDrain(tenantA, ev(forward, "10", listA))
	if len(up.Records) != 1 || up.Stale != 0 {
		t.Errorf("an unpadded version 10 after 9 prepared %d records and counted %d stale, want 1 and 0: "+
			"a decimal counter is ordered by its number", len(up.Records), up.Stale)
	}

	// Unpadded, backward: 10 then 9, which is the dangerous direction and the one that was
	// untested. The older record must be held back, not made the head.
	backward := "fake:task:backward"
	newer := e.mustDrain(tenantA, ev(backward, "10", listA)).Records[0]
	down := e.mustDrain(tenantA, ev(backward, "9", listA))
	if len(down.Records) != 0 || down.Stale != 1 {
		t.Fatalf("an unpadded version 9 after 10 prepared %d records and counted %d stale, want 0 and 1: "+
			"an older record took the head", len(down.Records), down.Stale)
	}
	wantHead(t, entityRows(e.ledgerRows(), backward), newer.ID)
}

// TestAReplayedDeadLetterNeverTakesTheHeadOrItsScope is the same rule in the shape the architecture
// produces on purpose, and the exact reproduction the round 1 review posted.
//
// A dead letter replayed after a newer version has gone out (architecture 3.2, "Replay goes to the
// back"), or a backfill running beside the live feed, arrives as an older version in a DIFFERENT
// scope. Byte order said "9" is newer than "10", so the older record superseded the newer one,
// became the head, and the scope access is decided on went from L1 back to L2, silently. What has
// to be true is that the sink's live version and the scope on it both stay where the newer record
// put them.
func TestAReplayedDeadLetterNeverTakesTheHeadOrItsScope(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	newer := e.mustDrain(tenantA, ev(entity, "10", listA)).Records[0]

	replay := e.mustDrain(tenantA, ev(entity, "9", listB))
	if len(replay.Records) != 0 || replay.Stale != 1 {
		t.Fatalf("the replayed older version prepared %d records and counted %d stale, want 0 and 1", len(replay.Records), replay.Stale)
	}
	rows := e.ledgerRows()
	if len(rows) != 1 {
		t.Fatalf("the ledger holds %d rows, want only the newer version: %+v", len(rows), rows)
	}
	wantHead(t, rows, newer.ID)
	if rows[0].version != "10" {
		t.Errorf("the head is version %q, want 10", rows[0].version)
	}
	if rows[0].scope != newer.Visibility.Scope {
		t.Errorf("the head's scope is %q, want the newer record's %q: an older record took the scope "+
			"that access is decided on", rows[0].scope, newer.Visibility.Scope)
	}
}

// TestAnotherTenantsLedgerAndRedactionMapAreInvisible is the behaviour behind the two policies
// migration 00004 adds, which until review round 2 nothing pinned at all.
//
// TestEveryPolicySaysWhatTheDesignSaysItSays in internal/store reads the predicates out of the
// catalog; this is the other half, because a predicate that reads right and does nothing would
// pass that. redaction_map is the one that matters most: it holds by construction the personal
// data the masker took out of records, so "one tenant cannot read another tenant's values" has to
// be a property of the database and not of every query in every package remembering to filter.
//
// The rows of the other tenant are planted as the superuser, so this tests the policy and not the
// pipeline's own habits: the pipeline would never write them under this transaction in the first
// place.
func TestAnotherTenantsLedgerAndRedactionMapAreInvisible(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	mine := ev(entity, "1", listA)
	mine.Text = "write to jane.doe@example.test"
	e.mustDrain(tenantA, mine)

	e.exec(`INSERT INTO lawang.record_ledger
	          (tenant_id, record_id, provider, external_id, version, scope, is_head)
	        VALUES ($1, 'rec_000000000000000000000000000000ff', 'fake', 'fake:task:theirs', '1', 'fake:list:L9', true)`,
		tenantB.String())
	e.exec(`INSERT INTO lawang.redaction_map (tenant_id, token, kind, value)
	        VALUES ($1, '[email:01KTHEIRS0000000000000000]', 'email', 'john.roe@example.test')`,
		tenantB.String())

	err := e.db.TenantTx(e.ctx, tenantA, func(tx pgx.Tx) error {
		for _, q := range []struct{ what, sql string }{
			{"record_ledger", "SELECT tenant_id FROM record_ledger"},
			{"redaction_map", "SELECT tenant_id FROM redaction_map"},
		} {
			rows, err := tx.Query(e.ctx, q.sql)
			if err != nil {
				return err
			}
			seen, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			if len(seen) != 1 || seen[0] != tenantA.String() {
				t.Errorf("tenant_a reads %v from %s, want only its own row: the policy is not isolating "+
					"a table that holds another tenant's data", seen, q.what)
			}
		}
		// A write aimed at the other tenant's rows touches nothing, silently, which is what a
		// USING predicate does.
		for _, stmt := range []string{
			`UPDATE record_ledger SET is_head = false WHERE tenant_id = 'tenant_b'`,
			`DELETE FROM record_ledger WHERE tenant_id = 'tenant_b'`,
			`UPDATE redaction_map SET value = 'stolen' WHERE tenant_id = 'tenant_b'`,
			`DELETE FROM redaction_map WHERE tenant_id = 'tenant_b'`,
		} {
			tag, err := tx.Exec(e.ctx, stmt)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("%q touched %d rows of another tenant", stmt, tag.RowsAffected())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading under tenant_a: %v", err)
	}

	// And planting a row FOR the other tenant is an error rather than a silent no-op, which is
	// what the WITH CHECK half is for.
	for _, stmt := range []struct{ what, sql string }{
		{"a ledger row", `INSERT INTO record_ledger
		    (tenant_id, record_id, provider, external_id, version, scope, is_head)
		  VALUES ('tenant_b', 'rec_0000000000000000000000000000ee', 'fake', 'fake:task:planted', '1', 'fake:list:L9', true)`},
		{"a redaction mapping", `INSERT INTO redaction_map (tenant_id, token, kind, value)
		  VALUES ('tenant_b', '[email:01KPLANTED000000000000000]', 'email', 'planted@example.test')`},
	} {
		err := e.db.TenantTx(e.ctx, tenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, stmt.sql)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("tenant_a writing %s for tenant_b gave %v, want a row-level security violation (42501)",
				stmt.what, err)
		}
	}

	// The other tenant's rows are exactly as they were planted.
	if rows := e.redactionRows(); len(rows) != 2 {
		t.Errorf("the map holds %d rows, want the two that were written: %+v", len(rows), rows)
	}
	if rows := e.ledgerRows(); len(rows) != 2 {
		t.Errorf("the ledger holds %d rows, want the two that were written: %+v", len(rows), rows)
	}
}

// TestABase64CounterIsOrderedByWhatItDecodesTo is the round 2 review's reproduction, which the
// rule above did not cover and could not have covered.
//
// Two versions in a fixed width encoding are always the same length, so a rule that read an order
// out of "the same length" answered every such pair with a guess, and for base64 the guess is
// backwards: the alphabet puts 'z' at value 51 and '0' at value 52, while ASCII puts 'z' at 122
// and '0' at 48. A Microsoft Graph changeKey and an Exchange ETag are exactly that, which B13,
// B16 and B17 all depend on, and the reviewer drained the older "CQAAABYAAABz" after the newer
// "CQAAABYAAAB0" and watched it supersede the newer record and take its scope, with no error and
// nothing counted.
//
// The order is no longer read from the strings. The provider declares it (provider.VersionOrder,
// refused at registration when it is absent), and for base64 the pipeline decodes both versions
// and compares the bytes, so the alphabet cannot mislead it. Both directions are here, because a
// stage that called everything stale would pass the first half on its own.
func TestABase64CounterIsOrderedByWhatItDecodesTo(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{}, fake.NewOrdering(fake.DefaultKey, provider.VersionOrderBase64))
	const (
		older  = "CQAAABYAAABz" // decodes one below the next, and sorts ABOVE it in ASCII
		newer  = "CQAAABYAAAB0"
		newest = "CQAAABYAAAB1"
	)
	head := e.mustDrain(tenantA, ev(entity, newer, listA)).Records[0]

	replay := e.mustDrain(tenantA, ev(entity, older, listB))
	if len(replay.Records) != 0 || replay.Stale != 1 {
		t.Fatalf("the older changeKey prepared %d records and counted %d stale, want 0 and 1", len(replay.Records), replay.Stale)
	}
	rows := e.ledgerRows()
	if len(rows) != 1 {
		t.Fatalf("the ledger holds %d rows, want only the newer version: %+v", len(rows), rows)
	}
	wantHead(t, rows, head.ID)
	if rows[0].version != newer || rows[0].scope != head.Visibility.Scope {
		t.Fatalf("the head is version %q in scope %q, want %q in %q: an older changeKey took the head "+
			"and the scope access is decided on", rows[0].version, rows[0].scope, newer, head.Visibility.Scope)
	}

	// And forward. A stage that answered "stale" to everything would have passed the half above.
	moved := e.mustDrain(tenantA, ev(entity, newest, listB))
	if len(moved.Records) != 1 || moved.Stale != 0 {
		t.Fatalf("the newest changeKey prepared %d records and counted %d stale, want 1 and 0", len(moved.Records), moved.Stale)
	}
	if moved.Records[0].Supersedes != record.Ref(head.ID) {
		t.Errorf("the newest record supersedes %q, want the previous head %q", moved.Records[0].Supersedes, head.ID)
	}
}

// TestADeliveryWhoseProviderDeclaresNoVersionOrderIsRefused is the other end of the same rule.
//
// A provider that declares nothing cannot register (TestNewRegistryRefusesAProviderWithNoVersionOrder
// in internal/provider), so this is the case that gets past that: a Normalized that did not come
// from Normalize, which is what a later item wiring its own worker could build. The zero value
// orders nothing, and nothing is what it does here: no record is prepared and nothing is written.
func TestADeliveryWhoseProviderDeclaresNoVersionOrderIsRefused(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	n, err := normalizeFor(e, ev(entity, "1", listA))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	out, err := e.prepare(tenantA, pipeline.NormalizedFor(fake.DefaultKey, provider.VersionOrderUnset, pipeline.RecordsOf(n)))
	if !errors.Is(err, pipeline.ErrVersionNotComparable) {
		t.Fatalf("a delivery with no declared version order gave %v, want ErrVersionNotComparable", err)
	}
	if !errors.Is(err, pipeline.ErrDeadLetter) {
		t.Errorf("err = %v, want a dead letter: the declaration will be just as absent next time", err)
	}
	if len(out.Records) != 0 {
		t.Errorf("a refused delivery returned %d records", len(out.Records))
	}
	if rows := e.ledgerRows(); len(rows) != 0 {
		t.Errorf("the ledger holds %d rows after a refusal: %+v", len(rows), rows)
	}
}

// TestTwoVersionsThatCannotBeOrderedAreRefused is the other half of the new rule, and the reason
// it can be safe at all.
//
// A version that does not fit the spelling its provider declared carries no order anybody can
// read: here a provider that declares decimal counters sends "r9" and "r10". Guessing one is how
// the failure above happens, so the stage refuses instead: the head does not move, nothing is
// delivered, the delivery is a dead letter naming both versions and the declared order, and the
// count says it happened.
func TestTwoVersionsThatCannotBeOrderedAreRefused(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	first := e.mustDrain(tenantA, ev(entity, "r9", listA)).Records[0]

	out, err := e.drain(tenantA, ev(entity, "r10", listB))
	if !errors.Is(err, pipeline.ErrVersionNotComparable) {
		t.Fatalf("draining a version no order can be read from returned %v, want ErrVersionNotComparable", err)
	}
	if !errors.Is(err, pipeline.ErrDeadLetter) {
		t.Errorf("err = %v, want a dead letter: the same two strings will be just as unorderable next time", err)
	}
	if out.VersionUnordered != 1 {
		t.Errorf("the refusal counted %d, want 1: ADR 12 asks for the refusal and the count", out.VersionUnordered)
	}
	if len(out.Records) != 0 {
		t.Errorf("a failed Prepare returned %d records; nothing it decided may reach a sink", len(out.Records))
	}
	// The transaction rolled back, so the ledger is what it was before the refusal.
	rows := e.ledgerRows()
	if len(rows) != 1 {
		t.Fatalf("the ledger holds %d rows, want only the first: %+v", len(rows), rows)
	}
	wantHead(t, rows, first.ID)
}

// entityRows narrows a ledger dump to one entity, for the tests that use several in one database.
func entityRows(rows []ledgerRow, externalID string) []ledgerRow {
	var out []ledgerRow
	for _, r := range rows {
		if r.externalID == externalID {
			out = append(out, r)
		}
	}
	return out
}

// TestAMoveWithAnUnchangedVersionSupersedesWhatItWas. Two records of one entity with the SAME
// provider version and different scopes: the entity moved. There is nothing to order them by but
// arrival, which the outbox makes a real order (one entity's versions share an ordering key and
// only the head of a key is claimable, ADR 10 and ADR 11), so the second supersedes the first and
// the sink replaces what it had.
func TestAMoveWithAnUnchangedVersionSupersedesWhatItWas(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	inA := e.mustDrain(tenantA, ev(entity, "1", listA)).Records[0]
	inB := e.mustDrain(tenantA, ev(entity, "1", listB)).Records[0]

	if inA.ID == inB.ID {
		t.Fatal("the move did not change the record id, so the scope is not in the hash")
	}
	if got := string(inB.Supersedes); got != inA.ID {
		t.Errorf("the moved record supersedes %q, want the record it was (%s)", got, inA.ID)
	}
	if inA.Visibility.Scope == inB.Visibility.Scope {
		t.Fatal("the two records share a scope, so this is not a move")
	}
	wantHead(t, e.ledgerRows(), inB.ID)
}

// TestTheChainIsPerEntityAndNotPerScope. Both records above are one chain, keyed by the entity, so
// the sink is told to replace what it held. A chain kept per scope would leave the old copy in A
// with nothing pointing at it, and A's members would go on reading it.
func TestTheChainIsPerEntityAndNotPerScope(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	e.mustDrain(tenantA, ev(entity, "1", listA))
	e.mustDrain(tenantA, ev(entity, "1", listB))

	rows := e.ledgerRows()
	if len(rows) != 2 {
		t.Fatalf("the ledger holds %d rows, want two: %+v", len(rows), rows)
	}
	heads := 0
	for _, r := range rows {
		if r.externalID != entity {
			t.Errorf("a row is keyed by %q, want the entity %q", r.externalID, entity)
		}
		if r.isHead {
			heads++
		}
	}
	if heads != 1 {
		t.Errorf("the entity has %d heads, want exactly one: %+v", heads, rows)
	}
}

// TestAnEntityThatMovesBackIsDeadLetteredAndNotSkipped is ADR 4, decision 7, and the requirement
// carried onto this item from the review of B05.
//
// A moves to B and back to A while the provider's version never changes. The third record
// reproduces the FIRST record's id. A ledger that only asked "have I delivered this id" would skip
// it, the sink would keep the record in B, B's members would go on reading what they lost access
// to, A's members would never get it back, and nothing would report an error.
func TestAnEntityThatMovesBackIsDeadLetteredAndNotSkipped(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	first := e.mustDrain(tenantA, ev(entity, "1", listA)).Records[0]
	moved := e.mustDrain(tenantA, ev(entity, "1", listB)).Records[0]

	out, err := e.drain(tenantA, ev(entity, "1", listA))
	if !errors.Is(err, pipeline.ErrScopeReturned) {
		t.Fatalf("the move back gave %v, want ErrScopeReturned", err)
	}
	if !errors.Is(err, pipeline.ErrDeadLetter) {
		t.Errorf("the move back is not marked as a dead letter: %v", err)
	}
	// ADR 4 decision 7 asks for the dead letter AND a count, because the dead letter tells an
	// operator about one delivery and the count tells them whether it is happening at all. The
	// count travels on Prepared beside the error, so a caller that dead-letters the row still logs
	// it and B25 still adds it to a metric.
	if out.ScopeReturned != 1 {
		t.Errorf("the move back counted %d on Prepared, want 1: ADR 4 decision 7 asks for the count as well "+
			"as the reason, and a Prepared{} beside the error loses it", out.ScopeReturned)
	}
	if len(out.Records) != 0 {
		t.Errorf("a failed Prepare returned %d records; the transaction rolled back, so nothing it "+
			"decided may reach a sink", len(out.Records))
	}
	// The dead letter has to name what an operator looks at: the entity, and the two scopes.
	for _, want := range []string{entity, first.Visibility.Scope, moved.Visibility.Scope, string(tenantA)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the dead letter does not name %q: %v", want, err)
		}
	}
	// Nothing was written and nothing was delivered: the transaction rolled back with the error.
	rows := e.ledgerRows()
	if len(rows) != 2 {
		t.Fatalf("the ledger holds %d rows, want the two from before the move back: %+v", len(rows), rows)
	}
	wantHead(t, rows, moved.ID)
}

// TestADeliveryThatDiesHalfwayStillReportsWhatItDecided. Prepare used to return Prepared{} beside
// every error, so a delivery that refused one record threw away everything it had decided about
// the others: the bot noise it dropped, the records the ledger already held, and the count ADR 4
// decision 7 asks for by name. A caller logs one struct per delivery, and an empty one says the
// delivery did nothing, which is false.
func TestADeliveryThatDiesHalfwayStillReportsWhatItDecided(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	const other = "fake:task:2"
	e.mustDrain(tenantA, ev(entity, "1", listA))
	e.mustDrain(tenantA, ev(other, "1", listA))
	e.mustDrain(tenantA, ev(other, "1", listB))

	bot := ev("fake:task:bot", "1", listA)
	bot.Automation = true
	// In order: one the gate drops, one the ledger already holds, and one that moved back.
	out, err := e.drain(tenantA, bot, ev(entity, "1", listA), ev(other, "1", listA))
	if !errors.Is(err, pipeline.ErrScopeReturned) {
		t.Fatalf("the delivery gave %v, want ErrScopeReturned from its third record", err)
	}
	if out.Automation != 1 || out.Skipped != 1 || out.ScopeReturned != 1 {
		t.Errorf("the failed delivery reported %+v, want one dropped, one skipped and one scope returned", out)
	}
	if len(out.Records) != 0 {
		t.Errorf("the failed delivery returned %d records, want none", len(out.Records))
	}
}

// TestTheMoveBackRuleAlsoCatchesABCB. The rule is about the head's scope and not about the scope
// two steps back, so an entity that visits a third container and returns to the second is caught
// in the same way.
func TestTheMoveBackRuleAlsoCatchesABCB(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	e.mustDrain(tenantA, ev(entity, "1", listA))
	e.mustDrain(tenantA, ev(entity, "1", listB))
	e.mustDrain(tenantA, ev(entity, "1", listC))

	if _, err := e.drain(tenantA, ev(entity, "1", listB)); !errors.Is(err, pipeline.ErrScopeReturned) {
		t.Fatalf("A, B, C, B gave %v, want ErrScopeReturned", err)
	}
}

// TestAMoveBackWhoseFirstRecordWasPrunedIsAnOrdinarySupersede is the other half the review asked
// for. Retention (B25) prunes ledger rows that are not the head of their entity, and when the
// first record of the A, B, A story is gone the third record is simply a record nobody has seen:
// it supersedes the head and becomes the new head, which is exactly right and leaves the sink
// holding the entity in A again.
//
// It also says what retention may NOT prune: a head. The pruning here is the shape B25 must keep.
func TestAMoveBackWhoseFirstRecordWasPrunedIsAnOrdinarySupersede(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	first := e.mustDrain(tenantA, ev(entity, "1", listA)).Records[0]
	moved := e.mustDrain(tenantA, ev(entity, "1", listB)).Records[0]

	// Stand in for the retention sweep of B25: it deletes rows that are not their entity's head.
	e.exec(`DELETE FROM lawang.record_ledger WHERE NOT is_head`)

	back := e.mustDrain(tenantA, ev(entity, "1", listA))
	if len(back.Records) != 1 {
		t.Fatalf("the move back prepared %d records, want one: %+v", len(back.Records), back)
	}
	if got := back.Records[0].ID; got != first.ID {
		t.Errorf("the move back has id %s, want the first record's id %s again", got, first.ID)
	}
	if got := string(back.Records[0].Supersedes); got != moved.ID {
		t.Errorf("the move back supersedes %q, want the record in the other scope (%s)", got, moved.ID)
	}
	wantHead(t, e.ledgerRows(), first.ID)
}

// TestARecordSealedForAnotherTenantIsRefused. The tenant is in no field of the envelope, so a
// record sealed for tenant A marshals identically when it is delivered under tenant B and no sink
// can notice. record.SealedFor can, and this is the one place in the program that asks it.
//
// A false there is a bug of ours or an attack, never something that comes right on a retry, so it
// is a dead letter and not a failure for the ladder.
func TestARecordSealedForAnotherTenantIsRefused(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	n, err := normalizeFor(e, ev(entity, "1", listA))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if _, err := e.prepare(tenantB, n); !errors.Is(err, pipeline.ErrNotSealedForTenant) {
		t.Fatalf("preparing tenant A's record under tenant B gave %v, want ErrNotSealedForTenant", err)
	} else if !errors.Is(err, pipeline.ErrDeadLetter) {
		t.Errorf("a record under the wrong tenant is not marked as a dead letter: %v", err)
	}
	if rows := e.ledgerRows(); len(rows) != 0 {
		t.Errorf("the ledger holds %d rows, want none: %+v", len(rows), rows)
	}
}

// TestAnExternalIDTooLongToKeyAChainIsRefusedByName. The record format bounds an external id in
// characters and the ledger has to bound it in bytes, because it is a column of a btree index.
// The refusal is by name, here, rather than the database's "index row size exceeds maximum".
func TestAnExternalIDTooLongToKeyAChainIsRefusedByName(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	// 1,019 characters that are three bytes each, which the format allows and the index cannot
	// hold. Nothing a real provider mints looks like this; the point is that the bound is ours.
	long := fake.DefaultKey + ":" + strings.Repeat("中", 1019)
	scope, err := record.ScopeID(fake.DefaultKey, fake.ContainerKind, listA)
	if err != nil {
		t.Fatalf("ScopeID: %v", err)
	}
	r, err := record.Record{
		Op: record.OpUpsert, Kind: record.KindTask, ExternalID: long, Version: "1",
		OccurredAt: occurred, Title: "a task",
		Container:  record.Container{Kind: fake.ContainerKind, ID: listA},
		Visibility: record.Visibility{Scope: scope, Audience: record.AudienceGroup},
	}.Seal(fake.DefaultKey, tenantA)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if len(r.ExternalID) <= pipeline.MaxExternalIDBytes {
		t.Fatalf("the fixture external id is %d bytes, which the ledger can hold", len(r.ExternalID))
	}
	n := pipeline.NormalizedFor(fake.DefaultKey, provider.VersionOrderDecimal, []record.Record{r})
	// This guard runs before the ledger exists, so it is one of the paths ADR 12 decision 4 is
	// about: the counters the stage had already reached come back beside the refusal.
	n.Automation = 2
	out, err := e.prepare(tenantA, n)
	if !errors.Is(err, pipeline.ErrExternalIDTooLong) {
		t.Fatalf("a %d byte external id gave %v, want ErrExternalIDTooLong", len(r.ExternalID), err)
	}
	if !errors.Is(err, pipeline.ErrDeadLetter) {
		t.Errorf("an external id that can never be stored is not marked as a dead letter: %v", err)
	}
	if out.Automation != 2 {
		t.Errorf("Prepare reported Automation %d beside the refusal, want 2", out.Automation)
	}
}

// TestTheAutomationGateDropsBotNoise, and TestTheAutomationGateCanBeTurnedOff below it. A dropped
// record never reaches the ledger, so a deployment that changes its mind gets those entities again
// from their next change and not the history in between, which is what the option's doc promises.
func TestTheAutomationGateDropsBotNoise(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	bot := ev("fake:task:bot", "1", listA)
	bot.Automation = true

	out := e.mustDrain(tenantA, bot, ev(entity, "1", listA))
	if len(out.Records) != 1 || out.Automation != 1 {
		t.Fatalf("prepared %d records and dropped %d, want 1 and 1", len(out.Records), out.Automation)
	}
	if out.Records[0].ExternalID != entity {
		t.Errorf("the record that survived is %q, want the one a person wrote", out.Records[0].ExternalID)
	}
	if rows := e.ledgerRows(); len(rows) != 1 {
		t.Errorf("the ledger holds %d rows, want only the record that was delivered: %+v", len(rows), rows)
	}
}

func TestTheAutomationGateCanBeTurnedOff(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{Automation: pipeline.KeepAutomation})
	bot := ev("fake:task:bot", "1", listA)
	bot.Automation = true

	out := e.mustDrain(tenantA, bot)
	if len(out.Records) != 1 || out.Automation != 0 {
		t.Fatalf("prepared %d records and dropped %d, want 1 and 0", len(out.Records), out.Automation)
	}
	if !out.Records[0].Origin.Automation {
		t.Error("the record no longer says it was written by an automation")
	}
}

// TestADegradedRecordHasTheIDTheHydratedOneWouldHave is the rule ADR 4 decision 7 puts on the
// degraded path, and the reason it is a rule: if the two paths derived different scopes, one
// version of one entity would get two ids, be delivered twice, and look like a move to the ledger.
//
// The test is the ledger's own answer. The same event is drained twice, once with the API up and
// once with it down, and the second is skipped, which can only happen if the two ids agree.
func TestADegradedRecordHasTheIDTheHydratedOneWouldHave(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	hydrated := e.mustDrain(tenantA, ev(entity, "1", listA))

	down := ev(entity, "1", listA)
	down.Hydrate = fake.HydrateFail
	degraded, err := normalizeFor(e, down)
	if err != nil {
		t.Fatalf("the degraded path failed: %v", err)
	}
	if degraded.Degraded != 1 {
		t.Errorf("the delivery counted %d degraded records, want 1", degraded.Degraded)
	}
	recs := pipeline.RecordsOf(degraded)
	if len(recs) != 1 {
		t.Fatalf("the degraded delivery produced %d records, want one", len(recs))
	}
	if recs[0].ID != hydrated.Records[0].ID {
		t.Fatalf("the degraded record has id %s and the hydrated one %s: one version of one entity got two ids",
			recs[0].ID, hydrated.Records[0].ID)
	}
	out, err := e.prepare(tenantA, degraded)
	if err != nil {
		t.Fatalf("prepare the degraded record: %v", err)
	}
	if out.Skipped != 1 || len(out.Records) != 0 {
		t.Errorf("the degraded record prepared %d records and skipped %d, want 0 and 1", len(out.Records), out.Skipped)
	}
}

// TestADeliveryThatCannotBeDegradedIsRetriedAndNotGuessed. The webhook body does not say which
// container the entity is in, so there is nothing to build a scope from. A guessed scope would be
// a second id for one version, so the delivery waits for hydration instead: a plain error, which
// the caller gives to the retry ladder, and never a dead letter.
func TestADeliveryThatCannotBeDegradedIsRetriedAndNotGuessed(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	down := ev(entity, "1", fake.UnknownContainer)
	down.Hydrate = fake.HydrateFail

	_, err := e.drain(tenantA, down)
	if err == nil {
		t.Fatal("a change that can be neither hydrated nor degraded was prepared")
	}
	if errors.Is(err, pipeline.ErrDeadLetter) {
		t.Errorf("a provider API that is down was treated as a dead letter: %v", err)
	}
	if !errors.Is(err, fake.ErrHydrateUnavailable) {
		t.Errorf("the error does not name the hydration failure: %v", err)
	}
	if rows := e.ledgerRows(); len(rows) != 0 {
		t.Errorf("the ledger holds %d rows, want none: %+v", len(rows), rows)
	}
}

// TestAProviderWithNoDegradedPathWaitsForItsAPI. Degrader is optional, and a provider that does
// not implement it must not have its changes dropped or dead-lettered: they wait, exactly as a
// change that cannot be degraded does.
func TestAProviderWithNoDegradedPathWaitsForItsAPI(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{}, neverDegrades{fake.New(fake.DefaultKey)})
	down := ev(entity, "1", listA)
	down.Hydrate = fake.HydrateFail

	_, err := e.drain(tenantA, down)
	if err == nil {
		t.Fatal("a hydration failure with no degraded path was prepared anyway")
	}
	if errors.Is(err, pipeline.ErrDeadLetter) {
		t.Errorf("a provider API that is down was treated as a dead letter: %v", err)
	}
}

// TestADeliveryForAProviderThatIsNotAWebhookSourceIsADeadLetter. Those bytes cannot be read by
// anything, now or later.
func TestADeliveryForAProviderThatIsNotAWebhookSourceIsADeadLetter(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{}, apiOnly{fake.New(fake.DefaultKey)})

	if _, err := e.drain(tenantA, ev(entity, "1", listA)); !errors.Is(err, pipeline.ErrNotAWebhookSource) {
		t.Fatalf("got %v, want ErrNotAWebhookSource", err)
	}
}

// TestADeliveryForAnUnknownProviderIsADeadLetter. The provider of an outbox row is a constant of
// this program, so this is a deployment that dropped a provider while its rows were queued.
func TestADeliveryForAnUnknownProviderIsADeadLetter(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	n, err := e.p.Normalize(e.ctx, pipeline.Delivery{
		Tenant: tenantA, Provider: "gone", ID: "d1", Body: body(t, ev(entity, "1", listA)),
	})
	if !errors.Is(err, pipeline.ErrUnknownProvider) || !errors.Is(err, pipeline.ErrDeadLetter) {
		t.Fatalf("got %v (%+v), want ErrUnknownProvider and ErrDeadLetter", err, n)
	}
}

// TestABodyThatCannotBeParsedIsADeadLetter. The bytes are already stored, so a second attempt
// reads the same bytes and fails the same way.
func TestABodyThatCannotBeParsedIsADeadLetter(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	_, err := e.p.Normalize(e.ctx, pipeline.Delivery{
		Tenant: tenantA, Provider: fake.DefaultKey, ID: "d1", Body: []byte(`{"type":"nonsense"}`),
	})
	if !errors.Is(err, pipeline.ErrDeadLetter) {
		t.Fatalf("got %v, want a dead letter", err)
	}
}

// TestNormalizeRefusesAnEmptyTenant. Fail closed: a record sealed for no tenant is a record no
// sink can safely be given, and an empty tenant must never mint an id.
func TestNormalizeRefusesAnEmptyTenant(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	_, err := e.p.Normalize(e.ctx, pipeline.Delivery{
		Provider: fake.DefaultKey, ID: "d1", Body: body(t, ev(entity, "1", listA)),
	})
	if !errors.Is(err, pipeline.ErrDeadLetter) {
		t.Fatalf("an empty tenant gave %v, want a dead letter", err)
	}
}

// TestAPreparedRecordStillMarshals. Everything this stage does to a record after record.Seal
// (setting Supersedes, masking the title and the text, stamping meta.delivery) is allowed to
// happen after sealing, and the seal's guard is strict about what is not. If the stage ever
// reassigned something the id stands for, this is where it would show, rather than as a dead
// letter at a strict sink.
func TestAPreparedRecordStillMarshals(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	e.mustDrain(tenantA, ev(entity, "1", listA))
	withText := ev(entity, "2", listA)
	withText.Text = "write to jane.doe@example.test"

	out := e.mustDrain(tenantA, withText)
	if len(out.Records) != 1 {
		t.Fatalf("prepared %d records, want one", len(out.Records))
	}
	doc, err := json.Marshal(out.Records[0])
	if err != nil {
		t.Fatalf("a prepared record does not marshal: %v", err)
	}
	if !strings.Contains(string(doc), `"supersedes":"rec_`) {
		t.Errorf("the marshalled record carries no supersede link: %s", doc)
	}
	if strings.Contains(string(doc), "jane.doe@example.test") {
		t.Errorf("the marshalled record still carries an address: %s", doc)
	}
}

// TestTheDeliveryIDTravelsWithTheRecord, so that a support question about one record can be traced
// back to the delivery it came from.
func TestTheDeliveryIDTravelsWithTheRecord(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	n, err := e.p.Normalize(e.ctx, pipeline.Delivery{
		Tenant: tenantA, Provider: fake.DefaultKey, ID: "01JDELIVERY0000000000000001",
		Body: body(t, ev(entity, "1", listA)),
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	out, err := e.prepare(tenantA, n)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := out.Records[0].Meta.Delivery; got != "01JDELIVERY0000000000000001" {
		t.Errorf("meta.delivery is %q, want the outbox row's id", got)
	}
}

// TestTwoWritersOfOneEntityLeaveOneChain. The outbox already delivers one entity's versions one at
// a time, but not every path into this stage goes through one ordering key (two subscriptions of
// one tenant, a reconciliation pass beside the live feed), so the stage takes the entity's
// advisory lock rather than rely on that.
//
// Without the lock the two transactions both read the same head, and the second demotes what the
// first inserted: two records would supersede one, and one would be orphaned in the middle of the
// chain with no constraint violated.
func TestTwoWritersOfOneEntityLeaveOneChain(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	e.mustDrain(tenantA, ev(entity, "1", listA))

	// Six moves of one entity, racing. Every one carries the same provider version and a container
	// of its own, so every one is a new record that must supersede whatever the head is by then:
	// no arrival order can make any of them stale, and none reproduces another's id. What is left
	// for the test to see is the chain itself.
	const writers = 6
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.drain(tenantA, ev(entity, "1", "M"+string(rune('a'+i))))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	rows := e.ledgerRows()
	if len(rows) != writers+1 {
		t.Fatalf("the ledger holds %d rows, want %d: %+v", len(rows), writers+1, rows)
	}
	wantOneChain(t, rows)
}

// wantHead fails unless exactly one row is the head and it is the record it should be.
func wantHead(t *testing.T, rows []ledgerRow, recordID string) {
	t.Helper()
	var heads []string
	for _, r := range rows {
		if r.isHead {
			heads = append(heads, r.recordID)
		}
	}
	if len(heads) != 1 || heads[0] != recordID {
		t.Errorf("the heads are %v, want exactly %s", heads, recordID)
	}
}

// wantForwardOnly fails when any row supersedes a record that was prepared after it. The rows come
// back in the order they were prepared, so "after it" is a position in the slice.
func wantForwardOnly(t *testing.T, rows []ledgerRow) {
	t.Helper()
	at := map[string]int{}
	for i, r := range rows {
		at[r.recordID] = i
	}
	for i, r := range rows {
		if r.supersedes == "" {
			continue
		}
		j, ok := at[r.supersedes]
		if !ok {
			t.Errorf("%s supersedes %s, which is not in the ledger", r.recordID, r.supersedes)
			continue
		}
		if j > i {
			t.Errorf("%s (prepared %d) supersedes %s (prepared %d), which is newer", r.recordID, i, r.supersedes, j)
		}
	}
}

// wantOneChain fails unless the rows form a single path from the one record that supersedes
// nothing to the one head, visiting every row exactly once.
//
// It walks the links rather than comparing timestamps, because prepared_at is the transaction's
// start and concurrent transactions do not commit in that order. A broken chain shows here as a
// record superseded twice, a second root, or a walk that does not reach every row.
func wantOneChain(t *testing.T, rows []ledgerRow) {
	t.Helper()
	next := map[string]ledgerRow{} // the record that supersedes this one
	var root string
	roots := 0
	for _, r := range rows {
		if r.supersedes == "" {
			root, roots = r.recordID, roots+1
			continue
		}
		if by, twice := next[r.supersedes]; twice {
			t.Fatalf("%s is superseded by both %s and %s", r.supersedes, by.recordID, r.recordID)
		}
		next[r.supersedes] = r
	}
	if roots != 1 {
		t.Fatalf("the chain has %d records that supersede nothing, want one: %+v", roots, rows)
	}
	at, seen := root, 1
	for {
		r, more := next[at]
		if !more {
			break
		}
		at, seen = r.recordID, seen+1
		if seen > len(rows) {
			t.Fatalf("the chain loops: %+v", rows)
		}
	}
	if seen != len(rows) {
		t.Errorf("the chain from %s covers %d of %d rows: %+v", root, seen, len(rows), rows)
	}
	wantHead(t, rows, at)
}
