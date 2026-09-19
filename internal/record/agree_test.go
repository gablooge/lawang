package record

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Principle 5 applied to the format: the Go side and the schema refuse the same records. Every
// case below goes through both. A case marked goOnly is one of the rules JSON Schema cannot
// express, and those are the only disagreements allowed.

type docCase struct {
	name string
	edit func(map[string]any)
	// ok: both accept. Otherwise both refuse.
	ok bool
	// goOnly: Go refuses, the schema has no way to.
	goOnly bool
}

const (
	goodID  = "rec_0123456789abcdef0123456789abcdef"
	hex32   = "0123456789abcdef0123456789abcdef"
	scope3  = "slack:channel:" // 14 bytes
	longest = MaxScopeID - len(scope3)
)

func docCases() []docCase {
	accepted := []docCase{
		{name: "the example", edit: func(map[string]any) {}},
		{name: "unknown top-level field", edit: set("x_future", map[string]any{"a": 1})},
		{name: "unknown field in author", edit: set("author.email_hash", "abc")},
		{name: "unknown field in container", edit: set("container.name", "general")},
		{name: "unknown field in origin", edit: set("origin.imported", true)},
		{name: "unknown field in edges", edit: set("edges.parent", "clickup:task:1")},
		{name: "unknown field in meta", edit: set("meta.worker", "w1")},
		{name: "no meta", edit: del("meta")},
		{name: "empty meta", edit: set("meta", map[string]any{})},
		{name: "empty meta.delivery", edit: set("meta.delivery", "")},
		{name: "supersedes a record", edit: set("supersedes", goodID)},
		{name: "empty text", edit: set("text", "")},
		{name: "a title", edit: set("title", "Q3 numbers")},
		{name: "newlines and tabs in title and text", edit: both(set("title", "a\tb"), set("text", "a\nb\r\n\tc"))},
		{name: "text of exactly MaxText characters", edit: set("text", strings.Repeat("a", MaxText))},
		{name: "text of exactly MaxText two-byte characters", edit: set("text", strings.Repeat("é", MaxText))},
		{name: "title of exactly MaxTitle characters", edit: set("title", strings.Repeat("é", MaxTitle))},
		{name: "a tombstone", edit: both(set("op", "delete"), set("text", ""))},
		{name: "kind task", edit: set("kind", "task")},
		{name: "kind ticket", edit: set("kind", "ticket")},
		{name: "kind document", edit: set("kind", "document")},
		{name: "kind page", edit: set("kind", "page")},
		{name: "audience direct", edit: set("visibility.audience", "direct")},
		{name: "one fractional digit", edit: set("occurred_at", "2026-07-09T12:30:45.5Z")},
		{name: "nine fractional digits", edit: set("occurred_at", "2026-07-09T12:30:45.123456789Z")},
		{name: "the year 1000", edit: set("occurred_at", "1000-01-01T00:00:00Z")},
		{name: "the year 9999", edit: set("occurred_at", "9999-12-31T23:59:59.999999999Z")},
		{name: "a leap day", edit: set("occurred_at", "2028-02-29T00:00:00Z")},
		{name: "a reply", edit: set("edges.reply_parent", "slack:C0GENERAL:1752064000.000100")},
		{name: "unknown author", edit: both(set("author.id", ""), set("author.display", ""))},
		{name: "a scope with escapes", edit: set("visibility.scope", "outlook:mailbox:ben%40example.com")},
		{name: "a scope with an escaped colon, slash, plus, equals and percent", edit: set("visibility.scope", "teams:channel:19%3Aabc%2Fd%2Be%3D%25%40thread.tacv2")},
		{name: "a scope with escaped UTF-8", edit: set("visibility.scope", "slack:channel:%C3%A9")},
		{name: "a scope of exactly MaxScopeID bytes", edit: set("visibility.scope", scope3+strings.Repeat("a", longest))},
		{name: "a wire name with a hyphen", edit: set("source", "slack-eu")},
		{name: "a source of 64 characters", edit: set("source", "s"+strings.Repeat("a", 63))},
		{name: "external_id of exactly MaxExternalID", edit: set("external_id", strings.Repeat("é", MaxExternalID))},
		{name: "version of exactly MaxVersion", edit: set("version", strings.Repeat("9", MaxVersion))},
		{name: "author fields of exactly MaxAuthor", edit: both(set("author.id", strings.Repeat("u", MaxAuthor)), set("author.display", strings.Repeat("é", MaxAuthor)))},
		{name: "container.id of exactly MaxContainerID, with a colon", edit: set("container.id", "19:"+strings.Repeat("c", MaxContainerID-3))},
		{name: "container.kind of 32 characters", edit: set("container.kind", "c"+strings.Repeat("a", 31))},
		{name: "meta.delivery of exactly MaxDelivery", edit: set("meta.delivery", strings.Repeat("d", MaxDelivery))},
	}
	for i := range accepted {
		accepted[i].ok = true
	}

	refused := []docCase{
		// format
		{name: "another format version", edit: set("format", "sluiceway.record/v2")},
		{name: "empty format", edit: set("format", "")},
		{name: "format a number", edit: set("format", 1)},

		// id
		{name: "id of 31 hex", edit: set("id", "rec_"+hex32[:31])},
		{name: "id of 33 hex", edit: set("id", "rec_"+hex32+"0")},
		{name: "id in uppercase hex", edit: set("id", "rec_"+strings.ToUpper(hex32))},
		{name: "id with an uppercase prefix", edit: set("id", "REC_"+hex32)},
		{name: "id with no prefix", edit: set("id", hex32+"0123")},
		{name: "id with a non-hex letter", edit: set("id", "rec_"+hex32[:31]+"g")},
		{name: "id with a trailing newline", edit: set("id", goodID+"\n")},
		{name: "id with a NUL", edit: set("id", "rec_"+hex32[:31]+"\x00")},
		{name: "id empty", edit: set("id", "")},
		{name: "id a number", edit: set("id", 7)},

		// op, kind, audience
		{name: "unknown op", edit: set("op", "patch")},
		{name: "op in capitals", edit: set("op", "UPSERT")},
		{name: "op empty", edit: set("op", "")},
		{name: "op a boolean", edit: set("op", true)},
		{name: "unknown kind", edit: set("kind", "email")},
		{name: "kind empty", edit: set("kind", "")},
		{name: "unknown audience", edit: set("visibility.audience", "public")},
		{name: "audience private", edit: set("visibility.audience", "private")},
		{name: "audience empty", edit: set("visibility.audience", "")},

		// visibility is closed
		{name: "a private flag in visibility", edit: set("visibility.private", true)},
		{name: "members in visibility", edit: set("visibility.members", []any{"U0BEN"})},
		{name: "visibility a string", edit: set("visibility", "slack:channel:C0GENERAL")},

		// source
		{name: "source with a capital", edit: set("source", "Slack")},
		{name: "source empty", edit: set("source", "")},
		{name: "source starting with a digit", edit: set("source", "1slack")},
		{name: "source with a space", edit: set("source", "slack eu")},
		{name: "source of 65 characters", edit: set("source", "s"+strings.Repeat("a", 64))},
		{name: "source with a trailing newline", edit: set("source", "slack\n")},

		// external_id and version
		{name: "external_id empty", edit: set("external_id", "")},
		{name: "external_id with the 0x1F separator", edit: set("external_id", "a\x1fb")},
		{name: "external_id with a newline", edit: set("external_id", "a\nb")},
		{name: "external_id with DEL", edit: set("external_id", "a\x7fb")},
		{name: "external_id one over MaxExternalID", edit: set("external_id", strings.Repeat("é", MaxExternalID+1))},
		{name: "version empty", edit: set("version", "")},
		{name: "version with a NUL", edit: set("version", "v\x00")},
		{name: "version one over MaxVersion", edit: set("version", strings.Repeat("9", MaxVersion+1))},
		{name: "version a number", edit: set("version", 3)},

		// supersedes
		{name: "supersedes garbage", edit: set("supersedes", "the previous one")},
		{name: "supersedes empty", edit: set("supersedes", "")},
		{name: "supersedes an external id", edit: set("supersedes", "slack:C0GENERAL:1")},
		{name: "supersedes a number", edit: set("supersedes", 5)},
		{name: "supersedes itself", edit: set("supersedes", "rec_bb4dc9bd2347887adae051e72346bec6"), goOnly: true},

		// title and text
		{name: "title one over MaxTitle", edit: set("title", strings.Repeat("é", MaxTitle+1))},
		{name: "title with a NUL", edit: set("title", "a\x00b")},
		{name: "title a number", edit: set("title", 5)},
		{name: "text one over MaxText", edit: set("text", strings.Repeat("a", MaxText+1))},
		{name: "text one over MaxText in two-byte characters", edit: set("text", strings.Repeat("é", MaxText+1))},
		{name: "text with a NUL", edit: set("text", "a\x00b")},
		{name: "text an array", edit: set("text", []any{"a"})},
		{name: "a tombstone with text", edit: set("op", "delete")},
		{name: "a tombstone with a title", edit: both(set("op", "delete"), set("text", ""), set("title", "x"))},

		// author, container
		{name: "author.id one over MaxAuthor", edit: set("author.id", strings.Repeat("u", MaxAuthor+1))},
		{name: "author.id with a control character", edit: set("author.id", "U0\x1bBEN")},
		{name: "author.display with a newline", edit: set("author.display", "ben\nadmin")},
		{name: "author.display one over MaxAuthor", edit: set("author.display", strings.Repeat("é", MaxAuthor+1))},
		{name: "author a string", edit: set("author", "ben")},
		{name: "container.kind with a capital", edit: set("container.kind", "Channel")},
		{name: "container.kind empty", edit: set("container.kind", "")},
		{name: "container.kind of 33 characters", edit: set("container.kind", "c"+strings.Repeat("a", 32))},
		{name: "container.kind with a hyphen", edit: set("container.kind", "direct-message")},
		{name: "container.id empty", edit: set("container.id", "")},
		{name: "container.id one over MaxContainerID", edit: set("container.id", strings.Repeat("c", MaxContainerID+1))},
		{name: "container.id with a control character", edit: set("container.id", "C0\tGENERAL")},
		{name: "container.id a number", edit: set("container.id", 5)},

		// origin, edges, meta
		{name: "origin.automation in text", edit: set("origin.automation", "false")},
		{name: "origin.untrusted a number", edit: set("origin.untrusted", 0)},
		{name: "origin an array", edit: set("origin", []any{})},
		{name: "edges an array", edit: set("edges", []any{})},
		{name: "reply_parent empty", edit: set("edges.reply_parent", "")},
		{name: "reply_parent with a control character", edit: set("edges.reply_parent", "a\x00")},
		{name: "reply_parent one over MaxExternalID", edit: set("edges.reply_parent", strings.Repeat("p", MaxExternalID+1))},
		{name: "reply_parent a number", edit: set("edges.reply_parent", 5)},
		{name: "meta a string", edit: set("meta", "x")},
		{name: "meta null", edit: set("meta", nil)},
		{name: "meta.delivery one over MaxDelivery", edit: set("meta.delivery", strings.Repeat("d", MaxDelivery+1))},
		{name: "meta.delivery with a control character", edit: set("meta.delivery", "01J\n")},
		{name: "meta.delivery a number", edit: set("meta.delivery", 5)},
		{name: "meta.delivery null", edit: set("meta.delivery", nil)},

		// field names
		{name: "ID beside id", edit: set("ID", goodID)},
		{name: "Visibility beside visibility", edit: set("Visibility", map[string]any{"scope": "slack:channel:OTHER", "audience": "group"})},
		{name: "a Kelvin sign for the k of kind", edit: set(string(rune(0x212A))+"ind", "task")},
		{name: "a long s in a field name", edit: set("ver"+string(rune(0x017F))+"ion", "2")},
		{name: "a field name with a hyphen", edit: set("x-future", 1)},
		{name: "an empty field name", edit: set("", 1)},
		{name: "a capital in a nested field name", edit: set("author.Id", "U0EVE")},
		{name: "a capital in a meta field name", edit: set("meta.Delivery", "x")},
		{name: "a field name with a trailing newline", edit: set("future\n", 1)},

		// the day the pattern cannot judge, which format assertion and the time package can
		{name: "February 30", edit: set("occurred_at", "2026-02-30T12:00:00Z")},
	}

	for _, s := range badScopeIDs() {
		refused = append(refused, docCase{name: "scope " + s.name, edit: set("visibility.scope", s.id)})
	}
	for name, value := range notRFC3339 {
		refused = append(refused, docCase{name: "occurred_at: " + name, edit: set("occurred_at", value)})
	}
	for _, field := range recordRule.required {
		refused = append(refused, docCase{name: "no " + field, edit: del(field)})
		if field != "supersedes" {
			refused = append(refused, docCase{name: field + " null", edit: set(field, nil)})
		}
	}
	for _, nested := range nestedRules {
		for _, field := range nested.rule.required {
			path := nested.name + "." + field
			refused = append(refused, docCase{name: "no " + path, edit: del(path)})
			if path != "edges.reply_parent" {
				refused = append(refused, docCase{name: path + " null", edit: set(path, nil)})
			}
		}
	}
	return append(accepted, refused...)
}

