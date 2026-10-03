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
	"strings"
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
// status, and what the sink does about it.
func TestTheStatusTable(t *testing.T) {
	t.Parallel()
	type want struct {
		action sink.Action
		class  string
	}
	cases := map[int]want{
		401: {sink.ActionHalt, "sink refused the credential (status 401)"},
		403: {sink.ActionHalt, "sink refused the credential (status 403)"},
		400: {sink.ActionDeadLetter, "sink rejected the record (status 400)"},
		404: {sink.ActionDeadLetter, "sink rejected the record (status 404)"},
		409: {sink.ActionDeadLetter, "sink rejected the record (status 409)"},
		413: {sink.ActionDeadLetter, "sink rejected the record (status 413)"},
		422: {sink.ActionDeadLetter, "sink rejected the record (status 422)"},
		499: {sink.ActionDeadLetter, "sink rejected the record (status 499)"},
		408: {sink.ActionRetry, "sink unavailable (status 408)"},
		429: {sink.ActionRetry, "sink unavailable (status 429)"},
		500: {sink.ActionRetry, "sink unavailable (status 500)"},
		502: {sink.ActionRetry, "sink unavailable (status 502)"},
		503: {sink.ActionRetry, "sink unavailable (status 503)"},
		504: {sink.ActionRetry, "sink unavailable (status 504)"},
		599: {sink.ActionRetry, "sink unavailable (status 599)"},
		// Not a status the table names: retried, which is the answer that loses nothing.
		301: {sink.ActionRetry, "sink unavailable (status 301)"},
		302: {sink.ActionRetry, "sink unavailable (status 302)"},
		100: {sink.ActionRetry, "sink unavailable (status 100)"},
	}
	for status, w := range cases {
		f := sink.StatusFault(status, "")
		if f.Action != w.action {
			t.Errorf("status %d: action %v, want %v", status, f.Action, w.action)
		}
		if got := f.Cause.String(); got != w.class {
			t.Errorf("status %d: cause %q, want %q", status, got, w.class)
		}
	}
	// Every status from 100 to 599 gets one of the three actions, and never the zero value.
	for status := 100; status <= 599; status++ {
		switch a := sink.StatusFault(status, "").Action; a {
		case sink.ActionRetry, sink.ActionHalt, sink.ActionDeadLetter:
		default:
			t.Fatalf("status %d: action %v", status, a)
		}
	}
}

