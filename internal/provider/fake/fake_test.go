package fake_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

const secret = "s3cr3t-for-this-test-only"

// event is deliberately spelled so that re-serializing it produces different bytes: the fields
// are not in the order the Go struct declares them, and there is whitespace inside. A fixture
// that happened to marshal back to itself would let a provider that re-serialized its payload
// pass every test here.
const event = `{"op":"upsert", "external_id":"fake:task:1",  "container":"L1","version":"2",` +
	`"title":"hello","occurred_at":"2026-09-20T10:00:00Z"}`

func delivery() string {
	return `{"type":"event","workspace":"W1","subscription":"S1","events":[` + event + `]}`
}

// request is a delivery as the edge hands it to Verify. The method and the URL are filled in with
// something a real request would carry, so that a Verify which started reading them would not
// silently see zero values here.
func request(body []byte, h http.Header) provider.Request {
	return provider.Request{
		Method: http.MethodPost,
		URL:    "https://lawang.example.test/ingress/" + fake.DefaultKey,
		Header: h,
		Body:   body,
	}
}

func signed(t *testing.T, body string) http.Header {
	t.Helper()
	h := http.Header{}
	h.Set(fake.SignatureHeader, fake.Sign([]byte(secret), []byte(body)))
	return h
}

// TestVerifyRefusesEverythingARealProviderRefuses is architecture principle 5 for the one method
// where a lenient double would hide a real hole: every refusal is a plain false, and a missing
// secret is a refusal and never a pass.
func TestVerifyRefusesEverythingARealProviderRefuses(t *testing.T) {
	t.Parallel()

	p := fake.New(fake.DefaultKey)
	body := []byte(delivery())
	good := fake.Sign([]byte(secret), body)

	if !p.Verify(request(body, signed(t, string(body))), []byte(secret)) {
		t.Fatal("a correctly signed delivery must verify")
	}

	twoSigs := http.Header{}
	twoSigs.Add(fake.SignatureHeader, good)
	twoSigs.Add(fake.SignatureHeader, good)

	cases := []struct {
		name   string
		body   []byte
		header http.Header
		secret []byte
	}{
		{"no secret", body, signed(t, string(body)), nil},
		{"empty secret", body, signed(t, string(body)), []byte{}},
		// The one an absent check would let through: a sender who knows the secret is missing
		// can compute the HMAC of an empty key themselves.
		{"signed with the empty secret", body, headerWith(fake.Sign(nil, body)), nil},
		{"signed with the empty secret, empty given", body, headerWith(fake.Sign([]byte{}, body)), []byte{}},
		{"no signature", body, http.Header{}, []byte(secret)},
		{"two signatures", body, twoSigs, []byte(secret)},
		{"signature is not hex", body, headerWith("nothex" + strings.Repeat("z", 58)), []byte(secret)},
		{"signature is short", body, headerWith(good[:len(good)-2]), []byte(secret)},
		{"signature is long", body, headerWith(good + "ab"), []byte(secret)},
		{"signature is empty", body, headerWith(""), []byte(secret)},
		{"wrong secret", body, signed(t, string(body)), []byte("another secret entirely")},
		{"body changed by one byte", append([]byte{' '}, body...), signed(t, string(body)), []byte(secret)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if p.Verify(request(c.body, c.header), c.secret) {
				t.Fatal("verified something it must refuse")
			}
		})
	}
}

func headerWith(sig string) http.Header {
	h := http.Header{}
	h.Set(fake.SignatureHeader, sig)
	return h
}

// TestVerifyIsOverTheExactBytes is principle 1. A re-serialization of the same JSON is a different
// byte string, and must not verify.
func TestVerifyIsOverTheExactBytes(t *testing.T) {
	t.Parallel()

	p := fake.New(fake.DefaultKey)
	raw := []byte(" {\"type\":\"event\",  \"workspace\":\"W1\",\n\"events\":[" + event + "]} ")

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("the fixture must be valid JSON: %v", err)
	}
	reserialized, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(reserialized) == string(raw) {
		t.Fatal("the fixture must re-serialize differently, or it proves nothing")
	}

	h := headerWith(fake.Sign([]byte(secret), raw))
	if !p.Verify(request(raw, h), []byte(secret)) {
		t.Fatal("the raw bytes must verify")
	}
	if p.Verify(request(reserialized, h), []byte(secret)) {
		t.Fatal("re-serialized JSON must not verify against a signature over the raw bytes")
	}
}

