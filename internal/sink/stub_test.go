package sink_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/sink"
	"github.com/gablooge/lawang/internal/tenancy"
)

func newStub(t testing.TB, cfg sink.StubConfig) *sink.Stub {
	t.Helper()
	s, err := sink.NewStub(cfg)
	if err != nil {
		t.Fatalf("NewStub: %v", err)
	}
	return s
}

// docCase is one document and what is supposed to happen to it.
type docCase struct {
	name string
	edit func(map[string]any)
	// goOnly marks a document the schema accepts and the stub refuses: one of the rules of the
	// format that no JSON Schema can state.
	goOnly bool
}

// refusedCases are documents the schema refuses, one rule each, plus the ones marked goOnly,
// which the schema cannot see.
func refusedCases() []docCase {
	cases := []docCase{
		{name: "no format", edit: func(d map[string]any) { delete(d, "format") }},
		{name: "another format", edit: func(d map[string]any) { d["format"] = "lawang.record/v2" }},
		{name: "no id", edit: func(d map[string]any) { delete(d, "id") }},
		{name: "id is not a record id", edit: func(d map[string]any) { d["id"] = "rec_nothex" }},
		{name: "no op", edit: func(d map[string]any) { delete(d, "op") }},
		{name: "another op", edit: func(d map[string]any) { d["op"] = "patch" }},
		{name: "no source", edit: func(d map[string]any) { delete(d, "source") }},
		{name: "source with a capital", edit: func(d map[string]any) { d["source"] = "ClickUp" }},
		{name: "another kind", edit: func(d map[string]any) { d["kind"] = "email" }},
		{name: "external id with no provider key", edit: func(d map[string]any) { d["external_id"] = "86a1" }},
		{name: "empty version", edit: func(d map[string]any) { d["version"] = "" }},
		{name: "version over its limit", edit: func(d map[string]any) { d["version"] = strings.Repeat("v", record.MaxVersion+1) }},
		{name: "version with a control character", edit: func(d map[string]any) { d["version"] = "1\u0007" }},
		{name: "supersedes is empty", edit: func(d map[string]any) { d["supersedes"] = "" }},
		{name: "supersedes is not a record id", edit: func(d map[string]any) { d["supersedes"] = "rec_short" }},
		{name: "no supersedes", edit: func(d map[string]any) { delete(d, "supersedes") }},
		{name: "occurred_at with an offset", edit: func(d map[string]any) { d["occurred_at"] = "2026-07-09T12:30:45+00:00" }},
		{name: "occurred_at is not a time", edit: func(d map[string]any) { d["occurred_at"] = "yesterday" }},
		{name: "title over its limit", edit: func(d map[string]any) { d["title"] = strings.Repeat("t", record.MaxTitle+1) }},
		{name: "title on two lines", edit: func(d map[string]any) { d["title"] = "one\ntwo" }},
		{name: "text with a NUL", edit: func(d map[string]any) { d["text"] = "a\x00b" }},
		{name: "title is null", edit: func(d map[string]any) { d["title"] = nil }},
		{name: "no author", edit: func(d map[string]any) { delete(d, "author") }},
		{name: "author with no display", edit: func(d map[string]any) { d["author"] = map[string]any{"id": "5555"} }},
		{name: "author display with an override", edit: func(d map[string]any) {
			d["author"] = map[string]any{"id": "5555", "display": "ben\u202e"}
		}},
		{name: "no container", edit: func(d map[string]any) { delete(d, "container") }},
		{name: "container kind with a hyphen", edit: func(d map[string]any) {
			d["container"] = map[string]any{"kind": "sub-task", "id": "86a1"}
		}},
		{name: "no visibility", edit: func(d map[string]any) { delete(d, "visibility") }},
		{name: "an unknown field inside visibility", edit: func(d map[string]any) {
			v, _ := d["visibility"].(map[string]any)
			v["deny"] = []any{"ben"}
		}},
		{name: "another audience", edit: func(d map[string]any) {
			v, _ := d["visibility"].(map[string]any)
			v["audience"] = "world"
		}},
		{name: "a scope that is not a scope id", edit: func(d map[string]any) {
			v, _ := d["visibility"].(map[string]any)
			v["scope"] = "clickup:list"
		}},
		{name: "no origin", edit: func(d map[string]any) { delete(d, "origin") }},
		{name: "origin with no untrusted", edit: func(d map[string]any) {
			d["origin"] = map[string]any{"automation": false}
		}},
		{name: "untrusted is a string", edit: func(d map[string]any) {
			d["origin"] = map[string]any{"automation": false, "untrusted": "no"}
		}},
		{name: "no edges", edit: func(d map[string]any) { delete(d, "edges") }},
		{name: "reply parent with no provider key", edit: func(d map[string]any) {
			d["edges"] = map[string]any{"reply_parent": "86a0"}
		}},
		{name: "meta delivery over its limit", edit: func(d map[string]any) {
			d["meta"] = map[string]any{"delivery": strings.Repeat("d", record.MaxDelivery+1)}
		}},
		{name: "a field name with a capital", edit: func(d map[string]any) { d["Extra"] = 1 }},
		{name: "a delete that still has a text", edit: func(d map[string]any) { d["op"] = "delete" }},
	}
	// The one rule Validate adds to the schema: a record that supersedes itself. The schema
	// cannot compare two fields of one document.
	cases = append(cases, docCase{
		name:   "a record that supersedes itself",
		goOnly: true,
		edit:   func(d map[string]any) { d["supersedes"] = d["id"] },
	})
	return cases
}

