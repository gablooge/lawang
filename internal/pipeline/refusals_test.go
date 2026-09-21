package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/pipeline"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/record"
)

// TestNewRefusesAPipelineThatCanLookNothingUp. Fail closed at startup: a pipeline with no registry
// would dead-letter every delivery it was ever given, one at a time, in production.
func TestNewRefusesAPipelineThatCanLookNothingUp(t *testing.T) {
	t.Parallel()
	if _, err := pipeline.New(nil, pipeline.Options{}); err == nil {
		t.Fatal("pipeline.New(nil) returned a pipeline")
	}
}

// TestPrepareRefusesWhatItCannotDoSafely: no transaction, and no tenant. Neither can be defaulted,
// because the transaction is what binds row-level security and the tenant is what every check in
// the stage is made against.
func TestPrepareRefusesWhatItCannotDoSafely(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	n, err := normalizeFor(e, ev(entity, "1", listA))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if _, err := e.p.Prepare(e.ctx, nil, tenantA, n); err == nil {
		t.Error("Prepare with no transaction returned no error")
	}
	// An empty tenant, inside a transaction that is bound to a real one: store.TenantTx refuses to
	// open a transaction for no tenant at all, so the only way to reach the guard is to lie to
	// Prepare about which tenant the row belongs to.
	err = e.db.TenantTx(e.ctx, tenantA, func(tx pgx.Tx) error {
		_, err := e.p.Prepare(e.ctx, tx, "", n)
		return err
	})
	if !errors.Is(err, pipeline.ErrDeadLetter) {
		t.Errorf("Prepare with no tenant gave %v, want a dead letter", err)
	}
	// The tenant is checked before the records are, so the error names the tenant and not the
	// seal. Without its own check the record would fail SealedFor instead, which is true but says
	// the wrong thing to whoever reads the dead letter.
	if errors.Is(err, pipeline.ErrNotSealedForTenant) {
		t.Errorf("an empty tenant was reported as an unsealed record: %v", err)
	}
}

// TestADatabaseThatCannotBeReachedIsNotADeadLetter. Everything this stage refuses by name is
// about the record; a database that is not answering is about this deployment, and the row must
// go back on the ladder rather than die.
func TestADatabaseThatCannotBeReachedIsNotADeadLetter(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	n, err := normalizeFor(e, ev(entity, "1", listA))
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	stopped, cancel := context.WithCancel(e.ctx)
	cancel()
	err = e.db.TenantTx(e.ctx, tenantA, func(tx pgx.Tx) error {
		_, err := e.p.Prepare(stopped, tx, tenantA, n)
		return err
	})
	if err == nil {
		t.Fatal("Prepare on a context that is already done returned no error")
	}
	if errors.Is(err, pipeline.ErrDeadLetter) {
		t.Errorf("a database call that did not happen was treated as a dead letter: %v", err)
	}
}

// TestANormalizerThatFailsIsADeadLetter. The stored bytes produce the same failure on every
// attempt, so retrying is only a way of finding out later.
func TestANormalizerThatFailsIsADeadLetter(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{}, brokenNormalizer{neverDegrades{fake.New(fake.DefaultKey)}})
	if _, err := e.drain(tenantA, ev(entity, "1", listA)); !errors.Is(err, pipeline.ErrDeadLetter) {
		t.Fatalf("a normalizer that fails gave %v, want a dead letter", err)
	}
}

// TestADegraderThatFailsForItsOwnReasonsIsADeadLetter. ErrCannotDegrade means "not from this body,
// ask the API", which is a wait. Any other error from Degrade is the provider package being
// broken, and the same body will break it again.
func TestADegraderThatFailsForItsOwnReasonsIsADeadLetter(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{}, brokenDegrader{neverDegrades{fake.New(fake.DefaultKey)}})
	down := ev(entity, "1", listA)
	down.Hydrate = fake.HydrateFail

	if _, err := e.drain(tenantA, down); !errors.Is(err, pipeline.ErrDeadLetter) {
		t.Fatalf("a Degrade that fails gave %v, want a dead letter", err)
	}
}

