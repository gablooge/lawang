package sink_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/sink"
	"github.com/gablooge/lawang/internal/tenancy"
)

func newHTTP(t testing.TB, cfg sink.HTTPConfig) *sink.HTTP {
	t.Helper()
	if cfg.Token == nil {
		cfg.Token = tokenFor("t0ken")
	}
	s, err := sink.NewHTTP(cfg)
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	return s
}

// serving starts a test server and returns a sink pointed at it.
func serving(t testing.TB, h http.HandlerFunc, cfg sink.HTTPConfig) *sink.HTTP {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cfg.Endpoint = srv.URL + "/deliver"
	return newHTTP(t, cfg)
}

// TestTheStatusTable is the second acceptance line of B09, as the table the code is: every
// status, and what the sink does about it. A status is either a fault about the delivery, with
// an action for the worker, or the receiver refusing the records of the request it answered.
func TestTheStatusTable(t *testing.T) {
	t.Parallel()
	type want struct {
		action  sink.Action
		refused bool
		class   string
	}
	cases := map[int]want{
		401: {action: sink.ActionHalt, class: "sink refused the credential (status 401)"},
		403: {action: sink.ActionHalt, class: "sink refused the credential (status 403)"},
		// Every 4xx that the refusal band does not name is a verdict on the request and
		// not on anything in it, so it halts and kills nothing. This is the default and not
		// a list, which is what makes a status nobody has thought of safe: an operator
		// changes a number, a credential, an endpoint or a receiver, and until then the
		// ladder would only send the same request again.
		400: {action: sink.ActionHalt, class: "sink refused the request (status 400)"},
		402: {action: sink.ActionHalt, class: "sink refused the request (status 402)"},
		404: {action: sink.ActionHalt, class: "sink refused the request (status 404)"},
		405: {action: sink.ActionHalt, class: "sink refused the request (status 405)"},
		406: {action: sink.ActionHalt, class: "sink refused the request (status 406)"},
		409: {action: sink.ActionHalt, class: "sink refused the request (status 409)"},
		410: {action: sink.ActionHalt, class: "sink refused the request (status 410)"},
		411: {action: sink.ActionHalt, class: "sink refused the request (status 411)"},
		413: {action: sink.ActionHalt, class: "sink refused the request (status 413)"},
		414: {action: sink.ActionHalt, class: "sink refused the request (status 414)"},
		415: {action: sink.ActionHalt, class: "sink refused the request (status 415)"},
		421: {action: sink.ActionHalt, class: "sink refused the request (status 421)"},
		426: {action: sink.ActionHalt, class: "sink refused the request (status 426)"},
		431: {action: sink.ActionHalt, class: "sink refused the request (status 431)"},
		451: {action: sink.ActionHalt, class: "sink refused the request (status 451)"},
		499: {action: sink.ActionHalt, class: "sink refused the request (status 499)"},
		// The whole refusal band: the one status HTTP defines as a verdict on the content
		// of the request rather than on the request message.
		422: {refused: true, class: "sink rejected the record (status 422)"},
		408: {action: sink.ActionRetry, class: "sink unavailable (status 408)"},
		429: {action: sink.ActionRetry, class: "sink unavailable (status 429)"},
		500: {action: sink.ActionRetry, class: "sink unavailable (status 500)"},
		502: {action: sink.ActionRetry, class: "sink unavailable (status 502)"},
		503: {action: sink.ActionRetry, class: "sink unavailable (status 503)"},
		504: {action: sink.ActionRetry, class: "sink unavailable (status 504)"},
		599: {action: sink.ActionRetry, class: "sink unavailable (status 599)"},
		// Not a status the table names: retried, which is the answer that loses nothing.
		301: {action: sink.ActionRetry, class: "sink unavailable (status 301)"},
		302: {action: sink.ActionRetry, class: "sink unavailable (status 302)"},
		100: {action: sink.ActionRetry, class: "sink unavailable (status 100)"},
	}
	for status, w := range cases {
		action, cause, refused := sink.StatusVerdict(status, "")
		if refused != w.refused {
			t.Errorf("status %d: refused %v, want %v", status, refused, w.refused)
		}
		if action != w.action {
			t.Errorf("status %d: action %v, want %v", status, action, w.action)
		}
		if got := cause.String(); got != w.class {
			t.Errorf("status %d: cause %q, want %q", status, got, w.class)
		}
	}
	// The refusal band is exactly 422, and nothing else may kill a record. This is the
	// assertion that fails if the default for an unrecognised 4xx is ever inverted back:
	// before ADR 13's second revision, 404, 405, 406, 410, 411, 421 and 431 all dead-lettered
	// every record of every chunk of every batch on that endpoint, permanently and quietly,
	// so an endpoint typo destroyed a tenant's records and nothing halted to say so.
	for status := 400; status <= 499; status++ {
		_, _, refused := sink.StatusVerdict(status, "")
		if want := status == http.StatusUnprocessableEntity; refused != want {
			t.Errorf("status %d: refused %v, want %v: the refusal band is 422 alone", status, refused, want)
		}
	}
	// Every status from 100 to 599 is either a refusal of the records or a fault with one of
	// the two actions, and never the zero value of Action.
	for status := 100; status <= 599; status++ {
		action, _, refused := sink.StatusVerdict(status, "")
		if refused {
			if action != sink.ActionUnset {
				t.Fatalf("status %d: a refusal carries action %v", status, action)
			}
			continue
		}
		switch action {
		case sink.ActionRetry, sink.ActionHalt:
		default:
			t.Fatalf("status %d: action %v", status, action)
		}
	}
}

