package hub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// The doubles below are provider packages that break a promise the interfaces make and the type
// system cannot enforce (architecture 7). Each embeds the strict fake, so everything it does not
// override is the behaviour a well-written provider has, and only the one broken promise differs.

// panicking is a provider whose Verify panics. Architecture 7 says Verify never panics, and
// nothing enforces it; left alone, net/http recovers the panic per connection and the provider
// sees a dropped response rather than a status.
type panicking struct{ *fake.Provider }

func (panicking) Verify(provider.Request, []byte) bool {
	panic("a provider package that should not have panicked")
}

// panickingWithTheSecret is a provider whose Verify panics with a value built out of the secret it
// was handed. A panic value is the provider package's to choose, so the hub must not put one in a
// log line: the stack is ours, and that is what it keeps.
type panickingWithTheSecret struct{ *fake.Provider }

func (panickingWithTheSecret) Verify(_ provider.Request, secret []byte) bool {
	panic("this provider leaked " + string(secret))
}

// panickingWithASecretInATypeName is the same attack one level up, and it is the reason the hub
// does not trust a type name either. reflect.StructOf builds a type at run time and takes
// arbitrary bytes in a struct tag, and %T prints the tag, so the secret ends up in the type's own
// name. A type name is not a compile-time constant.
type panickingWithASecretInATypeName struct{ *fake.Provider }

func (panickingWithASecretInATypeName) Verify(_ provider.Request, secret []byte) bool {
	typ := reflect.StructOf([]reflect.StructField{{
		Name: "X",
		Type: reflect.TypeOf(0),
		Tag:  reflect.StructTag(secret),
	}})
	panic(reflect.New(typ).Elem().Interface())
}

// panickingWithALongTypeName is the other way a run-time type can cost a log line: a name every
// byte of which a plainly written type could have, and which is as long as the provider cares to
// make it. Nesting is free (reflect.SliceOf of reflect.SliceOf), so without a length bound one
// panicking delivery writes as much as the provider wants into the log.
type panickingWithALongTypeName struct{ *fake.Provider }

func (panickingWithALongTypeName) Verify(provider.Request, []byte) bool {
	typ := reflect.TypeOf(0)
	for range 100 {
		typ = reflect.SliceOf(typ)
	}
	panic(reflect.New(typ).Elem().Interface())
}

// deeplyPanicking is a provider whose Verify recurses before it panics, so that the stack the hub
// logs is longer than the bound it puts on it. A provider bug that recurses is ordinary (a
// normalizer that follows a cycle in a body), and without the bound each such delivery writes
// megabytes into a log an operator keeps and pays for.
type deeplyPanicking struct {
	*fake.Provider
	depth int
}

func (d deeplyPanicking) Verify(r provider.Request, secret []byte) bool {
	if d.depth > 0 {
		return deeplyPanicking{Provider: d.Provider, depth: d.depth - 1}.Verify(r, secret)
	}
	panic("a provider package that recursed and then panicked")
}

// blocking is a provider whose Verify never returns until release is closed. A test closes it in
// a t.Cleanup, so a hub that waited for Verify fails the test with a deadline rather than hanging.
type blocking struct {
	*fake.Provider
	entered chan struct{} // closed by the first Verify, so a test can wait for it
	release chan struct{}
}

func (b *blocking) Verify(provider.Request, []byte) bool {
	select {
	case <-b.entered:
	default:
		close(b.entered)
	}
	<-b.release
	return true
}

// mutating is a provider whose Verify writes over the body it was handed, which is the one thing
// provider.Request.Body asks an implementation not to do. It fills the slice with 'X' and then
// verifies what it was given, so it verifies nothing: a candidate after it that still sees the
// real bytes is the proof that the hub copies.
type mutating struct{ *fake.Provider }

func (m mutating) Verify(r provider.Request, secret []byte) bool {
	ok := m.Provider.Verify(r, secret)
	for i := range r.Body {
		r.Body[i] = 'X'
	}
	return ok
}

// keyless is a provider that reads no delivery key at all out of a body it is happy with. There is
// nothing to narrow a lookup by, and finding the owner would mean verifying every subscription of
// the provider, which is work a stranger could ask for with an empty body.
type keyless struct{ *fake.Provider }

