package record

import (
	"encoding/json"
	"fmt"
	"testing"
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
// character in the same value.
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