// TestALiveAnswerIsClassifiedTheSameWay runs the rows of the acceptance line against a real
// server, so the table above is wired to what Deliver does and not only to a function. The 422
// is the row that is a refusal of the records rather than a fault, and it arrives as a Rejection
// carrying the same status and code.
func TestALiveAnswerIsClassifiedTheSameWay(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]sink.Action{
		401: sink.ActionHalt,
		403: sink.ActionHalt,
		404: sink.ActionHalt,
		413: sink.ActionHalt,
		426: sink.ActionHalt,
		431: sink.ActionHalt,
		500: sink.ActionRetry,
		503: sink.ActionRetry,
		422: sink.ActionUnset,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			r := rec(t, tenantA)
			s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"code":"nope_"}`)
			}, sink.HTTPConfig{})
			result, err := s.Deliver(context.Background(), tenantA, []record.Record{r})
			cause := ""
			if want == sink.ActionUnset {
				// The row that is a refusal of the records rather than a fault.
				if err != nil {
					t.Fatalf("Deliver: %v, want the refusal reported record by record", err)
				}
				if len(result.Rejected) != 1 || result.Rejected[0].ID != r.ID {
					t.Fatalf("rejected %+v, want the one record sent", result.Rejected)
				}
				cause = result.Rejected[0].Cause.String()
			} else {
				f := fault(t, err)
				if f.Action != want {
					t.Errorf("action %v, want %v", f.Action, want)
				}
				cause = f.Cause.String()
			}
			if !strings.Contains(cause, fmt.Sprintf("status %d", status)) {
				t.Errorf("cause %q does not carry the status", cause)
			}
			if !strings.Contains(cause, "code nope_") {
				t.Errorf("cause %q does not carry the sink's error code", cause)
			}
		})
	}
}

// TestATimeoutIsRetried: the second half of "5xx and timeouts as retryable".
func TestATimeoutIsRetried(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	// defer, not t.Cleanup: the server's own cleanup is registered later and runs first, and
	// Close waits for the handler, so releasing it has to happen before that.
	defer close(release)
	s := serving(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}, sink.HTTPConfig{Timeout: 50 * time.Millisecond})

	_, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)})
	f := fault(t, err)
	if f.Action != sink.ActionRetry {
		t.Errorf("action %v, want retry", f.Action)
	}
	if got, want := f.Cause.String(), "sink unavailable"; got != want {
		t.Errorf("cause %q, want %q", got, want)
	}
	if f.Detail != "the request timed out" {
		t.Errorf("detail %q", f.Detail)
	}
}

// TestADeadlineOnTheCallersContextIsRetried: the worker's own deadline, rather than the client's.
func TestADeadlineOnTheCallersContextIsRetried(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	// defer, not t.Cleanup: the server's own cleanup is registered later and runs first, and
	// Close waits for the handler, so releasing it has to happen before that.
	defer close(release)
	s := serving(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}, sink.HTTPConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.Deliver(ctx, tenantA, []record.Record{rec(t, tenantA)})
	f := fault(t, err)
	if f.Action != sink.ActionRetry || f.Detail != "the request timed out" {
		t.Errorf("fault %v", f)
	}
}

// TestAConnectionThatCannotBeMadeIsRetried: nothing is listening, so there is no status at all.
func TestAConnectionThatCannotBeMadeIsRetried(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s := newHTTP(t, sink.HTTPConfig{Endpoint: "http://" + addr + "/deliver"})
	_, err = s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)})
	f := fault(t, err)
	if f.Action != sink.ActionRetry {
		t.Errorf("action %v, want retry", f.Action)
	}
	if f.Detail != "the connection was refused" && f.Detail != "the connection failed" {
		t.Errorf("detail %q", f.Detail)
	}
}

// TestAHostNameThatDoesNotResolveIsRetried.
func TestAHostNameThatDoesNotResolveIsRetried(t *testing.T) {
	t.Parallel()
	// .invalid is reserved and never resolves (RFC 2606).
	s := newHTTP(t, sink.HTTPConfig{Endpoint: "https://sink.invalid/deliver", Timeout: 5 * time.Second})
	_, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)})
	f := fault(t, err)
	if f.Action != sink.ActionRetry {
		t.Errorf("action %v, want retry", f.Action)
	}
}

// TestPerRecordRejectionsDeadLetterAndTheRestLands is the "sink rejects one record" row of
// architecture section 11, over the protocol the HTTP sink speaks.
func TestPerRecordRejectionsDeadLetterAndTheRestLands(t *testing.T) {
	t.Parallel()
	first := rec(t, tenantA, func(r *record.Record) { r.Version = "1" })
	second := rec(t, tenantA, func(r *record.Record) { r.Version = "2" })
	var got struct {
		Records []json.RawMessage `json:"records"`
	}
	s := serving(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("unmarshal: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"rejected":[{"id":%q,"code":"unsupported_kind"}]}`, second.ID)
	}, sink.HTTPConfig{})

	result, err := s.Deliver(context.Background(), tenantA, []record.Record{first, second})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(got.Records) != 2 {
		t.Fatalf("the request carried %d records, want 2", len(got.Records))
	}
	if len(result.Rejected) != 1 || result.Rejected[0].ID != second.ID {
		t.Fatalf("rejected %+v, want the second record alone", result.Rejected)
	}
	if want := "sink rejected the record (status 200, code unsupported_kind)"; result.Rejected[0].Cause.String() != want {
		t.Errorf("cause %q, want %q", result.Rejected[0].Cause.String(), want)
	}
}

// TestAnAnswerThatDoesNotSayWhatLandedIsRetried: nothing is marked delivered, because guessing
// is how a record is lost, and a repeat costs a sink that is idempotent on the id nothing.
func TestAnAnswerThatDoesNotSayWhatLandedIsRetried(t *testing.T) {
	t.Parallel()
	known := rec(t, tenantA)
	for name, body := range map[string]string{
		"not JSON":                `<html>ok</html>`,
		"an id that was not sent": `{"rejected":[{"id":"rec_00000000000000000000000000000001"}]}`,
		"the same id twice":       fmt.Sprintf(`{"rejected":[{"id":%q},{"id":%q}]}`, known.ID, known.ID),
		"an empty id":             `{"rejected":[{"id":""}]}`,
		// A non-empty body that carries no member the sink knows. This is the hole the
		// maintainer closed on issue #9: a receiver that means to refuse one record and
		// misspells the member, or writes it for a later version of the protocol, had
		// every record of the batch marked delivered and the refused one lost with no
		// dead letter and nothing in last_error.
		"the member misspelled":        fmt.Sprintf(`{"refused":[{"id":%q}]}`, known.ID),
		"a member from another layer":  `{"accepted":1}`,
		"the member in the wrong case": fmt.Sprintf(`{"Rejected":[{"id":%q}]}`, known.ID),
		"an object inside an object":   `{"result":{"rejected":[]}}`,
		// JSON that is not an object at all cannot carry the member.
		"a list":   `[]`,
		"a string": `"ok"`,
		"a number": `12`,
		"null":     `null`,
		// The member has to be a list. A receiver that writes something else there has
		// said something this sink cannot read, and the sink does not guess at it. null
		// is the one that matters: encoding/json reads the literal null into a slice as an
		// empty one with no error, so before this it meant "every record landed", which is
		// the one direction the strict answer exists to refuse. A serializer that writes an
		// absent list as null is ordinary.
		"the member is null":     `{"rejected":null}`,
		"the member is a string": `{"rejected":"all of them"}`,
		"the member is an object": fmt.Sprintf(
			`{"rejected":{"id":%q}}`, known.ID),
		"the member is a number":              `{"rejected":5}`,
		"the member is a list of strings":     fmt.Sprintf(`{"rejected":[%q]}`, known.ID),
		"the member is a list holding a null": `{"rejected":[null]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			}, sink.HTTPConfig{})
			_, err := s.Deliver(context.Background(), tenantA, []record.Record{known})
			f := fault(t, err)
			if f.Action != sink.ActionRetry {
				t.Errorf("action %v, want retry", f.Action)
			}
			if want := "the sink's answer could not be read (status 200)"; f.Cause.String() != want {
				t.Errorf("cause %q, want %q", f.Cause.String(), want)
			}
		})
	}
}

// TestAnAnswerOverTheReadLimitIsRetried: the response comes from outside the deployment, so the
// sink stops reading at its own limit, and an answer that did not fit is one it cannot act on.
//
// The server here offers 16 MiB and counts what it got out. The socket buffers are far smaller
// than that, so a client that stops reading leaves the server unable to finish: wroteAll is
// what says the sink read no further than it meant to.
func TestAnAnswerOverTheReadLimitIsRetried(t *testing.T) {
	t.Parallel()
	const offered = 16 << 20
	chunk := bytes.Repeat([]byte("a"), 64<<10)
	wroteAll := make(chan bool, 1)
	s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"rejected":[`)
		for written := 0; written < offered; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				wroteAll <- false
				return
			}
		}
		wroteAll <- true
	}, sink.HTTPConfig{})

	_, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)})
	f := fault(t, err)
	if f.Action != sink.ActionRetry || f.Detail != "the sink's answer could not be read" {
		t.Errorf("fault %v", f)
	}
	select {
	case all := <-wroteAll:
		if all {
			t.Errorf("the sink read all %d bytes the server offered", offered)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the server never finished writing")
	}
}