func (keyless) DeliveryKeys([]byte, provider.Header) (provider.DeliveryKeys, error) {
	return provider.DeliveryKeys{}, nil
}

// keyed is a provider that returns whatever delivery keys a test gives it, including keys no
// well-written provider would return. The hub is handed these before anything is verified, so it
// holds them to what a lookup can use rather than trusting the provider package.
type keyed struct {
	*fake.Provider
	keys provider.DeliveryKeys
}

func (k keyed) DeliveryKeys([]byte, provider.Header) (provider.DeliveryKeys, error) {
	return k.keys, nil
}

// nulKey is a workspace id with a NUL in it. Postgres stores neither a NUL nor invalid UTF-8 in a
// text column: it answers SQLSTATE 22021, which would reach the sender as a 500 for a delivery
// that is simply not ours.
var nulKey = provider.DeliveryKeys{Workspace: "W1\x00W2"}

// longKey is a workspace id longer than the column stores, so it can match no row at all.
var longKey = provider.DeliveryKeys{Workspace: strings.Repeat("w", 257)}

// urlSigning is a provider whose signature covers the public URL, as HubSpot v3 does. A deployment
// that registers one and configures no LAWANG_PUBLIC_BASE_URL can accept none of its deliveries.
type urlSigning struct{ *fake.Provider }

func (urlSigning) SignsPublicURL() {}

// noWebhooks is a provider that receives no webhooks at all. The edge answers 404 for one before
// the hub ever sees it, so the hub's own refusal is about another caller, or a wiring mistake.
type noWebhooks struct{}

func (noWebhooks) Key() string { return "no_webhooks" }

func (noWebhooks) Hydrate(context.Context, tenancy.ID, provider.Change) (provider.Hydrated, error) {
	return nil, errors.New("no_webhooks: nothing to hydrate")
}

func (noWebhooks) Normalize(provider.Hydrated, provider.Change) ([]record.Record, error) {
	return nil, errors.New("no_webhooks: nothing to normalize")
}

// lockedBuffer is a bytes.Buffer a test can read while a logger writes to it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// jsonLogger writes to buf as JSON, one record per line, at every level. A test that asserts about
// a field's exact value reads it decoded rather than after a text handler has escaped and quoted
// it, so what it compares is what the hub logged.
func jsonLogger(buf *lockedBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// loggedRecord is the subset of a hub log record the tests below assert about.
type loggedRecord struct {
	Msg           string `json:"msg"`
	PanicType     string `json:"panic_type"`
	Stack         string `json:"stack"`
	Subscriptions string `json:"subscriptions"`
	Tenants       string `json:"tenants"`
}

// findLogged returns the one record in logged that want accepts, and fails the test when there is
// none. A test asserting on a field it did not find would otherwise pass against an empty string.
func findLogged(t *testing.T, logged string, what string, want func(loggedRecord) bool) loggedRecord {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if line == "" {
			continue
		}
		var got loggedRecord
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("a log line is not JSON (%v):\n%s", err, line)
		}
		if want(got) {
			return got
		}
	}
	t.Fatalf("no log record %s:\n%s", what, logged)
	return loggedRecord{}
}

// ambiguityLine is the record a parked ambiguous delivery writes.
func ambiguityLine(t *testing.T, logged string) loggedRecord {
	t.Helper()
	return findLogged(t, logged, "names the subscriptions of an ambiguous delivery",
		func(r loggedRecord) bool { return r.Subscriptions != "" })
}

// crashLine is the record a recovered panic in a provider's Verify writes.
func crashLine(t *testing.T, logged string) loggedRecord {
	t.Helper()
	return findLogged(t, logged, "carries a recovered panic",
		func(r loggedRecord) bool { return r.PanicType != "" })
}

// The doubles are what they claim to be.
var (
	_ provider.WebhookSource = panicking{}
	_ provider.WebhookSource = panickingWithTheSecret{}
	_ provider.WebhookSource = panickingWithASecretInATypeName{}
	_ provider.WebhookSource = panickingWithALongTypeName{}
	_ provider.WebhookSource = deeplyPanicking{}
	_ provider.WebhookSource = (*blocking)(nil)
	_ provider.WebhookSource = mutating{}
	_ provider.WebhookSource = keyless{}
	_ provider.WebhookSource = keyed{}
	_ provider.URLSigner     = urlSigning{}
)
