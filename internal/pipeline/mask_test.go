package pipeline_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gablooge/lawang/internal/pipeline"
	"github.com/gablooge/lawang/internal/record"
)

// The fixture set the backlog's second "Done when" line refers to: "masked text contains no email,
// phone or IBAN from the fixture set".
//
// It is written to be awkward rather than convenient. An address hides inside a URL and inside a
// display name, a number is written the three ways people write one, and an IBAN appears with and
// without its spaces. Every pattern also has cases that must NOT be masked, because a test that
// only proves that secrets disappear is passed by a masker that returns a constant.
var (
	maskedEmails = []string{
		"jane.doe@example.test", // plain
		"https://intranet.example.test/u/jane.doe@example.test?tab=1", // inside a URL
		"Jane Doe <jane.doe@example.test>",                            // inside a display name
		"jane+lawang@example.test",                                    // a plus tag
		"JANE@EXAMPLE.TEST",                                           // upper case
		"a@b.co",                                                      // the shortest shape that counts
	}
	keptEmails = []string{
		"@janedoe",             // a handle, with no local part
		"postmaster@localhost", // no dot in the domain
		"deploy@v2",            // no dot at all
		"jane@example.t",       // a one letter top level domain
	}
	maskedPhones = []string{
		"+62 812-3456-7890", // an international prefix and separators
		"+442079460958",     // an international prefix and nothing else
		"+1 (555) 010-1234", // an international prefix, parentheses and separators
		"0812-3456-7890",    // national, hyphens
		"(555) 010-1234",    // national, parentheses and a space
		"555.010.1234",      // national, dots
	}
	keptPhones = []string{
		// Refused by the pattern, which needs a separator between groups and three digits in the
		// first group.
		"1752064245000",     // an epoch in milliseconds: digits, no separators
		"192.168.1.100",     // a dotted quad whose third octet is one digit
		"2026-09-21",        // a date: eight digits is not enough
		"1.0.0",             // a version
		"1752064245.000200", // a Slack timestamp
		"86a1xyz",           // a ClickUp task id

		// Refused by the digit count, which the pattern cannot express. Without these three the
		// rule would have no evidence outside its own unit test: every fixture above is already
		// refused a step earlier, so a masker that counted nothing would still pass.
		"+4420794",            // seven digits behind a country code
		"+4420794609581234",   // sixteen, one over what E.164 allows
		"1234 5678 9012 3456", // sixteen again, written as a person writes a number

		// IPv4 addresses, every one of which the pattern matches and validPhone refuses for having
		// four groups. These are the class the round 1 review found being replaced by a phone
		// token: for a connector whose first providers are a task tracker and a chat tool, "prod
		// is at 172.217.169.110" is ordinary content, and a masked address is gone from the sink
		// for good while the value sits in redaction_map.
		"192.168.100.200",      // every octet three digits
		"172.217.169.110",      // a public address, every octet three digits
		"198.051.100.042",      // zero-padded octets
		"192.168.100.200:8080", // an address and a port
		"10.0.0.1",             // the other end of the range, refused by the pattern

		// Three groups of three digits, which is not how anybody writes a telephone number and is
		// how people write plenty of other things.
		"100.200.300", // a commit or a build
		"123.456.789", // an invoice total written the Indonesian way

		// A first group that reads as a year: a date, a release, a reference.
		"2024.100.200",   // a version string
		"2026-0921-1234", // an order reference
		"1999-0102-0304", // the same shape in the other century

		// The classes the round 1 review asked for by name, which a pattern this shape has to be
		// held against: an order id, a version, a hash and a timestamp.
		"ORD-2026-0001-0042",   // an order id
		"v2026.100.200",        // a version with a prefix
		"e3b0c44298fc1c149afb", // a hash
		"2026-09-21T10:30:00Z", // a timestamp
		"1758448800.123456",    // a timestamp with microseconds
	}
	maskedIBANs = []string{
		"GB82 WEST 1234 5698 7654 32", // with spaces
		"GB82WEST12345698765432",      // without
		"NL91ABNA0417164300",
		"DE89 3704 0044 0532 0130 00",
	}
	keptIBANs = []string{
		// The same shape with the check digits changed. This is the case that proves the mod-97
		// check is doing the work: nothing about the string's shape says it is not an IBAN.
		"GB00WEST12345698765432",
		// An uppercase token of the right shape and the wrong checksum.
		"XX12ABCDEFGHIJKLMNOP",
	}
)

// tokensFor is a stand-in for the redaction map: a distinct, obviously artificial token per value.
// The database half is tested in TestTheRedactionMapKeepsOneTokenPerValue.
func tokensFor(scans ...pipeline.Scan) map[pipeline.Secret]string {
	out := map[pipeline.Secret]string{}
	for _, s := range scans {
		for _, sec := range s.Secrets() {
			if _, have := out[sec]; !have {
				out[sec] = fmt.Sprintf("[%s:TOKEN%d]", sec.Kind, len(out))
			}
		}
	}
	return out
}