// TestAnAnswerPaddedPastTheReadLimitIsActedOnByNobody is the other half of the read limit, and
// the half the test above cannot see. The sink reads maxResponseBytes+1 bytes so that it can
// tell a complete answer from one it cut short. Here the first 64 KiB are a complete, correct
// answer naming a refused record and the padding that follows takes the body past the limit,
// which some receivers really do send. So nothing except the size check can notice that the
// answer was truncated, and acting on the fragment would mark the rest of the batch delivered
// on the strength of an answer the sink only saw part of.
func TestAnAnswerPaddedPastTheReadLimitIsActedOnByNobody(t *testing.T) {
	t.Parallel()
	first := rec(t, tenantA, func(r *record.Record) { r.Version = "1" })
	second := rec(t, tenantA, func(r *record.Record) { r.Version = "2" })
	s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"rejected":[{"id":%q,"code":"unsupported_kind"}]}`, second.ID)
		// Whitespace, so that what the sink read is valid JSON on its own.
		_, _ = w.Write(bytes.Repeat([]byte(" "), 100<<10))
	}, sink.HTTPConfig{})

	result, err := s.Deliver(context.Background(), tenantA, []record.Record{first, second})
	f := fault(t, err)
	if f.Action != sink.ActionRetry {
		t.Errorf("action %v, want retry", f.Action)
	}
	if want := "the sink's answer could not be read (status 200)"; f.Cause.String() != want {
		t.Errorf("cause %q, want %q", f.Cause.String(), want)
	}
	if len(result.Rejected) != 0 {
		t.Errorf("the result carries %+v, want nothing acted on", result.Rejected)
	}
}

// TestAnAnswerThatStopsPartWayThroughIsRetried: the receiver promised more bytes than it sent
// and then went away, so what it did with the batch is unknown either way.
func TestAnAnswerThatStopsPartWayThroughIsRetried(t *testing.T) {
	t.Parallel()
	for name, status := range map[string]string{
		"a 200": "200 OK",
		"a 500": "500 Internal Server Error",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				conn, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				_, _ = io.WriteString(conn, "HTTP/1.1 "+status+"\r\nContent-Length: 4096\r\n\r\n{\"co")
				_ = conn.Close()
			}, sink.HTTPConfig{})
			_, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)})
			f := fault(t, err)
			if f.Action != sink.ActionRetry {
				t.Errorf("action %v, want retry", f.Action)
			}
		})
	}
}

// TestAnEmptyAnswerMeansEverythingLanded.
func TestAnEmptyAnswerMeansEverythingLanded(t *testing.T) {
	t.Parallel()
	for name, h := range map[string]http.HandlerFunc{
		"204 and no body": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
		"200 and no body": func(http.ResponseWriter, *http.Request) {},
		"200 and spaces":  func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "  \n") },
		"200 and an empty object": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{}`)
		},
		"200 and an empty list": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"rejected":[]}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := serving(t, h, sink.HTTPConfig{})
			result, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)})
			if err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			if len(result.Rejected) != 0 {
				t.Fatalf("rejected %+v, want none", result.Rejected)
			}
		})
	}
}

// TestTheRequestCarriesTheTenantsCredentialAndNoTenantField: the tenant is in no field of the
// envelope, and a sink delivery's trust root is the per-tenant credential (architecture 4).
func TestTheRequestCarriesTheTenantsCredentialAndNoTenantField(t *testing.T) {
	t.Parallel()
	var auth, contentType, accept, body string
	s := serving(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		accept = r.Header.Get("Accept")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusNoContent)
	}, sink.HTTPConfig{
		Token: func(_ context.Context, tn tenancy.ID) (string, error) { return "token-for-" + tn.String(), nil },
	})
	if _, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if want := "Bearer token-for-acme"; auth != want {
		t.Errorf("Authorization %q, want %q", auth, want)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type %q", contentType)
	}
	// The only answer this sink can read is the JSON one, and the protocol says so.
	if accept != "application/json" {
		t.Errorf("Accept %q", accept)
	}
	if !strings.HasPrefix(body, `{"v":1,"records":[`) || !strings.HasSuffix(body, `]}`) {
		t.Errorf("body %q is not the batch shape", body)
	}
	if strings.Contains(body, tenantA.String()) {
		t.Errorf("the body carries the tenant id")
	}
	// The version is read as a number and not as a prefix, because what a receiver decodes is
	// the member and not the spelling.
	var envelope struct {
		V       *int              `json:"v"`
		Records []json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("the posted body does not decode: %v", err)
	}
	if envelope.V == nil || *envelope.V != sink.ProtocolVersion {
		t.Errorf("v %v, want %d", envelope.V, sink.ProtocolVersion)
	}
	if len(envelope.Records) != 1 {
		t.Errorf("the request carried %d records, want 1", len(envelope.Records))
	}
}

// TestAVerdictOnTheRequestHaltsAndKillsNoRecord is should-fix 5 of the round 2 review. These
// statuses say something about the request this sink built, its size, its URL, its media type
// or its protocol version, and nothing about any record in it. Treating them as the receiver
// refusing the records dead-lettered every record of every chunk of every batch on that
// endpoint, permanently and quietly, with last_error reading "sink rejected the record
// (status 413)". The configuration mismatch behind it (a MaxRequestBytes larger than the
// receiver's body cap, or a receiver that speaks a later version of the protocol) is for an
// operator to fix, so it halts and nothing is marked delivered.
func TestAVerdictOnTheRequestHaltsAndKillsNoRecord(t *testing.T) {
	t.Parallel()
	for _, status := range []int{400, 413, 414, 415, 426} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			recs := []record.Record{
				rec(t, tenantA, func(r *record.Record) { r.Version = "1" }),
				rec(t, tenantA, func(r *record.Record) { r.Version = "2" }),
			}
			s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}, sink.HTTPConfig{})
			result, err := s.Deliver(context.Background(), tenantA, recs)
			f := fault(t, err)
			if f.Action != sink.ActionHalt {
				t.Errorf("action %v, want halt: a record the receiver said nothing about must not be killed", f.Action)
			}
			if want := fmt.Sprintf("sink refused the request (status %d)", status); f.Cause.String() != want {
				t.Errorf("cause %q, want %q", f.Cause.String(), want)
			}
			if len(result.Rejected) != 0 {
				t.Errorf("rejected %+v, want none", result.Rejected)
			}
		})
	}
}

