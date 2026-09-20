package record

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"
	"unicode/utf8"
)

// Which characters a field may hold (ADR 4), stated here as tables of code point ranges and not
// taken from validate.go, so that the test below holds the rule and not the code's copy of it.
// Every code point in this file is written as a number: an editor or a tool that turns an escape
// into the character itself would otherwise put an invisible character into the source.

type runeRange struct{ first, last rune }

var (
	controlRanges = []runeRange{{0x0000, 0x001F}, {0x007F, 0x009F}}

	refusedInAnIdentifier = append([]runeRange{
		{0x00AD, 0x00AD}, // soft hyphen
		{0x061C, 0x061C}, // Arabic letter mark
		{0x200B, 0x200F}, // zero-width space, non-joiner, joiner, left-to-right and right-to-left mark
		{0x2028, 0x202E}, // line and paragraph separator, the embeddings and overrides
		{0x2060, 0x206F}, // word joiner, invisible operators, the isolates, deprecated format characters
		{0xFEFF, 0xFEFF}, // byte order mark
	}, controlRanges...)

	// A display name keeps the zero-width non-joiner and joiner, U+200C and U+200D.
	refusedInADisplayName = append([]runeRange{
		{0x00AD, 0x00AD}, {0x061C, 0x061C}, {0x200B, 0x200B}, {0x200E, 0x200F},
		{0x2028, 0x202E}, {0x2060, 0x206F}, {0xFEFF, 0xFEFF},
	}, controlRanges...)

	refusedOnOneLine = append([]runeRange{{0x2028, 0x2029}}, controlRanges...)

	refusedInContent = []runeRange{{0x0000, 0x0000}}
)

type charField struct {
	path    string
	refused []runeRange
	// value puts the character under test into a value that is otherwise fine for the field.
	value func(c string) string
	// assign puts the value into a Go record.
	assign func(r *Record, v string)
}

func charFields() []charField {
	plain := func(c string) string { return "a" + c + "b" }
	prefixed := func(c string) string { return "slack:a" + c + "b" }
	return []charField{
		{"external_id", refusedInAnIdentifier, prefixed, func(r *Record, v string) { r.ExternalID = v }},
		{"version", refusedInAnIdentifier, plain, func(r *Record, v string) { r.Version = v }},
		{"author.id", refusedInAnIdentifier, plain, func(r *Record, v string) { r.Author.ID = v }},
		{"container.id", refusedInAnIdentifier, plain, func(r *Record, v string) { r.Container.ID = v }},
		{"edges.reply_parent", refusedInAnIdentifier, prefixed, func(r *Record, v string) { r.Edges.ReplyParent = Ref(v) }},
		{"meta.delivery", refusedInAnIdentifier, plain, func(r *Record, v string) { r.Meta.Delivery = v }},
		{"author.display", refusedInADisplayName, plain, func(r *Record, v string) { r.Author.Display = v }},
		{"title", refusedOnOneLine, plain, func(r *Record, v string) { r.Title = v }},
		{"text", refusedInContent, plain, func(r *Record, v string) { r.Text = v }},
	}
}

// charsUnderTest is every code point near a rule, and then some: all of U+0000 to U+02FF (ASCII,
// the C1 controls, the soft hyphen), the Arabic block, the Mongolian vowel separator's
// neighbourhood, all of General Punctuation and what follows it (U+2000 to U+20FF), the last two
// hundred code points of the BMP (the byte order mark, the specials), and astral characters: an
// emoji, a tag character, the last code point.
func charsUnderTest() []rune {
	var out []rune
	for _, r := range []runeRange{{0x0000, 0x02FF}, {0x0600, 0x06FF}, {0x1800, 0x180F}, {0x2000, 0x20FF}, {0xFE00, 0xFFFF}} {
		for c := r.first; c <= r.last; c++ {
			out = append(out, c)
		}
	}
	return append(out, 0xD7FF, 0xE000, 0x1F600, 0xE0001, 0xE0041, 0x10FFFF)
}

func inRanges(c rune, ranges []runeRange) bool {
	for _, r := range ranges {
		if c >= r.first && c <= r.last {
			return true
		}
	}
	return false
}

