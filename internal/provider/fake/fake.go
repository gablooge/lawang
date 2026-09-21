// Package fake is a strict test double of a webhook provider. It exists so that the ingress edge,
// the hub and the pipeline can be tested end to end before a real provider lands, and so that
// tests of those packages do not each invent a provider of their own.
//
// It is a package and not a _test.go file because more than one package's tests import it. The
// lawang binary never does: nothing outside a test refers to it, and the provider registry is
// built from a list in main.
//
// It is strict on purpose (architecture principle 5): a lenient double let a whole class of
// mis-routed records pass every end-to-end test in the Python predecessor. So it refuses
// everything a real provider refuses. A body that is not valid UTF-8, is not JSON, carries a
// field it does not know, or is missing one it needs is an error, never a zero value. A missing
// secret, a missing signature, a signature that is not hex, one of the wrong length and one that
// does not match are all a plain false, in constant time. A challenge that is not short printable
// ASCII is answered with a 400 and never echoed.
//
// # The wire shapes
//
// A delivery is a JSON object. A handshake is either the Microsoft Graph shape, a validationToken
// in the query string echoed back as text/plain, or the Slack shape, a challenge in the body
// echoed back inside a JSON object. Both are here because the edge must not assume either.
//
//	{"type":"handshake","challenge":"abc"}
//	{"type":"event","workspace":"W1","subscription":"S1","events":[
//	  {"external_id":"fake:task:1","op":"upsert","version":"2",
//	   "container":"L1","title":"hello","occurred_at":"2026-09-20T10:00:00Z"}]}
//
// A signed delivery carries SignatureHeader: the hex of HMAC-SHA256 over the exact request bytes,
// which Sign computes.
package fake

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// DefaultKey is the provider key most tests build a fake under. It is a valid provider key
// under ADR 3, so the registry accepts it.
const DefaultKey = "fake"

// SignatureHeader carries the hex HMAC-SHA256 of the raw body.
const SignatureHeader = "X-Fake-Signature"

// ValidationTokenParam is the query parameter of the text/plain handshake, spelled as Microsoft
// Graph spells it.
const ValidationTokenParam = "validationToken"

// maxChallenge bounds what the handshake echoes. A challenge is text a stranger sent, and this is
// the whole reason the double refuses instead of echoing whatever arrives.
const maxChallenge = 256

// maxProviderID bounds the provider's own identifiers on a delivery.
const maxProviderID = 128

// ErrBadDelivery reports a body this provider would not have sent. It never quotes the body.
var ErrBadDelivery = errors.New("fake: bad delivery")

// ErrBadChange reports a Change that did not come from this provider's Parse.
var ErrBadChange = errors.New("fake: bad change")

// Provider is the double. Build one with New and register it like any other provider.
type Provider struct{ key string }

// New returns a fake provider whose Key is exactly key. The key is deliberately not validated or
// defaulted here: the registry owns that rule, and a test that proves the registry refuses a bad
// key needs a provider that offers one.
func New(key string) *Provider { return &Provider{key: key} }

// Key is the provider key.
func (p *Provider) Key() string { return p.key }

// Sign is the signature a delivery of body carries, as SignatureHeader. It is here and not in a
// test so that every test signs the same way Verify checks.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether r.Body is signed with secret. It never errors and never panics: every
// refusal is a plain false (architecture principle 1).
//
// This double signs the body alone, the way ClickUp does, so it ignores r.Method and r.URL. A
// double that signed the URL would have to refuse when r.URL is empty, which is what
// provider.Request documents; there is a test of that shape at the edge instead, because the
// point belongs to the edge and not to any one provider.
func (p *Provider) Verify(r provider.Request, secret []byte) bool {
	if len(secret) == 0 {
		return false // fail closed: a missing secret verifies nothing
	}
	sigs := r.Header.Values(SignatureHeader)
	if len(sigs) != 1 {
		// Two signature headers are two claims, and picking one is how a smuggled second value
		// gets its chance.
		return false
	}
	// hmac.Equal already refuses a digest of the wrong length, so the length test here changes no
	// answer: it is the shape of the refusal written down, and no test can tell it from its
	// absence.
	want, err := hex.DecodeString(sigs[0])
	if err != nil || len(want) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(r.Body)
	return hmac.Equal(mac.Sum(nil), want)
}

