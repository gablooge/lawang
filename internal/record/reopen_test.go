package record

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/gablooge/lawang/internal/tenancy"
)

// TestReopenGivesBackTheRecordThatWasStored. The worker stores what it prepared and delivers it
// after a crash, from another process, so what comes back has to be the same record AND has to
// be sealed for the tenant again: a decoded record knows no tenant, and every sink refuses one
// it cannot see a tenant on.
func TestReopenGivesBackTheRecordThatWasStored(t *testing.T) {
	stored := sealed(t)
	stored.Supersedes = goodID // set after sealing, as the ledger does, and still on the wire
	doc, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}

	// The premise: decoding alone loses the tenant, which is what Reopen is for.
	var decoded Record
	if err := json.Unmarshal(doc, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SealedFor(testTenant) {
		t.Fatal("a decoded record is sealed for a tenant, so this test is not about anything")
	}

	got, err := Reopen(doc, testProvider, testTenant)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if !got.SealedFor(testTenant) {
		t.Error("the reopened record is not sealed for the tenant it was stored for")
	}
	again, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshalling the reopened record: %v", err)
	}
	if string(again) != string(doc) {
		t.Errorf("the reopened record marshals to\n%s\nwant\n%s", again, doc)
	}
}

// TestReopenRefusesADocumentThatIsNotThisTenantsRecord is the whole reason Reopen mints the id
// again instead of trusting the one in the document. The id hashes the provider, the entity, the
// version, the scope and the tenant, so each of these is a document that would otherwise be
// delivered to somebody it does not belong to, or under an id that does not stand for it.
func TestReopenRefusesADocumentThatIsNotThisTenantsRecord(t *testing.T) {
	stored := sealed(t)
	doc, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}

	otherTenant := tenancy.ID("tenant_b")
	if _, err := Reopen(doc, testProvider, otherTenant); !errors.Is(err, ErrNotThisTenantsRecord) {
		t.Errorf("another tenant's record: err = %v, want ErrNotThisTenantsRecord", err)
	}

	// A field that IS hashed into the id, edited in the table under the stored id. Each of
	// these is a valid document that marshals and decodes, which is exactly why the check
	// cannot be left to the format.
	edits := map[string]func(*Record){
		"the version":     func(r *Record) { r.Version = "1752064245.000201" },
		"the external id": func(r *Record) { r.ExternalID = "slack:C0GENERAL:1752064245.000201" },
		"the scope":       func(r *Record) { r.Visibility.Scope = "slack:channel:C0SECRET" },
	}
	for name, edit := range edits {
		var tampered Record
		if err := json.Unmarshal(doc, &tampered); err != nil {
			t.Fatal(err)
		}
		edit(&tampered)
		// Written out through the field-only type, because MarshalJSON would refuse it: this is
		// a document in the table, not a record this process sealed.
		bytes, err := json.Marshal(wire(tampered))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Reopen(bytes, testProvider, testTenant); !errors.Is(err, ErrNotThisTenantsRecord) {
			t.Errorf("%s edited under the stored id: err = %v, want ErrNotThisTenantsRecord", name, err)
		}
	}
}

// TestReopenRefusesWhatTheFormatRefuses: it is the strict decoder's door, not a second one.
func TestReopenRefusesWhatTheFormatRefuses(t *testing.T) {
	for name, doc := range map[string]string{
		"not JSON":             `{`,
		"not an object":        `[]`,
		"a missing field":      `{"id":"rec_0"}`,
		"a repeated field":     `{"id":"a","id":"b"}`,
		"not valid UTF-8":      "{\"id\":\"\xff\"}",
		"an empty document":    ``,
		"a document of 'null'": `null`,
	} {
		if _, err := Reopen([]byte(doc), testProvider, testTenant); err == nil {
			t.Errorf("%s: err = nil, want a refusal", name)
		}
	}
	// A record of another provider is refused by Seal, which is where the namespace rules are.
	doc, err := json.Marshal(sealed(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reopen(doc, "clickup", testTenant); err == nil {
		t.Error("a slack record reopened as clickup: err = nil, want a refusal")
	}
	// And a tenant that is not a tenant is a refusal, never a record sealed for nobody.
	if _, err := Reopen(doc, testProvider, tenancy.ID("")); err == nil {
		t.Error("an empty tenant: err = nil, want a refusal")
	}
}