// TestAnUnrecognisedStatusHaltsAndKillsNoRecord is the pin on the default the maintainer
// inverted, and it is a live test because the band is only worth anything through Deliver.
//
// Each of these dead-lettered both records of the batch before, permanently and quietly, with
// last_error reading "sink rejected the record (status NNN)" and nothing halting to point an
// operator anywhere. 404 is an endpoint typo, 405 a receiver that takes only GET on that path,
// 410 and 421 a receiver that moved, 406 the Accept header, and 411 and 431 the request's own
// headers. 431 in particular is provoked by a tenant whose bearer token is a large JWT in front
// of a proxy with a small header buffer, which made the loss per-tenant and decided by the shape
// of one tenant's credential. 402 and 451 are verdicts on the account and on the resource, 409
// is a conflict with the state of the endpoint that names no record, and 499 is a status no
// standard defines, which is the case the default exists for.
func TestAnUnrecognisedStatusHaltsAndKillsNoRecord(t *testing.T) {
	t.Parallel()
	for _, status := range []int{402, 404, 405, 406, 409, 410, 411, 421, 431, 451, 499} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			recs := []record.Record{
				rec(t, tenantA, func(r *record.Record) { r.Version = "1" }),
				rec(t, tenantA, func(r *record.Record) { r.Version = "2" }),
			}
			s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}, sink.HTTPConfig{})
			result, err := s.Deliver(context.Background(), tenantA, recs)
			f := fault(t, err)
			if f.Action != sink.ActionHalt {
				t.Errorf("action %v, want halt: nothing in this answer is about a record, so no record may be killed", f.Action)
			}
			if want := fmt.Sprintf("sink refused the request (status %d)", status); f.Cause.String() != want {
				t.Errorf("cause %q, want %q", f.Cause.String(), want)
			}
			if len(result.Rejected) != 0 {
				t.Errorf("rejected %+v, want none", result.Rejected)
			}
		})
	}
}

// TestTheRefusalBandIsOneStatus, live: 422 is the one status outside 2xx that refuses the records
// of the request it answered, and a receiver that uses it still has every record of that request
// dead-lettered, because which records share a request is the sender's arithmetic and not the
// receiver's choice. That is why the protocol asks a receiver to answer 2xx with a "rejected"
// list instead.
func TestTheRefusalBandIsOneStatus(t *testing.T) {
	t.Parallel()
	recs := []record.Record{
		rec(t, tenantA, func(r *record.Record) { r.Version = "1" }),
		rec(t, tenantA, func(r *record.Record) { r.Version = "2" }),
	}
	s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"code":"bad_kind"}`)
	}, sink.HTTPConfig{})
	result, err := s.Deliver(context.Background(), tenantA, recs)
	if err != nil {
		t.Fatalf("Deliver: %v, want the refusal reported record by record", err)
	}
	if len(result.Rejected) != 2 {
		t.Fatalf("rejected %+v, want both records of the request", result.Rejected)
	}
	for _, r := range result.Rejected {
		if want := "sink rejected the record (status 422, code bad_kind)"; r.Cause.String() != want {
			t.Errorf("cause %q, want %q", r.Cause.String(), want)
		}
	}
}

// TestAReceiverThatSpeaksALaterVersionLosesNoRecord: the one interaction between the version in
// the request and the band above. A receiver that does not speak v answers 426 (or 400), and
// under the old band that permanently dead-lettered every record of every chunk for a mismatch
// no record caused. The batch is split here, so the second request is the one refused.
func TestAReceiverThatSpeaksALaterVersionLosesNoRecord(t *testing.T) {
	t.Parallel()
	recs := make([]record.Record, 4)
	for i := range recs {
		recs[i] = rec(t, tenantA, func(r *record.Record) { r.Version = fmt.Sprint(i) })
	}
	limit := 2*len(doc(t, recs[0])) + 16 + 32

	var (
		mu       sync.Mutex
		requests int
		seen     []int
	)
	s := serving(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read: %v", err)
			return
		}
		var envelope struct {
			V int `json:"v"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Errorf("the posted body does not decode: %v", err)
			return
		}
		mu.Lock()
		requests++
		n := requests
		seen = append(seen, envelope.V)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// A receiver that only speaks v2 refuses v1 by the status the protocol names.
		w.WriteHeader(http.StatusUpgradeRequired)
	}, sink.HTTPConfig{MaxRequestBytes: limit})

	result, err := s.Deliver(context.Background(), tenantA, recs)
	f := fault(t, err)
	if f.Action != sink.ActionHalt {
		t.Errorf("action %v, want halt", f.Action)
	}
	if len(result.Rejected) != 0 {
		t.Errorf("rejected %+v, want none: a version the receiver will not speak is no record's fault", result.Rejected)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, v := range seen {
		if v != sink.ProtocolVersion {
			t.Errorf("a request carried v %d, want %d", v, sink.ProtocolVersion)
		}
	}
	if len(seen) != 2 {
		t.Errorf("%d requests, want the sink to stop at the one that was refused", len(seen))
	}
}

// TestNoCredentialIsARefusalAndNotADefault: fail closed.
func TestNoCredentialIsARefusalAndNotADefault(t *testing.T) {
	t.Parallel()
	reached := false
	for name, token := range map[string]func(context.Context, tenancy.ID) (string, error){
		"an error":       func(context.Context, tenancy.ID) (string, error) { return "", errors.New("vault is down") },
		"an empty token": func(context.Context, tenancy.ID) (string, error) { return "", nil },
	} {
		t.Run(name, func(t *testing.T) {
			s := serving(t, func(http.ResponseWriter, *http.Request) { reached = true },
				sink.HTTPConfig{Token: token})
			_, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)})
			f := fault(t, err)
			if f.Action != sink.ActionRetry {
				t.Errorf("action %v, want retry", f.Action)
			}
			if want := "vault unavailable"; f.Cause.String() != want {
				t.Errorf("cause %q, want %q", f.Cause.String(), want)
			}
			if reached {
				t.Errorf("the request was sent without a credential")
			}
		})
	}
}

// TestARedirectIsNotFollowed: a receiver that answered one would choose where the next request,
// with its Authorization header, goes.
func TestARedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Errorf("the redirect was followed")
	}))
	t.Cleanup(elsewhere.Close)
	s := serving(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/deliver", http.StatusTemporaryRedirect)
	}, sink.HTTPConfig{})
	_, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)})
	f := fault(t, err)
	if f.Action != sink.ActionRetry {
		t.Errorf("action %v, want retry", f.Action)
	}
	if want := "sink unavailable (status 307)"; f.Cause.String() != want {
		t.Errorf("cause %q, want %q", f.Cause.String(), want)
	}
}