// TestANormalizerThatProducesARecordTheFormatRefusesIsADeadLetter. record.Seal is where a
// normalizer's mistake is caught, and it is caught here rather than at a strict sink.
func TestANormalizerThatProducesARecordTheFormatRefusesIsADeadLetter(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	// A title with a line break. The format refuses one (ADR 4, decision 9: a title is one line),
	// and turning it into a space is the normalizer's job, not the format's.
	bad := ev(entity, "1", listA)
	bad.Title = "two\nlines"

	_, got := e.drain(tenantA, bad)
	if !errors.Is(got, pipeline.ErrDeadLetter) {
		t.Fatalf("a record the format refuses gave %v, want a dead letter", got)
	}
	if !errors.Is(got, record.ErrInvalid) {
		t.Errorf("the dead letter does not say what the format refused: %v", got)
	}
}

// TestATitleThatMaskingMakesTooLongIsADeadLetter. A placeholder is longer than a short address, so
// a title cut to exactly the limit can outgrow it. Cutting it again would lose content silently
// and leaving it unmasked is the one thing this stage must never do, so the delivery dies and the
// fix is on the normalizer.
func TestATitleThatMaskingMakesTooLongIsADeadLetter(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	event := ev(entity, "1", listA)
	event.Title = strings.Repeat("x", record.MaxTitle-len("a@b.co")-1) + " a@b.co"

	_, got := e.drain(tenantA, event)
	if !errors.Is(got, pipeline.ErrMaskedTooLong) {
		t.Fatalf("a title that masking made too long gave %v, want ErrMaskedTooLong", got)
	}
	if !errors.Is(got, pipeline.ErrDeadLetter) {
		t.Errorf("a field that can never be masked is not marked as a dead letter: %v", got)
	}
	if rows := e.ledgerRows(); len(rows) != 0 {
		t.Errorf("the failed delivery left %d ledger rows behind: %+v", len(rows), rows)
	}
}

// TestAMaskingFailureTheDatabaseCausedIsNotADeadLetter is the other side of the test above, and
// the one that makes the classification at the end of Prepare mean something.
//
// Everything that goes wrong in masking is either about the record (a field that cannot fit once
// the placeholders are in, more distinct values than one delivery may map) or about the database
// (a cancelled context, a lost connection, a refused write). Only the first kind is a dead letter.
// Without this test, "classify every masking failure as a dead letter" survives the whole package,
// because TestADatabaseThatCannotBeReachedIsNotADeadLetter dies at the first database call and
// never reaches the masker: a transient failure inside the redaction upsert would then destroy a
// delivery the next attempt would have carried.
//
// The failure is made by taking INSERT on the redaction map away from the application role, so the
// upsert is refused (SQLSTATE 42501) AFTER the ledger has written, which is exactly where a real
// transient failure would land.
func TestAMaskingFailureTheDatabaseCausedIsNotADeadLetter(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})
	e.exec(`REVOKE INSERT ON lawang.redaction_map FROM lawang`)

	event := ev(entity, "1", listA)
	event.Text = "write to jane.doe@example.test"
	_, got := e.drain(tenantA, event)
	if got == nil {
		t.Fatal("a redaction map the drain may not write to did not fail the delivery")
	}
	if errors.Is(got, pipeline.ErrDeadLetter) {
		t.Errorf("a masking failure the database caused was classified as a dead letter, which destroys "+
			"a delivery the retry ladder would have carried: %v", got)
	}
	// The cause has to survive, or an operator cannot tell this from a value that would never fit.
	// The value itself must not: the server's message can quote it.
	if !strings.Contains(got.Error(), "42501") {
		t.Errorf("the error does not name the SQLSTATE, so every database cause reads alike: %v", got)
	}
	if strings.Contains(got.Error(), "jane.doe@example.test") {
		t.Errorf("the error quotes the value being masked: %v", got)
	}
	if rows := e.ledgerRows(); len(rows) != 0 {
		t.Errorf("the failed delivery left %d ledger rows behind, so the transaction did not roll back: %+v", len(rows), rows)
	}
}

