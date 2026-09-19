package record

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"unicode/utf8"
)

// objectRule is what decoding demands of one JSON object of the envelope, beyond the types of
// its values. It mirrors "required", "additionalProperties" and the nullable fields of the
// schema, and the agreement tests hold the two together.
type objectRule struct {
	required []string
	optional []string
	// nullable names the required fields whose value may be null.
	nullable []string
	// closed refuses a field that is neither required nor optional.
	closed bool
}

var recordRule = objectRule{
	required: []string{
		"format", "id", "op", "source", "kind", "external_id", "version", "supersedes",
		"occurred_at", "title", "text", "author", "container", "visibility", "origin", "edges",
	},
	optional: []string{"meta"},
	nullable: []string{"supersedes"},
}

// nestedRules has an entry for every object inside the envelope.
var nestedRules = []struct {
	name string
	rule objectRule
}{
	{"author", objectRule{required: []string{"id", "display"}}},
	{"container", objectRule{required: []string{"kind", "id"}}},
	{"visibility", objectRule{required: []string{"scope", "audience"}, closed: true}},
	{"origin", objectRule{required: []string{"automation", "untrusted"}}},
	{"edges", objectRule{required: []string{"reply_parent"}, nullable: []string{"reply_parent"}}},
	{"meta", objectRule{optional: []string{"delivery"}}},
}

// UnmarshalJSON decodes a record and refuses whatever the schema refuses, so that a Go consumer
// which decodes into a Record has validated it. encoding/json alone would not do: it reads an
// absent field as its zero value, which would turn a record with no origin into one that says
// "trusted", and it matches field names without regard to case, which would let "ID" beside "id"
// reach the struct while a schema validator looked at "id". So this checks that every required
// field is present and not null, that every field name is lowercase (the format has no other),
// that Visibility holds nothing unknown, and that occurred_at is spelled the way the format
// spells it, and then runs Validate. Unknown fields outside Visibility are ignored, which is
// what lets v1 grow.
//
// One refusal goes beyond the schema, because no schema can make it: a field name that occurs
// twice in one object. Decoders disagree on which of the two counts, so a validator that keeps
// the first and a consumer that keeps the last would see two different scopes in one record.
//
// Two more refusals are about the bytes of the document, below the level a schema works at (ADR
// 4, "The bytes of a document"): a document that is not valid UTF-8, and a \u escape that names
// half of a surrogate pair. encoding/json accepts both and writes U+FFFD in their place, so two
// different documents would decode to one identity, while another consumer of the same bytes
// refuses the document or keeps the two apart.
//
// The error says which field, never what was in it.
func (r *Record) UnmarshalJSON(data []byte) error {
	if !utf8.Valid(data) {
		return invalid("the record", "is not valid UTF-8")
	}
	if err := checkNoDuplicateNames(data); err != nil {
		return err
	}
	if hasUnpairedSurrogate(data) {
		return invalid("the record", "has an escape for half of a surrogate pair")
	}
	top, err := checkObject("the record", data, recordRule)
	if err != nil {
		return err
	}
	for _, nested := range nestedRules {
		raw, ok := top[nested.name]
		if !ok {
			continue
		}
		if _, err := checkObject(nested.name, raw, nested.rule); err != nil {
			return err
		}
	}
	var occurredAt string
	if err := json.Unmarshal(top["occurred_at"], &occurredAt); err != nil || !validTimestamp(occurredAt) {
		return invalid("occurred_at", "is not an RFC 3339 time in UTC with the Z suffix")
	}

	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		// The error of encoding/json quotes numbers and times it could not decode.
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return invalid(typeErr.Field, "has the wrong JSON type")
		}
		if errors.Is(err, ErrInvalid) {
			return err
		}
		return invalid("the record", "has a value that cannot be decoded")
	}
	decoded := Record(w)
	if err := decoded.Validate(); err != nil {
		return err
	}
	// A decoded record was sealed by whoever wrote the document. What was read is pinned, so
	// that writing it out again after a change to what the id stands for is refused.
	decoded.sealed = decoded.currentSeal()
	*r = decoded
	return nil
}

