package sink_test

import (
	"net/http"
	"testing"

	"github.com/gablooge/lawang/internal/sink"
)

// TestEachSinkHasItsOwnConnectionPool. A nil Transport is http.DefaultTransport, which is one
// pool shared by every client in the process and which keeps 2 idle connections per host. B10 is
// the first item that runs more than one sink at once, so two tenants' sinks would be sharing
// those two slots, and under churn most deliveries would be dialling again (issue #55 measured a
// sevenfold difference in failures under the same stress).
//
// There is nothing in one delivery to assert this on: a shared pool delivers correctly, only
// slower and less reliably under load. So it is asserted on the identity, which is the
// observable difference between a pool of one's own and the one everything shares.
func TestEachSinkHasItsOwnConnectionPool(t *testing.T) {
	t.Parallel()
	first := newHTTP(t, sink.HTTPConfig{Endpoint: "https://example.test/one"})
	second := newHTTP(t, sink.HTTPConfig{Endpoint: "https://example.test/two"})

	for name, s := range map[string]*sink.HTTP{"first": first, "second": second} {
		switch got := sink.TransportOf(s); got {
		case nil:
			t.Errorf("the %s sink has no transport of its own, so it shares http.DefaultTransport", name)
		case http.DefaultTransport:
			t.Errorf("the %s sink uses http.DefaultTransport, which every client in the process shares", name)
		}
	}
	if sink.TransportOf(first) == sink.TransportOf(second) {
		t.Error("two sinks share one connection pool, so one tenant's deliveries wait for another's")
	}
	// The one setting the clone changes, which is the reason for cloning at all.
	tr, ok := sink.TransportOf(first).(*http.Transport)
	if !ok {
		t.Fatalf("the transport is a %T, want a *http.Transport cloned from the default", sink.TransportOf(first))
	}
	if tr.MaxIdleConnsPerHost <= 2 {
		t.Errorf("MaxIdleConnsPerHost = %d, want more than the standard library's 2: a drain delivers from a pool of goroutines",
			tr.MaxIdleConnsPerHost)
	}
	// And the rest of the default is still there, so a deployment behind a proxy keeps working.
	if tr.Proxy == nil {
		t.Error("the cloned transport has no Proxy, so HTTPS_PROXY would be ignored")
	}
}