func mask(t *testing.T, text string) string {
	t.Helper()
	scan := pipeline.Find(text)
	out, err := scan.Apply("text", tokensFor(scan), record.MaxText)
	if err != nil {
		t.Fatalf("Apply(%q): %v", text, err)
	}
	return out
}

// TestMaskedTextHoldsNoSecretFromTheFixtureSet is the backlog's second "Done when" line, one
// fixture at a time and then all of them in one text.
func TestMaskedTextHoldsNoSecretFromTheFixtureSet(t *testing.T) {
	t.Parallel()
	for _, group := range []struct {
		kind  pipeline.Kind
		texts []string
	}{
		{pipeline.KindEmail, maskedEmails},
		{pipeline.KindPhone, maskedPhones},
		{pipeline.KindIBAN, maskedIBANs},
	} {
		for _, text := range group.texts {
			got := mask(t, "before "+text+" after")
			if secret := theSecretIn(t, text, group.kind); strings.Contains(got, secret) {
				t.Errorf("masking %q left the %s in it: %q", text, group.kind, got)
			}
			if !strings.HasPrefix(got, "before ") || !strings.HasSuffix(got, " after") {
				t.Errorf("masking %q did not leave the text around it alone: %q", text, got)
			}
			if !strings.Contains(got, "["+string(group.kind)+":") {
				t.Errorf("masking %q put no %s token in: %q", text, group.kind, got)
			}
		}
	}

	// All of them in one text, which is the fixture set the "Done when" line names.
	var b strings.Builder
	for _, text := range concat(maskedEmails, maskedPhones, maskedIBANs) {
		fmt.Fprintf(&b, "line: %s\n", text)
	}
	got := mask(t, b.String())
	for _, text := range concat(maskedEmails, maskedPhones, maskedIBANs) {
		for _, kind := range []pipeline.Kind{pipeline.KindEmail, pipeline.KindPhone, pipeline.KindIBAN} {
			if s := secretIn(text, kind); s != "" && strings.Contains(got, s) {
				t.Errorf("the masked fixture set still holds the %s %q", kind, s)
			}
		}
	}
}

// TestTheMaskerLeavesWhatIsNotASecretAlone is the other half, and the half a masker that returns a
// constant fails. Every one of these is the shape of a secret and is not one.
func TestTheMaskerLeavesWhatIsNotASecretAlone(t *testing.T) {
	t.Parallel()
	for _, text := range concat(keptEmails, keptPhones, keptIBANs) {
		in := "before " + text + " after"
		if got := mask(t, in); got != in {
			t.Errorf("masking changed %q, which is not a secret, into %q", in, got)
		}
	}
}

// TestTheMaskerLeavesOrdinaryTextExactlyAsItIs. Without this a masker that replaced the whole
// string would pass every test above that only looks for what is gone.
func TestTheMaskerLeavesOrdinaryTextExactlyAsItIs(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"",
		"Ship the release on Friday.",
		"Deploy 3 replicas to eu-west-1 at 09:30, see runbook/deploy.md",
		"Harga: Rp 1.500 per unit",
		"ümläute und ein Emoji \U0001F600",
	} {
		if got := mask(t, text); got != text {
			t.Errorf("masking changed %q into %q", text, got)
		}
	}
}

// TestAnIBANSwallowsTheDigitsInsideIt. The digit groups of an IBAN are a telephone number to the
// phone pattern, so the two patterns overlap on purpose. The earlier match wins, which is why the
// overlap rule is by position and not by the order the patterns are tried in.
func TestAnIBANSwallowsTheDigitsInsideIt(t *testing.T) {
	t.Parallel()
	got := mask(t, "pay to GB82 WEST 1234 5698 7654 32 today")
	if strings.Contains(got, "phone") {
		t.Errorf("the digits of an IBAN were masked as a telephone number: %q", got)
	}
	if want := 1; strings.Count(got, "[iban:") != want {
		t.Errorf("got %q, want exactly %d iban token", got, want)
	}
}

// TestTheIBANRuleIsTheChecksumAndNotTheShape. The two strings differ in nothing but their check
// digits, so a masker that matched on shape alone would mask both and one that matched on nothing
// would mask neither. This is the pair that says the mod-97 check is what decides.
func TestTheIBANRuleIsTheChecksumAndNotTheShape(t *testing.T) {
	t.Parallel()
	const good, bad = "GB82WEST12345698765432", "GB00WEST12345698765432"
	if got := secretIn(good, pipeline.KindIBAN); got != good {
		t.Errorf("a valid IBAN was not recognized: got %q", got)
	}
	if got := secretIn(bad, pipeline.KindIBAN); got != "" {
		t.Errorf("an IBAN with the wrong check digits was masked as %q", got)
	}
}

// TestOneValueBecomesOneToken. The same address twice in one text is one secret and one token, so
// a reader of the record can still see that the two mentions are the same person.
func TestOneValueBecomesOneToken(t *testing.T) {
	t.Parallel()
	got := mask(t, "ask jane.doe@example.test, and copy jane.doe@example.test")
	tok := "[email:TOKEN0]"
	if n := strings.Count(got, tok); n != 2 {
		t.Errorf("got %q, want the same token twice", got)
	}
	if strings.Contains(got, "TOKEN1") {
		t.Errorf("one value got two tokens: %q", got)
	}
}

