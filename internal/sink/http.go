package sink

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// The phrases this package puts in Fault.Detail and Rejection.Detail. Each one is written here,
// in this program, and none of them is built from anything a sink or a provider sent.
const (
	detailTimedOut        = "the request timed out"
	detailNoSuchHost      = "the host name does not resolve"
	detailRefused         = "the connection was refused"
	detailTLS             = "TLS negotiation failed"
	detailConnection      = "the connection failed"
	detailCancelled       = "the delivery was cancelled"
	detailNoCredential    = "this tenant has no sink credential"
	detailUnreadableReply = "the sink's answer could not be read"
	detailRecordTooLarge  = "the record is larger than one request of this sink"
	detailNoRequest       = "the request could not be built"
	detailInvalidRecord   = "the record does not pass the format"
	detailWriteFailed     = "the file could not be written"
	detailWrongTenant     = "the record was not sealed for this tenant"
)

// faultDetails is every phrase above, as a list a test can walk.
// TestADetailIsAlwaysThisPackagesOwnPhrase reads the constants out of this package's own source
// and holds the two together, so a fourteenth phrase that is not listed here fails.
var faultDetails = []string{
	detailTimedOut, detailNoSuchHost, detailRefused, detailTLS, detailConnection,
	detailCancelled, detailNoCredential, detailUnreadableReply, detailRecordTooLarge,
	detailNoRequest, detailInvalidRecord, detailWriteFailed, detailWrongTenant,
}

// The error codes this package puts in a Cause for a record Lawang itself refused. Detail says
// the same thing at length, but a Detail is for a log line and never reaches the outbox, so
// without these a record Lawang refused reads in last_error and dead_reason as a bare "internal
// error", which is also what a database failure of its own would say. Each is a constant of
// this program, which is what outbox.Cause.WithCode asks of a code.
const (
	codeRecordTooLarge = "record_too_large"
	codeInvalidRecord  = "invalid_record"
	codeWrongTenant    = "wrong_tenant"
)

// Defaults of HTTPConfig.
const (
	// DefaultMaxRequestBytes is the body size one request may reach.
	//
	// The arithmetic behind the number: every string field of a record has a limit in
	// internal/record (MaxText, MaxTitle, MaxExternalID, MaxVersion, MaxAuthor,
	// MaxContainerID, MaxScopeID, MaxDelivery), a limit counts Unicode code points, and
	// encoding/json writes one code point as at most six bytes (a control character and the
	// characters '<', '>', '&', U+2028 and U+2029 go out as a six byte escape). MaxText alone
	// is 1,048,576 of those, so the sum is a little over 6.3 MB, and 8 MiB leaves room for
	// several small documents in one request.
	DefaultMaxRequestBytes = 8 << 20
	// DefaultTimeout bounds one request, from dialling to the last byte of the response.
	DefaultTimeout = 30 * time.Second
	// maxResponseBytes bounds what is read back. The answer is a list of refused record ids, so
	// this is far more than it needs, and the reader is an io.LimitedReader because the response
	// comes from outside.
	maxResponseBytes = 64 << 10
)

// HTTPConfig configures an HTTP sink. Endpoint and Token are required.
type HTTPConfig struct {
	// Endpoint is the absolute http or https URL records are posted to. It may carry a query
	// string, which is where several receivers take their API key, so nothing in this package
	// puts it in an error, a Fault or a log line.
	Endpoint string
	// Names is the wire name per provider key (principle 4). It may be nil.
	Names Names
	// Token returns the bearer token for t: the per-tenant sink credential, which is what
	// establishes the tenant at the receiver (architecture section 4), since the tenant is in no
	// field of the envelope. An error, or an empty token, is a refusal and not a default: the
	// delivery goes back on the ladder and nothing is sent. B13's vault is what fills this in;
	// until then a deployment wires its own function.
	Token func(ctx context.Context, t tenancy.ID) (string, error)
	// MaxRequestBytes is the body size one request may reach. Zero means
	// DefaultMaxRequestBytes. A batch is sent in as many requests as it takes to stay under it,
	// and a record whose document alone does not fit is rejected before anything is sent.
	MaxRequestBytes int
	// Timeout bounds one request. Zero means DefaultTimeout.
	Timeout time.Duration
}