// TestTheStubRefusesWhateverTheSchemaRefuses is the first acceptance line of B09. Every document
// the schema has something against must come back from the stub as a refusal, and the valid one
// it is built from must not.
func TestTheStubRefusesWhateverTheSchemaRefuses(t *testing.T) {
	t.Parallel()
	valid := doc(t, rec(t, tenantA))
	if schemaRefuses(t, valid) {
		t.Fatalf("the document the sink produces does not validate")
	}
	if err := newStub(t, sink.StubConfig{}).Accept(tenantA, valid); err != nil {
		t.Fatalf("a valid document was refused: %v", err)
	}
	for _, c := range refusedCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			edited := edit(t, valid, c.edit)
			refusedBySchema := schemaRefuses(t, edited)
			if !refusedBySchema && !c.goOnly {
				t.Fatalf("the schema accepts this document, so the case proves nothing")
			}
			if refusedBySchema && c.goOnly {
				t.Fatalf("the schema refuses this document, so it is not a rule beyond the schema")
			}
			if err := newStub(t, sink.StubConfig{}).Accept(tenantA, edited); err == nil {
				t.Fatalf("the stub accepted a document the schema refuses")
			}
		})
	}
}

// TestTheStubRefusesTheThreeByteAndNameRulesNoSchemaCanMake: the three checks ADR 4 says a sink
// validating with the schema alone has to make itself, in the form a schema cannot see. Each
// document here is one a JSON Schema validator accepts.
//
// Each case asserts the rule that refused it and not merely that something did. Every one of
// these documents differs from a valid one in more than one way a decoder could trip over, so
// one refusal that swallowed all four would otherwise keep the test green while the three rules
// it is named for had gone.
func TestTheStubRefusesTheThreeByteAndNameRulesNoSchemaCanMake(t *testing.T) {
	t.Parallel()
	valid := doc(t, rec(t, tenantA))
	id := rec(t, tenantA).ID

	for name, c := range map[string]struct {
		document []byte
		want     string
	}{
		// encoding/json puts U+FFFD in place of a lone surrogate escape, so this document and
		// the one with \uDC00 would decode to one external id.
		"an unpaired surrogate escape": {
			document: bytes.Replace(valid,
				[]byte(`"title":"Numbers are in"`), []byte(`"title":"\uD800 is half a pair"`), 1),
			want: "the record has an escape for half of a surrogate pair",
		},
		// The same, one level down: an invalid byte becomes U+FFFD as well.
		"bytes that are not UTF-8": {
			document: bytes.Replace(valid,
				[]byte(`"title":"Numbers are in"`), []byte("\"title\":\"\xff\xfe\""), 1),
			want: "the record is not valid UTF-8",
		},
		// Decoders disagree about which of the two counts, so a validator and a consumer can
		// read two different scopes out of this one document.
		"a field name used twice": {
			document: bytes.Replace(valid,
				[]byte(`"title":`), []byte(`"text":"first","title":`), 1),
			want: "the record uses a field name twice in one object",
		},
		// JSON Schema cannot compare two fields of one document.
		"a record that supersedes itself": {
			document: bytes.Replace(valid,
				[]byte(`"supersedes":null`), []byte(`"supersedes":"`+id+`"`), 1),
			want: "supersedes names the record itself",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if bytes.Equal(c.document, valid) {
				t.Fatalf("the document was not edited, so the case proves nothing")
			}
			if schemaRefuses(t, c.document) {
				t.Fatalf("the schema refuses this document, so it is not a rule beyond the schema")
			}
			err := newStub(t, sink.StubConfig{}).Accept(tenantA, c.document)
			if err == nil {
				t.Fatalf("the stub accepted it")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("Accept: %v, want the refusal to be %q", err, c.want)
			}
		})
	}
}

