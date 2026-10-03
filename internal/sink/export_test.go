package sink

import (
	"net/http"

	"github.com/gablooge/lawang/internal/outbox"
)

// TransportOf is the round tripper one HTTP sink uses, which is to say its connection pool. It
// is exported for the test that holds two sinks apart: the field is set in NewHTTP and read
// nowhere else, so nothing but an identity can show that each sink has its own.
func TransportOf(h *HTTP) http.RoundTripper { return h.client.Transport }

// What the tests in package sink_test reach into. Each is a name of this package, not a second
// implementation of anything.
var (
	FaultDetails    = faultDetails
	AppendLine      = appendLine
	ContentOf       = contentOf
	TransportDetail = transportDetail
	StubCode        = stubCode
)

// StatusVerdict is statusOf, flattened so a test can walk it: the action and the cause when the
// delivery is what failed, or refused with the record's own Cause when the receiver refused what
// was sent.
func StatusVerdict(status int, code string) (action Action, cause outbox.Cause, refused bool) {
	v := statusOf(status, code)
	if v.fault != nil {
		return v.fault.Action, v.fault.Cause, false
	}
	return ActionUnset, v.refused, true
}