// HTTP posts records to an HTTP endpoint.
//
// # The wire protocol
//
// This is the frozen shape, which architecture section 7 states for a receiver author and
// ADR 13 records with its cost.
//
// One request is POST to the configured endpoint with Content-Type application/json, an
// Authorization header carrying the tenant's bearer token, and a body of
//
//	{"v":1,"records":[<record document>, ...]}
//
// Each element is a record document exactly as record.Record.MarshalJSON writes it. The tenant is
// in no field: the receiver learns it from the credential, which is the trust root for a sink
// delivery (architecture section 4). "v" is the version of this protocol, ProtocolVersion, and
// it is what lets the answer grow a second member one day without every receiver written for v1
// having to guess. A receiver that does not speak the version refuses the request with 426, or
// with 400, and both halt: see the status bands below. Nothing bounds the bearer token's
// length, so a large credential in front of a proxy with a small header buffer draws a 431,
// which halts for the same reason.
//
// The request also carries Accept: application/json, because the only answer this sink can read
// is the JSON one below.
//
// A 2xx answer means the batch landed. Its body may name the records the receiver refused:
//
//	{"rejected":[{"id":"rec_...","code":"unsupported_kind"}]}
//
// The answer is strict about that member, which is the decision on issue #9. An empty body,
// whitespace, or {} means every record was taken. Any other body must be a JSON object carrying
// a "rejected" member, spelled exactly that way, whose value is a JSON list; a body that is
// valid JSON and carries no member this sink knows is unreadable, as are a body that does not
// parse, a body that is not an object, a "rejected" that is not a list (null included), an id
// that was not in the request and an id named twice. Unreadable means nothing is marked
// delivered and the batch is retried. The cost, which the maintainer accepted: a receiver that
// answers 200 with its own bookkeeping and no "rejected" member now fails loudly instead of
// losing the records it refused silently.
//
// Any other status is classified by statusOf, in three bands.
//
//   - Halt, a verdict on the request and not on anything in it: the credential (401, 403) and
//     every other 4xx outside the two bands below, which is the default. An operator changes a
//     credential, a number, an endpoint or a receiver, and until then the ladder would only
//     send the same request again. Nothing is killed.
//   - Retried: 408, 429 and everything that is not a 4xx.
//   - A refusal of the records of that one request: 422 alone, which is the one status HTTP
//     defines as a verdict on the content of the request. Each record of that request comes
//     back as a Rejection and the records of the other requests are still offered.
//
// So the two faults leave the whole batch undelivered, and a Rejection speaks for the records of
// the request it answered and for no others. A sink never kills a record it has not offered.
// Which records share a request is decided by the byte arithmetic below and not by the receiver,
// so a receiver that wants to refuse one record of a request answers 2xx with a "rejected" list:
// a 422 kills every record of the request it answered.
//
// statusOf has the reasoning for the band, including why the default for an unrecognised 4xx is
// halt and which statuses were weighed for the refusal band and left out.
//
// The client's CheckRedirect returns http.ErrUseLastResponse, so a 3xx comes back as the
// response and the next request still goes to the configured endpoint rather than to one the
// receiver named.
type HTTP struct {
	endpoint string
	names    Names
	token    func(ctx context.Context, t tenancy.ID) (string, error)
	maxBytes int
	client   *http.Client
}

