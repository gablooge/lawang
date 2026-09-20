package record

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gablooge/lawang/internal/ids"
)

// ErrInvalid reports a record, or a part of one, that the format forbids. The message names the
// field and the rule, never the value: a title, a text and an author are personal data, and an
// error ends up in a log or in an outbox row.
var ErrInvalid = errors.New("record: invalid")

// Limits, in characters (Unicode code points, which is what a JSON Schema maxLength counts).
// They exist so that nothing a sender controls is unbounded. A normalizer cuts a text down to
// MaxText itself: Validate refuses a longer one, it does not truncate.
const (
	MaxExternalID  = 1024
	MaxVersion     = 256
	MaxTitle       = 1024
	MaxText        = 1 << 20
	MaxAuthor      = 256
	MaxContainerID = 512
	MaxDelivery    = 128
)

const (
	// maxSource bounds Source, which may also hold a sink's wire name.
	maxSource = 64
	// maxKind bounds a provider key and a container kind, the first two segments of a scope id.
	maxKind = 32
	// recordIDLen is "rec_" and 32 hex characters.
	recordIDLen = len(ids.RecordPrefix) + 32
	minYear     = 1000
	maxYear     = 9999
)

func invalid(field, rule string) error {
	return fmt.Errorf("%w: %s %s", ErrInvalid, field, rule)
}

// Validate reports the first thing about r that the format forbids, as an error that wraps
// ErrInvalid. A record that passes is one the schema accepts.
//
// It checks exactly what the schema checks, and one rule the schema cannot express: Supersedes
// is not the record's own ID (JSON Schema cannot compare two fields). A sink that validates with
// the schema alone should add that check itself.
//
// It also refuses two Go values that have no faithful form on the wire, where encoding/json
// would quietly write something else: a string that is not valid UTF-8 (written with U+FFFD in
// place of the bad bytes), and an OccurredAt whose Location is not time.UTC. The second is
// refused even at offset zero, where the wire form would happen to be right, so that a
// normalizer which forgot UTC() fails on every record and not only in summer.
//
// It does not and cannot check that ID was minted by Seal, that Version moves forward, or that
// Supersedes names a record that exists.
func (r Record) Validate() error {
	if r.Format != FormatV1 {
		return invalid("format", "is not "+FormatV1)
	}
	if !validRecordID(r.ID) {
		return invalid("id", "is not a record id")
	}
	if r.Op != OpUpsert && r.Op != OpDelete {
		return invalid("op", "is not one of upsert, delete")
	}
	if !validName(r.Source, maxSource, true) {
		return invalid("source", "is not a source name")
	}
	switch r.Kind {
	case KindTask, KindMessage, KindTicket, KindDocument, KindPage:
	default:
		return invalid("kind", "is not one of task, message, ticket, document, page")
	}
	if err := checkExternalID("external_id", r.ExternalID); err != nil {
		return err
	}
	if err := checkString("version", r.Version, 1, MaxVersion, identifierChars); err != nil {
		return err
	}
	if r.Supersedes != "" {
		if !validRecordID(string(r.Supersedes)) {
			return invalid("supersedes", "is not a record id")
		}
		if string(r.Supersedes) == r.ID {
			return invalid("supersedes", "names the record itself")
		}
	}
	if err := checkTime(r.OccurredAt); err != nil {
		return err
	}
	if err := checkString("title", r.Title, 0, MaxTitle, oneLineChars); err != nil {
		return err
	}
	if err := checkString("text", r.Text, 0, MaxText, contentChars); err != nil {
		return err
	}
	if r.Op == OpDelete && (r.Title != "" || r.Text != "") {
		return invalid("title and text", "are empty on a delete")
	}
	if err := checkString("author.id", r.Author.ID, 0, MaxAuthor, identifierChars); err != nil {
		return err
	}
	if err := checkString("author.display", r.Author.Display, 0, MaxAuthor, displayChars); err != nil {
		return err
	}
	if !validName(r.Container.Kind, maxKind, false) {
		return invalid("container.kind", "is not a container kind")
	}
	if err := checkString("container.id", r.Container.ID, 1, MaxContainerID, identifierChars); err != nil {
		return err
	}
	if err := checkScopeID(r.Visibility.Scope); err != nil {
		return invalid("visibility.scope", "is not a scope id")
	}
	if r.Visibility.Audience != AudienceDirect && r.Visibility.Audience != AudienceGroup {
		return invalid("visibility.audience", "is not one of direct, group")
	}
	if r.Edges.ReplyParent != "" {
		if err := checkExternalID("edges.reply_parent", string(r.Edges.ReplyParent)); err != nil {
			return err
		}
	}
	return checkString("meta.delivery", r.Meta.Delivery, 0, MaxDelivery, identifierChars)
}

// charRule says which characters a string may not contain. The four rules are the same four in
// the schema ($defs identifier, displayName and oneLine, and the pattern of text), and ADR 4 has
// the reasons ("Which characters a field may hold"). Each is a fixed list of code points and
// never a Unicode category, because a category grows with every Unicode version and the format
// may not.
type charRule int