// TestTheStubRefusesARepeatedIDWithDifferentContent is the second half of the first acceptance
// line, and the check ADR 4 decision 7's promise to a sink rests on: one id never appears with
// two scopes.
func TestTheStubRefusesARepeatedIDWithDifferentContent(t *testing.T) {
	t.Parallel()
	first := rec(t, tenantA)
	for name, changed := range map[string][]byte{
		"another title": edit(t, doc(t, first), func(d map[string]any) { d["title"] = "Numbers are out" }),
		"another scope": edit(t, doc(t, first), func(d map[string]any) {
			v, _ := d["visibility"].(map[string]any)
			v["scope"] = "clickup:list:902"
		}),
		"another text": edit(t, doc(t, first), func(d map[string]any) { d["text"] = "" }),
		"another supersedes": edit(t, doc(t, first), func(d map[string]any) {
			d["supersedes"] = "rec_00000000000000000000000000000001"
		}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t, sink.StubConfig{})
			if err := s.Accept(tenantA, doc(t, first)); err != nil {
				t.Fatalf("the first document was refused: %v", err)
			}
			err := s.Accept(tenantA, changed)
			if !errors.Is(err, sink.ErrRepeatDiffers) {
				t.Fatalf("second Accept: %v, want ErrRepeatDiffers", err)
			}
			if got := len(s.Documents(tenantA)); got != 1 {
				t.Fatalf("the stub holds %d documents, want the first one only", got)
			}
		})
	}
}

// TestTheStubComparesANumberByTheLiteralItWasWrittenWith: contentOf decodes with
// json.Decoder.UseNumber, so a number keeps the literal it arrived with. Without it every number
// is read as a float64 and written back in the shortest spelling that round-trips, and two
// documents that differ in the last digit of a twenty digit integer become one content: the stub
// would answer "the same record" to the second and go on holding the first. That is a silent
// wrong acceptance in the one check ADR 4 decision 7's promise to a sink rests on, and it is the
// dangerous direction, because a refusal would at least be loud.
func TestTheStubComparesANumberByTheLiteralItWasWrittenWith(t *testing.T) {
	t.Parallel()
	first := doc(t, rec(t, tenantA))
	for name, pair := range map[string][2]string{
		"a twenty digit integer and the same integer plus one": {"12345678901234567890", "12345678901234567891"},
		"an integer and the same value written as a decimal":   {"1", "1.0"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t, sink.StubConfig{})
			if err := s.Accept(tenantA, withRaw(t, first, "extra", pair[0])); err != nil {
				t.Fatalf("the first document was refused: %v", err)
			}
			err := s.Accept(tenantA, withRaw(t, first, "extra", pair[1]))
			if !errors.Is(err, sink.ErrRepeatDiffers) {
				t.Fatalf("second Accept: %v, want ErrRepeatDiffers", err)
			}
			if got := len(s.Documents(tenantA)); got != 1 {
				t.Fatalf("the stub holds %d documents, want the first one only", got)
			}
		})
	}
}