// TestADeliveryWithMoreSecretsThanTheMapTakesIsRefusedByName. Everything the masker finds becomes a
// permanent row, in one statement, and the text it scans comes from a sender. Without a bound, one
// delivery decides how large that statement is and how much personal data this deployment keeps.
func TestADeliveryWithMoreSecretsThanTheMapTakesIsRefusedByName(t *testing.T) {
	t.Parallel()
	e := setup(t, pipeline.Options{})

	var b strings.Builder
	for i := range pipeline.MaxSecretsPerDelivery + 1 {
		fmt.Fprintf(&b, "u%d@example.test ", i)
	}
	event := ev(entity, "1", listA)
	event.Text = b.String()

	_, got := e.drain(tenantA, event)
	if !errors.Is(got, pipeline.ErrTooManySecrets) {
		t.Fatalf("a delivery with %d distinct addresses gave %v, want ErrTooManySecrets", pipeline.MaxSecretsPerDelivery+1, got)
	}
	if !errors.Is(got, pipeline.ErrDeadLetter) {
		t.Errorf("the refusal is not a dead letter, so the same bytes will be tried until the ladder runs out: %v", got)
	}
	if rows := e.redactionRows(); len(rows) != 0 {
		t.Errorf("the refused delivery wrote %d redaction rows, so the bound did not bound anything", len(rows))
	}

	// One under the bound goes through, which is what says the number is a bound and not a
	// coincidence of the fixture.
	b.Reset()
	for i := range pipeline.MaxSecretsPerDelivery {
		fmt.Fprintf(&b, "u%d@example.test ", i)
	}
	ok := ev("fake:task:2", "1", listA)
	ok.Text = b.String()
	if out := e.mustDrain(tenantA, ok); len(out.Records) != 1 {
		t.Fatalf("a delivery of exactly %d distinct addresses prepared %d records, want one",
			pipeline.MaxSecretsPerDelivery, len(out.Records))
	}
	if rows := e.redactionRows(); len(rows) != pipeline.MaxSecretsPerDelivery {
		t.Errorf("the map holds %d rows, want %d", len(rows), pipeline.MaxSecretsPerDelivery)
	}
}

// brokenNormalizer hydrates and then refuses to normalize, the way a provider package with a bug
// in it would. brokenDegrader fails to degrade for a reason that is not ErrCannotDegrade.
//
// Both embed neverDegrades, which spells every method out rather than embedding *fake.Provider, so
// that nothing is promoted here by accident.
type brokenNormalizer struct{ neverDegrades }

func (brokenNormalizer) Normalize(provider.Hydrated, provider.Change) ([]record.Record, error) {
	return nil, errors.New("this normalizer is broken")
}

type brokenDegrader struct{ neverDegrades }

func (brokenDegrader) Degrade(provider.Change) ([]record.Record, error) {
	return nil, errors.New("this degrader is broken")
}

var (
	_ provider.WebhookSource = brokenNormalizer{}
	_ provider.Degrader      = brokenDegrader{}
)

// TestEntityLocksAreTakenInOneOrder. Two transactions that took the locks of the same two entities
// in opposite orders would deadlock, and Postgres would abort one of them a second later. One
// sorted order for every caller in the program turns that into queueing.
//
// It is asserted on the order itself and not on the absence of a deadlock, because a deadlock
// needs an interleaving no test can ask for: a test that ran two opposite deliveries and saw no
// error would pass just as happily with the sorting gone.
func TestEntityLocksAreTakenInOneOrder(t *testing.T) {
	t.Parallel()
	forward := []record.Record{{ExternalID: "fake:task:b"}, {ExternalID: "fake:task:a"}, {ExternalID: "fake:task:c"}}
	backward := []record.Record{{ExternalID: "fake:task:c"}, {ExternalID: "fake:task:a"}, {ExternalID: "fake:task:b"}}
	want := []string{"fake:task:a", "fake:task:b", "fake:task:c"}

	for _, recs := range [][]record.Record{forward, backward} {
		if got := pipeline.EntityKeys(recs); !slices.Equal(got, want) {
			t.Errorf("EntityKeys = %v, want %v: two deliveries of the same entities must lock in one order", got, want)
		}
	}
	// One entity twice, which a delivery carrying two versions of it produces, is one lock.
	twice := []record.Record{{ExternalID: "fake:task:a"}, {ExternalID: "fake:task:a"}}
	if got := pipeline.EntityKeys(twice); !slices.Equal(got, []string{"fake:task:a"}) {
		t.Errorf("EntityKeys = %v, want one key", got)
	}
}

