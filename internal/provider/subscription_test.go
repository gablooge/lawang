package provider_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/tenancy"
)

// TestEntriesAreOrderedByKey. A start-up refusal that names a provider (hub.New refuses a
// URL-signing provider when no public base URL is configured) must name the same one on every
// start, or an operator who fixes the first one meets the next one at the next restart, and the
// order a Go map gives is not an order.
func TestEntriesAreOrderedByKey(t *testing.T) {
	t.Parallel()
	reg, err := provider.NewRegistry(fake.New("slack"), fake.New("clickup"), fake.New("ms_graph"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"clickup", "ms_graph", "slack"}
	for range 20 { // a map's iteration order differs from run to run, so ask more than once
		var got []string
		for _, e := range reg.Entries() {
			got = append(got, e.Key())
		}
		if !slices.Equal(got, want) {
			t.Fatalf("Entries() = %v, want %v", got, want)
		}
	}
	// Each entry carries the provider itself, so the caller can ask a capability of it.
	for _, e := range reg.Entries() {
		if _, ok := e.WebhookSource(); !ok {
			t.Errorf("the entry for %q lost its provider", e.Key())
		}
	}
	empty, err := provider.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if got := empty.Entries(); len(got) != 0 {
		t.Errorf("an empty registry has %d entries", len(got))
	}
}

// TestASubscriptionNeverPrintsItsSecret. A Subscription carries the one credential the accept path
// needs, and it travels through the operator API, a Registrar and whatever logs either of those.
// The default printing of a struct would put the secret in all of them.
func TestASubscriptionNeverPrintsItsSecret(t *testing.T) {
	t.Parallel()
	const secret = "the-signing-secret"
	sub := provider.Subscription{
		ID:        "01SUB",
		Tenant:    tenancy.ID("tenant_a"),
		Provider:  "clickup",
		Resource:  "workspace-1",
		Workspace: "W1",
		External:  "S1",
		Secret:    []byte(secret),
	}

	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("registered", "subscription", sub)

	printed := map[string]string{
		"%v":         fmt.Sprintf("%v", sub),
		"%+v":        fmt.Sprintf("%+v", sub),
		"%s":         fmt.Sprintf("%s", sub), //nolint:staticcheck // S1025: what %s does with this type is the test
		"%#v":        fmt.Sprintf("%#v", sub),
		"a log line": logged.String(),
	}
	for how, text := range printed {
		if strings.Contains(text, secret) {
			t.Errorf("%s prints the secret: %s", how, text)
		}
		// The rest has to survive, or the redaction costs an operator the ability to tell one
		// subscription from another.
		if !strings.Contains(text, "01SUB") || !strings.Contains(text, "tenant_a") {
			t.Errorf("%s says too little to identify the subscription: %s", how, text)
		}
	}
	if !strings.Contains(printed["%v"], "18 bytes") {
		t.Errorf("%%v does not say a secret is there at all: %s", printed["%v"])
	}
}
