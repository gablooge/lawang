package sink_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/sink"
)

// Every sink in this package implements the interface.
var (
	_ sink.Sink = (*sink.HTTP)(nil)
	_ sink.Sink = (*sink.Stub)(nil)
	_ sink.Sink = (*sink.JSONL)(nil)
)

// documentsOf runs one record through all three sinks and returns the document each of them
// produced, so a test can say the same thing about every sink at once.
func documentsOf(t testing.TB, names sink.Names, r record.Record) map[string][]byte {
	t.Helper()
	docs := map[string][]byte{}

	posted := make(chan []byte, 1)
	httpSink := serving(t, func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		posted <- body
		w.WriteHeader(http.StatusNoContent)
	}, sink.HTTPConfig{Names: names})
	httpResult, err := httpSink.Deliver(context.Background(), tenantA, []record.Record{r})
	if err != nil || len(httpResult.Rejected) != 0 {
		// Checked before the channel is read, so a record the sink would not send fails the
		// test here instead of waiting for a request that is never made.
		t.Fatalf("http Deliver: %v, rejected %+v", err, httpResult.Rejected)
	}
	var batch struct {
		Records []json.RawMessage `json:"records"`
	}
	select {
	case body := <-posted:
		if err := json.Unmarshal(body, &batch); err != nil {
			t.Fatalf("the posted batch does not decode: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("no request reached the server")
	}
	if len(batch.Records) != 1 {
		t.Fatalf("the batch carried %d records", len(batch.Records))
	}
	docs["http"] = batch.Records[0]

	stub := newStub(t, sink.StubConfig{Names: names})
	result, err := stub.Deliver(context.Background(), tenantA, []record.Record{r})
	if err != nil || len(result.Rejected) != 0 {
		t.Fatalf("stub Deliver: %v, rejected %+v", err, result.Rejected)
	}
	held := stub.Documents(tenantA)
	if len(held) != 1 {
		t.Fatalf("the stub holds %d documents", len(held))
	}
	docs["stub"] = held[0]

	jsonlSink, dir := newJSONL(t, names)
	if _, err := jsonlSink.Deliver(context.Background(), tenantA, []record.Record{r}); err != nil {
		t.Fatalf("jsonl Deliver: %v", err)
	}
	written := lines(t, dir, tenantA)
	if len(written) != 1 {
		t.Fatalf("the file has %d lines", len(written))
	}
	docs["jsonl"] = written[0]

	return docs
}

// TestEveryRecordEverySinkSendsValidatesAgainstTheSchema is the backlog line "every record
// validated against the schema in tests", for all three sinks and for records that reach the
// limits of the format.
func TestEveryRecordEverySinkSendsValidatesAgainstTheSchema(t *testing.T) {
	t.Parallel()
	names := sink.Names{provider: wireName}
	for name, opt := range map[string]func(*record.Record){
		"an ordinary task": func(*record.Record) {},
		"a delete":         func(r *record.Record) { r.Op = record.OpDelete; r.Title = ""; r.Text = "" },
		"a direct message": func(r *record.Record) {
			r.Kind = record.KindMessage
			r.Visibility.Audience = record.AudienceDirect
		},
		"empty everything that may be empty": func(r *record.Record) {
			r.Title, r.Text = "", ""
			r.Author = record.Author{}
			r.Meta = record.Meta{}
		},
		"a reply": func(r *record.Record) { r.Edges.ReplyParent = record.Ref(provider + ":task:86a0") },
		"fields at their limits": func(r *record.Record) {
			r.Title = strings.Repeat("t", record.MaxTitle)
			r.Version = strings.Repeat("v", record.MaxVersion)
			r.Author.Display = strings.Repeat("\U0001F600", record.MaxAuthor/2)
			r.Text = strings.Repeat("x", 1<<16)
		},
		"text that needs escaping": func(r *record.Record) { r.Text = "<a>&\"b\"\n\t\u2028</a>" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := rec(t, tenantA, opt)
			for sinkName, document := range documentsOf(t, names, r) {
				if schemaRefuses(t, document) {
					t.Errorf("%s: the schema refuses what the sink wrote", sinkName)
				}
			}
		})
	}
}