// TestVerifyIgnoresWhatThisSchemeDoesNotSign holds the double to what it says it is. It signs the
// body alone, like ClickUp, so a different method and an empty URL (which is what
// provider.Request carries when no public base URL is configured) must change no answer. A double
// that quietly started reading them would make the edge's fail-closed behaviour untestable.
func TestVerifyIgnoresWhatThisSchemeDoesNotSign(t *testing.T) {
	t.Parallel()

	p := fake.New(fake.DefaultKey)
	body := []byte(delivery())
	h := headerWith(fake.Sign([]byte(secret), body))

	for _, r := range []provider.Request{
		{Method: http.MethodPost, URL: "", Header: h, Body: body},
		{Method: "PUT", URL: "https://elsewhere.example.test/ingress/fake?x=1", Header: h, Body: body},
		{Method: "", URL: "", Header: h, Body: body},
	} {
		if !p.Verify(r, []byte(secret)) {
			t.Fatalf("a body-only scheme refused over method %q and URL %q", r.Method, r.URL)
		}
	}
}

func TestHandshakeAnswersBothShapesAndIsNotJSONOnly(t *testing.T) {
	t.Parallel()

	p := fake.New(fake.DefaultKey)

	t.Run("text echo in the query", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/ingress/fake?validationToken=abc123", nil)
		reply, ok := p.Handshake(r, nil)
		if !ok {
			t.Fatal("a validationToken is a handshake")
		}
		if got := string(reply.Body); got != "abc123" {
			t.Fatalf("body = %q", got)
		}
		if reply.ContentType != "text/plain; charset=utf-8" {
			t.Fatalf("content type = %q, a handshake reply is not always JSON", reply.ContentType)
		}
	})

	t.Run("json challenge in the body", func(t *testing.T) {
		body := []byte(`{"type":"handshake","challenge":"xyz"}`)
		r := httptest.NewRequest(http.MethodPost, "/ingress/fake", nil)
		reply, ok := p.Handshake(r, body)
		if !ok {
			t.Fatal("a challenge body is a handshake")
		}
		if got := string(reply.Body); got != `{"challenge":"xyz"}` {
			t.Fatalf("body = %q", got)
		}
		if reply.ContentType != "application/json" {
			t.Fatalf("content type = %q", reply.ContentType)
		}
	})

	t.Run("an ordinary delivery is not a handshake", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/ingress/fake", nil)
		if _, ok := p.Handshake(r, []byte(delivery())); ok {
			t.Fatal("an event body must not be taken for a handshake")
		}
	})
}