// Every character under test, in every field that has a character rule, through the schema, the
// strict decoder and Validate. One character per document, because a refusal hides every other
// character in the same value. This is a sample, and what it proves is the wiring: that each
// field is held to its own rule on all three sides. That the rules themselves agree on every
// code point there is, and not only on the sample, is TestEveryCodePointInGoAndInThePatterns.
func TestEveryCharacterRuleInGoAndInTheSchema(t *testing.T) {
	// One compilation is enough here: a character rule is a pattern, which does not depend on
	// whether formats are asserted.
	with, _ := schemas(t)
	chars := charsUnderTest()
	base := sealed(t)
	for _, field := range charFields() {
		t.Run(field.path, func(t *testing.T) {
			t.Parallel()
			refusals := 0
			for _, c := range chars {
				want := !inRanges(c, field.refused)
				if !want {
					refusals++
				}
				what := fmt.Sprintf("U+%04X", c)
				value := field.value(string(c))

				doc := exampleDoc(t)
				set(field.path, value)(doc)
				data, err := json.Marshal(doc)
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				if err := schemaError(t, with, data); (err == nil) != want {
					t.Errorf("%s: the schema accepted = %v, want %v", what, err == nil, want)
				}
				var decoded Record
				if err := json.Unmarshal(data, &decoded); (err == nil) != want {
					t.Errorf("%s: the decoder accepted = %v, want %v (%v)", what, err == nil, want, err)
				}

				r := base
				field.assign(&r, value)
				if err := r.Validate(); (err == nil) != want {
					t.Errorf("%s: Validate accepted = %v, want %v (%v)", what, err == nil, want, err)
				}
			}
			// The ranges above, counted by hand, so that a table that lost a row is noticed.
			wantRefusals := map[string]int{"text": 1, "title": 67, "author.display": 94}[field.path]
			if wantRefusals == 0 {
				wantRefusals = 96 // an identifier
			}
			if refusals != wantRefusals {
				t.Errorf("%d of the characters under test are refused, want %d", refusals, wantRefusals)
			}
		})
	}
}

