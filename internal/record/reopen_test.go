package record

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
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

	got, err := Reopen(Stored{ID: stored.ID, Op: stored.Op, Kind: stored.Kind, Document: doc}, testProvider, testTenant)
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

// TestReopenRefusesADocumentThatIsNotTheRecordThatWasStored covers the property Reopen is named
// for, which is wider than the id: every field the seal covers, and not only the ones the id
// hashes.
//
// The id hashes the provider, the entity, the version, the scope and the tenant, so a document
// edited in one of those re-mints to an id that is not the one it carries. Op and Kind are in
// the seal and NOT in the id (the seal type says why), so a document edited in one of them
// agrees with itself: re-sealing it mints the same id and nothing inside the document
// disagrees. Those two are held against the columns the outbox stores beside the document, and
// so is the record id of the row itself, which is what a delivery is keyed, leased and
// dead-lettered on.
//
// Each case is one edit a tenant's own SQL could make. Without the check on Op, an upsert
// edited to a delete goes out as a tombstone and the receiver removes the entity.
func TestReopenRefusesADocumentThatIsNotTheRecordThatWasStored(t *testing.T) {
	stored := sealed(t)
	doc, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	beside := Stored{ID: stored.ID, Op: stored.Op, Kind: stored.Kind, Document: doc}

	otherTenant := tenancy.ID("tenant_b")
	if _, err := Reopen(beside, testProvider, otherTenant); !errors.Is(err, ErrNotThisTenantsRecord) {
		t.Errorf("another tenant's record: err = %v, want ErrNotThisTenantsRecord", err)
	}

	// Each edit is made to the document alone, under the columns the row already holds. Each
	// is a valid document that marshals and decodes, which is exactly why the check cannot be
	// left to the format.
	edits := map[string]struct {
		edit func(*Record)
		want error
	}{
		// Hashed into the id: the re-minted id is not the one the document carries.
		"the version":     {func(r *Record) { r.Version = "1752064245.000201" }, ErrNotThisTenantsRecord},
		"the external id": {func(r *Record) { r.ExternalID = "slack:C0GENERAL:1752064245.000201" }, ErrNotThisTenantsRecord},
		"the scope":       {func(r *Record) { r.Visibility.Scope = "slack:channel:C0SECRET" }, ErrNotThisTenantsRecord},
		// Sealed and not hashed: only the columns beside the document can see these. Validate
		// refuses a delete that still carries a title and a text, so the tombstone edit clears
		// them too. That is one more field in the same edit, not a second guard.
		"the op":   {func(r *Record) { r.Op, r.Title, r.Text = OpDelete, "", "" }, ErrNotTheRecordStored},
		"the kind": {func(r *Record) { r.Kind = KindTask }, ErrNotTheRecordStored},
		// The document replaced wholesale by another record of the same tenant, which is
		// self-consistent and carries its own id: only the stored record id sees it.
		"another record of the same tenant": {
			func(r *Record) {
				r.Version = "1752064245.000202"
				r.ExternalID = "slack:C0GENERAL:1752064245.000202"
				other, err := r.Seal(testProvider, testTenant)
				if err != nil {
					t.Fatal(err)
				}
				*r = other
			},
			ErrNotTheRecordStored,
		},
	}
	for name, tc := range edits {
		var tampered Record
		if err := json.Unmarshal(doc, &tampered); err != nil {
			t.Fatal(err)
		}
		tc.edit(&tampered)
		// Written out through the field-only type, because MarshalJSON would refuse it: this is
		// a document in the table, not a record this process sealed.
		bytes, err := json.Marshal(wire(tampered))
		if err != nil {
			t.Fatal(err)
		}
		edited := beside
		edited.Document = bytes
		if _, err := Reopen(edited, testProvider, testTenant); !errors.Is(err, tc.want) {
			t.Errorf("%s edited under the stored columns: err = %v, want %v", name, err, tc.want)
		}
	}

	// The mirror image: the columns edited and the document left alone. Neither side is the one
	// that is trusted, so a row whose op column says something else is refused as well.
	for name, col := range map[string]Stored{
		"the stored record id": {ID: "rec_somebody_elses", Op: stored.Op, Kind: stored.Kind, Document: doc},
		"the stored op":        {ID: stored.ID, Op: OpDelete, Kind: stored.Kind, Document: doc},
		"the stored kind":      {ID: stored.ID, Op: stored.Op, Kind: KindTask, Document: doc},
		"nothing beside it":    {Document: doc},
	} {
		if _, err := Reopen(col, testProvider, testTenant); !errors.Is(err, ErrNotTheRecordStored) {
			t.Errorf("%s edited beside the document: err = %v, want ErrNotTheRecordStored", name, err)
		}
	}
}

// TestEverySealedFieldIsCoveredWhenAStoredRecordIsReopened is a tripwire, not a behaviour test.
//
// Reopen can cover a field of the seal in one of two ways: by minting the id again, which
// covers what ids.RecordID hashes, or against a column the outbox stores beside the document,
// which is how Op and Kind are covered. A seventh field added to the seal would be covered by
// neither, and Reopen would quietly go back to accepting an edit to it. So the shape of the
// seal is pinned here, next to the reason.
func TestEverySealedFieldIsCoveredWhenAStoredRecordIsReopened(t *testing.T) {
	// id, externalID, version and scope are what the id hashes (with the provider and the
	// tenant, which are Reopen's own arguments). op and kind are what Stored carries.
	want := []string{"id", "externalID", "version", "scope", "op", "kind"}
	var got []string
	for _, f := range reflect.VisibleFields(reflect.TypeOf(seal{})) {
		got = append(got, f.Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the seal covers %v, want %v: a field added to the seal needs a way through Reopen, "+
			"either hashed into the id or carried in record.Stored and stored in outbox_record", got, want)
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
		s := Stored{ID: "rec_whatever", Op: OpUpsert, Kind: KindMessage, Document: []byte(doc)}
		if _, err := Reopen(s, testProvider, testTenant); err == nil {
			t.Errorf("%s: err = nil, want a refusal", name)
		}
	}
	// A record of another provider is refused by Seal, which is where the namespace rules are.
	stored := sealed(t)
	doc, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	beside := Stored{ID: stored.ID, Op: stored.Op, Kind: stored.Kind, Document: doc}
	if _, err := Reopen(beside, "clickup", testTenant); err == nil {
		t.Error("a slack record reopened as clickup: err = nil, want a refusal")
	}
	// And a tenant that is not a tenant is a refusal, never a record sealed for nobody.
	if _, err := Reopen(beside, testProvider, tenancy.ID("")); err == nil {
		t.Error("an empty tenant: err = nil, want a refusal")
	}
}