// TestHandshakeNeverEchoesWhatItShouldNotEcho keeps the one place that writes a stranger's text
// back out honest: a challenge that is not short printable ASCII is answered with a 400 and the
// text never appears in the reply.
func TestHandshakeNeverEchoesWhatItShouldNotEcho(t *testing.T) {
	t.Parallel()

	p := fake.New(fake.DefaultKey)
	long := strings.Repeat("a", 257)
	cases := []struct {
		name  string
		query string
		body  string
	}{
		{"empty token", "?validationToken=", ""},
		{"token with a newline", "?validationToken=a%0Ab", ""},
		{"token with a NUL", "?validationToken=a%00b", ""},
		{"token above ASCII", "?validationToken=caf%C3%A9", ""},
		{"token too long", "?validationToken=" + long, ""},
		{"two tokens", "?validationToken=a&validationToken=b", ""},
		{"challenge with a control character", "", `{"type":"handshake","challenge":"a\u0000b"}`},
		{"challenge too long", "", `{"type":"handshake","challenge":"` + long + `"}`},
		{"empty challenge", "", `{"type":"handshake","challenge":""}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/ingress/fake"+c.query, nil)
			reply, ok := p.Handshake(r, []byte(c.body))
			if !ok {
				t.Fatal("a malformed challenge is still a handshake, answered with a refusal")
			}
			if reply.Status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", reply.Status)
			}
			if string(reply.Body) != "bad challenge\n" {
				t.Fatalf("the reply echoed something: %q", reply.Body)
			}
		})
	}
}

// TestADeliveryThisProviderWouldNotHaveSentIsAnError is the strictness rule: never a zero value,
// always a refusal.
func TestADeliveryThisProviderWouldNotHaveSentIsAnError(t *testing.T) {
	t.Parallel()

	p := fake.New(fake.DefaultKey)
	cases := map[string]string{
		"empty":                     "",
		"not JSON":                  "not json at all",
		"not an object":             `[1,2,3]`,
		"trailing data":             delivery() + `{"type":"event"}`,
		"unknown field":             `{"type":"event","workspace":"W1","extra":1,"events":[]}`,
		"wrong type":                `{"type":"handshake","workspace":"W1","events":[]}`,
		"no workspace":              `{"type":"event","events":[]}`,
		"workspace with a NUL":      `{"type":"event","workspace":"a\u0000b","events":[]}`,
		"workspace too long":        `{"type":"event","workspace":"` + strings.Repeat("w", 129) + `","events":[]}`,
		"subscription with a NUL":   `{"type":"event","workspace":"W1","subscription":"a\u0000b","events":[]}`,
		"events is not an array":    `{"type":"event","workspace":"W1","events":{}}`,
		"workspace is not a string": `{"type":"event","workspace":1,"events":[]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := p.DeliveryKeys([]byte(body), nil); err == nil {
				t.Fatal("DeliveryKeys accepted it")
			}
			if _, err := p.Parse([]byte(body)); err == nil {
				t.Fatal("Parse accepted it")
			}
		})
	}
}

// TestBytesThatAreNotUTF8AreRefusedBeforeTheDecoder covers the one refusal in decodeStrict that
// encoding/json does not make on its own, and that the first version of this test did not reach:
// it sent {0xff,0xfe,'}'}, which is not JSON at all, so the decoder refused it whether or not
// utf8.Valid was there, and the check survived its own mutation.
//
// Go's JSON scanner accepts any byte above 0x1f inside a string, and the decoder then rewrites
// whatever is not valid UTF-8 to U+FFFD. Two deliveries that differ in one byte therefore become
// one parsed value while their delivery ids (which hash the raw bytes) differ, which is exactly
// the "two different deliveries become one" that this double exists to refuse.
//
// Every case below except the last is valid JSON, asserted here, so utf8.Valid is the only thing
// in the package that can refuse it. The bad byte sits in title, the one field parseEvent does
// not otherwise constrain: workspace, subscription, version and container are printable ASCII
// only, so a bad byte in those is refused twice over and would prove nothing.
func TestBytesThatAreNotUTF8AreRefusedBeforeTheDecoder(t *testing.T) {
	t.Parallel()

	p := fake.New(fake.DefaultKey)
	withTitle := func(title string) []byte {
		return []byte(`{"type":"event","workspace":"W1","subscription":"S1","events":[` +
			`{"op":"upsert","external_id":"fake:task:1","container":"L1","version":"2",` +
			`"title":"` + title + `","occurred_at":"2026-09-20T10:00:00Z"}]}`)
	}
	cases := map[string]struct {
		body      []byte
		validJSON bool
	}{
		"a lone 0xff in a string":           {withTitle("x\xff"), true},
		"a lone 0xfe in a string":           {withTitle("x\xfe"), true},
		"a truncated two byte sequence":     {withTitle("x\xc3("), true},
		"a bare continuation byte":          {withTitle("x\x80"), true},
		"a surrogate half encoded as UTF-8": {withTitle("x\xed\xa0\x80"), true},
		"an overlong encoding of a slash":   {withTitle("x\xc0\xaf"), true},
		"not JSON either":                   {[]byte{'{', 0xff, 0xfe, '}'}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if json.Valid(tc.body) != tc.validJSON {
				t.Fatalf("json.Valid = %v, want %v: this fixture no longer isolates the UTF-8 check",
					json.Valid(tc.body), tc.validJSON)
			}
			if _, err := p.Parse(tc.body); err == nil {
				t.Fatal("Parse accepted bytes that are not UTF-8")
			}
			if _, err := p.DeliveryKeys(tc.body, nil); err == nil {
				t.Fatal("DeliveryKeys accepted bytes that are not UTF-8")
			}
		})
	}

	// The same delivery with a valid title is accepted, so the cases above are not passing
	// against a Parse that refuses everything.
	if _, err := p.Parse(withTitle("x?")); err != nil {
		t.Fatalf("the same delivery with a valid title was refused: %v", err)
	}
}