// NewHTTP builds an HTTP sink, or reports what is wrong with the configuration. Its errors name
// the field and never its value, because Endpoint can hold an API key.
func NewHTTP(cfg HTTPConfig) (*HTTP, error) {
	if err := cfg.Names.Validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("sink: http: the endpoint is not an absolute http or https URL")
	}
	if cfg.Token == nil {
		return nil, errors.New("sink: http: no token function, and a sink delivery has no other way to say whose records these are")
	}
	maxBytes := cfg.MaxRequestBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxRequestBytes
	}
	if maxBytes < minRequestBytes {
		return nil, errors.New("sink: http: the maximum request size is too small to hold one record document")
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	return &HTTP{
		endpoint: cfg.Endpoint,
		names:    cfg.Names,
		token:    cfg.Token,
		maxBytes: maxBytes,
		client: &http.Client{
			Timeout: timeout,
			// A 3xx is handed back as the response instead of being followed. A receiver that
			// answered one would otherwise choose where the next request, with its
			// Authorization header, goes.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// minRequestBytes is the smallest configurable request size: the framing plus a record id, which
// is enough that the "one record does not fit" branch is about the record and not about the
// configuration being absurd.
const minRequestBytes = 128

// ProtocolVersion is the "v" member of every request body. It is here so that a receiver author
// and a test can name the number rather than a spelling of it:
// TestTheRequestCarriesTheTenantsCredentialAndNoTenantField decodes the member out of a posted
// body and holds it against this constant, which is what keeps the literal in bodyPrefix and
// this number from drifting apart.
const ProtocolVersion = 1

const (
	bodyPrefix = `{"v":1,"records":[`
	bodySuffix = `]}`
	// bodyFraming is what the prefix and the suffix cost. A record has to fit inside it, which
	// is why marshalAll measures a document against MaxRequestBytes minus this.
	bodyFraming = len(bodyPrefix) + len(bodySuffix)
	// memberRejected is the one member of a 2xx answer this sink reads, spelled exactly. A
	// body that carries anything else and not this is unreadable rather than read as
	// acceptance, which is the decision on issue #9.
	memberRejected = "rejected"
)

// Deliver posts recs to the endpoint. See HTTP for the protocol.
func (h *HTTP) Deliver(ctx context.Context, t tenancy.ID, recs []record.Record) (DeliveryResult, error) {
	if err := checkTenant(t); err != nil {
		return DeliveryResult{}, err
	}
	if len(recs) == 0 {
		return DeliveryResult{}, nil
	}
	token, err := h.token(ctx, t)
	if err != nil || token == "" {
		return DeliveryResult{}, &Fault{
			Action: ActionRetry,
			Cause:  outbox.NewCause(outbox.ClassVaultUnavailable),
			Detail: detailNoCredential,
		}
	}
	docs, result := h.marshalAll(t, recs)
	var chunk [][]byte
	var ids []string
	size := bodyFraming
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		rejected, err := h.send(ctx, token, chunk, ids, size)
		if err != nil {
			return err
		}
		result.Rejected = append(result.Rejected, rejected...)
		chunk, ids, size = nil, nil, bodyFraming
		return nil
	}
	for i, doc := range docs {
		if doc == nil {
			continue
		}
		// The comma that joins this document to the one before it.
		cost := len(doc)
		if len(chunk) > 0 {
			cost++
		}
		if size+cost > h.maxBytes {
			if err := flush(); err != nil {
				return DeliveryResult{}, err
			}
			cost = len(doc)
		}
		chunk = append(chunk, doc)
		ids = append(ids, recs[i].ID)
		size += cost
	}
	if err := flush(); err != nil {
		return DeliveryResult{}, err
	}
	return result, nil
}

// marshalAll turns every record into its document, under the configured wire name. The entry of a
// record that cannot be sent is nil, and its rejection is in the result: a record that is not
// this tenant's, a record the format refuses, which is Lawang's own defect and not the
// receiver's, and one whose document alone cannot fit a request.
func (h *HTTP) marshalAll(t tenancy.ID, recs []record.Record) ([][]byte, DeliveryResult) {
	docs := make([][]byte, len(recs))
	var result DeliveryResult
	for i, r := range recs {
		// Before the record is marshalled, because a record that is not this tenant's has
		// no business being turned into bytes that go out under this tenant's credential.
		if rejection, wrong := rejectUnsealed(r, t); wrong {
			result.Rejected = append(result.Rejected, rejection)
			continue
		}
		doc, err := json.Marshal(h.names.rename(r))
		switch {
		case err != nil:
			result.Rejected = append(result.Rejected, Rejection{
				ID:     r.ID,
				Cause:  outbox.NewCause(outbox.ClassInternal).WithCode(codeInvalidRecord),
				Detail: detailInvalidRecord,
			})
		case bodyFraming+len(doc) > h.maxBytes:
			result.Rejected = append(result.Rejected, Rejection{
				ID:     r.ID,
				Cause:  outbox.NewCause(outbox.ClassInternal).WithCode(codeRecordTooLarge),
				Detail: detailRecordTooLarge,
			})
		default:
			docs[i] = doc
		}
	}
	return docs, result
}

// send posts one request and reads its answer. ids are the record ids of chunk, in order, and are
// what a "rejected" entry is checked against. size is the length the body will come to, which
// Deliver's loop has already added up, so a chunk of several megabytes is copied once and not
// grown a dozen times.
func (h *HTTP) send(ctx context.Context, token string, chunk [][]byte, ids []string, size int) ([]Rejection, error) {
	body := make([]byte, 0, size)
	body = append(body, bodyPrefix...)
	for i, doc := range chunk {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, doc...)
	}
	body = append(body, bodySuffix...)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(body))
	if err != nil {
		// NewHTTP parsed the endpoint and the method is a constant of this file, so this is
		// the branch for a caller that passed no context. It is a retry and not a panic, and
		// no test reaches it.
		return nil, &Fault{
			Action: ActionRetry,
			Cause:  outbox.NewCause(outbox.ClassInternal),
			Detail: detailNoRequest,
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	// The request URL is h.endpoint, which NewHTTP took from this deployment's own
	// configuration and checked, and which no record, request or response changes. The one
	// thing a response could otherwise choose is where the next request goes, and
	// CheckRedirect in NewHTTP takes that away.
	resp, err := h.client.Do(req) //nolint:gosec // G704: the endpoint is configuration, see above
	if err != nil {
		// err is a *url.Error, whose text quotes h.endpoint. It is read for its classification
		// by transportDetail and never kept.
		return nil, transportFault(ctx, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
	}()
	answer, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		v := statusOf(resp.StatusCode, codeFrom(answer, readErr))
		if v.fault != nil {
			return nil, v.fault
		}
		return refusals(ids, v.refused), nil
	}
	if readErr != nil || len(answer) > maxResponseBytes {
		return nil, unreadableFault(resp.StatusCode)
	}
	return rejectionsFrom(answer, ids, resp.StatusCode)
}

// rejectionsFrom reads the refused records out of a 2xx answer, and refuses to read an answer
// that does not say what the receiver did.
//
// An empty body, whitespace, or {} is the fast path and means every record was taken. Anything
// else must be a JSON object carrying memberRejected, because a receiver that misspells the
// member, or names the one a later version of this protocol uses, would otherwise decode to an
// empty list and have every record of the batch marked delivered with the refused ones lost and
// nothing in last_error. The member is matched by its exact spelling rather than by
// encoding/json's case-insensitive rule, so "Rejected" is a member this sink does not know.
//
// The member's value must be a JSON list, and null is not one: {"rejected":null} is unreadable
// and not an empty list, for the same reason.
func rejectionsFrom(answer []byte, ids []string, status int) ([]Rejection, error) {
	trimmed := bytes.TrimSpace(answer)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var members map[string]json.RawMessage
	// A nil map with no error is the JSON literal null, which is not an object either.
	if err := json.Unmarshal(trimmed, &members); err != nil || members == nil {
		return nil, unreadableFault(status)
	}
	raw, ok := members[memberRejected]
	if !ok {
		if len(members) == 0 {
			return nil, nil
		}
		return nil, unreadableFault(status)
	}
	// The member has to be a JSON list. encoding/json reads the literal null into a slice as
	// an empty one with no error, so without this line {"rejected":null} meant "every record
	// was taken", which is the one direction the strict answer exists to refuse: believing an
	// answer the sink did not understand. A serializer that writes an absent list as null is
	// ordinary, and a receiver that has refused nothing has an empty body and [] to say so.
	// Anything else in the member (a string, a number, an object) fails the decode below.
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, unreadableFault(status)
	}
	var rejected []struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &rejected); err != nil {
		return nil, unreadableFault(status)
	}
	inBatch := make(map[string]bool, len(ids))
	for _, id := range ids {
		inBatch[id] = true
	}
	seen := make(map[string]bool, len(rejected))
	var rejections []Rejection
	for _, entry := range rejected {
		// An id that was not sent leaves the sink unable to say which records landed, and
		// guessing is how a record is silently lost.
		if !inBatch[entry.ID] || seen[entry.ID] {
			return nil, unreadableFault(status)
		}
		seen[entry.ID] = true
		rejections = append(rejections, Rejection{
			ID:    entry.ID,
			Cause: outbox.NewCause(outbox.ClassSinkRejected).WithStatus(status).WithCode(entry.Code),
		})
	}
	return rejections, nil
}