// TestTheStubTakesARepeatThatDiffersOnlyInMeta: meta is not part of a record's content (ADR 4),
// so the same id with another meta.delivery is the same record and the repeat is a no-op.
func TestTheStubTakesARepeatThatDiffersOnlyInMeta(t *testing.T) {
	t.Parallel()
	first := doc(t, rec(t, tenantA))
	for name, repeat := range map[string][]byte{
		"another delivery": edit(t, first, func(d map[string]any) {
			d["meta"] = map[string]any{"delivery": "01JZXA8Q2K4M7N9P0R3S5T6V8X"}
		}),
		"no meta at all": edit(t, first, func(d map[string]any) { delete(d, "meta") }),
		"an empty meta":  edit(t, first, func(d map[string]any) { d["meta"] = map[string]any{} }),
		"the same bytes": first,
		"the fields shuffled": func() []byte {
			var tree map[string]json.RawMessage
			if err := json.Unmarshal(first, &tree); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			// encoding/json writes a map's keys sorted, which is a different order from the
			// struct's field order.
			out, err := json.Marshal(tree)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			return out
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newStub(t, sink.StubConfig{})
			if err := s.Accept(tenantA, first); err != nil {
				t.Fatalf("the first document was refused: %v", err)
			}
			if err := s.Accept(tenantA, repeat); err != nil {
				t.Fatalf("the repeat was refused: %v", err)
			}
			if got := len(s.Documents(tenantA)); got != 1 {
				t.Fatalf("the stub holds %d documents, want one", got)
			}
		})
	}
}

// TestTheStubKeepsTenantsApart: the tenant is in no field of the envelope, so two tenants'
// records would otherwise meet under one id.
func TestTheStubKeepsTenantsApart(t *testing.T) {
	t.Parallel()
	s := newStub(t, sink.StubConfig{})
	a, b := rec(t, tenantA), rec(t, tenantB)
	if a.ID == b.ID {
		t.Fatalf("the two tenants minted one id, so this test proves nothing")
	}
	// The same document, under the other tenant: the id is A's, and B has never seen it.
	if err := s.Accept(tenantA, doc(t, a)); err != nil {
		t.Fatalf("tenant A: %v", err)
	}
	if err := s.Accept(tenantB, doc(t, a)); err != nil {
		t.Fatalf("tenant B: %v", err)
	}
	if got := len(s.Documents(tenantB)); got != 1 {
		t.Fatalf("tenant B holds %d documents, want one", got)
	}
	// And a change under B does not touch what A holds.
	changed := edit(t, doc(t, a), func(d map[string]any) { d["title"] = "other" })
	if err := s.Accept(tenantB, changed); !errors.Is(err, sink.ErrRepeatDiffers) {
		t.Fatalf("tenant B repeat: %v, want ErrRepeatDiffers", err)
	}
	if got := len(s.Documents(tenantA)); got != 1 {
		t.Fatalf("tenant A holds %d documents, want one", got)
	}
}

