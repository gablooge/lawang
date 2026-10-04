package record

import (
	"strings"
	"unicode/utf8"
)

// The format refuses, it never repairs (ADR 4, decision 9). These are the other half of that
// rule: one function per cleanable field class, which removes or replaces exactly what the
// matching validation rule refuses, so that a normalizer can hand real data to Seal.
//
// Real data has all of it. A display name carries a pasted zero-width space, a task name carries
// a tab, a folded mail subject carries a line break, and a provider that passes any of them
// through turns a real event into a dead letter. Every normalizer therefore cleans
// author.display, title and text before it builds a Record, and the provider conformance harness
// (internal/provider/providertest) fails a normalizer that forgets.
//
// **There is no cleaner for an identifier, and there must never be one.** An external id, a
// version, an author id, a container id and a reply parent are compared, not read: removing a
// character from one makes it a different id, so one entity would get two external ids, be
// delivered twice, and look to the ledger like a move. An identifier a sender controls needs an
// injective encoding or a hash of its own, which is not what these functions do. A provider id
// that the identifier rule refuses is a refusal, never something to clean around.
//
// Each function is idempotent, bounds its result to the field's limit in characters, and
// replaces bytes that are not UTF-8 with U+FFFD rather than dropping them, so that a value which
// was something does not become silently nothing. A value that has been through the matching
// function always passes Validate for that field, and TestCleaningAgreesWithTheRuleOnEveryCodePoint
// holds the two together over every code point there is rather than over a list.

// CleanDisplay returns s as Author.Display may hold it: the characters displayChars refuses are
// REMOVED, and the zero-width joiner and non-joiner are kept, because Persian and Indic names are
// spelled with them and emoji are built with them.
//
// They are removed and not replaced with a space, because a display name is a name and not a line
// of content: a pasted zero-width space inside a name should leave the name, and a space in its
// place would make a second word. The result is at most MaxAuthor characters.
func CleanDisplay(s string) string { return clean(s, displayChars, MaxAuthor, -1) }

// CleanTitle returns s as Title may hold it: one line, at most MaxTitle characters.
//
// Every character oneLineChars refuses (a tab, a line break, a line or paragraph separator, any
// other control character) becomes **a space**, and the result is then trimmed of leading and
// trailing spaces. A line break is a word boundary in a title, so dropping it would join two
// words into one, and a title that was only whitespace becomes empty, which the format allows.
// Bidirectional formatting is kept, because right-to-left titles need it, which is why a title
// is still not safe to display as it stands (ADR 4, decision 9).
func CleanTitle(s string) string { return strings.Trim(clean(s, oneLineChars, MaxTitle, ' '), " ") }

// CleanText returns s as Text may hold it: everything but the NUL, which a Postgres text column
// cannot store, and at most MaxText characters. Newlines, tabs and bidirectional formatting are
// content and stay. Cutting a long text down is the normalizer's job (the format refuses and does
// not truncate), and this is where it is done.
func CleanText(s string) string { return clean(s, contentChars, MaxText, -1) }

// clean removes every character rule refuses, or replaces it with replacement when that is not
// negative, turns every byte that is not UTF-8 into U+FFFD, and cuts the result to maxChars
// characters.
//
// A byte that is not UTF-8 needs no arm of its own: DecodeRuneInString already hands it over as
// U+FFFD, and no rule refuses that character, so it is written once per bad byte. That is not a
// coincidence to rely on silently, so TestCleaningReplacesBytesThatAreNotUTF8 asserts the result
// still holds it, and a rule that ever did refuse U+FFFD would fail there rather than quietly
// delete a value's bad bytes.
//
// The cut is last, because removing a character can only shorten the string and a cut before it
// could leave a value longer than the limit. It counts characters and not bytes, which is what
// checkString and the schema's maxLength count.
func clean(s string, rule charRule, maxChars int, replacement rune) string {
	if ok, short := rule.permits(s); ok && short <= maxChars {
		return s // the common case allocates nothing
	}
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	for i := 0; i < len(s); {
		c, width := utf8.DecodeRuneInString(s[i:])
		i += width
		if rule.refuses(c) {
			if replacement < 0 {
				continue
			}
			c = replacement
		}
		if n == maxChars {
			break
		}
		b.WriteRune(c)
		n++
	}
	return b.String()
}

// permits reports whether s holds nothing rule refuses and is valid UTF-8, and how many
// characters it has. The count is only correct when the first answer is true, which is the only
// case that uses it.
func (rule charRule) permits(s string) (bool, int) {
	n := 0
	for i := 0; i < len(s); {
		c, width := utf8.DecodeRuneInString(s[i:])
		if c == utf8.RuneError && width == 1 || rule.refuses(c) {
			return false, 0
		}
		i += width
		n++
	}
	return true, n
}

// refusesIn reports whether s holds a character rule refuses, or a byte that is not UTF-8. It is
// the question checkString asks, without the limits, and the tests of this file ask it too.
func (rule charRule) refusesIn(s string) bool {
	ok, _ := rule.permits(s)
	return !ok
}