func both(edits ...func(map[string]any)) func(map[string]any) {
	return func(doc map[string]any) {
		for _, e := range edits {
			e(doc)
		}
	}
}

func TestSchemaAndGoAgreeOnDocuments(t *testing.T) {
	with, _ := schemas(t)
	cases := docCases()
	if len(cases) < 150 {
		t.Fatalf("only %d cases, the table lost some", len(cases))
	}
	t.Logf("%d document cases", len(cases))
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.name] {
			t.Fatalf("two cases are called %q", c.name)
		}
		seen[c.name] = true

		doc := exampleDoc(t)
		c.edit(doc)
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}

		schemaErr := schemaError(t, with, data)
		var r Record
		goErr := json.Unmarshal(data, &r)

		wantSchemaOK, wantGoOK := c.ok || c.goOnly, c.ok
		if (schemaErr == nil) != wantSchemaOK {
			t.Errorf("%s: schema accepted = %v, want %v (%v)", c.name, schemaErr == nil, wantSchemaOK, schemaErr)
		}
		if (goErr == nil) != wantGoOK {
			t.Errorf("%s: Go accepted = %v, want %v (%v)", c.name, goErr == nil, wantGoOK, goErr)
		}
		if goErr != nil && !errors.Is(goErr, ErrInvalid) {
			t.Errorf("%s: the Go error does not wrap ErrInvalid: %v", c.name, goErr)
		}
		if goErr != nil && r != (Record{}) {
			t.Errorf("%s: a refused document still filled in the record", c.name)
		}
	}
}

