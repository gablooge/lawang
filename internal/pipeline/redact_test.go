package pipeline_test

import (
	"math/big"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/gablooge/lawang/internal/pipeline"
	"github.com/gablooge/lawang/internal/provider/fake"
)

// TestTheMaskerRunsOnWhatLeavesTheStage is the second "Done when" line where it counts: not on a
// string in a unit test but on a record that the pipeline is about to hand to a sink.
func TestTheMaskerRunsOnWhatLeavesTheStage(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	event := ev(entity, "1", listA)
	event.Title = "invoice for jane.doe@example.test"
	event.Text = "call +62 812-3456-7890 or pay GB82 WEST 1234 5698 7654 32"

	out := e.mustDrain(tenantA, event)
	if len(out.Records) != 1 {
		t.Fatalf("prepared %d records, want one", len(out.Records))
	}
	got := out.Records[0]
	for _, secret := range []string{"jane.doe@example.test", "+62 812-3456-7890", "GB82 WEST 1234 5698 7654 32"} {
		if strings.Contains(got.Title, secret) || strings.Contains(got.Text, secret) {
			t.Errorf("the prepared record still holds %q: title %q, text %q", secret, got.Title, got.Text)
		}
	}
	// What is not a secret is untouched, so the record is still worth reading.
	if !strings.HasPrefix(got.Title, "invoice for ") {
		t.Errorf("the title lost more than the address: %q", got.Title)
	}
	if !strings.HasPrefix(got.Text, "call ") || !strings.Contains(got.Text, " or pay ") {
		t.Errorf("the text lost more than the number and the account: %q", got.Text)
	}
}

// TestTheRedactionMapStaysHereAndTheTokenGoesOut. The token is in the record, the value is in the
// table, and the table's row is the only copy of the value this deployment keeps.
func TestTheRedactionMapStaysHereAndTheTokenGoesOut(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	event := ev(entity, "1", listA)
	event.Text = "write to jane.doe@example.test"

	out := e.mustDrain(tenantA, event)
	rows := e.redactionRows()
	if len(rows) != 1 {
		t.Fatalf("the redaction map holds %d rows, want one: %+v", len(rows), rows)
	}
	if rows[0].value != "jane.doe@example.test" || rows[0].kind != string(pipeline.KindEmail) {
		t.Errorf("the map holds %+v, want the address as an email", rows[0])
	}
	if rows[0].tenant != string(tenantA) {
		t.Errorf("the mapping belongs to %q, want %q", rows[0].tenant, tenantA)
	}
	if !strings.Contains(out.Records[0].Text, rows[0].token) {
		t.Errorf("the record does not carry the token %q: %q", rows[0].token, out.Records[0].Text)
	}
	// The token says nothing about the value. This one only catches a token that quotes it;
	// TestMintingOneValueTwiceGivesTwoTokens is the test of the property itself.
	if strings.Contains(rows[0].token, "jane") || strings.Contains(rows[0].token, "example") {
		t.Errorf("the token quotes the value it stands for: %q", rows[0].token)
	}
}

// TestMintingOneValueTwiceGivesTwoTokens is ADR 12 decision 5's first bullet, pinned as the
// property it actually is.
//
// THE PROPERTY, in one sentence, so that nobody weakens it by accident: two mintings of one value
// that agree in EVERY input a sink can see must still give two different tokens.
//
// That sentence is what the bullet means by "a ULID, not a hash of the value". A token any sink
// could compute from a guess would hand it an oracle: guess an address, compute its token, look
// for that token in the records it holds, and the fact masking withholds (that this address
// appears in this tenant's records) comes straight back. Salting does not close it, because a
// sink holds the salt: the tenant is its own, the kind is in the token's prefix, the record id is
// on the record, and so is meta.delivery (TestTheDeliveryIDTravelsWithTheRecord pins that).
//
// Two earlier versions of this test were weaker than the sentence, and both survived a digest that
// restored the whole oracle. Asserting that the token does not quote the value sees nothing, since
// a digest quotes nothing; sha256(tenant, kind, value) died at that point. Minting the same value
// in a SECOND delivery then let sha256(tenant, kind, value, meta.delivery) through, because the
// two mintings differed in the delivery id and the external id, and a sink holds both.
//
// So this drains one delivery, takes away the redaction_map row AND the record_ledger row as the
// superuser (the ledger row is what would otherwise make the second drain a skip), and drains the
// very same delivery again. The second minting sees the same tenant, the same delivery id, the
// same record id, the same external id, the same version, the same scope, the same kind and the
// same value. Every function of every input a sink can see therefore gives ONE answer here, and
// only a token drawn from crypto/rand gives two.
func TestMintingOneValueTwiceGivesTwoTokens(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	const (
		addr     = "jane.doe@example.test"
		delivery = "01JDELIVERYMINTEDTWICE00000"
	)
	only := ev(entity, "1", listA)
	only.Text = "write to " + addr

	first, err := e.drainDelivery(tenantA, delivery, only)
	if err != nil {
		t.Fatalf("the first drain: %v", err)
	}
	a := first.Records[0]
	rows := e.redactionRows()
	if len(rows) != 1 {
		t.Fatalf("the map holds %d rows after one delivery, want one: %+v", len(rows), rows)
	}
	before := rows[0].token

	// Take away everything the deployment remembers about that drain, which is the two rows it
	// wrote: the mapping, as B25's retention sweep will, and the ledger row, so that the same
	// record id is not simply skipped the second time. Nothing a sink could see has changed.
	e.exec(`DELETE FROM lawang.redaction_map`)
	e.exec(`DELETE FROM lawang.record_ledger`)

	second, err := e.drainDelivery(tenantA, delivery, only)
	if err != nil {
		t.Fatalf("the second drain of the same delivery: %v", err)
	}
	b := second.Records[0]
	if b.ID != a.ID || b.Meta.Delivery != a.Meta.Delivery {
		t.Fatalf("the second drain is not the same delivery: record %q then %q, delivery %q then %q: "+
			"this test proves nothing unless every input a sink can see is identical",
			a.ID, b.ID, a.Meta.Delivery, b.Meta.Delivery)
	}
	rows = e.redactionRows()
	if len(rows) != 1 {
		t.Fatalf("the map holds %d rows after the second delivery, want one: %+v", len(rows), rows)
	}
	if rows[0].token == before {
		t.Errorf("minting %q twice from identical inputs gave the same token %q: the placeholder is a "+
			"function of what the sink already holds, which is the offline oracle ADR 12 decision 5 "+
			"exists to prevent", addr, before)
	}
	if !strings.Contains(a.Text, before) || !strings.Contains(b.Text, rows[0].token) {
		t.Fatalf("a record does not carry the token its delivery minted: %q, then %q", a.Text, b.Text)
	}
	if a.Text == b.Text {
		t.Errorf("two mintings of one address produced the same masked text: %q", a.Text)
	}
}