// TestTwoValuesNeverShareAToken, the other direction: two addresses must not collapse into one, or
// a record would say two people are one.
func TestTwoValuesNeverShareAToken(t *testing.T) {
	t.Parallel()
	got := mask(t, "jane.doe@example.test and john.roe@example.test")
	if !strings.Contains(got, "TOKEN0") || !strings.Contains(got, "TOKEN1") {
		t.Errorf("two addresses did not get two tokens: %q", got)
	}
}

// TestMaskingIsIdempotent. A token must not look like a secret to the masker, or a second pass
// would mask the masking.
func TestMaskingIsIdempotent(t *testing.T) {
	t.Parallel()
	once := mask(t, strings.Join(concat(maskedEmails, maskedPhones, maskedIBANs), " | "))
	if twice := mask(t, once); twice != once {
		t.Errorf("a second pass changed the masked text:\n once: %q\ntwice: %q", once, twice)
	}
}

// TestApplyRefusesAValueWithNoToken. Leaving a value in place because its token is missing would
// be the worst failure this package has: a record that looks masked and is not.
func TestApplyRefusesAValueWithNoToken(t *testing.T) {
	t.Parallel()
	scan := pipeline.Find("write to jane.doe@example.test")
	if _, err := scan.Apply("text", nil, record.MaxText); err == nil {
		t.Fatal("Apply with no tokens returned no error")
	}
	if _, err := scan.Apply("text", map[pipeline.Secret]string{
		{Kind: pipeline.KindEmail, Value: "jane.doe@example.test"}: "",
	}, record.MaxText); err == nil {
		t.Fatal("Apply with an empty token returned no error")
	}
}

// TestMaskingThatOutgrowsTheFieldIsRefused. A token is longer than a short address, so a title cut
// to exactly the limit can outgrow it here. Cutting it down again would lose somebody's content
// without saying so, and leaving it unmasked is the one thing this stage exists to prevent.
func TestMaskingThatOutgrowsTheFieldIsRefused(t *testing.T) {
	t.Parallel()
	const addr, tok = "a@b.co", "[email:TOKEN0]"
	// A title of exactly the limit, whose last word is an address: masking makes it eight
	// characters too long.
	tooLong := strings.Repeat("x", record.MaxTitle-len(addr)-1) + " " + addr
	scan := pipeline.Find(tooLong)
	if _, err := scan.Apply("title", tokensFor(scan), record.MaxTitle); err == nil {
		t.Fatalf("a title of %d characters after masking was accepted", record.MaxTitle+len(tok)-len(addr))
	}
	// The longest title that still fits once the token is in, which proves the bound is the
	// field's own and not an accident of the fixture.
	fits := strings.Repeat("x", record.MaxTitle-len(tok)-1) + " " + addr
	scan = pipeline.Find(fits)
	got, err := scan.Apply("title", tokensFor(scan), record.MaxTitle)
	if err != nil {
		t.Fatalf("a title that is exactly the limit after masking was refused: %v", err)
	}
	if len(got) != record.MaxTitle {
		t.Errorf("the masked title is %d characters, want exactly the limit %d", len(got), record.MaxTitle)
	}
}

// TestTheMaskerCountsCharactersAndNotBytes, because the record format's limits are in Unicode code
// points. A text of multi-byte characters that fits must not be refused for being long in bytes.
func TestTheMaskerCountsCharactersAndNotBytes(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("é", record.MaxTitle-len("[email:TOKEN0]")) + "a@b.co"
	scan := pipeline.Find(text)
	if _, err := scan.Apply("title", tokensFor(scan), record.MaxTitle); err != nil {
		t.Fatalf("a title of %d characters was refused: %v", record.MaxTitle, err)
	}
}

// TestAValueTooLongToMapIsNotMasked. The redaction map bounds what it stores, so the masker must
// not find something it could not store: that would be a value with no token, which Apply refuses,
// and a whole delivery dead-lettered by one absurd address.
func TestAValueTooLongToMapIsNotMasked(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 600) + "@example.test"
	if got := pipeline.Find(long).Secrets(); len(got) != 0 {
		t.Errorf("a %d byte address was taken as a secret: %v", len(long), got)
	}
}

// theSecretIn is the exact substring the masker must have removed from a fixture, and it fails the
// test when the fixture does not carry one: a fixture that matches nothing would make the "no
// secret is left" assertion pass for the wrong reason.
func theSecretIn(t *testing.T, text string, kind pipeline.Kind) string {
	t.Helper()
	s := secretIn(text, kind)
	if s == "" {
		t.Fatalf("the fixture %q holds no %s at all, so it proves nothing", text, kind)
	}
	return s
}

func secretIn(text string, kind pipeline.Kind) string {
	for _, sec := range pipeline.Find(text).Secrets() {
		if sec.Kind == kind {
			return sec.Value
		}
	}
	return ""
}

func concat(groups ...[]string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}