func unreadableFault(status int) *Fault {
	return &Fault{
		Action: ActionRetry,
		Cause:  outbox.NewCause(outbox.ClassSinkUnreadable).WithStatus(status),
		Detail: detailUnreadableReply,
	}
}

// statusVerdict is what the sink makes of a status outside 2xx. Exactly one of the two is set,
// and which one is the difference between a failure of the delivery and a failure of the
// records, which is the whole reason a batch too large for one request is still safe to split.
type statusVerdict struct {
	// fault is set when the delivery is what went wrong: the credential or the request itself
	// was refused (halt), or the sink could not take the batch just now (retry). Neither says
	// anything about a record, so nothing in the batch counts as delivered and the whole batch
	// goes again.
	fault *Fault
	// refused is set when the receiver refused what was sent. It is the Cause that each record
	// of that one request carries as its own Rejection: a refusal speaks for the records in the
	// request it answered and for nothing else in the batch.
	refused outbox.Cause
}

// statusOf is what the sink does about a status outside 2xx, and it is the whole of the rule.
// TestTheStatusTable walks it, status by status from 100 to 599.
//
// # Why an unrecognised 4xx halts
//
// Under the frozen protocol, a receiver that wants to refuse records answers 2xx with a
// "rejected" list. So the band that kills records on a 4xx only ever serves a receiver that does
// not follow the protocol, and whatever its default is, is what an unforeseen status does to a
// tenant's records. It used to be "kill every record of the request". That dead-lettered both
// records of a two record batch on 404, 405, 406, 410, 411, 421 and 431, permanently and
// quietly, with last_error reading "sink rejected the record (status NNN)": an endpoint typo
// destroyed a tenant's records, a receiver that takes only GET on that path did the same, and a
// 431 made the loss per-tenant, decided by the length of one tenant's bearer token in front of a
// proxy with a small header buffer. Nothing halted, so nothing told an operator to look.
//
// The default is now the other way, which is this project's fail-closed rule and the same trade
// already taken for the strict 2xx body: a non-conformant receiver stalls loudly instead of
// dead-lettering silently, and the records are still there when an operator has fixed it. It
// also covers every 4xx nobody has thought of, which an enumeration never can.
//
// # The refusal band
//
// 422 alone. It is the one status HTTP defines as a verdict on the content of the request rather
// than on the request message: the syntax is correct and the server could not process the
// instructions it carried (RFC 9110 section 15.5.21). Each of the nearest candidates was weighed
// and left out:
//
//   - 400 is a verdict on the request message, and a receiver refusing this protocol's version
//     answers it (ADR 13), so it would kill records for a mismatch no record caused.
//   - 409 is a conflict with the state of the target resource, which is the endpoint and not a
//     record. It names no record, and the per-record conflict this format can have, an id that
//     arrives again with different content, is reported by a conformant receiver in a
//     "rejected" list. A receiver that answers 409 to the re-send that idempotence on Record.ID
//     makes routine is therefore not conformant, so it gets the default below and halts, which
//     stalls and loses nothing: in the refusal band it would dead-letter records that in fact
//     landed, and in the retry band the ladder would dead-letter them at its end.
//   - 404, 405, 406, 410, 411, 415, 421 and 431 are all verdicts on the request target, the
//     method, the Accept header, the resource's existence, Content-Length, the media type, the
//     authority or the header fields. None of them is about a record.
//   - 413 and 414 are the request's size and URL, which an operator fixes with MaxRequestBytes
//     or with the endpoint.
//
// Even inside the band, a 422 kills every record of the request it answered, because which
// records share a request is this sink's byte arithmetic and not the receiver's choice. That is
// why the protocol asks a receiver to answer 2xx with a "rejected" list instead.
func statusOf(status int, code string) statusVerdict {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		// The credential was refused, so the ladder would only repeat it.
		return statusVerdict{fault: &Fault{
			Action: ActionHalt,
			Cause:  outbox.NewCause(outbox.ClassSinkUnauthorized).WithStatus(status).WithCode(code),
		}}
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests:
		// Both ask for the same bytes again later.
	case status == http.StatusUnprocessableEntity:
		// The whole refusal band: the receiver refused the content of the request, and the
		// same bytes come back the same way.
		return statusVerdict{
			refused: outbox.NewCause(outbox.ClassSinkRejected).WithStatus(status).WithCode(code),
		}
	case status >= 400 && status <= 499:
		// A verdict on the request this sink built, not on anything in it. An operator
		// changes a number, a credential, an endpoint or a receiver, and until then the
		// ladder would only send the same request again. Halting leaves the row prepared
		// and loses nothing.
		return statusVerdict{fault: &Fault{
			Action: ActionHalt,
			Cause:  outbox.NewCause(outbox.ClassSinkRefused).WithStatus(status).WithCode(code),
		}}
	}
	return statusVerdict{fault: &Fault{
		Action: ActionRetry,
		Cause:  outbox.NewCause(outbox.ClassSinkUnavailable).WithStatus(status).WithCode(code),
	}}
}

