package pipeline

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// secretKind is a class of value the masker recognizes. The three of them are the conservative regex
// baseline of architecture section 7: an email address, a telephone number and an IBAN. The
// strings are what the redaction_map.kind column holds, and its CHECK is the same closed set.
type secretKind string

// The kinds the masker recognizes.
const (
	kindEmail secretKind = "email"
	kindPhone secretKind = "phone"
	kindIBAN  secretKind = "iban"
)

// foundSecret is one value the masker found, and the unit the redaction map is keyed by: one token
// stands for one (kind, value) within one tenant, so the same address reads as the same
// placeholder everywhere in that tenant's records.
type foundSecret struct {
	Kind  secretKind
	Value string
}

// ErrMaskedTooLong reports a field that masking pushed past the record format's limit.
//
// A placeholder is longer than most of what it replaces, so a title or a text that the normalizer
// cut to exactly the limit can outgrow it here. The alternative to failing would be to cut the
// masked text down, which loses somebody's content silently, or to leave the field unmasked,
// which is the one thing this stage exists to prevent. So it fails, and the fix is on the
// normalizer: cut the text further.
var ErrMaskedTooLong = errors.New("pipeline: masking made the field longer than the format allows")

// maxSecretBytes is the longest value the masker will keep. It is the redaction_map.value CHECK,
// and every pattern below is bounded well under it; a candidate longer than this is not one of
// the three things the masker knows.
const maxSecretBytes = 512

