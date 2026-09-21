package pipeline_test

import (
	"context"
	"errors"
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

func TestThePhoneRuleCountsDigits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"+442079460958", true},
		{"+4420794", false},          // seven digits behind a country code
		{"+4420794609581234", false}, // sixteen, one over what E.164 allows
		{"555.010.1234", true},
		{"555.010.12", false}, // eight digits and no country code
	} {
		if got := pipeline.ValidPhone(tc.in); got != tc.want {
			t.Errorf("ValidPhone(%q) = %v, want %v", tc.in, got, tc.want)
		}
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