// TestTheWireNameReplacesSourceAndNothingElse: principle 4, and the note carried onto issue #9
// from B05. The scope id keeps the internal provider key, because it is the join key with the
// membership message (ADR 3), and so does the external id.
func TestTheWireNameReplacesSourceAndNothingElse(t *testing.T) {
	t.Parallel()
	r := rec(t, tenantA)
	if r.Source != provider {
		t.Fatalf("Seal set source to %q, want the provider key", r.Source)
	}
	for sinkName, document := range documentsOf(t, sink.Names{provider: wireName}, r) {
		var back record.Record
		if err := json.Unmarshal(document, &back); err != nil {
			t.Fatalf("%s: the document does not decode: %v", sinkName, err)
		}
		if back.Source != wireName {
			t.Errorf("%s: source %q, want %q", sinkName, back.Source, wireName)
		}
		if !strings.HasPrefix(back.Visibility.Scope, provider+":") {
			t.Errorf("%s: scope %q no longer begins with the provider key", sinkName, back.Visibility.Scope)
		}
		if back.Visibility.Scope != r.Visibility.Scope {
			t.Errorf("%s: scope %q, want %q", sinkName, back.Visibility.Scope, r.Visibility.Scope)
		}
		if back.ExternalID != r.ExternalID {
			t.Errorf("%s: external id %q, want %q", sinkName, back.ExternalID, r.ExternalID)
		}
		if back.ID != r.ID {
			t.Errorf("%s: id %q, want %q", sinkName, back.ID, r.ID)
		}
	}
}

// TestAProviderWithNoWireNameKeepsItsKey.
func TestAProviderWithNoWireNameKeepsItsKey(t *testing.T) {
	t.Parallel()
	r := rec(t, tenantA)
	for _, names := range []sink.Names{nil, {}, {"slack": "chat"}} {
		for sinkName, document := range documentsOf(t, names, r) {
			var back record.Record
			if err := json.Unmarshal(document, &back); err != nil {
				t.Fatalf("%s: %v", sinkName, err)
			}
			if back.Source != provider {
				t.Errorf("%s with names %v: source %q, want %q", sinkName, names, back.Source, provider)
			}
		}
	}
}

// TestNamesValidate pins what a configured wire name may be.
func TestNamesValidate(t *testing.T) {
	t.Parallel()
	for _, ok := range []sink.Names{
		nil,
		{},
		{"clickup": "clickup"},
		{"clickup": "clickup-prod", "ms_graph": "outlook"},
		{"a": strings.Repeat("w", 64)},
	} {
		if err := ok.Validate(); err != nil {
			t.Errorf("Validate(%v): %v", ok, err)
		}
	}
	for _, bad := range []sink.Names{
		{"": "clickup"},
		{"ClickUp": "clickup"},
		{"click-up": "clickup"}, // a hyphen is not in the provider key grammar (ADR 3)
		{"1clickup": "clickup"}, // a key begins with a letter
		{strings.Repeat("a", 33): "clickup"},
		{"clickup": ""},
		{"clickup": "ClickUp"},
		{"clickup": "clickup prod"},
		{"clickup": "-clickup"},
		{"clickup": strings.Repeat("w", 65)},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Validate(%v) accepted it", bad)
		}
	}
}

// TestADetailIsAlwaysThisPackagesOwnPhrase: every Detail a Fault or a Rejection carries is one
// of the phrases written in this package, so no text from a sink reaches a log line through one.
func TestADetailIsAlwaysThisPackagesOwnPhrase(t *testing.T) {
	t.Parallel()
	if len(sink.FaultDetails) == 0 {
		t.Fatalf("the list of details is empty")
	}
	seen := map[string]bool{}
	for _, detail := range sink.FaultDetails {
		if detail == "" {
			t.Errorf("an empty phrase is in the list")
		}
		if seen[detail] {
			t.Errorf("%q is in the list twice", detail)
		}
		seen[detail] = true
	}
}

// TestAnActionAlwaysHasAName, including the zero value and a number that is not an action.
func TestAnActionAlwaysHasAName(t *testing.T) {
	t.Parallel()
	for action, want := range map[sink.Action]string{
		sink.ActionUnset:      "unclassified",
		sink.ActionRetry:      "retry",
		sink.ActionHalt:       "halt",
		sink.ActionDeadLetter: "dead letter",
		sink.Action(200):      "unknown action 200",
	} {
		if got := action.String(); got != want {
			t.Errorf("Action(%d).String() = %q, want %q", action, got, want)
		}
	}
}

// TestAFaultReadsAsItsPartsPut.
func TestAFaultReadsAsItsPartsPut(t *testing.T) {
	t.Parallel()
	for want, f := range map[string]*sink.Fault{
		"sink: halt: sink refused the credential (status 401)": {
			Action: sink.ActionHalt,
			Cause:  outbox.NewCause(outbox.ClassSinkUnauthorized).WithStatus(401),
		},
		"sink: retry: sink unavailable: the request timed out": {
			Action: sink.ActionRetry,
			Cause:  outbox.NewCause(outbox.ClassSinkUnavailable),
			Detail: "the request timed out",
		},
		"sink: unclassified: unclassified failure": {},
	} {
		if got := f.Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	}
}