// An email address, conservatively. The local part is the characters that are common in real
// addresses rather than everything RFC 5322 permits, and the domain must have at least one dot
// and end in two or more letters, which is what keeps "user@example" and "@handle" out.
//
// It matches inside a URL and inside a display name on purpose ("https://x.test/u/jane@a.test",
// "Jane Doe <jane@a.test>"): an address is an address wherever it is written, and the characters
// that begin those contexts ("/", "<") are not in the local-part class, so the match starts at
// the address.
var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@(?:[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,24}`)

// A telephone number, in the two spellings worth recognizing:
//
//   - international: a plus, a country code, then two to five groups of two to four digits,
//     with at most two separator characters between groups ("+62 812-3456-7890",
//     "+1 (555) 010-1234", "+442079460958");
//   - grouped national: three or four digits, optionally in parentheses, then two or three more
//     such groups, each behind one separator ("0812-3456-7890", "(555) 010-1234",
//     "555.010.1234").
//
// The pattern is the shape only, and it is deliberately loose: a bare run of digits is never a
// phone number here, whatever its length (an id, an epoch and an amount are bare runs of digits
// too), but everything else is left to validPhone, which is where the rules that keep an address,
// a version and a date out actually live. Read that function before trusting this one.
var phoneRe = regexp.MustCompile(
	`\+[1-9][0-9]{0,3}(?:[ .()\-]{0,2}[0-9]{2,4}){2,5}` +
		`|\(?[0-9]{3,4}\)?[ .\-][0-9]{3,4}[ .\-][0-9]{3,4}(?:[ .\-][0-9]{2,4})?`)

// An IBAN, in the two spellings a person writes: all together, or in groups of four separated by
// single spaces. Uppercase only, which is how an IBAN is written.
//
// The pattern is only the shape. What decides is the ISO 7064 mod-97 check below, which is why
// this pattern can afford to be loose: a string of the right shape passes only if its check
// digits are right, so the masker does not have to guess whether an uppercase token is a bank
// account.
var ibanRe = regexp.MustCompile(`[A-Z]{2}[0-9]{2}(?: [A-Z0-9]{4}){2,7}(?: [A-Z0-9]{1,3})?|[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}`)

// span is one stretch of a string that will be replaced.
type span struct {
	start, end int
	secret     foundSecret
}

// textScan is one string together with everything the masker found in it. Finding and replacing
// are two steps because the token that replaces a value comes from the database (see redactor): the
// pure part is here and is tested on its own, with no database in sight.
type textScan struct {
	text  string
	spans []span
}

// findSecrets returns what the masker recognizes in text.
//
// Overlaps are resolved by taking the match that starts earliest. That is what keeps the digit
// groups of an IBAN from also being read as a telephone number, and it holds whatever order the
// patterns happen to be tried in.
//
// Two matches that begin at the same byte would be decided by the order they were appended in
// (email, then IBAN, then phone), which is why the sort is stable. No input is known that produces
// one: an address and an account number begin with different characters, and where one pattern's
// match would begin where another's does, the boundary rule has already refused it.
func findSecrets(text string) textScan {
	var found []span
	// An address is not boundary checked, because the contexts it hides in end in exactly the
	// characters a boundary check would refuse: the slash of a URL path and the angle bracket of a
	// display name. Its own character classes already keep it from starting inside a word, since
	// the local part is greedy leftwards.
	found = appendMatches(found, text, emailRe, kindEmail, nil, false)
	found = appendMatches(found, text, ibanRe, kindIBAN, validIBAN, true)
	found = appendMatches(found, text, phoneRe, kindPhone, validPhone, true)
	sort.SliceStable(found, func(i, j int) bool { return found[i].start < found[j].start })
	kept := make([]span, 0, len(found))
	end := 0
	for _, s := range found {
		if s.start < end {
			continue // overlaps something already kept, which started earlier or is longer
		}
		kept = append(kept, s)
		end = s.end
	}
	return textScan{text: text, spans: kept}
}

// appendMatches adds every match of re that ok accepts, with the boundary rule applied when
// bounded is set.
func appendMatches(dst []span, text string, re *regexp.Regexp, kind secretKind, ok func(string) bool, bounded bool) []span {
	for _, m := range re.FindAllStringIndex(text, -1) {
		v := text[m[0]:m[1]]
		// A value longer than the redaction map can store is not one of the three things the
		// masker knows, and taking it would leave a value apply has no token for.
		if len(v) > maxSecretBytes {
			continue
		}
		if bounded && !boundedBy(text, m[0], m[1]) {
			continue
		}
		if ok != nil && !ok(v) {
			continue
		}
		dst = append(dst, span{start: m[0], end: m[1], secret: foundSecret{Kind: kind, Value: v}})
	}
	return dst
}

// boundedBy reports whether the match at [start,end) stands on its own rather than in the middle
// of a longer token. Without it a pattern that matched a prefix would mask part of something else:
// three digits of a hash, the first labels of a hostname.
//
// It is a byte test and not a rune test on purpose: every character either side that could
// continue one of these three patterns is ASCII, and a multi-byte rune's bytes are all >= 0x80,
// which none of the sets below holds.
func boundedBy(text string, start, end int) bool {
	if start > 0 {
		switch c := text[start-1]; {
		case isAlnum(c), c == '/', c == '@', c == '_', c == '+', c == '.', c == '-':
			return false
		}
	}
	if end < len(text) {
		switch c := text[end]; {
		case isAlnum(c), c == '/', c == '@', c == '_':
			return false
		case (c == '.' || c == '-') && end+1 < len(text) && isDigit(text[end+1]):
			// A dotted quad or a longer dashed run that the pattern only matched the front of.
			return false
		}
	}
	return true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isAlnum(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// validPhone holds every rule the pattern cannot express. It is the whole of the phone rule, and
// the pattern above only narrows what it has to look at.
//
// A leading plus is a country code, and a country code is what makes a run of grouped digits a
// telephone number rather than something else that is written the same way. So the international
// form needs nothing but E.164's own bounds: at most 15 digits, and at least eight, because
// nothing shorter is a number somebody could be called on.
//
// The national form has no country code, so it has to be told apart from the other things people
// write as groups of digits, and getting that wrong is expensive in one direction: an over-reach
// replaces somebody's text with a placeholder, the sink never sees the original and the value is
// written into redaction_map, which the migration calls personal data by construction. A miss only
// costs a number reaching the sink. So the national form is narrow on purpose:
//
//   - nine to fifteen digits, since eight grouped digits is a date as often as a number;
//   - EXACTLY THREE groups. Four groups is a dotted quad (192.168.100.200, 172.217.169.110) or a
//     build number, and nobody writes a national telephone number in four groups;
//   - AT LEAST ONE group of exactly four digits. Every national spelling has one (0812-3456-7890,
//     (555) 010-1234, 555.010.1234), and three groups of three digits is an address
//     (100.200.300), an invoice total (123.456.789) or a version;
//   - a FIRST GROUP THAT IS NOT A YEAR. 2024.100.200 and 2026-0921-1234 have the shape of a
//     number and read as a release and a reference; no national numbering plan starts an area
//     code with 19 or 20 and four digits.
//
// What this gives up is a real number whose first group happens to read as a year, which reaches
// the sink unmasked. ADR 12 takes that trade explicitly: the masker is a conservative baseline and
// what it misses is recoverable, while what it destroys is not.
func validPhone(s string) bool {
	groups := digitGroups(s)
	digits := 0
	for _, g := range groups {
		digits += len(g)
	}
	if strings.HasPrefix(s, "+") {
		return digits >= 8 && digits <= 15
	}
	if digits < 9 || digits > 15 {
		return false
	}
	if len(groups) != 3 {
		return false
	}
	if !slices.ContainsFunc(groups, func(g string) bool { return len(g) == 4 }) {
		return false
	}
	return !looksLikeAYear(groups[0])
}

// digitGroups is the runs of decimal digits in s, in order. Everything between them is a
// separator, which is the only thing the phone patterns put there.
func digitGroups(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		if !isDigit(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isDigit(s[j]) {
			j++
		}
		out = append(out, s[i:j])
		i = j
	}
	return out
}

// looksLikeAYear reports whether g is four digits that read as a year of this era (1900 to 2099).
// A date, a release and a reference number all begin with one; a national area code does not.
func looksLikeAYear(g string) bool {
	return len(g) == 4 && (strings.HasPrefix(g, "19") || strings.HasPrefix(g, "20"))
}

// validIBAN is the ISO 7064 mod-97 check, which is what makes the masker's IBAN rule conservative:
// a string of the right shape is masked only when its check digits are right, so an uppercase
// token that merely looks like an account number is left alone.
func validIBAN(s string) bool {
	compact := strings.ReplaceAll(s, " ", "")
	if len(compact) < 15 || len(compact) > 34 {
		return false
	}
	// The check runs over the string rotated by four: country code and check digits move to the
	// end, and every letter becomes two digits (A is 10). The remainder of that number modulo 97
	// is 1 for a well formed IBAN.
	rotated := compact[4:] + compact[:4]
	rem := 0
	for i := range len(rotated) {
		switch c := rotated[i]; {
		case isDigit(c):
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			rem = (rem*100 + int(c-'A') + 10) % 97
		default:
			return false
		}
	}
	return rem == 1
}

// secrets is every distinct value the scan found, in the order they first appear. The caller maps
// each one to a token and hands the map back to apply.
func (s textScan) secrets() []foundSecret {
	seen := make(map[foundSecret]struct{}, len(s.spans))
	out := make([]foundSecret, 0, len(s.spans))
	for _, sp := range s.spans {
		if _, dup := seen[sp.secret]; dup {
			continue
		}
		seen[sp.secret] = struct{}{}
		out = append(out, sp.secret)
	}
	return out
}

// apply returns the text with every value replaced by its token, refusing a result longer than
// maxChars characters (the record format's limit for the field).
//
// A missing token is an error and never a value left in place: a masker that silently passed
// something through would be worse than no masker, because the record would look masked.
func (s textScan) apply(field string, tokens map[foundSecret]string, maxChars int) (string, error) {
	if len(s.spans) == 0 {
		return s.text, nil
	}
	var b strings.Builder
	b.Grow(len(s.text))
	at := 0
	for _, sp := range s.spans {
		token, ok := tokens[sp.secret]
		if !ok || token == "" {
			return "", fmt.Errorf("pipeline: no redaction token for a %s in %s", sp.secret.Kind, field)
		}
		b.WriteString(s.text[at:sp.start])
		b.WriteString(token)
		at = sp.end
	}
	b.WriteString(s.text[at:])
	out := b.String()
	if utf8.RuneCountInString(out) > maxChars {
		return "", fmt.Errorf("%w: %s", ErrMaskedTooLong, field)
	}
	return out, nil
}
