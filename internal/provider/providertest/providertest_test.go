package providertest_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gablooge/lawang/internal/provider/providertest"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// Every case has teeth, and cleaning takes them out.
//
// The harness is a check in front of other checks, and the way it fails silently is for a case
// to carry a value the record format would have accepted anyway: a normalizer that cleaned
// nothing would then pass it, and the harness would be decoration. So each case is held to both
// halves: the values as a source sends them are refused, and the values as the cleaners leave
// them are accepted.
func TestEveryCaseIsRefusedUncleanedAndAcceptedCleaned(t *testing.T) {
	for _, c := range providertest.Cases() {
		t.Run(c.Name, func(t *testing.T) {
			if _, err := sealable(c.Display, c.Title).Seal("fake", tenancy.ID("tenant_x")); err == nil {
				t.Error("the format accepts this case as it stands, so a normalizer that cleans nothing would pass it")
			}
			cleaned := sealable(record.CleanDisplay(c.Display), record.CleanTitle(c.Title))
			if _, err := cleaned.Seal("fake", tenancy.ID("tenant_x")); err != nil {
				t.Errorf("the format refuses the cleaned case, so no normalizer could ever pass it: %v", err)
			}
		})
	}
}

// Every case still has teeth after a JSON round trip, which is the one thing the test above
// cannot see.
//
// It checks the Case value against the format directly, and a provider does not: the value goes
// into that provider's payload and comes back out of its decoder before the normalizer ever
// reads it. encoding/json rewrites a byte that is not UTF-8 to U+FFFD on the way in, and the
// cleaners produce U+FFFD for the same byte, so a case whose only hostile content is bad bytes
// arrives already cleaned and is passed by a normalizer that cleans nothing. That was true of
// the "bytes that are not UTF-8" case: with record.CleanDisplay removed from the first real
// provider, that case passed while others failed, and after the invisible character was added to
// it, it fails with them. No count is given. The one written here first was wrong (it said six
// where seven is measured), and a number in a comment that nothing holds to the code is a number
// that goes stale; the before and after is the whole of the argument anyway.
//
// Every provider's transport is JSON today, so this is the transport to hold the cases to.
func TestEveryCaseSurvivesAJSONTransport(t *testing.T) {
	for _, c := range providertest.Cases() {
		t.Run(c.Name, func(t *testing.T) {
			display, title := throughJSON(t, c.Display), throughJSON(t, c.Title)
			if _, err := sealable(display, title).Seal("fake", tenancy.ID("tenant_x")); err == nil {
				t.Errorf("the format accepts this case after a JSON round trip (%q, %q), so it cannot "+
					"catch a provider whose transport is JSON and whose normalizer cleans nothing",
					display, title)
			}
		})
	}
}

// throughJSON is what a provider's payload does to a value: marshalled into a document and read
// back out of it.
func throughJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back string
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return back
}

// sealable is a record that is complete but for the two fields under test.
func sealable(display, title string) record.Record {
	return record.Record{
		Op:         record.OpUpsert,
		Kind:       record.KindTask,
		ExternalID: "fake:task:1",
		Version:    "1",
		OccurredAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC),
		Title:      title,
		Author:     record.Author{ID: "u1", Display: display},
		Container:  record.Container{Kind: "list", ID: "L1"},
		Visibility: record.Visibility{Scope: "fake:list:L1", Audience: record.AudienceGroup},
	}
}
