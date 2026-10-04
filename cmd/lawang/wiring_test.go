package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gablooge/lawang/internal/ingress"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
)

// stubHub stands in for the hub where a test is about the wiring and not about resolving an owner.
// It is never reached by these tests: no provider is registered, so the edge answers 404 first.
type stubHub struct{}

func (stubHub) Accept(context.Context, provider.Entry, provider.Request) (ingress.Verdict, error) {
	return ingress.Parked, nil
}

func emptyRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	reg, err := provider.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// testHandler is the handler serve builds, with no provider registered and nothing to store.
func testHandler(t *testing.T) http.Handler {
	t.Helper()
	h, err := newHandler(emptyRegistry(t), stubHub{}, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	return h
}

// TestEveryRouteIsBehindTheIngressGuard. The reason /healthz is an ingress.Route and not a route on
// a mux of the command's own is that net/http.ServeMux answers a 307 with a Location before any
// handler runs, and a provider follows a 307 by re-POSTing to a path it did not sign. A route
// registered anywhere else would bring that back for the whole server, so the property is asserted
// on the handler the command actually serves.
func TestEveryRouteIsBehindTheIngressGuard(t *testing.T) {
	t.Parallel()
	handler := testHandler(t)

	for _, path := range []string{
		"//healthz",      // the mux would clean this one and redirect
		"/healthz/",      // and this one
		"/x/../healthz",  // and this one
		"//ingress/fake", // the webhook endpoint, the same three ways
		"/ingress//fake",
		"/ingress/fake/../fake",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil)
		handler.ServeHTTP(rec, req)
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("%s answered %d with Location %q: a provider would re-POST to a path it did not sign",
				path, rec.Code, loc)
		}
		if rec.Code >= 300 && rec.Code < 400 {
			t.Errorf("%s answered %d, a redirect", path, rec.Code)
		}
	}
}

// TestTheWebhookEndpointIsMounted. Before this item the command served a mux with only /healthz on
// it, and a POST to /ingress/{provider} was a 404 from that mux rather than from the edge. The two
// are indistinguishable from outside, so this asserts the one thing that differs: the edge logs a
// registered provider that is not a webhook source, and it answers a registered one.
func TestTheWebhookEndpointIsMounted(t *testing.T) {
	t.Parallel()
	reg, err := provider.NewRegistry(fake.New("stub"))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newHandler(reg, stubHub{}, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/ingress/stub", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Errorf("POST /ingress/stub = %d, want 200 (the stub hub parks everything): the edge is not mounted", rec.Code)
	}
	// An unregistered segment is a 404 with nothing read and nothing said about what is registered.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://example.com/ingress/nope", http.NoBody))
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /ingress/nope = %d, want 404", rec.Code)
	}
}
