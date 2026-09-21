package pipeline_test

import (
	"strings"
	"testing"

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
// The bullet says the placeholder is "a ULID, not a hash of the value", because a token that any
// sink could compute from a guess would hand it an oracle: guess an address, compute its token,
// look for that token in the records it holds, and the fact masking withholds (that this address
// appears in this tenant's records) comes straight back. Salting with the tenant does not close
// it, because a tenant id is not a secret and every sink holds one.
//
// Asserting that the token does not quote the value cannot see any of that: a digest quotes
// nothing. The distinguishing fact is that the token must not be a function of its inputs AT ALL,
// so the test mints the same value twice, from nothing but the same inputs, and requires two
// different answers. The redaction map is emptied in between, the way B25's retention sweep will
// empty it, so the second minting has nothing to look up and nothing but the minting decides.
//
// Any digest of (value), (tenant, value) or (tenant, kind, value) fails this. A random placeholder
// passes it.
func TestMintingOneValueTwiceGivesTwoTokens(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	const addr = "jane.doe@example.test"

	first := ev(entity, "1", listA)
	first.Text = "write to " + addr
	a := e.mustDrain(tenantA, first).Records[0]
	rows := e.redactionRows()
	if len(rows) != 1 {
		t.Fatalf("the map holds %d rows after one delivery, want one: %+v", len(rows), rows)
	}
	before := rows[0].token

	// Take the mapping away, as retention does. Nothing about the address, the tenant or the kind
	// has changed; the only thing gone is the row that remembers the answer.
	e.exec(`DELETE FROM lawang.redaction_map`)

	second := ev("fake:task:2", "1", listA)
	second.Text = "write to " + addr
	b := e.mustDrain(tenantA, second).Records[0]
	rows = e.redactionRows()
	if len(rows) != 1 {
		t.Fatalf("the map holds %d rows after the second delivery, want one: %+v", len(rows), rows)
	}
	if rows[0].token == before {
		t.Errorf("minting %q twice gave the same token %q: the placeholder is a function of its inputs, "+
			"which is the offline oracle ADR 12 decision 5 exists to prevent", addr, before)
	}
	if !strings.Contains(a.Text, before) || !strings.Contains(b.Text, rows[0].token) {
		t.Fatalf("a record does not carry the token its delivery minted: %q, then %q", a.Text, b.Text)
	}
	if a.Text == b.Text {
		t.Errorf("two mintings of one address produced the same masked text: %q", a.Text)
	}
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