// TestABatchIsSplitToStayUnderTheRequestSize.
func TestABatchIsSplitToStayUnderTheRequestSize(t *testing.T) {
	t.Parallel()
	recs := make([]record.Record, 6)
	for i := range recs {
		recs[i] = rec(t, tenantA, func(r *record.Record) { r.Version = fmt.Sprint(i) })
	}
	one := len(doc(t, recs[0]))
	// Room for two documents per request, and not three.
	limit := 2*one + 16 + 32

	var sizes []int
	s := serving(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sizes = append(sizes, len(b))
		w.WriteHeader(http.StatusNoContent)
	}, sink.HTTPConfig{MaxRequestBytes: limit})

	if _, err := s.Deliver(context.Background(), tenantA, recs); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(sizes) != 3 {
		t.Fatalf("%d requests of sizes %v, want 3", len(sizes), sizes)
	}
	for _, size := range sizes {
		if size > limit {
			t.Errorf("a request of %d bytes is over the %d byte limit", size, limit)
		}
	}
}

// TestARecordLargerThanOneRequestIsRejectedBeforeAnythingIsSent.
func TestARecordLargerThanOneRequestIsRejectedBeforeAnythingIsSent(t *testing.T) {
	t.Parallel()
	small := rec(t, tenantA, func(r *record.Record) { r.Version = "1" })
	big := rec(t, tenantA, func(r *record.Record) {
		r.Version = "2"
		r.Text = strings.Repeat("x", 4096)
	})
	var sent int
	s := serving(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent += len(b)
		w.WriteHeader(http.StatusNoContent)
	}, sink.HTTPConfig{MaxRequestBytes: len(doc(t, small)) + 64})

	result, err := s.Deliver(context.Background(), tenantA, []record.Record{big, small})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(result.Rejected) != 1 || result.Rejected[0].ID != big.ID {
		t.Fatalf("rejected %+v, want the big record alone", result.Rejected)
	}
	// The code is the actionable half, because a Detail is not stored in the outbox.
	if want := "internal error (code record_too_large)"; result.Rejected[0].Cause.String() != want {
		t.Errorf("cause %q, want %q", result.Rejected[0].Cause.String(), want)
	}
	if result.Rejected[0].Detail != "the record is larger than one request of this sink" {
		t.Errorf("detail %q", result.Rejected[0].Detail)
	}
	if sent == 0 {
		t.Errorf("the small record was not sent")
	}
}

// TestTheRecordThatDoesNotFitIsMeasuredWithTheFraming: a document never travels on its own, it
// goes inside {"v":1,"records":[ ... ]}, so what has to fit MaxRequestBytes is the document plus
// the 20 bytes of framing. The band between the two is narrow and the whole of the difference,
// so the test sits on both sides of it: one byte more room than the document needs and the
// record goes, one byte less and it is a rejection with nothing sent. The literal is written out
// here rather than taken from the package, so that a change to the envelope has to be made twice
// and is seen once.
func TestTheRecordThatDoesNotFitIsMeasuredWithTheFraming(t *testing.T) {
	t.Parallel()
	r := rec(t, tenantA)
	const framing = len(`{"v":1,"records":[`) + len(`]}`)
	for name, c := range map[string]struct {
		limit    int
		rejected bool
	}{
		"room for the document and its framing": {limit: len(doc(t, r)) + framing},
		"one byte less":                         {limit: len(doc(t, r)) + framing - 1, rejected: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			sent := 0
			s := serving(t, func(w http.ResponseWriter, req *http.Request) {
				body, _ := io.ReadAll(req.Body)
				mu.Lock()
				sent += len(body)
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
			}, sink.HTTPConfig{MaxRequestBytes: c.limit})

			result, err := s.Deliver(context.Background(), tenantA, []record.Record{r})
			if err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case c.rejected && (len(result.Rejected) != 1 || sent != 0):
				t.Errorf("rejected %+v and %d bytes sent, want the record refused and nothing sent", result.Rejected, sent)
			case !c.rejected && (len(result.Rejected) != 0 || sent == 0):
				t.Errorf("rejected %+v and %d bytes sent, want the record sent", result.Rejected, sent)
			}
		})
	}
}

// TestABatchWhoseRecordsAreAllRejectedSendsNothing.
func TestABatchWhoseRecordsAreAllRejectedSendsNothing(t *testing.T) {
	t.Parallel()
	reached := false
	s := serving(t, func(http.ResponseWriter, *http.Request) { reached = true }, sink.HTTPConfig{})
	result, err := s.Deliver(context.Background(), tenantA, []record.Record{{}, {}})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(result.Rejected) != 2 {
		t.Errorf("rejected %+v, want both", result.Rejected)
	}
	if reached {
		t.Errorf("a request was sent with no records in it")
	}
}

// TestAFaultOnTheFirstRequestOfASplitBatchStopsTheRest: on a Fault nothing counts as delivered,
// so there is no reason to go on sending, and the worker re-sends the whole batch.
//
// The batch opens with the zero Record, which is sealed for no tenant, so marshalAll has put a
// Rejection in the result before the first request is built. That is what makes the zero-value
// assertion below able to fail: without it the result is empty whatever Deliver does with it,
// and the check is satisfied by the handler rather than by the code. Which of marshalAll's
// rejections it is does not matter here, only that there is one.
func TestAFaultOnTheFirstRequestOfASplitBatchStopsTheRest(t *testing.T) {
	t.Parallel()
	recs := make([]record.Record, 4)
	for i := range recs {
		recs[i] = rec(t, tenantA, func(r *record.Record) { r.Version = fmt.Sprint(i) })
	}
	batch := append([]record.Record{{}}, recs...)
	requests := 0
	s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusServiceUnavailable)
	}, sink.HTTPConfig{MaxRequestBytes: len(doc(t, recs[0])) + 64})

	result, err := s.Deliver(context.Background(), tenantA, batch)
	f := fault(t, err)
	if f.Action != sink.ActionRetry {
		t.Errorf("action %v, want retry", f.Action)
	}
	if requests != 1 {
		t.Errorf("%d requests, want the first one only", requests)
	}
	if len(result.Rejected) != 0 {
		t.Errorf("the result carries %+v, want the zero value: the rejection the format made before the fault goes with it", result.Rejected)
	}
}

// TestAnErrorBodyThatIsNotJSONCarriesNoCode.
func TestAnErrorBodyThatIsNotJSONCarriesNoCode(t *testing.T) {
	t.Parallel()
	s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	}, sink.HTTPConfig{})
	_, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)})
	f := fault(t, err)
	if want := "sink unavailable (status 502)"; f.Cause.String() != want {
		t.Errorf("cause %q, want %q", f.Cause.String(), want)
	}
}