const (
	// identifierChars is for what a program compares and an operator reads in a log:
	// ExternalID, Version, Author.ID, Container.ID, Edges.ReplyParent, Meta.Delivery. Refused:
	// every control character (C0, DEL, C1), the line and paragraph separators, every
	// bidirectional formatting character, and the zero-width and invisible format characters
	// listed at invisible.
	identifierChars charRule = iota
	// displayChars is for Author.Display: identifierChars, less the zero-width non-joiner and
	// joiner (U+200C, U+200D), which Persian and Indic names are spelled with and emoji are built
	// with.
	displayChars
	// oneLineChars is for Title, which is content in any language, on a single line: every
	// control character and the line and paragraph separators are refused. Bidirectional
	// formatting stays, because right-to-left titles use it.
	oneLineChars
	// contentChars is for Text: only NUL is refused, which a Postgres text column cannot hold.
	// Newlines, tabs and everything else are content.
	contentChars
)

// refuses reports whether the rule forbids c. It is for the three rules that read characters:
// contentChars refuses one byte, and checkString looks for that byte without decoding the text.
func (rule charRule) refuses(c rune) bool {
	switch {
	case c < 0x20, c >= 0x7F && c <= 0x9F, c == 0x2028, c == 0x2029:
		return true
	case rule == oneLineChars:
		return false
	case rule == displayChars && (c == 0x200C || c == 0x200D):
		return false
	}
	return invisible(c)
}

// invisible is the soft hyphen, the Arabic letter mark, U+200B to U+200F (the zero-width space,
// non-joiner and joiner, and the left-to-right and right-to-left marks), U+202A to U+202E (the
// embeddings and overrides), U+2060 to U+206F (the word joiner, the invisible operators, the
// isolates and the deprecated format characters) and the byte order mark.
func invisible(c rune) bool {
	return c == 0x00AD || c == 0x061C || c >= 0x200B && c <= 0x200F || c >= 0x202A && c <= 0x202E ||
		c >= 0x2060 && c <= 0x206F || c == 0xFEFF
}

func checkString(field, s string, minChars, maxChars int, rule charRule) error {
	if len(s) < minChars {
		return invalid(field, "is empty")
	}
	// A string has at most as many characters as bytes, so the count is only taken when the
	// byte length leaves the question open.
	if len(s) > maxChars && utf8.RuneCountInString(s) > maxChars {
		return invalid(field, fmt.Sprintf("is longer than %d characters", maxChars))
	}
	if rule == contentChars {
		// A text may be a megabyte, and in UTF-8 a zero byte is always NUL.
		if strings.IndexByte(s, 0) >= 0 {
			return invalid(field, "contains a NUL")
		}
	} else {
		for _, c := range s {
			if rule.refuses(c) {
				return invalid(field, "contains a character this field does not allow")
			}
		}
	}
	if !utf8.ValidString(s) {
		return invalid(field, "is not valid UTF-8")
	}
	return nil
}

// checkExternalID is for ExternalID and for Edges.ReplyParent, which is one: an identifier of 1
// to MaxExternalID characters that begins with a provider key and a colon, with something after
// the colon. Seal checks that the key is the sealing provider's.
func checkExternalID(field, s string) error {
	if err := checkString(field, s, 1, MaxExternalID, identifierChars); err != nil {
		return err
	}
	colon := strings.IndexByte(s, ':')
	if colon < 0 || colon == len(s)-1 || !ValidProviderKey(s[:colon]) {
		return invalid(field, "does not begin with a provider key and a colon")
	}
	return nil
}

func checkTime(t time.Time) error {
	if t.Location() != time.UTC {
		return invalid("occurred_at", "is not in UTC")
	}
	if y := t.Year(); y < minYear || y > maxYear {
		return invalid("occurred_at", "is not set, or its year is outside 1000 to 9999")
	}
	return nil
}

func validRecordID(s string) bool {
	if len(s) != recordIDLen || s[:len(ids.RecordPrefix)] != ids.RecordPrefix {
		return false
	}
	for i := len(ids.RecordPrefix); i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validName reports whether s is a lowercase letter followed by a-z, 0-9 and underscore (and the
// hyphen, where a wire name may have one), maxLen bytes at most.
func validName(s string, maxLen int, hyphen bool) bool {
	if s == "" || len(s) > maxLen || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || hyphen && c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// ValidProviderKey reports whether s is an internal provider key: a lowercase letter followed by
// up to 31 of a-z, 0-9 and underscore, so at most 32 bytes, and never a hyphen
// ([ADR 3](../../docs/adr/0003-scope-id-format.md)).
//
// This function owns that rule for the whole program. The provider key is the first segment of
// every scope id and of every external id, so a key this refuses produces records that all fail
// in Seal, and a second copy of the pattern anywhere else is a way for the two to drift apart:
// the provider registry calls this, and a test holds the two to the same verdict on every
// candidate. ADR 3 freezes the grammar at v0.1.0 in both directions, hyphen included.
func ValidProviderKey(s string) bool { return validName(s, maxKind, false) }
