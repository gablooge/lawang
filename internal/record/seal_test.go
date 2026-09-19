package record

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/gablooge/sluiceway/internal/ids"
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
	if back != r {
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