// TestAnEmptyBatchSendsNothing, and asks nothing of the vault either. The early return is
// before the credential lookup, which is the one observable difference between keeping it and
// deleting it: without it a batch with no records in it fails for a tenant whose vault is down,
// and the worker puts a row with nothing to deliver back on the ladder. Mutation C8 of the
// round 2 review deleted the early return and the package stayed green, which is what the
// second case below is here for.
func TestAnEmptyBatchSendsNothing(t *testing.T) {
	t.Parallel()
	for name, token := range map[string]func(context.Context, tenancy.ID) (string, error){
		"a vault that answers":  tokenFor("token"),
		"a vault that is down":  func(context.Context, tenancy.ID) (string, error) { return "", errors.New("vault is down") },
		"a vault with no token": func(context.Context, tenancy.ID) (string, error) { return "", nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, batch := range [][]record.Record{nil, {}} {
				reached := false
				asked := false
				s := serving(t, func(http.ResponseWriter, *http.Request) { reached = true },
					sink.HTTPConfig{Token: func(ctx context.Context, tn tenancy.ID) (string, error) {
						asked = true
						return token(ctx, tn)
					}})
				result, err := s.Deliver(context.Background(), tenantA, batch)
				if err != nil || len(result.Rejected) != 0 || reached {
					t.Fatalf("Deliver of an empty batch: %v, %+v, request sent %v", err, result, reached)
				}
				if asked {
					t.Errorf("the vault was asked for a credential for a batch with nothing in it")
				}
			}
		})
	}
}

// TestTheHTTPSinkRefusesATenantItCannotParse.
func TestTheHTTPSinkRefusesATenantItCannotParse(t *testing.T) {
	t.Parallel()
	reached := false
	s := serving(t, func(http.ResponseWriter, *http.Request) { reached = true }, sink.HTTPConfig{})
	for _, bad := range []tenancy.ID{"", "../etc", "a b"} {
		if _, err := s.Deliver(context.Background(), bad, []record.Record{rec(t, tenantA)}); !errors.Is(err, tenancy.ErrInvalidID) {
			t.Errorf("Deliver(%q): %v, want ErrInvalidID", bad, err)
		}
	}
	if reached {
		t.Errorf("a request was sent for a tenant that does not parse")
	}
}

// The two secrets a failed delivery must not spill: one in the endpoint's query string, where
// several receivers take their API key, and one in the bearer token.
const (
	marker      = "sUp3rS3cr3tMark3r"
	tokenMarker = "t0k3nMark3rV4lue"
)