// TestTwoTokensMintedTogetherAreNotOneStepApart is ADR 12 decision 5's placeholder again, from the
// other side: not "is it a function of its inputs" but "where does its randomness come from".
//
// ids.New's own doc comment forbids using it for anything that must be unpredictable, and says
// why: the 80 bits that are not the clock come from math/rand seeded once at process start, and
// the default ULID entropy is MONOTONIC, so two ids minted in one millisecond differ by a random
// increment of at most 2^32. From one observed token, everything minted after it in that
// millisecond lies in a window a laptop can walk. A placeholder is not a capability, so this is
// not a disclosure on its own; what it costs is that a planted placeholder can be made to collide
// with a real mapping an operator later resolves, and that the gap between two tokens says how
// many values the deployment masked in between, across tenants.
//
// The tokens come from ids.NewUnpredictable (crypto/rand) instead, and this is what tells the two
// apart. One delivery carrying two addresses mints two tokens back to back, so they share a
// millisecond; under the monotonic source their entropies are at most 2^32 apart, and under
// crypto/rand the chance of landing that close is about 2^-48. The bar is set at 2^40, which no
// monotonic pair can reach and a random pair misses with probability about 2^-39.
func TestTwoTokensMintedTogetherAreNotOneStepApart(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	only := ev(entity, "1", listA)
	only.Text = "write to jane.doe@example.test or to john.roe@example.test"

	e.mustDrain(tenantA, only)
	rows := e.redactionRows()
	if len(rows) != 2 {
		t.Fatalf("the map holds %d rows, want two: %+v", len(rows), rows)
	}
	first, second := ulidIn(t, rows[0].token), ulidIn(t, rows[1].token)
	if first.Time() != second.Time() {
		// Two tokens of one delivery are minted in one loop, so this is all but impossible. If a
		// scheduler ever does make it happen, the test would be comparing entropies from two
		// different milliseconds, where even the monotonic source starts again.
		t.Skipf("the two tokens were minted in different milliseconds (%d and %d)", first.Time(), second.Time())
	}
	gap := new(big.Int).Sub(
		new(big.Int).SetBytes(first.Entropy()),
		new(big.Int).SetBytes(second.Entropy()),
	)
	gap.Abs(gap)
	if gap.BitLen() <= 40 {
		t.Errorf("two tokens minted in one millisecond are %s apart, want more than 2^40: the placeholder "+
			"is coming from a monotonic math/rand source (ids.New), whose own doc comment forbids it "+
			"for a value that must be unpredictable", gap)
	}
}

// ulidIn is the ULID inside a placeholder, which is everything between the colon and the bracket.
func ulidIn(t *testing.T, token string) ulid.ULID {
	t.Helper()
	_, rest, ok := strings.Cut(strings.TrimSuffix(token, "]"), ":")
	if !ok {
		t.Fatalf("the token %q has no kind prefix", token)
	}
	id, err := ulid.ParseStrict(rest)
	if err != nil {
		t.Fatalf("the token %q does not hold a ULID: %v", token, err)
	}
	return id
}