// Handshake answers the two challenge shapes this double knows, and reports whether it did.
//
// It reads body, never r.Body, which the edge has already replaced with http.NoBody.
func (p *Provider) Handshake(r *http.Request, body []byte) (provider.Reply, bool) {
	if q := r.URL.Query(); q.Has(ValidationTokenParam) {
		tokens := q[ValidationTokenParam]
		if len(tokens) != 1 || !printableASCII(tokens[0]) || len(tokens[0]) > maxChallenge || tokens[0] == "" {
			return badChallenge(), true
		}
		return provider.Reply{
			ContentType: "text/plain; charset=utf-8",
			Body:        []byte(tokens[0]),
		}, true
	}

	var hs struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if err := decodeStrict(body, &hs); err != nil || hs.Type != "handshake" {
		return provider.Reply{}, false // not a handshake, and not this method's business
	}
	if hs.Challenge == "" || len(hs.Challenge) > maxChallenge || !printableASCII(hs.Challenge) {
		return badChallenge(), true
	}
	// Built with encoding/json, so the challenge is escaped and cannot break out of the string.
	reply, err := json.Marshal(struct {
		Challenge string `json:"challenge"`
	}{hs.Challenge})
	if err != nil {
		return badChallenge(), true
	}
	return provider.Reply{ContentType: "application/json", Body: reply}, true
}

func badChallenge() provider.Reply {
	return provider.Reply{Status: http.StatusBadRequest, Body: []byte("bad challenge\n")}
}

// DeliveryKeys reads the workspace and subscription ids out of a delivery.
func (p *Provider) DeliveryKeys(body []byte, _ provider.Header) (provider.DeliveryKeys, error) {
	env, err := parseEnvelope(body)
	if err != nil {
		return provider.DeliveryKeys{}, err
	}
	return provider.DeliveryKeys{Workspace: env.Workspace, Subscription: env.Subscription}, nil
}

// Parse turns a delivery into its changes.
func (p *Provider) Parse(body []byte) ([]provider.Change, error) {
	env, err := parseEnvelope(body)
	if err != nil {
		return nil, err
	}
	if len(env.Events) == 0 {
		return nil, fmt.Errorf("%w: no events", ErrBadDelivery)
	}
	changes := make([]provider.Change, 0, len(env.Events))
	for _, raw := range env.Events {
		ev, err := p.parseEvent(raw)
		if err != nil {
			return nil, err
		}
		changes = append(changes, provider.Change{
			ExternalID: ev.ExternalID,
			Op:         record.Op(ev.Op),
			// The event's own bytes, not a re-serialization: a degraded record is built out of
			// this, and a provider's bytes are the only thing that is certainly faithful.
			Payload: bytes.Clone(raw),
		})
	}
	return changes, nil
}

// Object is what Hydrate returns: this double's stand-in for the full object a real provider
// would have fetched from its API.
type Object struct {
	Event Event
	// Text is what hydration added to what the webhook carried. A real provider fetches it.
	Text string
}

// Hydrate returns the object a Change is about. There is no API to call, so it reads the change's
// own payload back and adds the one field a webhook body would not have carried.
func (p *Provider) Hydrate(ctx context.Context, t tenancy.ID, c provider.Change) (provider.Hydrated, error) {
	if err := ctx.Err(); err != nil {
		return nil, err // a real client would fail here, so the double does too
	}
	if t == "" {
		return nil, fmt.Errorf("%w: no tenant", ErrBadChange) // fail closed
	}
	ev, err := p.parseEvent(c.Payload)
	if err != nil {
		return nil, err
	}
	if ev.ExternalID != c.ExternalID {
		return nil, fmt.Errorf("%w: the payload is not this change's", ErrBadChange)
	}
	return Object{Event: ev, Text: "hydrated " + ev.ExternalID}, nil
}

