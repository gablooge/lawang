package provider

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/gablooge/lawang/internal/record"
)

// ErrBadKey reports a provider whose Key is not a provider key. It names the key, which is safe:
// a provider key is a constant of this program and never text from a request.
var ErrBadKey = errors.New("provider: not a provider key")

// ErrDuplicateKey reports two providers registered under one key.
var ErrDuplicateKey = errors.New("provider: key registered twice")

// ErrNilProvider reports a nil in the list handed to NewRegistry.
var ErrNilProvider = errors.New("provider: nil provider")

// Registry is the set of providers this process serves. It is built once, at startup, from a list
// known at compile time, and it never changes afterwards: there is no Add, so every read of it is
// safe from any number of goroutines with no lock, which is what the accept path needs.
//
// Its job on the accept path is to turn one path segment, which is text a stranger sent, into a
// provider and into that provider's key as this program spells it. Those are two different
// strings: the segment is whatever arrived (net/http decodes %00 in /ingress/{provider} into a NUL
// byte), and the key is the one validated at registration. Everything downstream is handed the
// key, never the segment, because outbox.Delivery.Provider must be a constant of the program.
type Registry struct {
	byKey map[string]Entry
}

// Entry is one provider in a Registry, together with the key the registry validated and keeps.
// The zero Entry names no provider: its Key is empty and its Provider is nil.
type Entry struct {
	key string
	p   Provider
}

// Key is the provider key as this program spells it: validated at registration, and the string to
// hand to the outbox, the ledger and every record. It is never the string a caller looked up with,
// even when the two hold the same bytes.
func (e Entry) Key() string { return e.key }

// Provider is the registered provider. Backlog item B08 calls Hydrate and Normalize through it:
// the drain path has the outbox row's provider key and needs the provider itself, while the
// accept path only ever needs WebhookSource.
func (e Entry) Provider() Provider { return e.p }

// WebhookSource reports whether the provider receives webhooks, and returns that capability. It
// is the type assertion of architecture section 7, in one place, so a caller does not repeat it.
func (e Entry) WebhookSource() (WebhookSource, bool) {
	ws, ok := e.p.(WebhookSource)
	return ws, ok
}

// NewRegistry validates every provider and returns the registry, or the first thing wrong with
// the list. It refuses a nil provider, a key that record.ValidProviderKey refuses, and a key
// registered twice.
//
// The key grammar is not written down here. It is ADR 3's, and record.ValidProviderKey owns it,
// because the same string is the first segment of every scope id and of every external id: a
// registry that accepted "ms-graph" would register a provider whose every record fails in
// record.Seal, far from here. TestTheRegistryCannotDriftFromTheRecordFormat holds the two to the
// same verdict.
//
// Key is called exactly once per provider, and the result is copied into the registry, so a
// provider whose Key changes later cannot change what is stored or what the edge answers.
func NewRegistry(providers ...Provider) (*Registry, error) {
	r := &Registry{byKey: make(map[string]Entry, len(providers))}
	for _, p := range providers {
		if p == nil {
			return nil, ErrNilProvider
		}
		key := p.Key()
		if !record.ValidProviderKey(key) {
			// The key is a program constant, so quoting it points at the bug. %q also keeps a
			// stray control character out of the log as an escape.
			return nil, fmt.Errorf("%w: %q", ErrBadKey, key)
		}
		if _, dup := r.byKey[key]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateKey, key)
		}
		r.byKey[key] = Entry{key: key, p: p}
	}
	return r, nil
}

// Entries returns every registered provider, ordered by key. The order is fixed so that a start-up
// refusal that names a provider (hub.New refuses a URL-signing provider with no public base URL
// configured) names the same one on every start, whatever the map iteration does that day.
//
// The slice is the caller's, and an Entry carries only the validated key and the provider itself,
// so nothing a caller does to it can change the registry.
func (r *Registry) Entries() []Entry {
	out := make([]Entry, 0, len(r.byKey))
	for _, key := range slices.Sorted(maps.Keys(r.byKey)) {
		out = append(out, r.byKey[key])
	}
	return out
}

// Lookup finds the provider registered under key. key may be anything at all, path segment
// included: a miss is the only thing an unregistered string can produce, and the Entry that comes
// back carries the registry's own copy of the key.
func (r *Registry) Lookup(key string) (Entry, bool) {
	e, ok := r.byKey[key]
	return e, ok
}
