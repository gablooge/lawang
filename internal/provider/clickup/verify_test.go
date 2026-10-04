package clickup_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/clickup"
)

// testSecret is a constant of this test file and of nothing else. It is not a ClickUp secret, it
// never was one, and no file in testdata carries it.
var testSecret = []byte("not-a-real-clickup-secret")

// sign is what ClickUp does: the hex of HMAC-SHA256 over the exact request bytes, with the
// webhook's secret (https://developer.clickup.com/docs/webhooksignature). It is written here,
// independently of the package, so that a change to the package's own verification cannot make
// the tests sign the new way too.
func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func signedRequest(t *testing.T, body []byte, sigs ...string) provider.Request {
	t.Helper()
	h := http.Header{}
	for _, s := range sigs {
		h.Add(clickup.SignatureHeader, s)
	}
	return provider.Request{Method: http.MethodPost, Header: provider.NewHeader(h), Body: body}
}

// The documentation's own example verifies. This is the premise beside the input: without it,
// every negative case below could be passing because the scheme is wrong rather than because the
// delivery is bad.
func TestVerifyAcceptsTheDocumentedExample(t *testing.T) {
	// The body and the key of the example at https://developer.clickup.com/docs/webhooksignature.
	key := []byte("secret")
	body := []byte(`{"webhook_id":"7689a169-a000-4985-8676-6902b96d6627","event":"taskCreated","task_id":"c0j"}`)
	want := sign(key, body)
	if !newProvider(t).Verify(signedRequest(t, body, want), key) {
		t.Fatal("the documented example does not verify")
	}
}

// Every way a delivery can fail to be signed is a plain false, never an error and never a panic
// (architecture principle 1, and the contract on provider.WebhookSource.Verify).
func TestVerifyRefuses(t *testing.T) {
	p := newProvider(t)
	body := []byte(`{"event":"taskUpdated","task_id":"86a1b2","webhook_id":"w1"}`)
	good := sign(testSecret, body)

	// The acceptances beside the refusals, so that a Verify which always answered false would
	// fail this test rather than pass it. The uppercase one is deliberate: ClickUp digests in
	// lowercase, and the same digest spelled in uppercase is the same bytes, so refusing it
	// would reject a signature that proves knowledge of the secret and buys nothing.
	for _, sig := range []string{good, strings.ToUpper(good)} {
		if !p.Verify(signedRequest(t, body, sig), testSecret) {
			t.Fatalf("a correctly signed delivery does not verify: %s", sig)
		}
	}

	other := sign([]byte("another-secret"), body)
	for _, tc := range []struct {
		name   string
		req    provider.Request
		secret []byte
	}{
		{"no secret", signedRequest(t, body, good), nil},
		{"empty secret", signedRequest(t, body, good), []byte{}},
		// The case the "no secret" guard is actually for. HMAC with an empty key is a perfectly
		// good MAC, so without the guard a row whose secret column was somehow empty would
		// verify anything a sender signed with the empty key, and the delivery would be routed
		// into that row's tenant.
		{"a signature made with the empty secret", signedRequest(t, body, sign(nil, body)), nil},
		{"a signature made with the empty secret, empty secret", signedRequest(t, body, sign([]byte{}, body)), []byte{}},
		{"no signature header", signedRequest(t, body), testSecret},
		{"two signature headers", signedRequest(t, body, good, good), testSecret},
		{"two headers, one right", signedRequest(t, body, other, good), testSecret},
		{"empty signature", signedRequest(t, body, ""), testSecret},
		{"not hex", signedRequest(t, body, strings.Repeat("z", 64)), testSecret},
		{"odd length hex", signedRequest(t, body, good[:63]), testSecret},
		{"short digest", signedRequest(t, body, good[:62]), testSecret},
		{"long digest", signedRequest(t, body, good+"00"), testSecret},
		{"another secret's signature", signedRequest(t, body, other), testSecret},
		{"one byte changed in the body", signedRequest(t, append([]byte{' '}, body...), good), testSecret},
		{"no body", signedRequest(t, nil, good), testSecret},
		{"signature of the empty body", signedRequest(t, body, sign(testSecret, nil)), testSecret},
	} {
		if p.Verify(tc.req, tc.secret) {
			t.Errorf("%s: verified", tc.name)
		}
	}
}

// ClickUp signs the body alone, so a delivery verifies whatever the deployment's public URL is.
// A scheme that quietly started reading Request.URL would break every deployment that configures
// none, and the symptom would be a 401 visible only on ClickUp's own dashboard.
func TestVerifyIgnoresTheURLAndTheMethod(t *testing.T) {
	p := newProvider(t)
	body := []byte(`{"event":"taskUpdated","task_id":"86a1b2","webhook_id":"w1"}`)
	req := signedRequest(t, body, sign(testSecret, body))
	for _, url := range []string{"", "https://example.invalid/ingress/clickup", "nonsense"} {
		req.URL = url
		if !p.Verify(req, testSecret) {
			t.Errorf("URL %q: did not verify", url)
		}
	}
	// The marker interface that says a scheme covers the URL. ClickUp must not carry it: hub.New
	// refuses to start when a URL-signing provider is registered with no public base URL.
	if _, ok := any(p).(provider.URLSigner); ok {
		t.Error("the ClickUp provider claims to sign the public URL")
	}
}

// Verify is handed the same Request once per candidate subscription, and the bytes it is handed
// are the bytes the outbox stores. An implementation that normalized them in place would change
// what every later candidate verifies.
func TestVerifyDoesNotTouchWhatItIsHanded(t *testing.T) {
	p := newProvider(t)
	body := []byte("  {\"event\":\"taskUpdated\",\"task_id\":\"86a1b2\",\"webhook_id\":\"w1\"}\n")
	before := string(body)
	req := signedRequest(t, body, sign(testSecret, body))
	if !p.Verify(req, testSecret) {
		t.Fatal("a signed delivery with surrounding whitespace did not verify, so the bytes were not taken as they are")
	}
	if string(body) != before {
		t.Errorf("Verify changed the body it was handed: %q", body)
	}
	if got := req.Header.Get(clickup.SignatureHeader); got == "" {
		t.Error("Verify removed the signature header")
	}
}

// Verification does not parse. A body ClickUp would never send still verifies if it is signed,
// because a signature check that depended on the content would refuse with 401 (which the status
// table reserves for a forged signature) what is really poison to park.
func TestVerifyDoesNotParse(t *testing.T) {
	p := newProvider(t)
	for _, body := range [][]byte{
		[]byte("not json at all"),
		{0xff, 0xfe, 0x00},
		[]byte("{"),
		{},
	} {
		if !p.Verify(signedRequest(t, body, sign(testSecret, body)), testSecret) {
			t.Errorf("a signed body of %d bytes did not verify", len(body))
		}
	}
}

// ClickUp sends no challenge, so Handshake always says "this is not a handshake" and the
// delivery goes on down the accept path. It must never answer a body a stranger sent.
func TestHandshakeAnswersNothing(t *testing.T) {
	p := newProvider(t)
	r, err := http.NewRequest(http.MethodPost, "https://example.invalid/ingress/clickup?validationToken=abc", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range [][]byte{
		nil,
		[]byte(`{"challenge":"abc"}`),
		[]byte(`{"type":"url_verification","challenge":"abc"}`),
		[]byte("<script>alert(1)</script>"),
	} {
		reply, ok := p.Handshake(r, body)
		if ok {
			t.Errorf("answered a handshake for %q", body)
		}
		if reply.Status != 0 || reply.Body != nil || reply.ContentType != "" {
			t.Errorf("answered with %+v", reply)
		}
	}
}