// TestTheRedactionMapKeepsOneTokenPerValue. The same address in two deliveries reads as the same
// placeholder, so a sink can still see that two records are about one person, and the map does not
// grow with every repeat.
func TestTheRedactionMapKeepsOneTokenPerValue(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	first := ev(entity, "1", listA)
	first.Text = "write to jane.doe@example.test"
	second := ev("fake:task:2", "1", listA)
	second.Text = "jane.doe@example.test again, and john.roe@example.test"

	a := e.mustDrain(tenantA, first).Records[0]
	b := e.mustDrain(tenantA, second).Records[0]

	rows := e.redactionRows()
	if len(rows) != 2 {
		t.Fatalf("the redaction map holds %d rows, want two: %+v", len(rows), rows)
	}
	tokens := map[string]string{}
	for _, r := range rows {
		tokens[r.value] = r.token
	}
	jane := tokens["jane.doe@example.test"]
	if jane == "" {
		t.Fatalf("the map lost the address: %+v", rows)
	}
	if !strings.Contains(a.Text, jane) || !strings.Contains(b.Text, jane) {
		t.Errorf("two deliveries used different tokens for one address: %q and %q", a.Text, b.Text)
	}
	if tokens["john.roe@example.test"] == jane {
		t.Error("two addresses share one token")
	}
}

// TestTwoTenantsNeverShareAMapping. One address in two tenants is two rows and two tokens: the map
// is row-level secured per tenant, and one tenant's token must not resolve in another's.
func TestTwoTenantsNeverShareAMapping(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	event := ev(entity, "1", listA)
	event.Text = "write to jane.doe@example.test"

	a := e.mustDrain(tenantA, event).Records[0]
	b := e.mustDrain(tenantB, event).Records[0]

	if a.Text == b.Text {
		t.Errorf("two tenants got the same token for one address: %q", a.Text)
	}
	rows := e.redactionRows()
	if len(rows) != 2 {
		t.Fatalf("the map holds %d rows, want one per tenant: %+v", len(rows), rows)
	}
	if rows[0].tenant == rows[1].tenant {
		t.Errorf("both mappings belong to %q", rows[0].tenant)
	}
}

// TestASkippedRecordIsNotMasked. Masking is the last stage, after the ledger, so a record that is
// not going anywhere costs no redaction row. Without the ordering, every re-drain of a delivery
// would touch the map again for nothing.
func TestASkippedRecordIsNotMasked(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	event := ev(entity, "1", listA)
	event.Text = "write to jane.doe@example.test"
	e.mustDrain(tenantA, event)
	before := e.redactionRows()

	skipped := e.mustDrain(tenantA, event)
	if skipped.Skipped != 1 {
		t.Fatalf("the repeat was not skipped: %+v", skipped)
	}
	after := e.redactionRows()
	if len(after) != len(before) {
		t.Fatalf("a skipped record added %d redaction rows", len(after)-len(before))
	}
	// The row count cannot see it: one value is one row however often the masker runs. What can is
	// last_seen_at, which the upsert writes on every visit. If it moved, the masker ran on a record
	// that was going nowhere.
	for i := range after {
		if !after[i].lastSeen.Equal(before[i].lastSeen) {
			t.Errorf("the masker touched %q again for a record that was skipped", after[i].value)
		}
	}
}

// TestADroppedAutomationRecordIsNotMasked, for the same reason, one stage earlier: the gate runs
// before anything is sealed, so bot noise never reaches the map at all.
func TestADroppedAutomationRecordIsNotMasked(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	bot := ev("fake:task:bot", "1", listA)
	bot.Automation = true
	bot.Text = "the build failed, mail ci@example.test"

	out := e.mustDrain(tenantA, bot)
	if out.Automation != 1 {
		t.Fatalf("the bot record was not dropped: %+v", out)
	}
	if rows := e.redactionRows(); len(rows) != 0 {
		t.Errorf("a dropped record left %d redaction rows: %+v", len(rows), rows)
	}
}

// TestOneDeliveryTakesOneTripToTheRedactionMap. The masker scans every record of a delivery first
// and maps everything it found in one statement, so a long thread of addresses is one round trip
// and not one per address.
func TestOneDeliveryTakesOneTripToTheRedactionMap(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	var events []fake.Event
	for i := range 5 {
		event := ev("fake:task:"+string(rune('a'+i)), "1", listA)
		event.Text = "mail person" + string(rune('a'+i)) + "@example.test and jane.doe@example.test"
		events = append(events, event)
	}
	out := e.mustDrain(tenantA, events...)
	if len(out.Records) != len(events) {
		t.Fatalf("prepared %d records, want %d", len(out.Records), len(events))
	}
	// Five distinct addresses and one shared: six mappings, and the shared one is one row.
	if rows := e.redactionRows(); len(rows) != len(events)+1 {
		t.Errorf("the map holds %d rows, want %d: %+v", len(rows), len(events)+1, rows)
	}
	for _, r := range out.Records {
		if strings.Contains(r.Text, "@example.test") {
			t.Errorf("a record in a multi-record delivery was not masked: %q", r.Text)
		}
	}
}
