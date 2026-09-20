package outbox_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gablooge/lawang/internal/outbox"
)

func TestCauseText(t *testing.T) {
	for want, cause := range map[string]outbox.Cause{
		"unclassified failure":                           {},
		"unclassified failure (status 500)":              outbox.NewCause(outbox.Class(200)).WithStatus(500),
		"normalizer failed":                              outbox.NewCause(outbox.ClassNormalizer),
		"sink unavailable (status 503)":                  outbox.NewCause(outbox.ClassSinkUnavailable).WithStatus(503),
		"provider unavailable (status 429, code slow.1)": outbox.NewCause(outbox.ClassProviderUnavailable).WithStatus(429).WithCode("slow.1"),
		"sink rejected the record (code Bad_Shape-2)":    outbox.NewCause(outbox.ClassSinkRejected).WithCode("Bad_Shape-2"),
		"sink refused the credential (status 401)":       outbox.NewCause(outbox.ClassSinkUnauthorized).WithStatus(401).WithCode(""),
		"vault unavailable":                              outbox.NewCause(outbox.ClassVaultUnavailable).WithStatus(0),
		"internal error":                                 outbox.NewCause(outbox.ClassInternal).WithStatus(99).WithStatus(600).WithStatus(-1),
	} {
		if got := cause.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

// TestCauseKeepsOnlyWhatLooksLikeACode: the boundary of what WithCode lets through. Everything a
// message, a URL, a header or JSON is made of is outside it.
func TestCauseKeepsOnlyWhatLooksLikeACode(t *testing.T) {
	kept := []string{"a", "rate_limited", "invalid-grant", "E1001", "v2.quota", strings.Repeat("c", 64)}
	for _, code := range kept {
		if got, want := outbox.NewCause(outbox.ClassSinkRejected).WithCode(code).String(), "sink rejected the record (code "+code+")"; got != want {
			t.Errorf("WithCode(%q): %q, want %q", code, got, want)
		}
	}
	withheld := []string{
		strings.Repeat("c", 65), "two words", "a/b", "a?b", "a=b", "a&b", "a:b", "user@host", `"quoted"`, "{}", "a\nb",
		"tab\t", "nul\x00", "\xff", "é", "ｒａｔｅ", "%41", "a+b", "a,b", "a;b", "(a)", "#a", "~a", "a|b", "a\\b", "'a'", "<a>", "*",
	}
	for _, code := range withheld {
		got := outbox.NewCause(outbox.ClassSinkRejected).WithCode(code).String()
		if want := "sink rejected the record (code withheld)"; got != want {
			t.Errorf("WithCode(%q): %q, want %q", code, got, want)
		}
	}
}

// TestClipMakesAnyTextStorable: nothing reaches the two text columns but a Cause today, and a
// Cause is short, valid text by construction. clip stays in front of them all the same, for the day
// something else is written there: Postgres refuses invalid UTF-8 and NUL in text, and a failure
// that cannot be recorded is a row that loops at lease cadence with no backoff and no dead letter.
func TestClipMakesAnyTextStorable(t *testing.T) {
	for name, in := range map[string]string{
		"short":           "sink unavailable (status 503)",
		"exactly 1000":    strings.Repeat("a", 1000),
		"long":            strings.Repeat("a", 5000),
		"long multi-byte": "a" + strings.Repeat("é", 600),           // byte 1000 falls inside a rune
		"long 4-byte":     "ab" + strings.Repeat("\U0001F600", 400), // and here too
		"invalid UTF-8":   "sink said \xff\xfe\xc3",
		"NUL":             "bad\x00byte",
	} {
		got := outbox.Clip(in)
		if len(got) > 1000 || !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
			t.Errorf("%s: clipped to %d bytes, valid UTF-8 %v, NUL %v", name, len(got), utf8.ValidString(got), strings.ContainsRune(got, 0))
		}
		if len(in) <= 1000 && utf8.ValidString(in) && !strings.ContainsRune(in, 0) && got != in {
			t.Errorf("%s: storable text was changed to %q", name, got)
		}
		if len(in) > 1000 && len(got) < 997 {
			t.Errorf("%s: clipped to %d bytes, want it cut at the last whole rune before 1000", name, len(got))
		}
	}
}
