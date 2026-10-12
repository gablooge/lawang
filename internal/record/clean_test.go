package record

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// cleaner is one field class: the exported function a normalizer calls, the rule Validate holds
// that field to, and the field's limit. The three rows are every cleanable field class there is:
// identifierChars has no row on purpose, and TestThereIsNoCleanerForAnIdentifier is why.
var cleaners = []struct {
	name     string
	clean    func(string) string
	rule     charRule
	maxChars int
}{
	{"display", CleanDisplay, displayChars, MaxAuthor},
	{"title", CleanTitle, oneLineChars, MaxTitle},
	{"text", CleanText, contentChars, MaxText},
}

// A cleaned value always passes, for every code point there is.
//
// This is the test the round 2 review of #43 asked for: the helper and the validation rule it
// mirrors must not be able to drift. It does not range over a list of interesting characters,
// because a list in a test is the same second copy a list in a comment is. It ranges over every
// code point Go can put in a string, asks the rule itself whether the character is refused, and
// requires the cleaner to agree: a refused character is gone from the result, a permitted one is
// still there, and the result passes checkString either way.
func TestCleaningAgreesWithTheRuleOnEveryCodePoint(t *testing.T) {
	for _, c := range cleaners {
		t.Run(c.name, func(t *testing.T) {
			for r := rune(0); r <= utf8.MaxRune; r++ {
				if r >= 0xD800 && r <= 0xDFFF {
					continue // a surrogate is not a code point a Go string can hold
				}
				// Between two ordinary letters, so that a cleaner which trims the edges
				// (CleanTitle does) cannot hide a character it failed to remove.
				in := "a" + string(r) + "b"
				got := c.clean(in)
				if err := checkString(c.name, got, 0, c.maxChars, c.rule); err != nil {
					t.Fatalf("U+%04X: the cleaned value does not pass: %v", r, err)
				}
				if c.rule.refusesIn(got) {
					t.Fatalf("U+%04X: the cleaned value still holds a refused character", r)
				}
				if want := !c.rule.refusesIn(in); want != (got == in) {
					t.Fatalf("U+%04X: refused=%v but cleaning %s the string", r, !want,
						map[bool]string{true: "left", false: "changed"}[got == in])
				}
			}
		})
	}
}

// The per-character rule and the per-string check give one verdict, for all four rules, over
// every code point there is.
//
// They are two pieces of code that read the same ADR 4 decision 9 lists, and only one of them
// (checkString) is what Validate runs, so a cleaner built on the other could remove the wrong
// characters and every cleaned value would still pass. contentChars is the case that makes this
// worth a test: checkString finds its one forbidden character as a byte without decoding, and
// refuses had to grow an arm of its own to answer for it.
func TestTheCharacterRulesAgreeWithValidation(t *testing.T) {
	for _, rule := range []charRule{identifierChars, displayChars, oneLineChars, contentChars} {
		for r := rune(0); r <= utf8.MaxRune; r++ {
			if r >= 0xD800 && r <= 0xDFFF {
				continue
			}
			refused := checkString("f", string(r), 0, MaxText, rule) != nil
			if refused != rule.refuses(r) {
				t.Fatalf("rule %d, U+%04X: checkString says refused=%v, refuses says %v",
					rule, r, refused, rule.refuses(r))
			}
		}
	}
}

// Cleaning is idempotent: a value that has been through it once is unchanged by a second pass.
// Without that, a normalizer that cleans a field and a later stage that cleans it again would
// produce two different strings, and the record id hashes none of these fields but the title and
// the display name are what an operator reads back.
func TestCleaningIsIdempotent(t *testing.T) {
	for _, c := range cleaners {
		t.Run(c.name, func(t *testing.T) {
			for _, in := range hostileStrings() {
				once := c.clean(in)
				if twice := c.clean(once); twice != once {
					t.Errorf("%q: first pass %q, second pass %q", in, once, twice)
				}
			}
		})
	}
}

// hostileStrings is what real data has in it: the examples the round 1 review of #43 named, plus
// the lengths and the bytes that are not characters at all.
func hostileStrings() []string {
	return []string{
		"",
		"ben",
		"ben\u200b",            // a pasted zero-width space
		"\u200fben",            // a right-to-left mark
		"ben\u00adjamin",       // a soft hyphen
		"a\tb",                 // a tab
		"a\nb",                 // a line break
		"a\r\nb",               // a folded header
		"a\u2028b",             // a line separator
		"   ",                  // only whitespace
		"\u202eevil",           // a right-to-left override
		"ben\x00jamin",         // a NUL
		"ben\xffjamin",         // not UTF-8
		"\u200d\u200c",         // the joiners, which a display name may keep
		strings.Repeat("a", 3), // ordinary
	}
}