// TestTheStubBoundsADocumentBeforeParsing: nothing in the format bounds a document, because
// unknown fields are allowed and unbounded, so the sink's own limit is the bound there is, and
// it answers before the decoder is handed the bytes.
func TestTheStubBoundsADocumentBeforeParsing(t *testing.T) {
	t.Parallel()
	s := newStub(t, sink.StubConfig{MaxDocumentBytes: 4096})
	small := doc(t, rec(t, tenantA))
	if len(small) > 4096 {
		t.Fatalf("the fixture is %d bytes, which is over the limit under test", len(small))
	}
	if err := s.Accept(tenantA, small); err != nil {
		t.Fatalf("a document under the limit was refused: %v", err)
	}
	// Over the limit, and not parseable either: the answer must be the size and not a parse
	// error, which is what "before the decoder" means here.
	big := append([]byte("{not json"), bytes.Repeat([]byte("x"), 8192)...)
	err := s.Accept(tenantA, big)
	if !errors.Is(err, sink.ErrTooLarge) {
		t.Fatalf("Accept of an oversized document: %v, want ErrTooLarge", err)
	}
	// And a valid document over the limit is refused for its size too.
	padded := edit(t, small, func(d map[string]any) { d["pad"] = strings.Repeat("p", 8192) })
	if err := s.Accept(tenantA, padded); !errors.Is(err, sink.ErrTooLarge) {
		t.Fatalf("Accept of an oversized valid document: %v, want ErrTooLarge", err)
	}
	if got := len(s.Documents(tenantA)); got != 1 {
		t.Fatalf("the stub holds %d documents, want the small one only", got)
	}
}

// TestTheStubRefusesASupersedeCycle: a cycle across records is invisible to anything that
// validates one record at a time, and a receiver that walks the chain without a visited set
// walks it forever.
func TestTheStubRefusesASupersedeCycle(t *testing.T) {
	t.Parallel()
	s := newStub(t, sink.StubConfig{})
	first := rec(t, tenantA, func(r *record.Record) { r.Version = "1" })
	second := rec(t, tenantA, func(r *record.Record) { r.Version = "2" })
	third := rec(t, tenantA, func(r *record.Record) { r.Version = "3" })

	// second names first, which has not arrived: a chain may point forward.
	withSupersedes := func(r record.Record, id string) []byte {
		r.Supersedes = record.Ref(id)
		return doc(t, r)
	}
	if err := s.Accept(tenantA, withSupersedes(second, first.ID)); err != nil {
		t.Fatalf("second: %v", err)
	}
	if err := s.Accept(tenantA, withSupersedes(third, second.ID)); err != nil {
		t.Fatalf("third: %v", err)
	}
	// first closes the loop: first to third to second to first.
	err := s.Accept(tenantA, withSupersedes(first, third.ID))
	if !errors.Is(err, sink.ErrSupersedeCycle) {
		t.Fatalf("the closing record: %v, want ErrSupersedeCycle", err)
	}
	if got := len(s.Documents(tenantA)); got != 2 {
		t.Fatalf("the stub holds %d documents, want the two that did not close a loop", got)
	}
	// A chain that does not close is still taken.
	if err := s.Accept(tenantA, withSupersedes(first, "rec_00000000000000000000000000000001")); err != nil {
		t.Fatalf("a chain pointing at a record that never arrived: %v", err)
	}
}

// TestTheStubRefusesATwoRecordSupersedeCycle is the shortest cycle that spans two records, and
// the one the walk reaches on its second step rather than its third. A one-record cycle is the
// self-supersede rule, which Validate makes on its own.
func TestTheStubRefusesATwoRecordSupersedeCycle(t *testing.T) {
	t.Parallel()
	s := newStub(t, sink.StubConfig{})
	first := rec(t, tenantA, func(r *record.Record) { r.Version = "1" })
	second := rec(t, tenantA, func(r *record.Record) { r.Version = "2" })
	withSupersedes := func(r record.Record, id string) []byte {
		r.Supersedes = record.Ref(id)
		return doc(t, r)
	}
	if err := s.Accept(tenantA, withSupersedes(first, second.ID)); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.Accept(tenantA, withSupersedes(second, first.ID)); !errors.Is(err, sink.ErrSupersedeCycle) {
		t.Fatalf("the closing record: %v, want ErrSupersedeCycle", err)
	}
	if got := len(s.Documents(tenantA)); got != 1 {
		t.Fatalf("the stub holds %d documents, want the one that did not close a loop", got)
	}
}

