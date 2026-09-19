package record

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/gablooge/sluiceway/internal/tenancy"
)

const examplePath = "testdata/example.json"

// The schema is compiled from Schema(), the embedded bytes, and never from the file: what the
// tests prove is then true of what the binary carries.
var (
	compileOnce sync.Once
	asserting   *jsonschema.Schema // formats asserted, which is how a careful sink validates
	annotating  *jsonschema.Schema // "format" is an annotation, which is the default of many validators
	errCompile  error
)

func compile(assertFormat bool) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(Schema()))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if assertFormat {
		c.AssertFormat()
	}
	if err := c.AddResource(SchemaID, doc); err != nil {
		return nil, err
	}
	return c.Compile(SchemaID)
}

func schemas(t testing.TB) (withFormat, withoutFormat *jsonschema.Schema) {
	t.Helper()
	compileOnce.Do(func() {
		if asserting, errCompile = compile(true); errCompile != nil {
			return
		}
		annotating, errCompile = compile(false)
	})
	if errCompile != nil {
		t.Fatalf("the schema does not compile: %v", errCompile)
	}
	return asserting, annotating
}

// schemaError validates one JSON document and returns what the schema has against it, or nil.
func schemaError(t testing.TB, sch *jsonschema.Schema, data []byte) error {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	return sch.Validate(inst)
}

func exampleBytes(t testing.TB) []byte {
	t.Helper()
	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// exampleDoc is the architecture's example as a generic document, for tests that break it.
func exampleDoc(t testing.TB) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(exampleBytes(t), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// set returns an edit that puts v at a dotted path, and del one that removes the path.
func set(path string, v any) func(map[string]any) {
	return func(doc map[string]any) {
		parent, last := walk(doc, path)
		parent[last] = v
	}
}

func del(path string) func(map[string]any) {
	return func(doc map[string]any) {
		parent, last := walk(doc, path)
		delete(parent, last)
	}
}

func walk(doc map[string]any, path string) (map[string]any, string) {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		doc = doc[p].(map[string]any)
	}
	return doc, parts[len(parts)-1]
}

const (
	testProvider = "slack"
	testTenant   = tenancy.ID("tenant_a")
)

// draft is what a normalizer hands over: everything but Format, Source and ID. It is the
// architecture's example.
func draft() Record {
	return Record{
		Op:         OpUpsert,
		Kind:       KindMessage,
		ExternalID: "slack:C0GENERAL:1752064245.000200",
		Version:    "1752064245.000200",
		OccurredAt: time.Date(2026, 7, 9, 12, 30, 45, 0, time.UTC),
		Text:       "Numbers are in, call me at [PHONE]",
		Author:     Author{ID: "U0BEN", Display: "ben"},
		Container:  Container{Kind: "channel", ID: "C0GENERAL"},
		Visibility: Visibility{Scope: "slack:channel:C0GENERAL", Audience: AudienceGroup},
		Meta:       Meta{Delivery: "01JZXA8Q2K4M7N9P0R3S5T6V8W"},
	}
}

func sealed(t testing.TB) Record {
	t.Helper()
	r, err := draft().Seal(testProvider, testTenant)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// onTheWire is r as it comes back from a document: the same record in every field, sealed for no
// tenant, because the tenant is not on the wire (see SealedFor).
func onTheWire(r Record) Record {
	r.sealedFor = ""
	return r
}