// TestASinkURLSecretNeverLeaves is the acceptance added to issue #9 from the round 3 review of
// #32: a sink URL carrying a marker secret appears in nothing a delivery that went wrong
// produces. What is searched is every string this package hands back (the error, the Cause that
// goes into the outbox column, every Rejection with its own Cause and Detail) and everything
// written to the default logger while the delivery runs. The bearer token carries a marker of
// its own, because it travels with the same request.
//
// Half of the attempts end in a Fault and half end in a Rejection, because those are two
// different sets of strings and only the second reaches DeliveryResult. An attempt says which
// it expects, so an attempt that stops producing what it is here to search fails rather than
// quietly searching nothing. That is what round 1 found: when every attempt required an error,
// and a Fault comes with the zero DeliveryResult, the loop over the rejections never ran.
//
// The test does not run in parallel: it replaces the default logger for the length of each
// delivery, and anything else running at the same time would be writing to its handler.
func TestASinkURLSecretNeverLeaves(t *testing.T) {
	recs := []record.Record{rec(t, tenantA)}
	big := rec(t, tenantA, func(r *record.Record) {
		r.Version = "2"
		r.Text = strings.Repeat("x", 4096)
	})
	// Sealed for this tenant and still refused by the format, built the same way
	// TestARecordTheFormatRefusesIsOneRejection builds it: the audience is changed after
	// Seal, which is outside what the seal covers, so SealedFor stays true and Validate
	// inside MarshalJSON goes false. The guard below pins that premise, because the site
	// this record drives is reached only by a record that gets past rejectUnsealed.
	broken := rec(t, tenantA, func(r *record.Record) { r.Version = "3" })
	broken.Visibility.Audience = "nobody"
	if !broken.SealedFor(tenantA) {
		t.Fatalf("the record is no longer sealed for the tenant, so the attempt below drives the wrong-tenant site and not the format one")
	}

	// The attempts below drive every place this package builds a Fault or a Rejection, against
	// an endpoint whose query string holds the marker. A receiver that echoes the request URL
	// back is a real thing for a 404. The places, so that a twelfth one added to this package
	// is visibly missing from this list rather than silently unreached:
	//
	//  1. the no-credential refusal, which is the one that holds the endpoint and the token
	//     at once;
	//  2. the credential halt (401, 403);
	//  3. the request halt, which is every other 4xx outside the refusal band;
	//  4. the status retry (5xx and anything else);
	//  5. the refusal of the records of one request (422);
	//  6. the unreadable answer;
	//  7. the transport failure;
	//  8. the 2xx "rejected" list;
	//  9. the record that is larger than one request;
	// 10. the record the format refuses;
	// 11. the record that is not sealed for the tenant it is being delivered under.
	//
	// The twelfth, the NewRequestWithContext branch, is unreachable from a caller that passes
	// a context, and its Detail is a constant of this package that carries nothing from the
	// request.
	//
	// Two of these are here because a leak reached them and nothing searched them. Site 3
	// arrived with the halt band and survived a Detail built from the receiver's echoed code
	// (mutation T3). A split batch is driven too, because a leak confined to a request that is
	// not the first of one is otherwise unsearched (mutation T2).
	//
	// An attempt drives the site it is named for only while the record it carries still gets
	// that far. Site 10 was reached by the zero Record until a check added ahead of it in
	// marshalAll, the one site 11 is about, began refusing that record first: the list still
	// read as eleven, the attempt still produced its one Rejection, and nothing failed, while
	// site 10 went unsearched and mutation T5 survived the package. So an attempt whose record
	// has to pass an earlier check asserts that it does, next to the record and not here.
	endpointOf := func(base string) string { return base + "/deliver?api_key=" + marker }
	echoing := `{"code":"see https://sink.example/deliver?api_key=` + marker + `"}`

	type attempt struct {
		name     string
		endpoint func(base string) string
		handler  http.HandlerFunc
		cfg      sink.HTTPConfig
		recs     []record.Record
		// token replaces the marker-bearing token function. It is how the one failure path
		// that holds the endpoint and the credential at once, the refusal at the top of
		// Deliver, is reached at all: every other attempt's Token succeeds.
		token func(context.Context, tenancy.ID) (string, error)
		// wantRejections is how many Rejection entries the attempt has to produce, and
		// wantFault whether it has to fail. One of the two is always set.
		wantRejections int
		wantFault      bool
	}
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, echoing)
		}
	}
	attempts := []attempt{
		{
			name:     "no credential, which is the one refusal that holds the endpoint and the token at once",
			endpoint: endpointOf, wantFault: true,
			handler: func(http.ResponseWriter, *http.Request) {
				t.Errorf("a request was sent without a credential")
			},
			token: func(context.Context, tenancy.ID) (string, error) {
				return "", errors.New("vault is down at " + marker)
			},
		},
		{name: "401", endpoint: endpointOf, handler: status(401), wantFault: true},
		// The halt band outside the credential, which is where the ClassSinkRefused fault
		// is built. 413 and 404 are the two shapes of it: a verdict on the request message,
		// and the default for a 4xx this sink does not recognise.
		{name: "413", endpoint: endpointOf, handler: status(413), wantFault: true},
		{name: "404", endpoint: endpointOf, handler: status(404), wantFault: true},
		{name: "500", endpoint: endpointOf, handler: status(500), wantFault: true},
		{
			// A split batch, so that the fault comes from a request that is not the
			// first. The first request answers 204 and the second halts.
			name: "a halt on the second request of a split batch", endpoint: endpointOf,
			wantFault: true, recs: []record.Record{big, recs[0]},
			cfg: sink.HTTPConfig{MaxRequestBytes: len(doc(t, big)) + 64},
			handler: func() http.HandlerFunc {
				var mu sync.Mutex
				sent := 0
				return func(w http.ResponseWriter, _ *http.Request) {
					mu.Lock()
					sent++
					n := sent
					mu.Unlock()
					if n == 1 {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					w.WriteHeader(http.StatusRequestEntityTooLarge)
					_, _ = io.WriteString(w, echoing)
				}
			}(),
		},
		{name: "an answer that cannot be read", endpoint: endpointOf, wantFault: true, handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `not json, and here is the url again: ?api_key=`+marker)
		}},
		{
			name:      "a host that does not resolve",
			endpoint:  func(string) string { return "https://sink.invalid/deliver?api_key=" + marker },
			handler:   func(http.ResponseWriter, *http.Request) {},
			cfg:       sink.HTTPConfig{Timeout: 5 * time.Second},
			wantFault: true,
		},
		// The three ways a Rejection is built, which is the half of the acceptance line that
		// round 1 found was never reached.
		{
			name: "a 2xx that names the record the receiver refused", endpoint: endpointOf,
			wantRejections: 1,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"rejected":[{"id":%q,"code":"see_?api_key=%s"}]}`, recs[0].ID, marker)
			},
		},
		{
			name:     "a 4xx, which refuses the records of the request it answered",
			endpoint: endpointOf, handler: status(422), wantRejections: 1,
		},
		{
			name: "a record larger than one request", endpoint: endpointOf,
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
			cfg:     sink.HTTPConfig{MaxRequestBytes: len(doc(t, recs[0])) + 64},
			// The batch holds the small record too, so the request is made and the
			// endpoint is reached on the way to the rejection.
			recs: []record.Record{big, recs[0]}, wantRejections: 1,
		},
		{
			// broken and not the zero Record. The zero Record is sealed for no tenant,
			// so rejectUnsealed refuses it ahead of json.Marshal and this attempt drove
			// site 11 twice while site 10 went undriven, which mutation T5 survived.
			name: "a record the format refuses", endpoint: endpointOf,
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
			recs:    []record.Record{broken, recs[0]}, wantRejections: 1,
		},
		{
			name: "a record sealed for another tenant", endpoint: endpointOf,
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
			recs:    []record.Record{rec(t, tenantB), recs[0]}, wantRejections: 1,
		},
	}
	for _, a := range attempts {
		t.Run(a.name, func(t *testing.T) {
			srv := httptest.NewServer(a.handler)
			t.Cleanup(srv.Close)
			cfg := a.cfg
			cfg.Endpoint = a.endpoint(srv.URL)
			cfg.Token = tokenFor(tokenMarker)
			if a.token != nil {
				cfg.Token = a.token
			}
			s := newHTTP(t, cfg)
			batch := a.recs
			if batch == nil {
				batch = recs
			}

			var logged strings.Builder
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
			result, err := s.Deliver(context.Background(), tenantA, batch)
			slog.SetDefault(old)

			if a.wantFault && err == nil {
				t.Fatalf("the delivery did not fail, so the case proves nothing")
			}
			if !a.wantFault && err != nil {
				t.Fatalf("Deliver: %v, want the records' own outcome", err)
			}
			if len(result.Rejected) != a.wantRejections {
				t.Fatalf("%d rejections, want %d: the case searches nothing it is here to search", len(result.Rejected), a.wantRejections)
			}
			searched := []string{logged.String()}
			if err != nil {
				searched = append(searched, err.Error())
				var f *sink.Fault
				if errors.As(err, &f) {
					searched = append(searched, f.Cause.String(), f.Detail)
				}
			}
			for _, r := range result.Rejected {
				searched = append(searched, r.ID, r.Cause.String(), r.Detail)
			}
			for i, text := range searched {
				for what, secret := range map[string]string{
					"the URL's marker": marker,
					"the token":        tokenMarker,
					"the endpoint":     srv.URL,
				} {
					if strings.Contains(text, secret) {
						t.Errorf("string %d holds %s: %q", i, what, text)
					}
				}
			}
			if logged.Len() != 0 {
				t.Errorf("the sink wrote to the default logger: %q", logged.String())
			}
		})
	}
}

// TestAFaultWrapsNoTransportError: errors.As must not reach a *url.Error through a Fault, because
// its text quotes the request URL and a Fault is what a worker prints.
func TestAFaultWrapsNoTransportError(t *testing.T) {
	t.Parallel()
	s := newHTTP(t, sink.HTTPConfig{Endpoint: "https://sink.invalid/deliver?api_key=" + marker, Timeout: 5 * time.Second})
	_, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)})
	if err == nil {
		t.Fatalf("the delivery did not fail")
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		t.Errorf("a *url.Error is reachable through the fault: %v", urlErr)
	}
	if errors.Unwrap(err) != nil {
		t.Errorf("the fault wraps %v", errors.Unwrap(err))
	}
}

// TestNewHTTPRefusesAConfigurationItCannotUse, and says so without quoting the endpoint.
func TestNewHTTPRefusesAConfigurationItCannotUse(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]sink.HTTPConfig{
		"no endpoint":         {Token: tokenFor("t")},
		"a relative endpoint": {Endpoint: "/deliver?api_key=" + marker, Token: tokenFor("t")},
		"another scheme":      {Endpoint: "ftp://host/deliver?api_key=" + marker, Token: tokenFor("t")},
		"no host":             {Endpoint: "http:///deliver?api_key=" + marker, Token: tokenFor("t")},
		"no token function":   {Endpoint: "https://host/deliver"},
		"a request size too small for a record": {
			Endpoint: "https://host/deliver", Token: tokenFor("t"), MaxRequestBytes: 8,
		},
		"a wire name that is not a source name": {
			Endpoint: "https://host/deliver", Token: tokenFor("t"), Names: sink.Names{"clickup": "ClickUp"},
		},
		"a key that is not a provider key": {
			Endpoint: "https://host/deliver", Token: tokenFor("t"), Names: sink.Names{"click-up": "clickup"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, err := sink.NewHTTP(cfg)
			if err == nil {
				t.Fatalf("NewHTTP returned %v and no error", s)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("the error quotes the endpoint: %v", err)
			}
		})
	}
}

// TestTheCauseCarriesOnlyACodeThatLooksLikeOne: outbox.Cause does the filtering, and this is the
// sink wired to it.
func TestTheCauseCarriesOnlyACodeThatLooksLikeOne(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"a URL with a key": `{"code":"https://sink.example/d?api_key=` + marker + `"}`,
		"a sentence":       `{"code":"the request was not acceptable"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// 422 and not 400: 400 is a verdict on the request and halts, and this test
			// is about the code that travels with a refusal of the records.
			s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, body)
			}, sink.HTTPConfig{})
			result, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)})
			if err != nil {
				t.Fatalf("Deliver: %v, want the refusal reported record by record", err)
			}
			if len(result.Rejected) != 1 {
				t.Fatalf("rejected %+v, want the one record sent", result.Rejected)
			}
			if want := "sink rejected the record (status 422, code withheld)"; result.Rejected[0].Cause.String() != want {
				t.Errorf("cause %q, want %q", result.Rejected[0].Cause.String(), want)
			}
		})
	}
}

