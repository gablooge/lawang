package sink_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/sink"
	"github.com/gablooge/lawang/internal/tenancy"
)

const (
	tenantA  = tenancy.ID("acme")
	tenantB  = tenancy.ID("globex")
	provider = "clickup"
	// wireName is what the receiver in these tests calls the provider. It is deliberately not
	// the provider key and not the first segment of the scope.
	wireName = "clickup-prod"
)

// tokenFor is an HTTPConfig.Token that hands out a token per tenant.
func tokenFor(token string) func(context.Context, tenancy.ID) (string, error) {
	return func(context.Context, tenancy.ID) (string, error) { return token, nil }
}

// rec builds a sealed record for a tenant. The options run before Seal, so they can change
// anything the id is minted from.
func rec(t testing.TB, tenant tenancy.ID, opts ...func(*record.Record)) record.Record {
	t.Helper()
	scope, err := record.ScopeID(provider, "list", "901")
	if err != nil {
		t.Fatalf("scope id: %v", err)
	}
	r := record.Record{
		Op:         record.OpUpsert,
		Kind:       record.KindTask,
		ExternalID: provider + ":task:86a1",
		Version:    "1752064245000",
		OccurredAt: time.Date(2026, 7, 9, 12, 30, 45, 0, time.UTC),
		Title:      "Numbers are in",
		Text:       "call me at [PHONE]",
		Author:     record.Author{ID: "5555", Display: "ben"},
		Container:  record.Container{Kind: "task", ID: "86a1"},
		Visibility: record.Visibility{Scope: scope, Audience: record.AudienceGroup},
		Meta:       record.Meta{Delivery: "01JZXA8Q2K4M7N9P0R3S5T6V8W"},
	}
	for _, opt := range opts {
		opt(&r)
	}
	sealedRecord, err := r.Seal(provider, tenant)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	return sealedRecord
}

// doc marshals a record the way a sink does.
func doc(t testing.TB, r record.Record) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// edit decodes a document into a tree, runs f on it and writes it back. It is how a test builds
// a document the Go types could not produce.
func edit(t testing.TB, in []byte, f func(map[string]any)) []byte {
	t.Helper()
	var tree map[string]any
	if err := json.Unmarshal(in, &tree); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	f(tree)
	out, err := json.Marshal(tree)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

// The schema is compiled from record.Schema(), the embedded bytes, so what these tests prove is
// true of what the binary carries. Formats are asserted, which is how a careful sink validates.
var (
	compileOnce sync.Once
	compiled    *jsonschema.Schema
	compileErr  error
)

func schema(t testing.TB) *jsonschema.Schema {
	t.Helper()
	compileOnce.Do(func() {
		var tree any
		if tree, compileErr = jsonschema.UnmarshalJSON(bytes.NewReader(record.Schema())); compileErr != nil {
			return
		}
		c := jsonschema.NewCompiler()
		c.AssertFormat()
		if compileErr = c.AddResource(record.SchemaID, tree); compileErr != nil {
			return
		}
		compiled, compileErr = c.Compile(record.SchemaID)
	})
	if compileErr != nil {
		t.Fatalf("the schema does not compile: %v", compileErr)
	}
	return compiled
}

// schemaRefuses reports whether the JSON Schema has something against a document. A document
// that is not JSON at all is refused here too, which is what a validator would do with it.
func schemaRefuses(t testing.TB, data []byte) bool {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return true
	}
	return schema(t).Validate(inst) != nil
}

// fault asserts that err is a *sink.Fault and returns it.
func fault(t testing.TB, err error) *sink.Fault {
	t.Helper()
	var f *sink.Fault
	if !errors.As(err, &f) {
		t.Fatalf("error %v (%T) is not a *sink.Fault", err, err)
	}
	return f
}
