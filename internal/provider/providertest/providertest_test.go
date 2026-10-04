package providertest_test

import (
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