func TestParseRefusesAnEventThisProviderWouldNotHaveSent(t *testing.T) {
	t.Parallel()

	p := fake.New(fake.DefaultKey)
	with := func(ev string) string {
		return `{"type":"event","workspace":"W1","events":[` + ev + `]}`
	}
	cases := map[string]string{
		"no events":              `{"type":"event","workspace":"W1","events":[]}`,
		"unknown op":             with(`{"external_id":"fake:task:1","op":"patch","version":"2","container":"L1","occurred_at":"2026-09-20T10:00:00Z"}`),
		"external id unprefixed": with(`{"external_id":"task:1","op":"upsert","version":"2","container":"L1","occurred_at":"2026-09-20T10:00:00Z"}`),
		"another provider's id":  with(`{"external_id":"slack:task:1","op":"upsert","version":"2","container":"L1","occurred_at":"2026-09-20T10:00:00Z"}`),
		"no version":             with(`{"external_id":"fake:task:1","op":"upsert","container":"L1","occurred_at":"2026-09-20T10:00:00Z"}`),
		"no container":           with(`{"external_id":"fake:task:1","op":"upsert","version":"2","occurred_at":"2026-09-20T10:00:00Z"}`),
		"no occurred_at":         with(`{"external_id":"fake:task:1","op":"upsert","version":"2","container":"L1"}`),
		"unknown event field":    with(`{"external_id":"fake:task:1","op":"upsert","version":"2","container":"L1","occurred_at":"2026-09-20T10:00:00Z","extra":1}`),
		// The prefix is right and everything else is valid, so the length bound is the only thing
		// that can refuse this. Without the case, dropping the bound changed nothing in the suite.
		"external id too long": with(`{"external_id":"fake:` + strings.Repeat("x", 129) +
			`","op":"upsert","version":"2","container":"L1","occurred_at":"2026-09-20T10:00:00Z"}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := p.Parse([]byte(body)); err == nil {
				t.Fatal("Parse accepted it")
			}
		})
	}
}

// TestAnOffsetTimeIsNormalizedToUTC pins the one line of parseEvent that changes a value rather
// than refusing one. encoding/json keeps the offset a delivery was written with, and the record
// format refuses any location but UTC, so without the conversion every delivery from a provider
// that writes local time would fail far from here, in Seal. Every occurred_at fixture elsewhere in
// this package is already spelled with a Z, so nothing else in the suite can see the difference.
func TestAnOffsetTimeIsNormalizedToUTC(t *testing.T) {
	t.Parallel()

	p := fake.New(fake.DefaultKey)
	const offset = `{"type":"event","workspace":"W1","events":[` +
		`{"external_id":"fake:task:1","op":"upsert","version":"2","container":"L1",` +
		`"title":"hello","occurred_at":"2026-09-20T17:00:00+07:00"}]}`

	changes, err := p.Parse([]byte(offset))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	obj, err := p.Hydrate(t.Context(), "t_1", changes[0])
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	recs, err := p.Normalize(obj, changes[0])
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if loc := recs[0].OccurredAt.Location(); loc != time.UTC {
		t.Fatalf("OccurredAt location = %v, want UTC", loc)
	}
	if want := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC); !recs[0].OccurredAt.Equal(want) {
		t.Fatalf("OccurredAt = %v, want %v", recs[0].OccurredAt, want)
	}
}

// TestThePipelineRoundTripProducesASealableRecord walks the provider interface end to end, so the
// double is known to produce something the record format accepts and not only something that
// compiles.
func TestThePipelineRoundTripProducesASealableRecord(t *testing.T) {
	t.Parallel()

	p := fake.New(fake.DefaultKey)
	body := []byte(delivery())

	keys, err := p.DeliveryKeys(body, nil)
	if err != nil {
		t.Fatalf("DeliveryKeys: %v", err)
	}
	if keys.Workspace != "W1" || keys.Subscription != "S1" {
		t.Fatalf("keys = %+v", keys)
	}

	changes, err := p.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(changes) != 1 || changes[0].ExternalID != "fake:task:1" || changes[0].Op != record.OpUpsert {
		t.Fatalf("changes = %+v", changes)
	}
	if string(changes[0].Payload) != event {
		t.Fatalf("the payload is not the provider's own bytes:\n got %s\nwant %s", changes[0].Payload, event)
	}

	tenant, err := tenancy.Parse("acme")
	if err != nil {
		t.Fatalf("tenancy.Parse: %v", err)
	}
	hydrated, err := p.Hydrate(t.Context(), tenant, changes[0])
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	recs, err := p.Normalize(hydrated, changes[0])
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want one record, got %d", len(recs))
	}
	sealed, err := recs[0].Seal(p.Key(), tenant)
	if err != nil {
		t.Fatalf("the double must produce a record the format accepts: %v", err)
	}
	if _, err := json.Marshal(sealed); err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if sealed.Visibility.Scope != "fake:list:L1" {
		t.Fatalf("scope = %q", sealed.Visibility.Scope)
	}
}

func TestHydrateFailsClosed(t *testing.T) {
	t.Parallel()

	p := fake.New(fake.DefaultKey)
	changes, err := p.Parse([]byte(delivery()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tenant, err := tenancy.Parse("acme")
	if err != nil {
		t.Fatalf("tenancy.Parse: %v", err)
	}

	if _, err := p.Hydrate(t.Context(), "", changes[0]); err == nil {
		t.Fatal("no tenant is a refusal, never a default")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.Hydrate(cancelled, tenant, changes[0]); err == nil {
		t.Fatal("a cancelled context must fail, as a real API client would")
	}
	other := provider.Change{ExternalID: "fake:task:99", Payload: changes[0].Payload}
	if _, err := p.Hydrate(t.Context(), tenant, other); err == nil {
		t.Fatal("a payload that belongs to another change must be refused")
	}
	if _, err := p.Hydrate(t.Context(), tenant, provider.Change{ExternalID: "fake:task:1", Payload: []byte("{")}); err == nil {
		t.Fatal("a payload that is not an event must be refused")
	}
	if _, err := p.Normalize("not an object", changes[0]); err == nil {
		t.Fatal("Normalize must refuse something it did not hydrate")
	}
}

// TestNormalizeRefusesAnObjectItCannotTurnIntoARecord: a scope id it cannot build, and an object
// that belongs to another change, are both refusals and never a record with a guessed field.
func TestNormalizeRefusesAnObjectItCannotTurnIntoARecord(t *testing.T) {
	t.Parallel()

	p := fake.New(fake.DefaultKey)
	changes, err := p.Parse([]byte(delivery()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tenant, err := tenancy.Parse("acme")
	if err != nil {
		t.Fatalf("tenancy.Parse: %v", err)
	}
	hydrated, err := p.Hydrate(t.Context(), tenant, changes[0])
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	other := provider.Change{ExternalID: "fake:task:99"}
	if _, err := p.Normalize(hydrated, other); err == nil {
		t.Fatal("an object from another change must be refused")
	}

	// A provider key the scope id grammar refuses (ADR 3) cannot produce a scope, so the record
	// is refused here rather than later in Seal.
	hyphened := fake.New("ms-graph")
	body := strings.ReplaceAll(delivery(), "fake:task:1", "ms-graph:task:1")
	hyphenedChanges, err := hyphened.Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	hydrated, err = hyphened.Hydrate(t.Context(), tenant, hyphenedChanges[0])
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if _, err := hyphened.Normalize(hydrated, hyphenedChanges[0]); err == nil {
		t.Fatal("a provider key the scope id grammar refuses must not produce a record")
	}
}