// The four character rules over all of Unicode, which ADR 4 promises is run in full: for every
// code point the schema's pattern (compiled as it is after JSON decoding, which is what a sink's
// validator is handed), the Go rule and the table of this file give one verdict. A rule tightened
// or loosened on one side only, anywhere from U+0000 to U+10FFFF, fails here. After v0.1.0 that
// would be a silent break of a frozen contract: a document the schema refuses that Go accepts,
// or the reverse.
//
// The surrogates U+D800 to U+DFFF are left out because no string holds one: a Go string, a
// regular expression and a record document are all UTF-8 (ADR 4 decision 8 refuses the escape of
// a lone one before anything is parsed).
func TestEveryCodePointInGoAndInThePatterns(t *testing.T) {
	var schema struct {
		Defs       map[string]struct{ Not struct{ Pattern string } } `json:"$defs"`
		Properties map[string]struct{ Not struct{ Pattern string } }
	}
	if err := json.Unmarshal(Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	rules := []struct {
		name    string
		pattern string
		rule    charRule
		refused []runeRange
		// wantRefused is the size of the table, counted by hand from ADR 4 (32 C0 controls, DEL
		// and 32 C1 controls, then for an identifier 1, 1, 5, 7, 16 and 1), so that a table and
		// both implementations changed together are still noticed.
		wantRefused int
	}{
		{"identifier", schema.Defs["identifier"].Not.Pattern, identifierChars, refusedInAnIdentifier, 96},
		{"displayName", schema.Defs["displayName"].Not.Pattern, displayChars, refusedInADisplayName, 94},
		{"oneLine", schema.Defs["oneLine"].Not.Pattern, oneLineChars, refusedOnOneLine, 67},
		{"text", schema.Properties["text"].Not.Pattern, contentChars, refusedInContent, 1},
	}
	for _, rule := range rules {
		t.Run(rule.name, func(t *testing.T) {
			t.Parallel()
			if rule.pattern == "" {
				t.Fatal("the schema has no such pattern, the test is not seeing the schema")
			}
			pattern, err := regexp.Compile(rule.pattern)
			if err != nil {
				t.Fatal(err)
			}
			refused, failures := 0, 0
			buf := make([]byte, 0, utf8.UTFMax)
			for c := rune(0); c <= utf8.MaxRune; c++ {
				if c >= 0xD800 && c <= 0xDFFF {
					continue
				}
				buf = utf8.AppendRune(buf[:0], c)
				want := inRanges(c, rule.refused)
				if want {
					refused++
				}
				bySchema := pattern.Match(buf)
				byGo := checkString("field", string(buf), 1, 1, rule.rule) != nil
				if bySchema != want || byGo != want {
					t.Errorf("U+%04X: the schema refuses = %v, Go refuses = %v, ADR 4 refuses = %v", c, bySchema, byGo, want)
					if failures++; failures == 20 {
						t.Fatal("and more")
					}
				}
			}
			if refused != rule.wantRefused {
				t.Errorf("the table refuses %d code points, want %d", refused, rule.wantRefused)
			}
		})
	}
}

// notRefused is what ADR 4 says the character rules deliberately do NOT refuse ("The lists are
// fixed code points, never a Unicode category"), each as a case with a name. They are invisible
// or nearly so, which makes each of them an invitation to "close the gap" later, and within v1
// that is forbidden in both directions: refusing one on the Go side only makes Go refuse what
// the schema accepts, and refusing one in the schema narrows a frozen pattern. Whoever wants to
// tighten a rule has to delete a test with a name first, and read this comment on the way.
//
// Every code point is a number, see the top of this file.
func notRefused() []struct {
	name   string
	assign func(*Record)
} {
	const (
		tagBegin, tagA, tagCancel = rune(0xE0001), rune(0xE0041), rune(0xE007F)
		blackFlag                 = rune(0x1F3F4)
		variation16, variation17  = rune(0xFE0F), rune(0xE0100) // the emoji selector, and the first of the supplement
		heart, ideograph          = rune(0x2764), rune(0x8FBB)
		choseongFiller            = rune(0x115F)
		jungseongFiller           = rune(0x1160)
		hangulFiller              = rune(0x3164)
		halfwidthFiller           = rune(0xFFA0)
		ideographicSpace          = rune(0x3000)
		zwj                       = rune(0x200D)
		woman, laptop             = rune(0x1F469), rune(0x1F4BB)
	)
	id := func(c ...rune) string { return "slack:C" + string(c) + "1" }
	return []struct {
		name   string
		assign func(*Record)
	}{
		{"tag characters in an external id", func(r *Record) { r.ExternalID = id(tagBegin, tagA, tagCancel) }},
		{"tag characters in a version", func(r *Record) { r.Version = string([]rune{'1', tagA, '2'}) }},
		{"the flag of Scotland in a display name, which is spelled with tag characters", func(r *Record) {
			r.Author.Display = string([]rune{blackFlag, 0xE0067, 0xE0062, 0xE0073, 0xE0063, 0xE0074, tagCancel})
		}},
		{"a tag character in a title", func(r *Record) { r.Title = string([]rune{'a', tagA, 'b'}) }},
		{"a variation selector in an external id", func(r *Record) { r.ExternalID = id(variation16) }},
		{"a variation selector in an author id", func(r *Record) { r.Author.ID = string([]rune{'U', variation16, '1'}) }},
		{"an emoji with its variation selector in a display name", func(r *Record) { r.Author.Display = string([]rune{'b', 'e', 'n', ' ', heart, variation16}) }},
		{"an ideographic variation sequence in a display name", func(r *Record) { r.Author.Display = string([]rune{ideograph, variation17}) }},
		{"a variation selector in a title", func(r *Record) { r.Title = string([]rune{heart, variation16}) }},
		{"the Hangul filler U+3164 in an external id", func(r *Record) { r.ExternalID = id(hangulFiller) }},
		{"the Hangul choseong and jungseong fillers in a container id", func(r *Record) {
			r.Container.ID = string([]rune{'C', choseongFiller, jungseongFiller, '1'})
		}},
		{"the halfwidth Hangul filler in a delivery id", func(r *Record) { r.Meta.Delivery = string([]rune{'d', halfwidthFiller, '1'}) }},
		{"the Hangul filler U+3164 in a display name", func(r *Record) { r.Author.Display = string([]rune{'b', hangulFiller, 'n'}) }},
		{"the Hangul filler U+3164 in a title", func(r *Record) { r.Title = string([]rune{'a', hangulFiller, 'b'}) }},
		{"the ideographic space U+3000 in an external id", func(r *Record) { r.ExternalID = id(ideographicSpace) }},
		{"the ideographic space U+3000 in a reply parent", func(r *Record) { r.Edges.ReplyParent = Ref(id(ideographicSpace)) }},
		{"the ideographic space U+3000 in a display name", func(r *Record) { r.Author.Display = string([]rune{'a', ideographicSpace, 'b'}) }},
		{"the ideographic space U+3000 in a title", func(r *Record) { r.Title = string([]rune{'a', ideographicSpace, 'b'}) }},
		{"the zero-width joiner in a display name, in a profession emoji", func(r *Record) { r.Author.Display = string([]rune{woman, zwj, laptop}) }},
	}
}

// The rules the tables cannot show: the characters a rule must leave alone, by name.
func TestWhatTheCharacterRulesLeaveAlone(t *testing.T) {
	const (
		zwnj, zwj           = rune(0x200C), rune(0x200D)
		lrm, rlm            = rune(0x200E), rune(0x200F)
		rlo, pdf, rli, pdi  = rune(0x202E), rune(0x202C), rune(0x2067), rune(0x2069)
		alef, bet           = rune(0x05D0), rune(0x05D1)
		heh, alefAr         = rune(0x0647), rune(0x0627)
		emojiMan, emojiGirl = rune(0x1F468), rune(0x1F467)
	)
	keeps := []struct {
		name   string
		assign func(*Record)
	}{
		{"a Persian name with a zero-width non-joiner", func(r *Record) { r.Author.Display = string([]rune{heh, zwnj, alefAr}) }},
		{"a display name with a family emoji", func(r *Record) { r.Author.Display = string([]rune{emojiMan, zwj, emojiGirl}) }},
		{"a right-to-left title with marks and isolates", func(r *Record) { r.Title = string([]rune{rli, alef, bet, pdi, lrm, ' ', rlm, '1'}) }},
		{"a title with an override, which is content", func(r *Record) { r.Title = string([]rune{'a', rlo, 'b', pdf}) }},
		{"a text with every kind of line break and an escape character", func(r *Record) {
			r.Text = string([]rune{'a', '\n', '\r', '\t', 0x0B, 0x0C, 0x1B, 0x85, 0x2028, 0x2029, rlo, 0xFEFF, 'b'})
		}},
	}
	for _, k := range keeps {
		r := sealed(t)
		k.assign(&r)
		if err := r.Validate(); err != nil {
			t.Errorf("%s: %v", k.name, err)
		}
	}
	with, _ := schemas(t)
	for _, k := range notRefused() {
		t.Run(k.name, func(t *testing.T) {
			r := draft()
			k.assign(&r)
			r, err := r.Seal(testProvider, testTenant)
			if err != nil {
				t.Fatalf("Seal refuses it: %v", err)
			}
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatalf("Marshal refuses it: %v", err)
			}
			if err := schemaError(t, with, data); err != nil {
				t.Errorf("the schema refuses it: %v", err)
			}
			var back Record
			if err := json.Unmarshal(data, &back); err != nil {
				t.Errorf("the decoder refuses it: %v", err)
			}
		})
	}
	refuses := []struct {
		name   string
		assign func(*Record)
	}{
		{"an override in a display name", func(r *Record) { r.Author.Display = string([]rune{'b', 'e', 'n', rlo, 'n', 'i', 'm', 'd', 'a'}) }},
		{"an isolate in a display name", func(r *Record) { r.Author.Display = string([]rune{rli, 'b', 'e', 'n', pdi}) }},
		{"a zero-width joiner in an author id", func(r *Record) { r.Author.ID = string([]rune{'U', zwj, '1'}) }},
		{"a zero-width non-joiner in an external id", func(r *Record) { r.ExternalID = "slack:" + string([]rune{'C', zwnj, '1'}) }},
		{"a next line character in an external id", func(r *Record) { r.ExternalID = "slack:" + string([]rune{'C', 0x85, '1'}) }},
		{"a line separator in a version", func(r *Record) { r.Version = string([]rune{'1', 0x2028, '2'}) }},
		{"a tab in a title", func(r *Record) { r.Title = "a\tb" }},
		{"a line feed in a title", func(r *Record) { r.Title = "a\nb" }},
		{"a next line character in a title", func(r *Record) { r.Title = string([]rune{'a', 0x85, 'b'}) }},
	}
	for _, k := range refuses {
		r := sealed(t)
		k.assign(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted", k.name)
		}
	}
}