// The other direction, starting from Go values: whatever Validate refuses cannot be marshalled,
// and if it could be, the schema would refuse it.

type recordCase struct {
	name string
	edit func(*Record)
	// goOnly: the schema cannot express the rule.
	goOnly bool
	// offWire: encoding/json cannot write the value at all, or rewrites it, so there is no
	// document to show the schema.
	offWire bool
}

func recordCases() []recordCase {
	return []recordCase{
		{name: "no format", edit: func(r *Record) { r.Format = "" }},
		{name: "another format", edit: func(r *Record) { r.Format = "sluiceway.record/v2" }},
		{name: "no id", edit: func(r *Record) { r.ID = "" }},
		{name: "an id that is a ULID", edit: func(r *Record) { r.ID = "01JZXA8Q2K4M7N9P0R3S5T6V8W" }},
		{name: "no op", edit: func(r *Record) { r.Op = "" }},
		{name: "unknown op", edit: func(r *Record) { r.Op = "patch" }},
		{name: "no source", edit: func(r *Record) { r.Source = "" }},
		{name: "a capital in source", edit: func(r *Record) { r.Source = "Slack" }},
		{name: "no kind", edit: func(r *Record) { r.Kind = "" }},
		{name: "unknown kind", edit: func(r *Record) { r.Kind = "email" }},
		{name: "no external id", edit: func(r *Record) { r.ExternalID = "" }},
		{name: "separator in external id", edit: func(r *Record) { r.ExternalID = "a\x1fb" }},
		{name: "external id too long", edit: func(r *Record) { r.ExternalID = strings.Repeat("é", MaxExternalID+1) }},
		{name: "external id not UTF-8", edit: func(r *Record) { r.ExternalID = "a\xffb" }, offWire: true},
		{name: "no version", edit: func(r *Record) { r.Version = "" }},
		{name: "version too long", edit: func(r *Record) { r.Version = strings.Repeat("1", MaxVersion+1) }},
		{name: "supersedes garbage", edit: func(r *Record) { r.Supersedes = "previous" }},
		{name: "supersedes itself", edit: func(r *Record) { r.Supersedes = Ref(r.ID) }, goOnly: true},
		{name: "zero time", edit: func(r *Record) { r.OccurredAt = time.Time{} }},
		{name: "time in another zone", edit: func(r *Record) { r.OccurredAt = r.OccurredAt.In(time.FixedZone("CEST", 7200)) }},
		{name: "time in a zone that happens to be at offset zero", edit: func(r *Record) { r.OccurredAt = r.OccurredAt.In(time.FixedZone("GMT", 0)) }, offWire: true},
		{name: "the year 999", edit: func(r *Record) { r.OccurredAt = time.Date(999, 12, 31, 23, 59, 59, 0, time.UTC) }},
		{name: "the year 10000", edit: func(r *Record) { r.OccurredAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }, offWire: true},
		{name: "title too long", edit: func(r *Record) { r.Title = strings.Repeat("é", MaxTitle+1) }},
		{name: "NUL in title", edit: func(r *Record) { r.Title = "a\x00" }},
		{name: "text too long", edit: func(r *Record) { r.Text = strings.Repeat("é", MaxText+1) }},
		{name: "NUL in text", edit: func(r *Record) { r.Text = "a\x00b" }},
		{name: "text not UTF-8", edit: func(r *Record) { r.Text = "caf\xe9" }, offWire: true},
		{name: "a tombstone with text", edit: func(r *Record) { r.Op = OpDelete }},
		{name: "a tombstone with a title", edit: func(r *Record) { r.Op, r.Text, r.Title = OpDelete, "", "x" }},
		{name: "author id too long", edit: func(r *Record) { r.Author.ID = strings.Repeat("u", MaxAuthor+1) }},
		{name: "escape character in author display", edit: func(r *Record) { r.Author.Display = "ben\x1b[2J" }},
		{name: "no container kind", edit: func(r *Record) { r.Container.Kind = "" }},
		{name: "a capital in container kind", edit: func(r *Record) { r.Container.Kind = "Channel" }},
		{name: "no container id", edit: func(r *Record) { r.Container.ID = "" }},
		{name: "container id too long", edit: func(r *Record) { r.Container.ID = strings.Repeat("c", MaxContainerID+1) }},
		{name: "no scope", edit: func(r *Record) { r.Visibility.Scope = "" }},
		{name: "no visibility", edit: func(r *Record) { r.Visibility = Visibility{} }},
		{name: "a scope built by hand", edit: func(r *Record) { r.Visibility.Scope = "teams:channel:19:abc@thread.tacv2" }},
		{name: "no audience", edit: func(r *Record) { r.Visibility.Audience = "" }},
		{name: "audience public", edit: func(r *Record) { r.Visibility.Audience = "public" }},
		{name: "control character in reply parent", edit: func(r *Record) { r.Edges.ReplyParent = "a\nb" }},
		{name: "reply parent too long", edit: func(r *Record) { r.Edges.ReplyParent = Ref(strings.Repeat("p", MaxExternalID+1)) }},
		{name: "delivery too long", edit: func(r *Record) { r.Meta.Delivery = strings.Repeat("d", MaxDelivery+1) }},
		{name: "control character in delivery", edit: func(r *Record) { r.Meta.Delivery = "01J\x00" }},
		{name: "the zero Record", edit: func(r *Record) { *r = Record{} }, offWire: true},
	}
}