// TestTheStubsDeliverRejectsOneRecordAndKeepsTheRest is the "sink rejects one record" row of
// architecture section 11.
func TestTheStubsDeliverRejectsOneRecordAndKeepsTheRest(t *testing.T) {
	t.Parallel()
	s := newStub(t, sink.StubConfig{})
	good := rec(t, tenantA, func(r *record.Record) { r.Version = "1" })
	other := rec(t, tenantA, func(r *record.Record) { r.Version = "2" })

	if _, err := s.Deliver(context.Background(), tenantA, []record.Record{good}); err != nil {
		t.Fatalf("first Deliver: %v", err)
	}
	// repeat carries good's id with another title, because the title is not in the id.
	changed := rec(t, tenantA, func(r *record.Record) {
		r.Version = "1"
		r.Title = "Numbers are out"
	})
	if changed.ID != good.ID {
		t.Fatalf("the title changed the id, so this test proves nothing")
	}
	result, err := s.Deliver(context.Background(), tenantA, []record.Record{changed, other})
	if err != nil {
		t.Fatalf("second Deliver: %v", err)
	}
	if len(result.Rejected) != 1 || result.Rejected[0].ID != changed.ID {
		t.Fatalf("rejected %+v, want the repeated id alone", result.Rejected)
	}
	if got, want := result.Rejected[0].Cause.String(), "sink rejected the record (code repeat_differs)"; got != want {
		t.Errorf("cause %q, want %q", got, want)
	}
	if got := len(s.Documents(tenantA)); got != 2 {
		t.Fatalf("the stub holds %d documents, want the first and the one that landed beside the rejection", got)
	}
}

// TestTheStubRefusesATenantItCannotParse: ID is a string type, so a caller can build one without
// tenancy.Parse, and the stub keys what it holds by it.
func TestTheStubRefusesATenantItCannotParse(t *testing.T) {
	t.Parallel()
	s := newStub(t, sink.StubConfig{})
	valid := doc(t, rec(t, tenantA))
	for _, bad := range []tenancy.ID{"", "../etc", "a b", tenancy.ID(strings.Repeat("t", 65))} {
		if err := s.Accept(bad, valid); !errors.Is(err, tenancy.ErrInvalidID) {
			t.Errorf("Accept(%q): %v, want ErrInvalidID", bad, err)
		}
		if _, err := s.Deliver(context.Background(), bad, nil); !errors.Is(err, tenancy.ErrInvalidID) {
			t.Errorf("Deliver(%q): %v, want ErrInvalidID", bad, err)
		}
	}
}

// TestTheStubIsSafeForConcurrentUse: the worker drains with a pool of goroutines (architecture
// section 9), so the double they all deliver to has to hold up under -race.
func TestTheStubIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	s := newStub(t, sink.StubConfig{})
	const n = 16
	docs := make([][]byte, n)
	for i := range docs {
		docs[i] = doc(t, rec(t, tenantA, func(r *record.Record) { r.Version = string(rune('a' + i)) }))
	}
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := s.Accept(tenantA, docs[i]); err != nil {
				t.Errorf("Accept: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			s.Documents(tenantA)
		}()
	}
	wg.Wait()
	if got := len(s.Documents(tenantA)); got != n {
		t.Fatalf("the stub holds %d documents, want %d", got, n)
	}
}

// TestContentOfDropsMetaAndNothingElse pins the comparison the repeat check rests on.
func TestContentOfDropsMetaAndNothingElse(t *testing.T) {
	t.Parallel()
	withMeta := doc(t, rec(t, tenantA))
	content, err := sink.ContentOf(withMeta)
	if err != nil {
		t.Fatalf("ContentOf: %v", err)
	}
	var tree map[string]json.RawMessage
	if err := json.Unmarshal(content, &tree); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := tree["meta"]; ok {
		t.Errorf("meta is still there")
	}
	for _, field := range []string{
		"format", "id", "op", "source", "kind", "external_id", "version", "supersedes",
		"occurred_at", "title", "text", "author", "container", "visibility", "origin", "edges",
	} {
		if _, ok := tree[field]; !ok {
			t.Errorf("%s was dropped", field)
		}
	}
}
