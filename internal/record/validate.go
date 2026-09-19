package record

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/gablooge/sluiceway/internal/ids"
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
	if err := checkString("external_id", r.ExternalID, 1, MaxExternalID, noControl); err != nil {
		return err
	}
	if err := checkString("version", r.Version, 1, MaxVersion, noControl); err != nil {
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
	if err := checkString("title", r.Title, 0, MaxTitle, noNUL); err != nil {
		return err
	}
	if err := checkString("text", r.Text, 0, MaxText, noNUL); err != nil {
		return err
	}
	if r.Op == OpDelete && (r.Title != "" || r.Text != "") {
		return invalid("title and text", "are empty on a delete")
	}
	if err := checkString("author.id", r.Author.ID, 0, MaxAuthor, noControl); err != nil {
		return err
	}
	if err := checkString("author.display", r.Author.Display, 0, MaxAuthor, noControl); err != nil {
		return err
	}
	if !validName(r.Container.Kind, maxKind, false) {
		return invalid("container.kind", "is not a container kind")
	}
	if err := checkString("container.id", r.Container.ID, 1, MaxContainerID, noControl); err != nil {
		return err
	}
	if err := checkScopeID(r.Visibility.Scope); err != nil {
		return invalid("visibility.scope", "is not a scope id")
	}
	if r.Visibility.Audience != AudienceDirect && r.Visibility.Audience != AudienceGroup {
		return invalid("visibility.audience", "is not one of direct, group")
	}
	if r.Edges.ReplyParent != "" {
		if err := checkString("edges.reply_parent", string(r.Edges.ReplyParent), 1, MaxExternalID, noControl); err != nil {
			return err
		}
	}
	return checkString("meta.delivery", r.Meta.Delivery, 0, MaxDelivery, noControl)
}

// charRule says which bytes a string may not contain.
type charRule int

const (
	// noControl refuses the C0 control characters and DEL: identifiers and names have no use
	// for them, and they are what log forging and a NUL-terminated consumer are made of.
	noControl charRule = iota
	// noNUL refuses only NUL. Content keeps its newlines and tabs.
	noNUL
)

func checkString(field, s string, minChars, maxChars int, rule charRule) error {
	if len(s) < minChars {
		return invalid(field, "is empty")
	}
	// A string has at most as many characters as bytes, so the count is only taken when the
	// byte length leaves the question open.
	if len(s) > maxChars && utf8.RuneCountInString(s) > maxChars {
		return invalid(field, fmt.Sprintf("is longer than %d characters", maxChars))
	}
	for i := range len(s) {
		c := s[i]
		if c == 0 || rule == noControl && (c < 0x20 || c == 0x7F) {
			return invalid(field, "contains a control character")
		}
	}
	if !utf8.ValidString(s) {
		return invalid(field, "is not valid UTF-8")
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