// TestEveryStatusFaultCauseIsOneOfTheOutboxClasses keeps the sink and the outbox's own
// classification from drifting apart.
func TestEveryStatusFaultCauseIsOneOfTheOutboxClasses(t *testing.T) {
	t.Parallel()
	texts := map[string]bool{
		outbox.NewCause(outbox.ClassSinkUnavailable).String():  true,
		outbox.NewCause(outbox.ClassSinkRejected).String():     true,
		outbox.NewCause(outbox.ClassSinkUnauthorized).String(): true,
		outbox.NewCause(outbox.ClassSinkRefused).String():      true,
		outbox.NewCause(outbox.ClassSinkUnreadable).String():   true,
		outbox.NewCause(outbox.ClassVaultUnavailable).String(): true,
		outbox.NewCause(outbox.ClassInternal).String():         true,
		outbox.NewCause(outbox.ClassUnclassified).String():     true,
	}
	for status := 100; status <= 599; status++ {
		_, cause, _ := sink.StatusVerdict(status, "")
		text := cause.String()
		trimmed := strings.Split(text, " (")[0]
		if !texts[trimmed] {
			t.Fatalf("status %d: cause %q is not one of the outbox's classes", status, text)
		}
		if trimmed == outbox.NewCause(outbox.ClassUnclassified).String() {
			t.Fatalf("status %d: the cause was never given a class", status)
		}
	}
}

// batchIDs decodes a posted batch and returns the record ids in it, in order.
func batchIDs(t testing.TB, body []byte) []string {
	t.Helper()
	var batch struct {
		Records []struct {
			ID string `json:"id"`
		} `json:"records"`
	}
	if err := json.Unmarshal(body, &batch); err != nil {
		t.Errorf("the posted batch does not decode: %v", err)
		return nil
	}
	ids := make([]string, 0, len(batch.Records))
	for _, r := range batch.Records {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestARefusalOfOneRequestLeavesTheOtherRequestsOfTheBatchAlone is blocking finding 1 of the
// round 1 review. A batch too large for one request goes out in several, and a receiver that
// refuses one of them has said something about the records in that request and nothing about
// the records in the others. Reporting a whole-batch dead letter killed the records of the
// requests that had already landed and, worse, the records of the requests that were never sent.
func TestARefusalOfOneRequestLeavesTheOtherRequestsOfTheBatchAlone(t *testing.T) {
	t.Parallel()
	recs := make([]record.Record, 6)
	for i := range recs {
		recs[i] = rec(t, tenantA, func(r *record.Record) { r.Version = fmt.Sprint(i) })
	}
	// Room for two documents per request, and not three.
	limit := 2*len(doc(t, recs[0])) + 16 + 32

	var (
		mu       sync.Mutex
		offered  []string
		requests int
	)
	s := serving(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read: %v", err)
			return
		}
		mu.Lock()
		requests++
		n := requests
		offered = append(offered, batchIDs(t, body)...)
		mu.Unlock()
		if n != 2 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"code":"unsupported_kind"}`)
	}, sink.HTTPConfig{MaxRequestBytes: limit})

	result, err := s.Deliver(context.Background(), tenantA, recs)
	if err != nil {
		t.Fatalf("Deliver: %v, want the refusal reported record by record", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 3 {
		t.Errorf("%d requests, want 3", requests)
	}
	want := make([]string, len(recs))
	for i, r := range recs {
		want[i] = r.ID
	}
	if !slices.Equal(offered, want) {
		t.Errorf("the receiver was offered %v, want every record of the batch in order: %v", offered, want)
	}
	var rejected []string
	for _, r := range result.Rejected {
		rejected = append(rejected, r.ID)
		if got, w := r.Cause.String(), "sink rejected the record (status 422, code unsupported_kind)"; got != w {
			t.Errorf("cause %q, want %q", got, w)
		}
	}
	if !slices.Equal(rejected, []string{recs[2].ID, recs[3].ID}) {
		t.Errorf("rejected %v, want the two records of the request that was refused", rejected)
	}
}

// TestAHaltOnALaterRequestDeliversNothingAndRefusesNothing is the other half of the same
// finding. A credential the receiver will not take is about the delivery and not about any
// record, so nothing in the batch counts as delivered, no record is refused, and the whole
// batch goes again once an operator has fixed the credential.
//
// The first request is answered 200 with a "rejected" list naming one of its two records, so by
// the time the second request is refused the result already holds a Rejection. Without one the
// result would be empty whatever Deliver returned, and the zero-value assertion would be
// satisfied by the handler rather than by the code: the round 2 review proved exactly that by
// changing both of Deliver's error returns to "return result, err" and watching the suite stay
// green.
func TestAHaltOnALaterRequestDeliversNothingAndRefusesNothing(t *testing.T) {
	t.Parallel()
	recs := make([]record.Record, 6)
	for i := range recs {
		recs[i] = rec(t, tenantA, func(r *record.Record) { r.Version = fmt.Sprint(i) })
	}
	limit := 2*len(doc(t, recs[0])) + 16 + 32

	var (
		mu       sync.Mutex
		requests int
		refused  int
	)
	s := serving(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read: %v", err)
			return
		}
		ids := batchIDs(t, body)
		mu.Lock()
		requests++
		n := requests
		mu.Unlock()
		if n != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// The first request lands, and the receiver refuses one of the two records in it.
		// That Rejection is in the result before the halt arrives.
		if len(ids) == 0 {
			t.Errorf("the first request carried no records")
			return
		}
		mu.Lock()
		refused++
		mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"rejected":[{"id":%q,"code":"unsupported_kind"}]}`, ids[0])
	}, sink.HTTPConfig{MaxRequestBytes: limit})

	result, err := s.Deliver(context.Background(), tenantA, recs)
	f := fault(t, err)
	if f.Action != sink.ActionHalt {
		t.Errorf("action %v, want halt", f.Action)
	}
	if len(result.Rejected) != 0 {
		t.Errorf("the result carries %+v, want the zero value: the Rejection the first request produced goes with it", result.Rejected)
	}
	mu.Lock()
	defer mu.Unlock()
	if refused != 1 {
		t.Fatalf("the receiver refused %d records on the first request, want 1: with none the assertion above cannot fail", refused)
	}
	if requests != 2 {
		t.Errorf("%d requests, want the sink to stop at the one that was refused", requests)
	}
}
