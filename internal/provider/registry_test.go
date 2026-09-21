package provider_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"unsafe"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// TestTheRegistryCannotDriftFromTheRecordFormat is the test the provider key grammar exists for.
// ADR 3 freezes the grammar, record.ValidProviderKey owns it, and the registry must call that
// function and not carry a copy of the pattern: a registry that accepted "ms-graph" would register
// a provider whose every record fails in record.Seal, and a registry that refused something Seal
// accepts would make a provider unreachable.
//
// So the two are asked about every candidate, and must always agree. The candidates cover every
// byte in the first and in a later position, which is where a hand-written pattern differs from
// this one.
func TestTheRegistryCannotDriftFromTheRecordFormat(t *testing.T) {
	t.Parallel()

	var candidates []string
	for b := range 256 {
		candidates = append(candidates, string(rune(b)), "a"+string(rune(b)), "a"+string(rune(b))+"b")
	}
	candidates = append(candidates,
		"", "a", "clickup", "slack", "ms_graph", "hubspot", "fake",
		"ms-graph", "9lives", "Slack", "_leading", "with space", "a.b", "a:b",
		strings.Repeat("a", 31), strings.Repeat("a", 32), strings.Repeat("a", 33),
		"a"+strings.Repeat("0", 31), "a"+strings.Repeat("0", 32),
	)

	for _, key := range candidates {
		want := record.ValidProviderKey(key)
		_, err := provider.NewRegistry(fake.New(key))
		got := err == nil
		if got != want {
			t.Fatalf("key %q: registry accepts %v, record.ValidProviderKey says %v", key, got, want)
		}
		if !want && !errors.Is(err, provider.ErrBadKey) {
			t.Fatalf("key %q: want ErrBadKey, got %v", key, err)
		}
	}
}

// TestNewRegistryRefusesTheListItCannotServe covers the two other ways a list is wrong. Both are
// bugs in the wiring, so both are caught at startup and never at the first request.
func TestNewRegistryRefusesTheListItCannotServe(t *testing.T) {
	t.Parallel()

	if _, err := provider.NewRegistry(fake.New("a"), nil); !errors.Is(err, provider.ErrNilProvider) {
		t.Fatalf("nil provider: want ErrNilProvider, got %v", err)
	}
	_, err := provider.NewRegistry(fake.New("clickup"), fake.New("clickup"))
	if !errors.Is(err, provider.ErrDuplicateKey) {
		t.Fatalf("duplicate: want ErrDuplicateKey, got %v", err)
	}
	if !strings.Contains(err.Error(), "clickup") {
		t.Fatalf("the error should name the key, which is a program constant: %v", err)
	}
}

// shifty returns a different key every time it is asked. Nothing legitimate does that; the point
// is that the registry asks once and keeps its own copy, so nothing downstream can be steered by
// a provider that answers differently later.
type shifty struct{ asked int }

func (s *shifty) Key() string {
	s.asked++
	if s.asked == 1 {
		return "first"
	}
	return "second"
}

func (s *shifty) Hydrate(_ context.Context, _ tenancy.ID, _ provider.Change) (provider.Hydrated, error) {
	return nil, errors.New("not used")
}

func (s *shifty) Normalize(_ provider.Hydrated, _ provider.Change) ([]record.Record, error) {
	return nil, errors.New("not used")
}

func TestTheRegistryAsksForAKeyOnceAndKeepsItsOwnCopy(t *testing.T) {
	t.Parallel()

	p := &shifty{}
	reg, err := provider.NewRegistry(p)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if p.asked != 1 {
		t.Fatalf("Key was asked %d times, want exactly 1", p.asked)
	}
	if _, ok := reg.Lookup("second"); ok {
		t.Fatal("the registry followed the provider's second answer")
	}
	entry, ok := reg.Lookup("first")
	if !ok || entry.Key() != "first" {
		t.Fatalf("Lookup(first) = %q, %v", entry.Key(), ok)
	}
	if p.asked != 1 {
		t.Fatalf("Key was asked again after registration (%d times)", p.asked)
	}
}