// refusals turns a receiver's refusal of one request into one Rejection per record that was in
// it. The Detail is empty, as it is for a record named in a "rejected" list, because what refused
// the record is the receiver and its own error code is already in the Cause.
func refusals(ids []string, cause outbox.Cause) []Rejection {
	rejections := make([]Rejection, 0, len(ids))
	for _, id := range ids {
		rejections = append(rejections, Rejection{ID: id, Cause: cause})
	}
	return rejections
}

// codeFrom reads the receiver's own error code out of an error response, as the one field
// documented for it. outbox.Cause.WithCode keeps it only if it looks like a code.
func codeFrom(answer []byte, readErr error) string {
	if readErr != nil || len(bytes.TrimSpace(answer)) == 0 {
		return ""
	}
	var reply struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(answer, &reply); err != nil {
		return ""
	}
	return reply.Code
}

// transportFault is the Fault for a request that never got an answer. Every one of them is
// retried: a sink that could not be reached is the retryable row of architecture section 11.
func transportFault(ctx context.Context, errs ...error) *Fault {
	detail := detailConnection
	switch {
	case ctx.Err() != nil:
		detail = detailCancelled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			detail = detailTimedOut
		}
	case len(errs) > 0:
		detail = transportDetail(errs[0])
	}
	return &Fault{
		Action: ActionRetry,
		Cause:  outbox.NewCause(outbox.ClassSinkUnavailable),
		Detail: detail,
	}
}

// transportDetail reduces a transport error to one of this package's own phrases.
//
// Every phrase it returns is a constant of this file, so what the error quotes does not travel
// with it, and net/http's error quotes the request URL. The last test reads the error's text,
// because a TLS handshake the other end refused arrives here as a plain error with nothing to
// assert a type on; what is read there chooses a phrase and is not kept.
func transportDetail(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return detailNoSuchHost
		}
		return detailConnection
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return detailTimedOut
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return detailRefused
	}
	var (
		certErr      *tls.CertificateVerificationError
		recordErr    tls.RecordHeaderError
		alertErr     tls.AlertError
		authorityErr x509.UnknownAuthorityError
		hostErr      x509.HostnameError
		invalidErr   x509.CertificateInvalidError
	)
	if errors.As(err, &certErr) || errors.As(err, &recordErr) || errors.As(err, &alertErr) ||
		errors.As(err, &authorityErr) || errors.As(err, &hostErr) || errors.As(err, &invalidErr) {
		return detailTLS
	}
	if strings.Contains(err.Error(), "tls: ") {
		return detailTLS
	}
	return detailConnection
}
