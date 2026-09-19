package record

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/gablooge/sluiceway/internal/ids"
	"github.com/gablooge/sluiceway/internal/tenancy"
)

// A sink is promised that one id never appears with two scopes (ADR 4). The id hashes the scope,
// the external id and the version, and those fields stay assignable after Seal, so the promise
// needs MarshalJSON to notice a record that no longer says what was sealed.
func TestARecordChangedAfterSealCannotBeMarshalled(t *testing.T) {
	otherID, err := ids.RecordID(testProvider, "slack:C0GENERAL:1", "1", "slack:channel:C0GENERAL", testTenant.String())
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]func(*Record){
		"the scope":       func(r *Record) { r.Visibility.Scope = "slack:channel:C0SECRET" },
		"the external id": func(r *Record) { r.ExternalID = "slack:C0GENERAL:1752064245.000201" },
		"the version":     func(r *Record) { r.Version = "1752064245.000201" },
		"the id":          func(r *Record) { r.ID = otherID },
		// Neither is in the id. An upsert turned into a tombstone would go out under the id of the
		// version it was, and a sink that is idempotent on the id would drop it as a repeat.
		"the op":   func(r *Record) { r.Op, r.Title, r.Text = OpDelete, "", "" },
		"the kind": func(r *Record) { r.Kind = KindTask },
	}
	for name, change := range changes {
		for _, from := range []string{"sealed", "decoded"} {
			r := sealed(t)
			if from == "decoded" {
				r = Record{}
				if err := json.Unmarshal(exampleBytes(t), &r); err != nil {
					t.Fatal(err)
				}
			}
			change(&r)
			if err := r.Validate(); err != nil {
				t.Fatalf("%s: the changed record is meant to be valid, so that only the seal can refuse it: %v", name, err)
			}
			if out, err := json.Marshal(r); !errors.Is(err, ErrInvalid) {
				t.Errorf("a %s record with %s changed was marshalled: %s (%v)", from, name, out, err)
			}
			if out, err := json.Marshal([]*Record{&r}); !errors.Is(err, ErrInvalid) {
				t.Errorf("a %s record with %s changed was marshalled through a pointer: %s (%v)", from, name, out, err)
			}
			// Sealing again is the way out, and it gives the id of what the record now says.
			r.ID = ""
			again, err := r.Seal(testProvider, testTenant)
			if err != nil {
				t.Fatalf("%s: sealing again: %v", name, err)
			}
			if _, err := json.Marshal(again); err != nil {
				t.Errorf("%s: a record sealed again cannot be marshalled: %v", name, err)
			}
		}
	}
}

func TestWhatMayChangeAfterSeal(t *testing.T) {
	r := sealed(t)
	r.Supersedes = goodID
	r.Source = "chat-eu" // a sink's wire name, replaced on the way out
	r.Meta.Delivery = ""
	r.Text = "redacted later"
	r.Origin.Untrusted = true
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("a record with supersedes, source and content set after sealing: %v", err)
	}
	var back Record
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back != onTheWire(r) {
		t.Errorf("round trip:\n got %+v\nwant %+v", back, r)
	}
}

// A record with a well-formed id that Seal never minted: built by hand, or with the recipe called
// directly and another scope.
func TestARecordThatWasNeverSealedCannotBeMarshalled(t *testing.T) {
	r := draft()
	r.Format, r.Source = FormatV1, testProvider
	id, err := ids.RecordID(testProvider, r.ExternalID, r.Version, "slack:channel:C0SECRET", testTenant.String())
	if err != nil {
		t.Fatal(err)
	}
	r.ID = id
	if err := r.Validate(); err != nil {
		t.Fatalf("the record is meant to be valid, so that only the seal can refuse it: %v", err)
	}
	if out, err := json.Marshal(r); !errors.Is(err, ErrInvalid) {
		t.Errorf("a record that was never sealed was marshalled: %s (%v)", out, err)
	}
}

// The tenant is in no field of the envelope, so a record sealed for one tenant marshals the same
// under another and no sink can tell. SealedFor is how the stage that delivers can.
func TestSealedForIsTrueOnlyForTheTenantOfTheSeal(t *testing.T) {
	const otherTenant = tenancy.ID("tenant_b")
	r := sealed(t)
	if !r.SealedFor(testTenant) {
		t.Error("a sealed record is not sealed for the tenant it was sealed for")
	}
	if r.SealedFor(otherTenant) {
		t.Error("a record sealed for tenant_a is sealed for tenant_b")
	}
	// The case the check exists for: nothing else notices.
	if _, err := json.Marshal(r); err != nil {
		t.Errorf("Marshal cannot see the tenant, and refused: %v", err)
	}

	// What may be set after sealing does not unseal the record, and a copy is still sealed.
	r.Supersedes, r.Source, r.Text = goodID, "chat-eu", "redacted later"
	if held := []Record{r}; !held[0].SealedFor(testTenant) || held[0].SealedFor(otherTenant) {
		t.Error("a copy with supersedes, source and text set after sealing lost or changed its tenant")
	}

	// Sealing again for another tenant moves it, id and all.
	draftAgain := r
	draftAgain.ID, draftAgain.Source = "", ""
	moved, err := draftAgain.Seal(testProvider, otherTenant)
	if err != nil {
		t.Fatal(err)
	}
	if !moved.SealedFor(otherTenant) || moved.SealedFor(testTenant) {
		t.Error("a record sealed again for tenant_b is not sealed for tenant_b alone")
	}
	if moved.ID == r.ID {
		t.Error("two tenants share an id")
	}
}

// Fail closed: whatever is not known to be the tenant's is not the tenant's.
func TestSealedForRefusesWhatWasNotSealedHere(t *testing.T) {
	var decoded Record
	if err := json.Unmarshal(exampleBytes(t), &decoded); err != nil {
		t.Fatal(err)
	}
	// Decoding into a record that was sealed must not leave the old tenant behind.
	overwritten := sealed(t)
	if err := json.Unmarshal(exampleBytes(t), &overwritten); err != nil {
		t.Fatal(err)
	}
	byHand := draft()
	byHand.Format, byHand.Source, byHand.ID = FormatV1, testProvider, sealed(t).ID

	cases := map[string]Record{
		"the zero Record":                       {},
		"a decoded record":                      decoded,
		"a record decoded over a sealed one":    overwritten,
		"a record built by hand with a real id": byHand,
	}
	for name, r := range cases {
		for _, tenant := range []tenancy.ID{testTenant, "", "tenant_b"} {
			if r.SealedFor(tenant) {
				t.Errorf("%s is sealed for %q", name, tenant)
			}
		}
	}
	if sealed(t).SealedFor("") {
		t.Error("a sealed record is sealed for the empty tenant")
	}

	changes := map[string]func(*Record){
		"the scope":       func(r *Record) { r.Visibility.Scope = "slack:channel:C0SECRET" },
		"the external id": func(r *Record) { r.ExternalID = "slack:C0GENERAL:1752064245.000201" },
		"the version":     func(r *Record) { r.Version = "1752064245.000201" },
		"the id":          func(r *Record) { r.ID = goodID },
		"the op":          func(r *Record) { r.Op, r.Title, r.Text = OpDelete, "", "" },
		"the kind":        func(r *Record) { r.Kind = KindTask },
	}
	for name, change := range changes {
		r := sealed(t)
		change(&r)
		if r.SealedFor(testTenant) {
			t.Errorf("a record with %s changed after Seal is still sealed for its tenant", name)
		}
	}
}