// A cleaned value is short enough for its field, however long it arrives.
func TestCleaningCutsAValueDownToItsLimit(t *testing.T) {
	for _, c := range cleaners {
		t.Run(c.name, func(t *testing.T) {
			// Two bytes per rune, so that a cleaner counting bytes rather than runes is caught.
			in := strings.Repeat("\u00e9", c.maxChars+17)
			got := c.clean(in)
			if n := utf8.RuneCountInString(got); n != c.maxChars {
				t.Errorf("cleaned to %d characters, want %d", n, c.maxChars)
			}
			if err := checkString(c.name, got, 0, c.maxChars, c.rule); err != nil {
				t.Errorf("the cleaned value does not pass: %v", err)
			}
		})
	}
}

// What cleaning does to a title, stated here because the guide states it: a line break, a tab and
// every other character the one-line rule refuses becomes a space, and the edges are trimmed, so
// a title that was only whitespace is empty and a title is never padded.
func TestCleanTitleTurnsWhatItRefusesIntoASpace(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a\nb", "a b"},
		{"a\r\nb", "a  b"},
		{"a\tb", "a b"},
		{"a\u2028b", "a b"},
		{"  padded  ", "padded"},
		{"\t\n ", ""},
		{"", ""},
		{"ben\u200bjamin", "ben\u200bjamin"}, // a title keeps what only a display name refuses
		{"\u202bright to left\u202c", "\u202bright to left\u202c"},
	} {
		if got := CleanTitle(tc.in); got != tc.want {
			t.Errorf("CleanTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A display name keeps the two joiners, because Persian and Indic names are spelled with them and
// emoji are built with them, and loses every other invisible character.
func TestCleanDisplayKeepsTheJoiners(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"\u0646\u200c\u0627", "\u0646\u200c\u0627"},
		{"\U0001F468\u200d\U0001F469", "\U0001F468\u200d\U0001F469"},
		{"ben\u200bjamin", "benjamin"},
		{"\u202eneb", "neb"},
		{"a\nb", "ab"}, // removed, not replaced: a display name is not a line of content
		{"  padded  ", "  padded  "},
	} {
		if got := CleanDisplay(tc.in); got != tc.want {
			t.Errorf("CleanDisplay(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Text loses only the NUL, because everything else is content.
func TestCleanTextRemovesOnlyTheNUL(t *testing.T) {
	in := "line one\nline two\t\u202eand a bidi override\x00"
	want := "line one\nline two\t\u202eand a bidi override"
	if got := CleanText(in); got != want {
		t.Errorf("CleanText(%q) = %q, want %q", in, got, want)
	}
}

// Bytes that are not UTF-8 become U+FFFD rather than disappearing, so that a value which was
// something is not silently nothing, and the result is a string encoding/json can write.
func TestCleaningReplacesBytesThatAreNotUTF8(t *testing.T) {
	for _, c := range cleaners {
		got := c.clean("a\xffb")
		if !utf8.ValidString(got) {
			t.Errorf("%s: cleaned to something that is not UTF-8: %q", c.name, got)
		}
		if !strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("%s: cleaned %q, want the bad byte as U+FFFD", c.name, got)
		}
	}
}

// There is no cleaner for an identifier, and there must never be one. An identifier is compared,
// not read: removing a character from one makes it a different id, so two versions of one entity
// would get two external ids and a record would be delivered twice. A sender-controlled
// identifier needs an injective encoding or a hash of its own (the correction on #16), which is
// not what these helpers do.
//
// The test reads this package's own exported surface rather than a list, so that a fourth
// cleaner added later has to be named here or fail.
func TestThereIsNoCleanerForAnIdentifier(t *testing.T) {
	named := map[string]bool{}
	for _, c := range cleaners {
		named[c.name] = true
	}
	for _, want := range []string{"display", "title", "text"} {
		if !named[want] {
			t.Errorf("the cleaner table lost its %q row", want)
		}
	}
	if len(cleaners) != 3 {
		t.Errorf("there are %d cleaners: a new one must be a field class the format refuses "+
			"something in, and never an identifier", len(cleaners))
	}
}
