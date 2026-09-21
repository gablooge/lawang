package fake_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/tenancy"
)

// theTenant is the tenant every test in this file seals for. The tenant is hashed into a record
// id, so two records compared for equality have to be sealed for the same one.
const theTenant = tenancy.ID("acme")

// TestTheDegradedPathDerivesTheScopeTheHydratedOneWould is the rule provider.Degrader states, and
// the reason ADR 4 decision 7 makes it a rule: if the two paths derived different scopes, one
// version of one entity would get two record ids, be delivered twice, and look like a move to the
// ledger.
//
// The comparison is on the sealed ids, which is the one that matters: the scope is hashed into the
// id, so two equal ids is two equal scopes.
func TestTheDegradedPathDerivesTheScopeTheHydratedOneWould(t *testing.T) {
	t.Parallel()
	p := fake.New(fake.DefaultKey)
	changes, err := p.Parse([]byte(delivery()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	hydrated, err := p.Hydrate(t.Context(), theTenant, changes[0])
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	fromAPI, err := p.Normalize(hydrated, changes[0])
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	fromBody, err := p.Degrade(changes[0])
	if err != nil {
		t.Fatalf("Degrade: %v", err)
	}
	if len(fromAPI) != 1 || len(fromBody) != 1 {
		t.Fatalf("got %d hydrated and %d degraded records, want one each", len(fromAPI), len(fromBody))
	}
	if fromAPI[0].Visibility.Scope != fromBody[0].Visibility.Scope {
		t.Errorf("the degraded scope is %q and the hydrated one %q",
			fromBody[0].Visibility.Scope, fromAPI[0].Visibility.Scope)
	}
	a, err := fromAPI[0].Seal(fake.DefaultKey, theTenant)
	if err != nil {
		t.Fatalf("seal the hydrated record: %v", err)
	}
	b, err := fromBody[0].Seal(fake.DefaultKey, theTenant)
	if err != nil {
		t.Fatalf("seal the degraded record: %v", err)
	}
	if a.ID != b.ID {
		t.Errorf("one version of one entity got two ids: hydrated %s, degraded %s", a.ID, b.ID)
	}
	// The two records are not identical, and that matters: if hydration added nothing, the test
	// above would pass without proving that a degraded record can differ and keep its id.
	if a.Text == b.Text {
		t.Errorf("the degraded record carries the hydrated text %q, so hydration added nothing", a.Text)
	}
}

// TestADeliveryThatDoesNotNameItsContainerCannotBeDegraded. A guessed scope is the one thing the
// degraded path may never produce, so a body that does not carry what the scope is made of is
// refused by name and the delivery waits for hydration.
func TestADeliveryThatDoesNotNameItsContainerCannotBeDegraded(t *testing.T) {
	t.Parallel()
	p := fake.New(fake.DefaultKey)
	body := strings.ReplaceAll(delivery(), `"container":"L1"`, `"container":"`+fake.UnknownContainer+`"`)
	changes, err := p.Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := p.Degrade(changes[0]); !errors.Is(err, provider.ErrCannotDegrade) {
		t.Fatalf("Degrade of a body with no container gave %v, want provider.ErrCannotDegrade", err)
	}
	// Hydration is what resolves it, and it must still work: the refusal is about the body alone.
	hydrated, err := p.Hydrate(t.Context(), theTenant, changes[0])
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	recs, err := p.Normalize(hydrated, changes[0])
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if want := fake.DefaultKey + ":" + fake.ContainerKind + ":" + fake.ResolvedContainer; recs[0].Visibility.Scope != want {
		t.Errorf("the hydrated scope is %q, want %q", recs[0].Visibility.Scope, want)
	}
}

// TestHydrationCanBeMadeToFail, which is the whole reason the field exists: without it no test of
// the pipeline could reach the degraded path at all.
func TestHydrationCanBeMadeToFail(t *testing.T) {
	t.Parallel()
	p := fake.New(fake.DefaultKey)
	body := strings.ReplaceAll(delivery(), `"op":"upsert"`, `"op":"upsert","hydrate":"fail"`)
	changes, err := p.Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := p.Hydrate(t.Context(), theTenant, changes[0]); !errors.Is(err, fake.ErrHydrateUnavailable) {
		t.Fatalf("Hydrate gave %v, want ErrHydrateUnavailable", err)
	}
	// Any other value is a delivery this provider would not have sent, and the double refuses it
	// rather than reading it as "do not fail".
	bad := strings.ReplaceAll(delivery(), `"op":"upsert"`, `"op":"upsert","hydrate":"maybe"`)
	if _, err := p.Parse([]byte(bad)); !errors.Is(err, fake.ErrBadDelivery) {
		t.Fatalf("Parse of an unknown hydrate value gave %v, want ErrBadDelivery", err)
	}
}

// TestDegradeRefusesAPayloadThatIsNotItsChange, the same fail-closed check Hydrate and Normalize
// make: a Change whose payload belongs to another entity would build a record under the wrong id.
func TestDegradeRefusesAPayloadThatIsNotItsChange(t *testing.T) {
	t.Parallel()
	p := fake.New(fake.DefaultKey)
	changes, err := p.Parse([]byte(delivery()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	other := changes[0]
	other.ExternalID = "fake:task:999"
	if _, err := p.Degrade(other); !errors.Is(err, fake.ErrBadChange) {
		t.Fatalf("Degrade of another change's payload gave %v, want ErrBadChange", err)
	}
	if _, err := p.Degrade(provider.Change{ExternalID: "fake:task:1", Payload: []byte("not json")}); err == nil {
		t.Fatal("Degrade accepted a payload that is not this provider's")
	}
}

// TestAnAuthorThisProviderWouldNotHaveSentIsRefused, because the event gained a field and a double
// that validated everything but the new one would be a lenient double (architecture principle 5).
func TestAnAuthorThisProviderWouldNotHaveSentIsRefused(t *testing.T) {
	t.Parallel()
	p := fake.New(fake.DefaultKey)
	body := strings.ReplaceAll(delivery(), `"op":"upsert"`, `"op":"upsert","author":"a\nb"`)
	if _, err := p.Parse([]byte(body)); !errors.Is(err, fake.ErrBadDelivery) {
		t.Fatalf("Parse of an author with a line break gave %v, want ErrBadDelivery", err)
	}
}
