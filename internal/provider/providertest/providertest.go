// Package providertest is the conformance harness a provider's normalizer is held to: the cases
// every normalizer must survive, written once, so that each provider does not have to think of
// them again and so that forgetting is hard rather than easy.
//
// It is a package and not a _test.go file because each provider's own tests import it. The
// lawang binary never does.
//
// # Why it exists
//
// The record format refuses and never repairs (ADR 4, decision 9): record.Seal fails on an
// author.display holding an invisible or bidirectional-formatting character, and on a title that
// is not a single line. Real data has both, and a normalizer that passes a source's strings
// through turns a real event into a dead letter. internal/record has the cleaners
// (record.CleanDisplay, record.CleanTitle, record.CleanText), and before this package nothing
// made a provider call them: a fixture with a clean name passes whether a normalizer cleans or
// not, so the mistake was invisible until production.
//
// A provider's test calls CheckCleaning with a callback that plants the harness's values in
// whatever field of whatever payload that provider carries a name and a title in, and runs its
// own parse, hydration and normalization over them. A provider that skips cleaning then fails
// its own tests.
package providertest

import (
	"slices"
	"strings"
	"testing"

	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// tenant is who the harness seals for. Seal needs one, and which one it is changes nothing here:
// the tenant is hashed into the id and checked nowhere else in this package.
const tenant = tenancy.ID("tenant_conformance")

// Case is one hostile pair: a display name and a title, as a source really sends them.
//
// Both fields are set in every case, so a callback always has something to plant in each, and a
// provider whose records carry only one of the two (a ClickUp comment has no title) is covered
// by the other.
type Case struct {
	// Name says what is hostile about the case, for the subtest.
	Name string
	// Display is a name somebody chose for themselves: the field a zero-width space, a
	// right-to-left mark or a line break is planted in.
	Display string
	// Title is a heading of one line, which is the rule real titles break.
	Title string
}

// Cases is every value a normalizer must turn into a record the format accepts. They are the
// examples the reviews of #43 named, which is to say the ones that have actually been seen:
// a display name with a zero-width space, one with a right-to-left mark, one with a soft hyphen,
// a title with a tab, a title with a line break, and a title that is only whitespace.
//
// Three more are here because they are the same mistake one step further: a bidirectional
// override (which reorders the text around it in a log line or a sink's UI), bytes that are not
// UTF-8, and a value longer than the field holds.
//
// # Why the bad bytes travel with an invisible character
//
// A case is only a case if it reaches the normalizer as it is written here, and bytes that are
// not UTF-8 do not survive every transport: encoding/json rewrites them to U+FFFD on the way
// into a payload, which is also exactly what the cleaners produce, so a provider whose transport
// is JSON (which is all of them so far) would pass that case whether it cleaned or not. The
// zero-width space and the line break in that case are what give it teeth everywhere: they
// travel through JSON unchanged and are still refused. The bad bytes stay for the provider whose
// transport does carry them. TestEveryCaseSurvivesAJSONTransport is what holds every case to
// this, so the next one added cannot quietly be decoration.
func Cases() []Case {
	return []Case{
		{"a zero-width space in the name", "ben\u200bjamin", "an ordinary title"},
		{"a right-to-left mark in the name", "\u200fbenjamin", "an ordinary title"},
		{"a soft hyphen in the name", "ben\u00adjamin", "an ordinary title"},
		{"a right-to-left override in the name", "ben\u202ejamin", "an ordinary title"},
		{"a line break in the name", "ben\njamin", "an ordinary title"},
		{"a tab in the title", "benjamin", "two\tcolumns"},
		{"a line break in the title", "benjamin", "first line\nsecond line"},
		{"a folded title", "benjamin", "first line\r\n  second line"},
		{"a line separator in the title", "benjamin", "first\u2028second"},
		{"a title that is only whitespace", "benjamin", " \t "},
		{"bytes that are not UTF-8", "ben\xffja\u200bmin", "ti\xfft\nle"},
		{"a name and a title over the limit", strings.Repeat("n", record.MaxAuthor+10), strings.Repeat("t", record.MaxTitle+10)},
	}
}

// CheckCleaning runs every Case through build and requires that what comes back can be sealed.
//
// build plants c.Display and c.Title in the provider's own payloads and returns what its
// Normalize produced, unsealed. It is given a *testing.T of the subtest, so it can fail with its
// own message when the provider's own parse or hydration refuses, which is itself a conformance
// failure: these values are content, and refusing them is a dead letter by another road.
//
// providerKey is the key the records will be sealed under, which is the provider's own.
//
// It checks more than "Seal returned no error", because that alone would pass for a normalizer
// that dropped the fields on the floor, or that never read them. The planted values must be in
// the records, in their cleaned form: a name that was cleaned to something else, or a title the
// provider truncated its own way, fails here.
func CheckCleaning(t *testing.T, providerKey string, build func(t *testing.T, c Case) []record.Record) {
	t.Helper()
	for _, c := range Cases() {
		t.Run(c.Name, func(t *testing.T) {
			records := build(t, c)
			if len(records) == 0 {
				t.Fatal("the normalizer returned no records for a change that is nothing but content")
			}
			var displays, titles []string
			for i, r := range records {
				sealed, err := r.Seal(providerKey, tenant)
				if err != nil {
					t.Errorf("record %d does not seal: %v", i, err)
					continue
				}
				if _, err := sealed.MarshalJSON(); err != nil {
					t.Errorf("record %d does not marshal: %v", i, err)
				}
				displays = append(displays, r.Author.Display)
				titles = append(titles, r.Title)
			}
			// The premise beside the input: the hostile value reached the record. Without this,
			// a normalizer that wrote an empty author and an empty title would pass every case.
			wantDisplay := record.CleanDisplay(c.Display)
			if !slices.Contains(displays, wantDisplay) {
				t.Errorf("no record carries the cleaned display name %q, only %q", wantDisplay, displays)
			}
			wantTitle := record.CleanTitle(c.Title)
			if !slices.Contains(titles, wantTitle) {
				t.Errorf("no record carries the cleaned title %q, only %q", wantTitle, titles)
			}
		})
	}
}