// TestLookupReturnsTheRegistrysStringAndNotTheCallers is decision 2 of issue #6, at the level it
// actually matters: outbox.Delivery.Provider must be a constant of the program, so the key that
// comes back out of a lookup has to be the registry's own string and not the one the caller
// looked up with, even though the two hold the same bytes today. Comparing the string data
// pointers is the only way to tell them apart.
func TestLookupReturnsTheRegistrysStringAndNotTheCallers(t *testing.T) {
	t.Parallel()

	reg, err := provider.NewRegistry(fake.New("clickup"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	// A distinct allocation with the same bytes, as a decoded path segment would be.
	lookedUpWith := string([]byte("clickup"))
	entry, ok := reg.Lookup(lookedUpWith)
	if !ok {
		t.Fatal("Lookup missed")
	}
	if entry.Key() != lookedUpWith {
		t.Fatalf("Key() = %q, want the same bytes", entry.Key())
	}
	if unsafe.StringData(entry.Key()) == unsafe.StringData(lookedUpWith) {
		t.Fatal("Key() handed back the caller's own string, so request text could travel on")
	}
}

// TestWebhookSourceIsFoundByTypeAssertion covers the optional-capability rule of architecture
// section 7: a provider that has no webhook must not be reachable at the ingress edge.
func TestWebhookSourceIsFoundByTypeAssertion(t *testing.T) {
	t.Parallel()

	reg, err := provider.NewRegistry(fake.New("fake"), &shifty{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	withWebhook, _ := reg.Lookup("fake")
	if _, ok := withWebhook.WebhookSource(); !ok {
		t.Fatal("the fake provider is a WebhookSource")
	}
	withoutWebhook, _ := reg.Lookup("first")
	if _, ok := withoutWebhook.WebhookSource(); ok {
		t.Fatal("a provider with no webhook methods must not pass for a WebhookSource")
	}
}

// TestTheZeroEntryNamesNoProvider pins what a caller sees if it ever keeps an Entry from a failed
// lookup.
func TestTheZeroEntryNamesNoProvider(t *testing.T) {
	t.Parallel()

	var e provider.Entry
	if e.Key() != "" || e.Provider() != nil {
		t.Fatalf("zero Entry = %q, %v", e.Key(), e.Provider())
	}
	if _, ok := e.WebhookSource(); ok {
		t.Fatal("the zero Entry is not a WebhookSource")
	}
}

// TestAHeaderCannotBeUsedToChangeWhatTheNextCandidateReads is why Request.Header is a type of this
// package and not an http.Header.
//
// The hub hands one Request to Verify once per candidate subscription (B07), so anything an
// implementation could write to would be read by every candidate after it. A tenant with two
// subscriptions on one workspace would then see a signature fail for the second candidate only,
// and only sometimes, which is about the worst failure shape available. There is no Set and no
// Del to call here, and the one method that returns something a caller could write to returns a
// copy.
func TestAHeaderCannotBeUsedToChangeWhatTheNextCandidateReads(t *testing.T) {
	t.Parallel()

	live := http.Header{}
	live.Set("X-Signature", "first")
	live.Add("X-Signature", "second")
	h := provider.NewHeader(live)

	// What one candidate can reach: the slice Values returned, and nothing else.
	got := h.Values("X-Signature")
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("Values = %q, want both values in order", got)
	}
	got[0] = "tampered"

	// What every candidate after it reads.
	if again := h.Values("X-Signature"); len(again) != 2 || again[0] != "first" || again[1] != "second" {
		t.Fatalf("a candidate changed what the next one reads: Values = %q", again)
	}
	if v := h.Get("x-signature"); v != "first" {
		t.Fatalf("Get = %q, want the first value, matched without regard to case", v)
	}
	if live.Get("X-Signature") != "first" {
		t.Fatalf("the request's own header was changed through the wrapper: %q", live)
	}

	// The zero value reads as a request that sent no header at all, rather than panicking on a
	// nil map: a Request built by hand in a test, or by a later caller, must not be a trap.
	var zero provider.Header
	if zero.Get("X-Signature") != "" || zero.Values("X-Signature") != nil {
		t.Fatal("the zero Header must read as empty")
	}
}