func TestWhatValidateRefusesTheSchemaRefuses(t *testing.T) {
	with, _ := schemas(t)
	for _, c := range recordCases() {
		r := sealed(t)
		c.edit(&r)

		err := r.Validate()
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", c.name, err)
			continue
		}
		if _, err := json.Marshal(r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: the record was marshalled (err = %v)", c.name, err)
		}
		if _, err := json.Marshal([]Record{sealed(t), r}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: the record was marshalled inside a batch (err = %v)", c.name, err)
		}
		if c.offWire {
			continue
		}
		// Around MarshalJSON, to show the schema what Validate kept off the wire.
		data, err := json.Marshal(wire(r))
		if err != nil {
			t.Errorf("%s: %v (mark the case offWire if it cannot be written)", c.name, err)
			continue
		}
		schemaErr := schemaError(t, with, data)
		if c.goOnly && schemaErr != nil {
			t.Errorf("%s: marked goOnly, and the schema refuses it after all: %v", c.name, schemaErr)
		}
		if !c.goOnly && schemaErr == nil {
			t.Errorf("%s: Validate refuses it and the schema accepts it", c.name)
		}
	}
}

// An error ends up in a log line or an outbox row, and a record is somebody's message.
func TestErrorsNeverQuoteTheRecord(t *testing.T) {
	const marker = "s3cret"
	edits := map[string]func(*Record){
		"text":         func(r *Record) { r.Text = marker + "\x00" },
		"title":        func(r *Record) { r.Title = marker + "\x00" },
		"author":       func(r *Record) { r.Author.Display = marker + "\n" },
		"external id":  func(r *Record) { r.ExternalID = marker + "\n" },
		"scope":        func(r *Record) { r.Visibility.Scope = "slack:channel:" + marker + " x" },
		"kind":         func(r *Record) { r.Kind = marker },
		"container id": func(r *Record) { r.Container.ID = marker + "\n" },
		"id":           func(r *Record) { r.ID = marker },
	}
	for name, edit := range edits {
		r := sealed(t)
		edit(&r)
		err := r.Validate()
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), marker) {
			t.Errorf("%s: the error quotes the value: %v", name, err)
		}
	}

	docs := map[string]func(map[string]any){
		"a number for the text": set("text", 5318008),
		"a bad time":            set("occurred_at", marker),
		"a bad supersedes":      set("supersedes", marker),
		"a bad scope":           set("visibility.scope", marker),
		"an unknown field name": set("visibility."+marker, true),
		"a bad field name":      set("X"+marker, true),
	}
	for name, edit := range docs {
		doc := exampleDoc(t)
		edit(doc)
		data, _ := json.Marshal(doc)
		var r Record
		err := json.Unmarshal(data, &r)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "5318008") {
			t.Errorf("%s: the error quotes the document: %v", name, err)
		}
	}
}