// Normalize turns a hydrated object into one unsealed record.
func (p *Provider) Normalize(h provider.Hydrated, c provider.Change) ([]record.Record, error) {
	obj, ok := h.(Object)
	if !ok {
		return nil, fmt.Errorf("%w: not a fake.Object", ErrBadChange)
	}
	if obj.Event.ExternalID != c.ExternalID {
		return nil, fmt.Errorf("%w: the object is not this change's", ErrBadChange)
	}
	scope, err := record.ScopeID(p.key, "list", obj.Event.Container)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadChange, err)
	}
	return []record.Record{{
		Op:         record.Op(obj.Event.Op),
		Kind:       record.KindTask,
		ExternalID: obj.Event.ExternalID,
		Version:    obj.Event.Version,
		OccurredAt: obj.Event.OccurredAt,
		Title:      obj.Event.Title,
		Text:       obj.Text,
		Container:  record.Container{Kind: "list", ID: obj.Event.Container},
		Visibility: record.Visibility{Scope: scope, Audience: record.AudienceGroup},
	}}, nil
}

// Event is one change on the wire, as this double spells it.
type Event struct {
	ExternalID string    `json:"external_id"`
	Op         string    `json:"op"`
	Version    string    `json:"version"`
	Container  string    `json:"container"`
	Title      string    `json:"title"`
	OccurredAt time.Time `json:"occurred_at"`
}

type envelope struct {
	Type         string            `json:"type"`
	Workspace    string            `json:"workspace"`
	Subscription string            `json:"subscription"`
	Events       []json.RawMessage `json:"events"`
}

// parseEnvelope decodes a delivery and refuses everything this provider would not have sent.
func parseEnvelope(body []byte) (envelope, error) {
	var env envelope
	if err := decodeStrict(body, &env); err != nil {
		return envelope{}, err
	}
	if env.Type != "event" {
		return envelope{}, fmt.Errorf("%w: not an event", ErrBadDelivery)
	}
	if !validProviderID(env.Workspace) {
		return envelope{}, fmt.Errorf("%w: workspace", ErrBadDelivery)
	}
	// The subscription id is optional, as it is for the providers that send none.
	if env.Subscription != "" && !validProviderID(env.Subscription) {
		return envelope{}, fmt.Errorf("%w: subscription", ErrBadDelivery)
	}
	return env, nil
}

func (p *Provider) parseEvent(raw []byte) (Event, error) {
	var ev Event
	if err := decodeStrict(raw, &ev); err != nil {
		return Event{}, err
	}
	switch record.Op(ev.Op) {
	case record.OpUpsert, record.OpDelete:
	default:
		return Event{}, fmt.Errorf("%w: op", ErrBadDelivery)
	}
	// The external id carries the provider key, as the record format requires, so a normalizer
	// never has to add it and Seal never has to refuse it.
	if !bytes.HasPrefix([]byte(ev.ExternalID), []byte(p.key+":")) || len(ev.ExternalID) > maxProviderID {
		return Event{}, fmt.Errorf("%w: external_id", ErrBadDelivery)
	}
	if !validProviderID(ev.Version) || !validProviderID(ev.Container) {
		return Event{}, fmt.Errorf("%w: version or container", ErrBadDelivery)
	}
	if ev.OccurredAt.IsZero() {
		return Event{}, fmt.Errorf("%w: occurred_at", ErrBadDelivery)
	}
	ev.OccurredAt = ev.OccurredAt.UTC() // the record format refuses any other location
	return ev, nil
}

// decodeStrict refuses a body that is not exactly one JSON object of the expected shape. Three
// refusals matter and none of them is the default: bytes that are not UTF-8 (encoding/json would
// quietly rewrite them to U+FFFD and make two different deliveries one), a field the shape does
// not know, and anything after the object.
func decodeStrict(body []byte, into any) error {
	if !utf8.Valid(body) {
		return fmt.Errorf("%w: not UTF-8", ErrBadDelivery)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		// The decoder's message can quote the body, which is a stranger's text.
		return fmt.Errorf("%w: not the expected JSON", ErrBadDelivery)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data", ErrBadDelivery)
	}
	return nil
}

// validProviderID is what this double accepts as one of the provider's own identifiers: 1 to
// maxProviderID printable ASCII characters. A real provider's ids are narrower still.
func validProviderID(s string) bool {
	return s != "" && len(s) <= maxProviderID && printableASCII(s)
}

// printableASCII reports whether s is only visible ASCII and spaces: no control character, no
// newline (which forges a log line), no NUL (which truncates in C and cannot be stored in a
// Postgres text column), and nothing above ASCII.
func printableASCII(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}