// TestALiveAnswerIsClassifiedTheSameWay runs the three rows of the acceptance line against a real
// server, so the table above is wired to what Deliver does and not only to a function.
func TestALiveAnswerIsClassifiedTheSameWay(t *testing.T) {
	t.Parallel()
	for status, want := range map[int]sink.Action{
		401: sink.ActionHalt,
		403: sink.ActionHalt,
		500: sink.ActionRetry,
		503: sink.ActionRetry,
		422: sink.ActionDeadLetter,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"code":"nope_"}`)
			}, sink.HTTPConfig{})
			_, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)})
			f := fault(t, err)
			if f.Action != want {
				t.Errorf("action %v, want %v", f.Action, want)
			}
			if !strings.Contains(f.Cause.String(), fmt.Sprintf("status %d", status)) {
				t.Errorf("cause %q does not carry the status", f.Cause.String())
			}
			if !strings.Contains(f.Cause.String(), "code nope_") {
				t.Errorf("cause %q does not carry the sink's error code", f.Cause.String())
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
		"200 and a field it does not know": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"accepted":1}`)
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
	var auth, contentType, body string
	s := serving(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
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
	if !strings.HasPrefix(body, `{"records":[`) || !strings.HasSuffix(body, `]}`) {
		t.Errorf("body %q is not the batch shape", body)
	}
	if strings.Contains(body, tenantA.String()) {
		t.Errorf("the body carries the tenant id")
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
	if want := "internal error"; result.Rejected[0].Cause.String() != want {
		t.Errorf("cause %q, want %q", result.Rejected[0].Cause.String(), want)
	}
	if result.Rejected[0].Detail != "the record is larger than one request of this sink" {
		t.Errorf("detail %q", result.Rejected[0].Detail)
	}
	if sent == 0 {
		t.Errorf("the small record was not sent")
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
func TestAFaultOnTheFirstRequestOfASplitBatchStopsTheRest(t *testing.T) {
	t.Parallel()
	recs := make([]record.Record, 4)
	for i := range recs {
		recs[i] = rec(t, tenantA, func(r *record.Record) { r.Version = fmt.Sprint(i) })
	}
	requests := 0
	s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusServiceUnavailable)
	}, sink.HTTPConfig{MaxRequestBytes: len(doc(t, recs[0])) + 64})

	result, err := s.Deliver(context.Background(), tenantA, recs)
	f := fault(t, err)
	if f.Action != sink.ActionRetry {
		t.Errorf("action %v, want retry", f.Action)
	}
	if requests != 1 {
		t.Errorf("%d requests, want the first one only", requests)
	}
	if len(result.Rejected) != 0 {
		t.Errorf("the result carries %+v, want the zero value", result.Rejected)
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

// TestAnEmptyBatchSendsNothing.
func TestAnEmptyBatchSendsNothing(t *testing.T) {
	t.Parallel()
	reached := false
	s := serving(t, func(http.ResponseWriter, *http.Request) { reached = true }, sink.HTTPConfig{})
	result, err := s.Deliver(context.Background(), tenantA, nil)
	if err != nil || len(result.Rejected) != 0 || reached {
		t.Fatalf("Deliver of an empty batch: %v, %+v, request sent %v", err, result, reached)
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
// #32: a sink URL carrying a marker secret appears in nothing a failed delivery produces. What
// is searched is every string this package hands back (the error, the Cause that goes into the
// outbox column, each Rejection) and everything written to the default logger while the
// delivery runs. The bearer token carries a marker of its own, because it travels with the
// same request.
//
// The subtests do not run in parallel: each one replaces the default logger for the length of
// its delivery, and two of them doing that at once would each be reading the other's handler.
func TestASinkURLSecretNeverLeaves(t *testing.T) {
	t.Parallel()
	recs := []record.Record{rec(t, tenantA)}

	// Every way a delivery can fail, against an endpoint whose query string holds the marker.
	endpointOf := func(base string) string { return base + "/deliver?api_key=" + marker }

	type attempt struct {
		name     string
		endpoint func(base string) string
		handler  http.HandlerFunc
		cfg      sink.HTTPConfig
	}
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			// A receiver that echoes the request URL back, which is a real thing for a 404.
			_, _ = io.WriteString(w, `{"code":"see https://sink.example/deliver?api_key=`+marker+`"}`)
		}
	}
	attempts := []attempt{
		{name: "401", endpoint: endpointOf, handler: status(401)},
		{name: "422", endpoint: endpointOf, handler: status(422)},
		{name: "500", endpoint: endpointOf, handler: status(500)},
		{name: "an answer that cannot be read", endpoint: endpointOf, handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `not json, and here is the url again: ?api_key=`+marker)
		}},
		{
			name:     "a host that does not resolve",
			endpoint: func(string) string { return "https://sink.invalid/deliver?api_key=" + marker },
			handler:  func(http.ResponseWriter, *http.Request) {},
			cfg:      sink.HTTPConfig{Timeout: 5 * time.Second},
		},
	}
	for _, a := range attempts {
		t.Run(a.name, func(t *testing.T) {
			srv := httptest.NewServer(a.handler)
			t.Cleanup(srv.Close)
			cfg := a.cfg
			cfg.Endpoint = a.endpoint(srv.URL)
			cfg.Token = tokenFor(tokenMarker)
			s := newHTTP(t, cfg)

			var logged strings.Builder
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
			result, err := s.Deliver(context.Background(), tenantA, recs)
			slog.SetDefault(old)

			if err == nil {
				t.Fatalf("the delivery did not fail, so the case proves nothing")
			}
			searched := []string{err.Error(), logged.String()}
			var f *sink.Fault
			if errors.As(err, &f) {
				searched = append(searched, f.Cause.String(), f.Detail)
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
			s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, body)
			}, sink.HTTPConfig{})
			_, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)})
			f := fault(t, err)
			if want := "sink rejected the record (status 400, code withheld)"; f.Cause.String() != want {
				t.Errorf("cause %q, want %q", f.Cause.String(), want)
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
		outbox.NewCause(outbox.ClassSinkUnreadable).String():   true,
		outbox.NewCause(outbox.ClassVaultUnavailable).String(): true,
		outbox.NewCause(outbox.ClassInternal).String():         true,
		outbox.NewCause(outbox.ClassUnclassified).String():     true,
	}
	for status := 100; status <= 599; status++ {
		text := sink.StatusFault(status, "").Cause.String()
		trimmed := strings.Split(text, " (")[0]
		if !texts[trimmed] {
			t.Fatalf("status %d: cause %q is not one of the outbox's classes", status, text)
		}
		if trimmed == outbox.NewCause(outbox.ClassUnclassified).String() {
			t.Fatalf("status %d: the cause was never given a class", status)
		}
	}
}