// No schema can refuse a name used twice, and decoders disagree on which one counts, so the Go
// decoder refuses the document.
func TestDecodingRefusesAFieldNameUsedTwice(t *testing.T) {
	example := string(exampleBytes(t))
	docs := map[string]string{
		"two scopes":          strings.Replace(example, `"scope": "slack:channel:C0GENERAL",`, `"scope": "slack:channel:C0GENERAL", "scope": "slack:channel:C0SECRET",`, 1),
		"two visibilities":    strings.Replace(example, `"origin":`, `"visibility": { "scope": "slack:channel:C0SECRET", "audience": "group" }, "origin":`, 1),
		"two ids":             strings.Replace(example, `"op":`, `"id": "`+goodID+`", "op":`, 1),
		"twice in an unknown": strings.Replace(example, `"op":`, `"x_future": [{ "a": 1, "a": 2 }], "op":`, 1),
	}
	for name, doc := range docs {
		if doc == example {
			t.Fatalf("%s: the replacement found nothing", name)
		}
		var r Record
		if err := json.Unmarshal([]byte(doc), &r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}

	// encoding/json checks the syntax before it calls UnmarshalJSON. A caller that calls it
	// directly gets the same refusal, and not a panic, for anything that is not one JSON value.
	for _, broken := range []string{``, `{`, `{"id":`, `{"a":1}}`, `{"a":1} {"a":2}`, `[1,`, `nul`} {
		var r Record
		if err := r.UnmarshalJSON([]byte(broken)); !errors.Is(err, ErrInvalid) {
			t.Errorf("UnmarshalJSON(%q): err = %v, want ErrInvalid", broken, err)
		}
	}
	for _, notARecord := range []string{`null`, `[]`, `"text"`, `5`, `true`, `{}`} {
		var r Record
		if err := json.Unmarshal([]byte(notARecord), &r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", notARecord, err)
		}
	}

	// The same names in different objects are not duplicates ("id" and "kind" occur three times).
	var r Record
	nested := strings.Replace(example, `"op":`, `"x_future": { "id": 1, "sub": [{ "id": 2 }, { "id": 3 }] }, "op":`, 1)
	if err := json.Unmarshal([]byte(nested), &r); err != nil {
		t.Errorf("the same name in different objects was refused: %v", err)
	}
}
