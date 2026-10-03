package sink_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"syscall"
	"testing"

	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/sink"
)

// TestARecordTheFormatRefusesIsOneRejection, for every sink. A record that does not marshal is
// Lawang's own defect and not the receiver's, so the Cause says so and the rest of the batch
// still goes.
func TestARecordTheFormatRefusesIsOneRejection(t *testing.T) {
	t.Parallel()
	good := rec(t, tenantA)
	// The zero Record has no format, no id and no scope, so MarshalJSON refuses it.
	batch := []record.Record{{}, good}

	httpSink := serving(t, func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Errorf("read: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}, sink.HTTPConfig{})
	stub := newStub(t, sink.StubConfig{})
	jsonlSink, dir := newJSONL(t, nil)

	for name, s := range map[string]sink.Sink{"http": httpSink, "stub": stub, "jsonl": jsonlSink} {
		result, err := s.Deliver(context.Background(), tenantA, batch)
		if err != nil {
			t.Fatalf("%s: Deliver: %v", name, err)
		}
		if len(result.Rejected) != 1 {
			t.Fatalf("%s: rejected %+v, want one", name, result.Rejected)
		}
		// The code is what an operator reading last_error or dead_reason has to go on,
		// because a Detail is for a log line and is not stored.
		if want := "internal error (code invalid_record)"; result.Rejected[0].Cause.String() != want {
			t.Errorf("%s: cause %q, want %q", name, result.Rejected[0].Cause.String(), want)
		}
		if result.Rejected[0].Detail != "the record does not pass the format" {
			t.Errorf("%s: detail %q", name, result.Rejected[0].Detail)
		}
	}
	if got := len(stub.Documents(tenantA)); got != 1 {
		t.Errorf("the stub holds %d documents, want the good one", got)
	}
	if got := len(lines(t, dir, tenantA)); got != 1 {
		t.Errorf("the file has %d lines, want the good one", got)
	}
}

// TestTransportDetailIsReadFromTheErrorsTypeAndNotItsText. Each case is wrapped in a *url.Error
// the way net/http returns it, with the request URL in its text, so the phrase that comes back
// has to be chosen without reading that text.
func TestTransportDetailIsReadFromTheErrorsTypeAndNotItsText(t *testing.T) {
	t.Parallel()
	wrap := func(err error) error {
		return &url.Error{Op: "Post", URL: "https://sink.example/d?api_key=" + marker, Err: err}
	}
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"a name that does not resolve": {
			err:  &net.DNSError{Err: "no such host", Name: "sink.invalid", IsNotFound: true},
			want: "the host name does not resolve",
		},
		"a lookup that failed another way": {
			err:  &net.DNSError{Err: "server misbehaving", Name: "sink.invalid"},
			want: "the connection failed",
		},
		"a lookup that timed out": {
			err:  &net.DNSError{Err: "timeout", Name: "sink.invalid", IsTimeout: true},
			want: "the connection failed",
		},
		"a timeout": {
			err:  &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded},
			want: "the request timed out",
		},
		"a refused connection": {
			err:  &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
			want: "the connection was refused",
		},
		"a certificate that does not verify": {
			err:  &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
			want: "TLS negotiation failed",
		},
		"an unknown authority": {
			err:  x509.UnknownAuthorityError{},
			want: "TLS negotiation failed",
		},
		"a certificate for another host": {
			err:  x509.HostnameError{Host: "sink.example"},
			want: "TLS negotiation failed",
		},
		"a TLS alert": {
			err:  tls.AlertError(40),
			want: "TLS negotiation failed",
		},
		"a record header": {
			err:  tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"},
			want: "TLS negotiation failed",
		},
		"a handshake failure net/http only words": {
			err:  errors.New("remote error: tls: handshake failure"),
			want: "TLS negotiation failed",
		},
		"anything else": {
			err:  errors.New("something went wrong"),
			want: "the connection failed",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := sink.TransportDetail(wrap(c.err))
			if got != c.want {
				t.Errorf("TransportDetail = %q, want %q", got, c.want)
			}
			if !slices.Contains(sink.FaultDetails, got) {
				t.Errorf("%q is not one of this package's phrases", got)
			}
		})
	}
}

// TestStubCodeIsOneOfAClosedSet: the code that reaches the outbox when the stub refuses a
// record, which is the stub's own and never a decoder's text.
func TestStubCodeIsOneOfAClosedSet(t *testing.T) {
	t.Parallel()
	for want, err := range map[string]error{
		"too_large":       fmt.Errorf("wrapped: %w", sink.ErrTooLarge),
		"repeat_differs":  sink.ErrRepeatDiffers,
		"supersede_cycle": sink.ErrSupersedeCycle,
		"invalid_record":  fmt.Errorf("sink: stub: %w", record.ErrInvalid),
		"refused":         errors.New("something nobody classified"),
	} {
		if got := sink.StubCode(err); got != want {
			t.Errorf("StubCode(%v) = %q, want %q", err, got, want)
		}
	}
}

// TestNewStubRefusesAConfigurationItCannotUse.
func TestNewStubRefusesAConfigurationItCannotUse(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]sink.StubConfig{
		"a negative document size":              {MaxDocumentBytes: -1},
		"a wire name that is not a source name": {Names: sink.Names{"clickup": "ClickUp"}},
	} {
		if _, err := sink.NewStub(cfg); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestContentOfRefusesWhatIsNotAJSONObject. Accept decodes before it compares, so this is about
// the function and not about a document that can reach it.
func TestContentOfRefusesWhatIsNotAJSONObject(t *testing.T) {
	t.Parallel()
	if _, err := sink.ContentOf([]byte("{not json")); err == nil {
		t.Errorf("invalid JSON was accepted")
	}
	// A document that is valid JSON and not an object keeps its shape, with nothing to delete.
	out, err := sink.ContentOf([]byte(`[1,2]`))
	if err != nil {
		t.Fatalf("a JSON array: %v", err)
	}
	if string(out) != "[1,2]" {
		t.Errorf("ContentOf([1,2]) = %q", out)
	}
}