func checkObject(name string, data []byte, rule objectRule) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, invalid(name, "is not a JSON object")
	}
	for field, raw := range fields {
		if !validName(field, len(field), false) {
			return nil, invalid(name, "has a field name that is not lowercase a-z, 0-9 and underscore")
		}
		known := slices.Contains(rule.required, field) || slices.Contains(rule.optional, field)
		if !known && rule.closed {
			return nil, invalid(name, "has a field this format does not know")
		}
		if known && string(raw) == "null" && !slices.Contains(rule.nullable, field) {
			return nil, invalid(name+"."+field, "is null")
		}
	}
	for _, field := range rule.required {
		if _, ok := fields[field]; !ok {
			return nil, invalid(name+"."+field, "is missing")
		}
	}
	return fields, nil
}

// checkNoDuplicateNames walks the document once and refuses an object, at any depth, that uses
// one name twice. It also refuses anything that is not a single JSON value.
func checkNoDuplicateNames(data []byte) error {
	type frame struct {
		names   map[string]struct{} // nil for an array
		wantKey bool
	}
	var stack []frame
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].names != nil {
			stack[n-1].wantKey = true
		}
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return invalid("the record", "is not valid JSON")
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{':
				stack = append(stack, frame{names: map[string]struct{}{}, wantKey: true})
			case '[':
				stack = append(stack, frame{})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
			continue
		}
		if n := len(stack); n > 0 && stack[n-1].wantKey {
			name, _ := tok.(string)
			if _, seen := stack[n-1].names[name]; seen {
				return invalid("the record", "uses a field name twice in one object")
			}
			stack[n-1].names[name] = struct{}{}
			stack[n-1].wantKey = false
			continue
		}
		valueDone()
	}
}

// hasUnpairedSurrogate reports whether a \u escape in a JSON document names half of a surrogate
// pair with the other half missing: a high surrogate (D800 to DBFF) that is not followed at once
// by an escaped low surrogate (DC00 to DFFF), or a low surrogate with no high one before it.
//
// data must be valid JSON. A backslash then occurs only inside a string, and every escape is
// either one character after the backslash or 'u' and four hex digits, so the escapes can be
// read in order without following the structure of the document.
func hasUnpairedSurrogate(data []byte) bool {
	const escapeLen = 6 // a backslash, 'u' and four hex digits
	// surrogate returns 'h' or 'l' if an escape for a high or a low surrogate starts at i.
	surrogate := func(i int) byte {
		if i+escapeLen > len(data) || data[i] != '\\' || data[i+1] != 'u' || data[i+2]|0x20 != 'd' {
			return 0
		}
		switch c := data[i+3] | 0x20; {
		case c == '8' || c == '9' || c == 'a' || c == 'b':
			return 'h'
		case c >= 'c' && c <= 'f':
			return 'l'
		}
		return 0
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		switch surrogate(i) {
		case 'l':
			return true
		case 'h':
			if surrogate(i+escapeLen) != 'l' {
				return true
			}
			i += 2*escapeLen - 1
		default:
			i++ // the escaped character, which may be a backslash
		}
	}
	return false
}

// validTimestamp is the occurred_at pattern of the schema, by hand:
// YYYY-MM-DDTHH:MM:SS, an optional fraction of 1 to 9 digits, and Z. Years 1000 to 9999, no
// leap second. Whether the day exists in the month is left to the time package.
func validTimestamp(s string) bool {
	const base = len("2006-01-02T15:04:05")
	if len(s) < base+1 || len(s) > base+11 || s[len(s)-1] != 'Z' {
		return false
	}
	for i := range base {
		c := s[i]
		switch i {
		case 4, 7:
			if c != '-' {
				return false
			}
		case 10:
			if c != 'T' {
				return false
			}
		case 13, 16:
			if c != ':' {
				return false
			}
		default:
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	two := func(i int) int { return int(s[i]-'0')*10 + int(s[i+1]-'0') }
	month, day, hour, minute, second := two(5), two(8), two(11), two(14), two(17)
	if s[0] == '0' || month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || second > 59 {
		return false
	}
	frac := s[base : len(s)-1]
	if frac == "" {
		return true
	}
	if frac[0] != '.' || len(frac) < 2 {
		return false
	}
	for i := 1; i < len(frac); i++ {
		if frac[i] < '0' || frac[i] > '9' {
			return false
		}
	}
	return true
}