// TestTheIBANRuleRefusesWhatCannotBeAnIBAN and TestThePhoneRuleCountsDigits are the two rules the
// masker's patterns cannot state, on inputs the patterns themselves would never produce. A guard
// whose only proof is that nothing reaches it is not proved at all.
func TestTheIBANRuleRefusesWhatCannotBeAnIBAN(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		why  string
		want bool
	}{
		{"GB82WEST12345698765432", "the canonical valid example", true},
		{"GB82 WEST 1234 5698 7654 32", "the same, in groups", true},
		{"GB82WEST123456987654", "shorter than any IBAN there is", false},
		{"GB82WEST1234569876543212345678901234567", "longer than any IBAN there is", false},
		{"GB82WEST-2345698765432", "a character that is neither a letter nor a digit", false},
		{"gb82west12345698765432", "lower case, which an IBAN is never written in", false},
	} {
		if got := pipeline.ValidIBAN(tc.in); got != tc.want {
			t.Errorf("ValidIBAN(%q) = %v, want %v: %s", tc.in, got, tc.want, tc.why)
		}
	}
}

// TestThePhoneRuleIsEveryRuleThePatternCannotState. The pattern matches a shape; this function is
// the whole of what makes a shape a telephone number, so every rule in it needs a pair that
// differs in that rule alone.
func TestThePhoneRuleIsEveryRuleThePatternCannotState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want bool
		why  string
	}{
		// The digit count, which is all a country code needs.
		{"+442079460958", true, "twelve digits behind a country code"},
		{"+4420794", false, "seven digits behind a country code"},
		{"+4420794609581234", false, "sixteen, one over what E.164 allows"},
		{"+62 812-3456-7890", true, "a country code, separators, and no other rule applied"},
		{"+192.168.100.200", true, "a plus makes it a country code, and this is not an address"},

		// The digit count again, without a country code.
		{"555.010.1234", true, "ten digits in three groups"},
		{"555.010.12", false, "eight digits and no country code"},

		// The number of groups. Four is an address or a build number.
		{"192.168.100.200", false, "an IPv4 address, every octet three digits"},
		{"172.217.169.110", false, "a public IPv4 address"},
		{"198.051.100.042", false, "an IPv4 address with zero-padded octets"},
		{"0812-3456-7890", true, "the same digits in three groups is a number"},
		{"081-234-5678-90", false, "the same digits again, in four groups"},

		// A group of four digits. Three groups of three is not a spelling anybody uses.
		{"100.200.300", false, "three groups of three digits"},
		{"123.456.789", false, "an invoice total"},
		{"100.200.3000", true, "the same, with a fourth digit in the last group"},

		// A first group that reads as a year.
		{"2024.100.200", false, "a version string"},
		{"2026-0921-1234", false, "an order reference"},
		{"1999-0102-0304", false, "the same shape in the other century"},
		{"2126-0921-1234", true, "the same shape with a first group that is not a year"},
		{"0812-3456-7890", true, "a leading zero is not a year"},
	} {
		if got := pipeline.ValidPhone(tc.in); got != tc.want {
			t.Errorf("ValidPhone(%q) = %v, want %v: %s", tc.in, got, tc.want, tc.why)
		}
	}
}

// TestTheVersionOrderIsTheThreeRulesAndNothingElse is ADR 12 decision 1, on inputs an integration
// test cannot reach in a useful number.
//
// The three rules have to be here together because each one is only safe while the other two hold:
// a number for two decimal counters, bytes for two versions of equal length, and a refusal for
// everything else. A table that only tested the first two would be passed by a function that
// guessed, and guessing is what lets an older record take the head.
func TestTheVersionOrderIsTheThreeRulesAndNothingElse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b      string
		want      int
		wantOK    bool
		why       string
		symmetric bool
	}{
		// Decimal counters, by their number and not by their bytes.
		{"9", "10", -1, true, "an unpadded counter goes forward", true},
		{"10", "9", 1, true, "and backward, which byte order got wrong", true},
		{"2", "10", -1, true, "the same across the first decade", true},
		{"100", "99", 1, true, "and the next", true},
		{"9", "9", 0, true, "the same counter twice is the same version", true},
		{"009", "9", 0, true, "leading zeros are not part of a number", true},
		{"0", "00", 0, true, "nor is a run of them", true},
		{"0", "1", -1, true, "zero is a version like any other", true},
		// A counter far past anything an int would hold, because a version is a string.
		{strings.Repeat("9", 40), strings.Repeat("9", 39) + "8", 1, true, "forty digits, one apart", true},

		// Equal length, by bytes. Every encoding whose byte order is its value order is fixed
		// width while it is in use.
		{"2026-09-21T10:00:00Z", "2026-09-21T11:00:00Z", -1, true, "RFC 3339, one hour apart", true},
		{"1758448800000", "1758448800001", -1, true, "an epoch in milliseconds", true},
		{"01K5XJ9Z7QA", "01K5XJ9Z7QB", -1, true, "a ULID-shaped counter", true},
		{"v002", "v001", 1, true, "a prefixed padded counter", true},
		{"abc", "abc", 0, true, "the same opaque version twice", true},

		// Neither: no order can be read, and the function says so instead of guessing.
		{"v9", "v10", 0, false, "a prefixed unpadded counter", true},
		{"9", "v9", 0, false, "a counter and something else", true},
		{"2026-09-21T10:00:00Z", "2026-09-21T10:00:00.5Z", 0, false, "a timestamp that grew a fraction", true},
		{"abc", "abcd", 0, false, "two opaque strings of different lengths", true},
	} {
		got, ok := pipeline.CompareVersions(tc.a, tc.b)
		if ok != tc.wantOK {
			t.Errorf("CompareVersions(%q, %q) ok = %v, want %v: %s", tc.a, tc.b, ok, tc.wantOK, tc.why)
			continue
		}
		if ok && sign(got) != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d: %s", tc.a, tc.b, sign(got), tc.want, tc.why)
		}
		// The order has to be total, or two calls in one delivery could disagree and the chain
		// would depend on which record arrived first.
		back, backOK := pipeline.CompareVersions(tc.b, tc.a)
		if backOK != ok || sign(back) != -sign(got) {
			t.Errorf("CompareVersions(%q, %q) = (%d, %v) but the other way round is (%d, %v): the order is not antisymmetric",
				tc.a, tc.b, sign(got), ok, sign(back), backOK)
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// TestAPatternIsNotTakenInTheMiddleOfSomethingLonger is the boundary rule: a match that begins or
// ends inside a longer token is part of that token and not a secret of its own.
func TestAPatternIsNotTakenInTheMiddleOfSomethingLonger(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"build-0812-3456-7890x",   // a number that runs into a letter
		"x0812-3456-7890",         // a number that runs out of a letter
		"0812-3456-7890.1",        // the front of a longer dotted thing
		"/0812-3456-7890",         // the tail of a path
		"0812-3456-7890/segment",  // the head of one
		"GB82WEST12345698765432X", // an account number with something stuck to it
	} {
		if got := mask(t, text); got != text {
			t.Errorf("masking took part of %q and made it %q", text, got)
		}
	}
}
