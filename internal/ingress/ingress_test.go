package ingress_test

import (
	"bufio"
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gablooge/lawang/internal/ingress"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

const secret = "the-subscription-secret-for-this-test"

// event and delivery are the fake provider's wire shapes, written out here so that a test can
// change one byte of them.
const event = `{"external_id":"fake:task:1","op":"upsert","version":"2","container":"L1",` +
	`"title":"hello","occurred_at":"2026-09-20T10:00:00Z"}`

const delivery = `{"type":"event","workspace":"W1","subscription":"S1","events":[` + event + `]}`

// hubCall is what the edge handed the hub.
type hubCall struct {
	key    string
	body   []byte
	header http.Header
}

// testHub records every call and answers with fn, or with Stored when fn is nil.
type testHub struct {
	mu    sync.Mutex
	calls []hubCall
	fn    func(ctx context.Context, p provider.Entry, body []byte, h http.Header) (ingress.Verdict, error)
}

func (h *testHub) Accept(ctx context.Context, p provider.Entry, body []byte, hdr http.Header) (ingress.Verdict, error) {
	h.mu.Lock()
	h.calls = append(h.calls, hubCall{key: p.Key(), body: body, header: hdr})
	h.mu.Unlock()
	if h.fn != nil {
		return h.fn(ctx, p, body, hdr)
	}
	return ingress.Stored, nil
}

func (h *testHub) recorded() []hubCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hubCall(nil), h.calls...)
}

func (h *testHub) count() int { return len(h.recorded()) }

// safeBuffer collects log output from any number of goroutines.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// plain is a provider with no webhook capability, so it has no endpoint at the edge.
type plain struct{}

func (plain) Key() string { return "plain" }

func (plain) Hydrate(context.Context, tenancy.ID, provider.Change) (provider.Hydrated, error) {
	return nil, errors.New("not used")
}

func (plain) Normalize(provider.Hydrated, provider.Change) ([]record.Record, error) {
	return nil, errors.New("not used")
}

// scripted is a WebhookSource whose handshake a test writes, for the replies a well-behaved
// provider would never return.
type scripted struct {
	plain
	handshake func(r *http.Request, body []byte) (provider.Reply, bool)
}

func (scripted) Key() string { return "scripted" }

func (s scripted) Handshake(r *http.Request, body []byte) (provider.Reply, bool) {
	return s.handshake(r, body)
}

func (scripted) DeliveryKeys([]byte, http.Header) (provider.DeliveryKeys, error) {
	return provider.DeliveryKeys{}, errors.New("not used")
}

func (scripted) Verify([]byte, http.Header, []byte) bool { return false }

func (scripted) Parse([]byte) ([]provider.Change, error) { return nil, errors.New("not used") }

// edge builds a mux with the ingress route on it, plus the log it wrote.
func edge(t *testing.T, hub ingress.Hub, opts ingress.Options, extra ...provider.Provider) (*http.ServeMux, *safeBuffer) {
	t.Helper()
	providers := append([]provider.Provider{fake.New(fake.DefaultKey), plain{}}, extra...)
	reg, err := provider.NewRegistry(providers...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	logs := &safeBuffer{}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	h, err := ingress.New(reg, hub, opts)
	if err != nil {
		t.Fatalf("ingress.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	return mux, logs
}

func signedRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/ingress/"+fake.DefaultKey, strings.NewReader(body))
	r.Header.Set(fake.SignatureHeader, fake.Sign([]byte(secret), []byte(body)))
	return r
}

// unsizedRequest is a POST whose body length is unknown, as a chunked request's is:
// httptest.NewRequest only sets ContentLength for the reader types it recognizes.
func unsizedRequest(body []byte) *http.Request {
	hidden := struct{ io.Reader }{bytes.NewReader(body)}
	r := httptest.NewRequest(http.MethodPost, "/ingress/"+fake.DefaultKey, hidden)
	if r.ContentLength >= 0 {
		panic("this helper must produce a request of unknown length")
	}
	return r
}

// TestTheVerifierGetsByteIdenticalInput is the first acceptance line of B06, and principle 1 of
// the architecture. The delivery is JSON whose re-serialization differs from what was sent, and
// the hub verifies the signature the way the real hub will: by calling the provider's Verify over
// what the edge handed it. If the edge normalized, re-encoded or even trimmed the bytes anywhere,
// the HMAC would not match and this answers 401 instead of 202.
func TestTheVerifierGetsByteIdenticalInput(t *testing.T) {
	t.Parallel()

	// Spacing, key order and an escape that a re-serializer would all write differently.
	raw := "  {\"type\":\"event\",\n  \"events\" : [" + event + "],\t\"workspace\":\"caf\\u00e9\"}  "

	var asMap map[string]any
	if err := json.Unmarshal([]byte(raw), &asMap); err != nil {
		t.Fatalf("the fixture must be valid JSON: %v", err)
	}
	reserialized, err := json.Marshal(asMap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(reserialized) == raw {
		t.Fatal("the fixture must re-serialize differently, or this test proves nothing")
	}

	hub := &testHub{fn: func(_ context.Context, p provider.Entry, body []byte, h http.Header) (ingress.Verdict, error) {
		source, ok := p.WebhookSource()
		if !ok {
			return 0, errors.New("no webhook source")
		}
		if !source.Verify(body, h, []byte(secret)) {
			return ingress.Unverified, nil
		}
		return ingress.Stored, nil
	}}
	mux, _ := edge(t, hub, ingress.Options{})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, signedRequest(raw))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: the verifier did not get the bytes that were signed", rec.Code)
	}

	calls := hub.recorded()
	if len(calls) != 1 {
		t.Fatalf("hub called %d times", len(calls))
	}
	if !bytes.Equal(calls[0].body, []byte(raw)) {
		t.Fatalf("the hub got different bytes:\n got %q\nwant %q", calls[0].body, raw)
	}
	if calls[0].key != fake.DefaultKey {
		t.Fatalf("the hub got the key %q", calls[0].key)
	}

	// The control: the same signature over the re-serialized body must be refused, which is what
	// makes the 202 above meaningful.
	control := httptest.NewRequest(http.MethodPost, "/ingress/"+fake.DefaultKey, bytes.NewReader(reserialized))
	control.Header.Set(fake.SignatureHeader, fake.Sign([]byte(secret), []byte(raw)))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, control)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("re-serialized body: status = %d, want 401", rec.Code)
	}
}

// TestABodyThatIsNotJSONReachesTheHubUnchanged goes further than the acceptance line: the edge
// must not care what the bytes are. A body that is not UTF-8, holds NUL bytes or is empty travels
// through untouched, because a provider may sign anything at all.
func TestABodyThatIsNotJSONReachesTheHubUnchanged(t *testing.T) {
	t.Parallel()

	bodies := map[string][]byte{
		"empty":                             {},
		"not UTF-8":                         {0xff, 0xfe, 0x00, 0x80},
		"NUL inside":                        []byte("a\x00b"),
		"not JSON":                          []byte("<xml/>"),
		"one byte":                          {'x'},
		"every byte":                        allBytes(),
		"looks like a handshake but is not": []byte(`{"type":"handshake"`),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hub := &testHub{}
			mux, _ := edge(t, hub, ingress.Options{})
			r := httptest.NewRequest(http.MethodPost, "/ingress/"+fake.DefaultKey, bytes.NewReader(body))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", rec.Code)
			}
			calls := hub.recorded()
			if len(calls) != 1 || !bytes.Equal(calls[0].body, body) {
				t.Fatalf("the hub got %q, want %q", calls[0].body, body)
			}
		})
	}
}

func allBytes() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// endless is a request body that never ends. Reading it to completion would never return, so a
// handler that answered 413 after reading the whole body would hang here instead of failing, and
// a handler that buffered it would run out of memory. That is the point: this proves the cap is
// enforced while reading and not after.
type endless struct{ read atomic.Int64 }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	e.read.Add(int64(len(p)))
	return len(p), nil
}

// TestAnOversizeBodyIsRefusedWithoutBeingReadIntoMemory is the second acceptance line of B06.
func TestAnOversizeBodyIsRefusedWithoutBeingReadIntoMemory(t *testing.T) {
	t.Parallel()

	const cap64 = 1024

	t.Run("a body with no end is cut at the cap", func(t *testing.T) {
		t.Parallel()
		hub := &testHub{}
		mux, _ := edge(t, hub, ingress.Options{MaxBody: cap64})
		body := &endless{}
		// httptest.NewRequest leaves ContentLength at -1 for a reader of unknown size, so the
		// fast path on an honest Content-Length is not what is being tested here.
		r := httptest.NewRequest(http.MethodPost, "/ingress/"+fake.DefaultKey, body)
		rec := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			defer close(done)
			mux.ServeHTTP(rec, r)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the handler is still reading a body with no end")
		}

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", rec.Code)
		}
		if got := body.read.Load(); got > cap64+4096 {
			t.Fatalf("the handler read %d bytes of an oversize body, the cap is %d", got, cap64)
		}
		if hub.count() != 0 {
			t.Fatal("an oversize body reached the hub")
		}
	})

	t.Run("exactly at the cap is accepted and one byte over is not", func(t *testing.T) {
		t.Parallel()
		hub := &testHub{}
		mux, _ := edge(t, hub, ingress.Options{MaxBody: cap64})

		atCap := bytes.Repeat([]byte("a"), cap64)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ingress/"+fake.DefaultKey, bytes.NewReader(atCap)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("a body of exactly the cap: status = %d, want 202", rec.Code)
		}
		if calls := hub.recorded(); len(calls) != 1 || len(calls[0].body) != cap64 {
			t.Fatalf("the hub got %d bytes", len(calls[0].body))
		}

		overCap := bytes.Repeat([]byte("a"), cap64+1)
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ingress/"+fake.DefaultKey, bytes.NewReader(overCap)))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("one byte over the cap: status = %d, want 413", rec.Code)
		}
		if hub.count() != 1 {
			t.Fatal("the oversize body reached the hub")
		}
	})

	t.Run("the cap holds when the length is unknown", func(t *testing.T) {
		t.Parallel()
		// With no Content-Length the fast path cannot fire, so this is the boundary of
		// http.MaxBytesReader itself, which is the check that actually holds.
		hub := &testHub{}
		mux, _ := edge(t, hub, ingress.Options{MaxBody: cap64})

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, unsizedRequest(bytes.Repeat([]byte("a"), cap64)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("exactly the cap, length unknown: status = %d, want 202", rec.Code)
		}

		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, unsizedRequest(bytes.Repeat([]byte("a"), cap64+1)))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("one byte over the cap, length unknown: status = %d, want 413", rec.Code)
		}
		if hub.count() != 1 {
			t.Fatalf("the hub saw %d deliveries, want only the one at the cap", hub.count())
		}
	})

	t.Run("a chunked body with no Content-Length is capped too", func(t *testing.T) {
		t.Parallel()
		hub := &testHub{}
		mux, _ := edge(t, hub, ingress.Options{MaxBody: cap64})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)

		// A pipe means the client sends chunked, with no Content-Length to check.
		pr, pw := io.Pipe()
		go func() {
			defer func() { _ = pw.Close() }()
			for range 64 {
				if _, err := pw.Write(bytes.Repeat([]byte("a"), 1024)); err != nil {
					return
				}
			}
		}()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/ingress/"+fake.DefaultKey, pr)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
		if hub.count() != 0 {
			t.Fatal("an oversize chunked body reached the hub")
		}
	})

	t.Run("an honest Content-Length over the cap is refused before any read", func(t *testing.T) {
		t.Parallel()
		hub := &testHub{}
		mux, _ := edge(t, hub, ingress.Options{MaxBody: cap64})
		body := &endless{}
		r := httptest.NewRequest(http.MethodPost, "/ingress/"+fake.DefaultKey, body)
		r.ContentLength = 1 << 30
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", rec.Code)
		}
		if got := body.read.Load(); got != 0 {
			t.Fatalf("read %d bytes of a body that announced itself as oversize", got)
		}
	})
}

// TestAnAnnouncedLengthCannotDriveAnAllocation. A Content-Length is a claim, not a promise. The
// edge uses it to size one buffer, which is worth doing on a path that runs per webhook, but a
// deployment that raised MaxBody for a provider with large payloads must not thereby let one
// header make the process reserve that much for a request that then sends ten bytes.
func TestAnAnnouncedLengthCannotDriveAnAllocation(t *testing.T) {
	t.Parallel()

	const announced = 64 << 20
	hub := &testHub{}
	mux, _ := edge(t, hub, ingress.Options{MaxBody: 128 << 20})

	r := httptest.NewRequest(http.MethodPost, "/ingress/"+fake.DefaultKey, strings.NewReader("ten bytes!"))
	r.ContentLength = announced
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	calls := hub.recorded()
	if len(calls) != 1 || string(calls[0].body) != "ten bytes!" {
		t.Fatalf("the hub got %q", calls[0].body)
	}
	// The capacity of the buffer the body came out of is the allocation that was made.
	if got := cap(calls[0].body); got > 4<<20 {
		t.Fatalf("a body of 10 bytes was read into a buffer of %d, announced %d", got, announced)
	}
}

// TestAnUnknownProviderIs404AndNothingElseHappens is the acceptance added by the delta review of
// #32: the path segment is request text, net/http decodes %00 in it to a NUL byte, and
// outbox.Delivery.Provider must be a constant of the program. So the lookup comes first, and an
// unknown segment is answered before a body is read or a hub is called.
func TestAnUnknownProviderIs404AndNothingElseHappens(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("\x01\xff\x00qZ", 100) // 500 bytes, well past the 32 a key may have
	segments := map[string]string{
		"a NUL":                 "\x00",
		"a NUL inside":          "fa\x00ke",
		"500 bytes":             long,
		"the empty segment":     "",
		"a capital":             "Fake",
		"a hyphen":              "ms-graph",
		"a newline":             "fake\nx",
		"a slash":               "fake/x",
		"a space":               "fa ke",
		"a leading digit":       "9lives",
		"33 characters":         strings.Repeat("a", 33),
		"registered but silent": "plain", // a provider with no webhook has no endpoint here
	}
	for name, segment := range segments {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hub := &testHub{}
			mux, logs := edge(t, hub, ingress.Options{})
			// PathEscape keeps the bytes intact on the way in; net/http decodes them again.
			target := "/ingress/" + url.PathEscape(segment)
			r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(delivery))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if hub.count() != 0 {
				t.Fatal("an unknown provider reached the hub, so something could have been stored")
			}
			if body := rec.Body.String(); strings.Contains(body, segment) && segment != "" {
				t.Fatalf("the response echoed the path segment: %q", body)
			}
			if segment != "" && strings.Contains(logs.String(), segment) {
				t.Fatal("the log quoted the path segment")
			}
		})
	}
}

// TestPathValueReallyDecodesNUL documents the fact decision 2 rests on. If a future Go release
// stopped decoding %00 here, the guard would still be right but this note would be stale.
func TestPathValueReallyDecodesNUL(t *testing.T) {
	t.Parallel()

	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ingress/{provider}", func(_ http.ResponseWriter, r *http.Request) {
		got = r.PathValue("provider")
	})
	r := httptest.NewRequest(http.MethodPost, "/ingress/fa%00ke", nil)
	mux.ServeHTTP(httptest.NewRecorder(), r)
	if got != "fa\x00ke" {
		t.Fatalf("PathValue = %q, want a NUL byte in it", got)
	}
}

func TestTheHandshakeAnswersAndStopsBeforeTheHub(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		target     string
		body       string
		wantStatus int
		wantBody   string
		wantType   string
	}{
		{
			name:       "the text echo",
			target:     "/ingress/fake?validationToken=abc123",
			wantStatus: http.StatusOK,
			wantBody:   "abc123",
			wantType:   "text/plain; charset=utf-8",
		},
		{
			name:       "the json challenge",
			target:     "/ingress/fake",
			body:       `{"type":"handshake","challenge":"xyz"}`,
			wantStatus: http.StatusOK,
			wantBody:   `{"challenge":"xyz"}`,
			wantType:   "application/json",
		},
		{
			name:       "a challenge the provider refuses",
			target:     "/ingress/fake?validationToken=" + url.QueryEscape("a\nb"),
			wantStatus: http.StatusBadRequest,
			wantBody:   "bad challenge\n",
			wantType:   "text/plain; charset=utf-8",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			hub := &testHub{}
			mux, _ := edge(t, hub, ingress.Options{})
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, c.target, strings.NewReader(c.body)))

			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, c.wantStatus)
			}
			if got := rec.Body.String(); got != c.wantBody {
				t.Fatalf("body = %q, want %q", got, c.wantBody)
			}
			if got := rec.Header().Get("Content-Type"); got != c.wantType {
				t.Fatalf("content type = %q, want %q", got, c.wantType)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("a reply that echoes a stranger's text must be nosniff, got %q", got)
			}
			if hub.count() != 0 {
				t.Fatal("a handshake must stop before the hub: no tenant exists yet")
			}
		})
	}
}

// TestTheHandshakeCannotReadTheBodyTwice pins the contract of WebhookSource.Handshake: the body it
// is given is the one the edge captured, and r.Body is spent. A provider that read r.Body instead
// would verify one set of bytes and store another.
func TestTheHandshakeCannotReadTheBodyTwice(t *testing.T) {
	t.Parallel()

	var fromRequestBody []byte
	var fromArgument []byte
	var sawNoBody bool
	peeker := scripted{handshake: func(r *http.Request, body []byte) (provider.Reply, bool) {
		sawNoBody = r.Body == http.NoBody
		fromRequestBody, _ = io.ReadAll(r.Body)
		fromArgument = body
		return provider.Reply{Body: []byte("ok")}, true
	}}
	mux, _ := edge(t, &testHub{}, ingress.Options{}, peeker)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ingress/scripted", strings.NewReader(delivery)))

	if !sawNoBody {
		t.Fatal("r.Body must be http.NoBody, so a provider cannot read a second copy of the bytes")
	}
	if len(fromRequestBody) != 0 {
		t.Fatalf("r.Body yielded %d bytes, it must be spent", len(fromRequestBody))
	}
	if string(fromArgument) != delivery {
		t.Fatalf("the body argument = %q", fromArgument)
	}
}

// TestAHandshakeReplyAProviderMayNotAskForIsRefused covers the replies that would turn the edge
// into something it is not. A provider is this program's own code, so each of these is a bug, and
// a bug is a 500 with nothing of the provider's reply written.
func TestAHandshakeReplyAProviderMayNotAskForIsRefused(t *testing.T) {
	t.Parallel()

	cases := map[string]provider.Reply{
		"a 500 from a handshake":    {Status: http.StatusInternalServerError, Body: []byte("leaked")},
		"a redirect":                {Status: http.StatusFound, Body: []byte("leaked")},
		"a status below 200":        {Status: 100, Body: []byte("leaked")},
		"a status above 599":        {Status: 999, Body: []byte("leaked")},
		"a body over the reply cap": {Body: bytes.Repeat([]byte("l"), (8<<10)+1)},
		"a content type with a newline": {
			ContentType: "text/plain\r\nX-Injected: 1",
			Body:        []byte("leaked"),
		},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := scripted{handshake: func(*http.Request, []byte) (provider.Reply, bool) {
				return reply, true
			}}
			mux, logs := edge(t, &testHub{}, ingress.Options{}, s)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ingress/scripted", nil))

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "leaked") {
				t.Fatalf("the refused reply was written anyway: %q", rec.Body.String())
			}
			if rec.Header().Get("X-Injected") != "" {
				t.Fatal("a header was injected through the content type")
			}
			if !strings.Contains(logs.String(), "scripted") {
				t.Fatal("a bug in a provider must be logged, with the provider key")
			}
		})
	}

	t.Run("a well-formed reply is written", func(t *testing.T) {
		t.Parallel()
		s := scripted{handshake: func(*http.Request, []byte) (provider.Reply, bool) {
			return provider.Reply{Status: http.StatusOK, ContentType: "application/json", Body: []byte(`{"ok":true}`)}, true
		}}
		mux, _ := edge(t, &testHub{}, ingress.Options{}, s)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ingress/scripted", nil))
		if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
			t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
		}
	})
}

// TestTheVerdictDecidesTheStatus is the response-code contract of architecture 3.1. A provider
// retries anything that is not 2xx, so every one of these numbers is load bearing: 401 is only
// ever a signature failure, and poison and an unowned delivery are 200 on purpose.
func TestTheVerdictDecidesTheStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		verdict ingress.Verdict
		want    int
	}{
		{ingress.Stored, http.StatusAccepted},
		{ingress.Duplicate, http.StatusOK},
		{ingress.Parked, http.StatusOK},
		{ingress.Unverified, http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.verdict.String(), func(t *testing.T) {
			t.Parallel()
			hub := &testHub{fn: func(context.Context, provider.Entry, []byte, http.Header) (ingress.Verdict, error) {
				return c.verdict, nil
			}}
			mux, _ := edge(t, hub, ingress.Options{})
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, signedRequest(delivery))
			if rec.Code != c.want {
				t.Fatalf("%s: status = %d, want %d", c.verdict, rec.Code, c.want)
			}
		})
	}
}

// TestAHubThatReturnsNoVerdictAndNoErrorIsABug keeps the zero value from meaning 200.
func TestAHubThatReturnsNoVerdictAndNoErrorIsABug(t *testing.T) {
	t.Parallel()

	hub := &testHub{fn: func(context.Context, provider.Entry, []byte, http.Header) (ingress.Verdict, error) {
		return 0, nil
	}}
	mux, logs := edge(t, hub, ingress.Options{})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, signedRequest(delivery))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(logs.String(), "no verdict") {
		t.Fatalf("the bug was not logged: %s", logs.String())
	}
}

// TestASaturatedAcceptPathAnswersFastWithARetryableStatus is the pool-sizing decision carried over
// from the round 3 review of #31. Every accept holds a pool connection, the pool is small, and a
// full pool makes an acquire wait. The edge bounds that wait and answers a retryable 503 with a
// Retry-After instead of holding the provider's connection open until it gives up.
func TestASaturatedAcceptPathAnswersFastWithARetryableStatus(t *testing.T) {
	t.Parallel()

	hub := &testHub{fn: func(ctx context.Context, _ provider.Entry, _ []byte, _ http.Header) (ingress.Verdict, error) {
		// What pgxpool.Acquire does when every connection is busy: wait on the context.
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("acquiring a connection: %w", ctx.Err())
		case <-time.After(3 * time.Second):
			// Only reached if the edge stopped bounding the accept. Returning rather than
			// blocking keeps this test a failure and not a hung run.
			return ingress.Stored, nil
		}
	}}

	mux, logs := edge(t, hub, ingress.Options{AcceptTimeout: 50 * time.Millisecond})
	rec := httptest.NewRecorder()
	start := time.Now()
	mux.ServeHTTP(rec, signedRequest(delivery))
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("a 503 must say when to come back")
	}
	if elapsed > time.Second {
		t.Fatalf("the edge waited %v for a 50ms bound, it must answer fast rather than hang", elapsed)
	}
	if !strings.Contains(logs.String(), "timed out") {
		t.Fatalf("the timeout was not logged: %s", logs.String())
	}
}

// TestAHubFailureIs500AndSaysNothing: a genuine failure on our side is the one case a provider
// should retry, and its text never reaches the sender.
func TestAHubFailureIs500AndSaysNothing(t *testing.T) {
	t.Parallel()

	hub := &testHub{fn: func(context.Context, provider.Entry, []byte, http.Header) (ingress.Verdict, error) {
		return 0, errors.New("connection refused to the-database-host:5432")
	}}
	mux, _ := edge(t, hub, ingress.Options{})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, signedRequest(delivery))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "5432") {
		t.Fatalf("the error reached the sender: %q", rec.Body.String())
	}
}

// TestABodyThatStopsEarlyIs400 needs a real server, because a lying Content-Length is something
// only net/http's own body reader can notice. It is the shape of a request smuggling probe, and
// there is nothing to store.
func TestABodyThatStopsEarlyIs400(t *testing.T) {
	t.Parallel()

	hub := &testHub{}
	mux, _ := edge(t, hub, ingress.Options{})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	// 4096 bytes promised, 5 sent, then the write side is closed.
	request := "POST /ingress/" + fake.DefaultKey + " HTTP/1.1\r\nHost: x\r\n" +
		"Content-Length: 4096\r\n\r\nshort"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write: %v", err)
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("want a TCP connection, got %T", conn)
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if hub.count() != 0 {
		t.Fatal("a truncated body reached the hub")
	}
}

// TestPathTraversalNeverReachesTheHandler. The Cloudflare Tunnel in front of a developer machine
// answers these itself, but the edge is not allowed to lean on that: net/http's mux cleans a path
// before it matches, so a traversal ends as a redirect or a 404 and never as a delivery.
func TestPathTraversalNeverReachesTheHandler(t *testing.T) {
	t.Parallel()

	for _, target := range []string{
		"/ingress/..", "/ingress/../x", "/ingress/%2e%2e/x", "/ingress/a/../../x",
		"/ingress/fake/..", "/ingress/fake/x", "/ingress/", "/ingress",
	} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			hub := &testHub{}
			mux, _ := edge(t, hub, ingress.Options{})
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, strings.NewReader(delivery)))
			if rec.Code >= 200 && rec.Code < 300 {
				t.Fatalf("status = %d, a traversal must never be accepted", rec.Code)
			}
			if hub.count() != 0 {
				t.Fatal("a traversal reached the hub")
			}
		})
	}
}

// TestOnlyPostReachesTheEdge: the method is part of the route, so nothing else gets as far as
// reading a body.
func TestOnlyPostReachesTheEdge(t *testing.T) {
	t.Parallel()

	hub := &testHub{}
	mux, _ := edge(t, hub, ingress.Options{})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, "/ingress/"+fake.DefaultKey, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
	}
	if hub.count() != 0 {
		t.Fatal("something other than a POST reached the hub")
	}
}

// TestConcurrentDeliveriesKeepTheirOwnBodies is the aliasing test: the edge captures one buffer
// per request, and a shared one would show up here as a body handed to the wrong delivery. It is
// also what the race detector needs in order to say anything about this handler.
func TestConcurrentDeliveriesKeepTheirOwnBodies(t *testing.T) {
	t.Parallel()

	const senders = 32
	hub := &testHub{fn: func(_ context.Context, p provider.Entry, body []byte, h http.Header) (ingress.Verdict, error) {
		source, _ := p.WebhookSource()
		// Verifying inside the hub means a body that belonged to another request fails here.
		if !source.Verify(body, h, []byte(secret)) {
			return ingress.Unverified, nil
		}
		return ingress.Stored, nil
	}}
	mux, _ := edge(t, hub, ingress.Options{})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var wg sync.WaitGroup
	errs := make(chan error, senders)
	for i := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"type":"event","workspace":"W%d","events":[%s]}`, i, event)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				srv.URL+"/ingress/"+fake.DefaultKey, strings.NewReader(body))
			if err != nil {
				errs <- err
				return
			}
			req.Header.Set(fake.SignatureHeader, fake.Sign([]byte(secret), []byte(body)))
			resp, err := srv.Client().Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusAccepted {
				errs <- fmt.Errorf("status = %d, want 202: a body was crossed with another request's", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := hub.count(); got != senders {
		t.Fatalf("the hub saw %d deliveries, want %d", got, senders)
	}
}

// TestNothingASenderControlsReachesTheLogOrTheResponse is the rule this endpoint lives by: a body,
// a header and a path segment are hostile text, and neither a log line nor a response may carry
// any of them.
func TestNothingASenderControlsReachesTheLogOrTheResponse(t *testing.T) {
	t.Parallel()

	const marker = "zzmarkerzz"
	hub := &testHub{fn: func(context.Context, provider.Entry, []byte, http.Header) (ingress.Verdict, error) {
		return ingress.Parked, nil
	}}
	mux, logs := edge(t, hub, ingress.Options{})

	body := `{"type":"event","workspace":"` + marker + `","events":[` + event + `]}`
	r := httptest.NewRequest(http.MethodPost, "/ingress/"+fake.DefaultKey+"?q="+marker, strings.NewReader(body))
	r.Header.Set(fake.SignatureHeader, marker)
	r.Header.Set("User-Agent", marker)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), marker) {
		t.Fatalf("the response echoed the sender: %q", rec.Body.String())
	}
	for _, values := range rec.Header() {
		for _, v := range values {
			if strings.Contains(v, marker) {
				t.Fatalf("a response header echoed the sender: %q", v)
			}
		}
	}
	if strings.Contains(logs.String(), marker) {
		t.Fatalf("the log quoted the sender: %s", logs.String())
	}
}

func TestNewRefusesAnEdgeThatCouldNotDoItsJob(t *testing.T) {
	t.Parallel()

	reg, err := provider.NewRegistry(fake.New(fake.DefaultKey))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, err := ingress.New(nil, &testHub{}, ingress.Options{}); err == nil {
		t.Fatal("a nil registry must be refused")
	}
	if _, err := ingress.New(reg, nil, ingress.Options{}); err == nil {
		t.Fatal("a nil hub must be refused: it would answer a provider without storing anything")
	}
	if _, err := ingress.New(reg, &testHub{}, ingress.Options{MaxBody: -1}); err == nil {
		t.Fatal("a negative cap must be refused")
	}
	if _, err := ingress.New(reg, &testHub{}, ingress.Options{AcceptTimeout: -time.Second}); err == nil {
		t.Fatal("a negative timeout must be refused")
	}
}

// TestTheDefaultsAreTheDocumentedOnes: the zero Options is what serve will use, so the numbers in
// the package documentation have to be the numbers in force.
func TestTheDefaultsAreTheDocumentedOnes(t *testing.T) {
	t.Parallel()

	hub := &testHub{}
	mux, _ := edge(t, hub, ingress.Options{})

	atCap := bytes.Repeat([]byte("a"), int(ingress.DefaultMaxBody))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ingress/"+fake.DefaultKey, bytes.NewReader(atCap)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a body of DefaultMaxBody: status = %d, want 202", rec.Code)
	}

	over := bytes.Repeat([]byte("a"), int(ingress.DefaultMaxBody)+1)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/ingress/"+fake.DefaultKey, bytes.NewReader(over)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("one byte over DefaultMaxBody: status = %d, want 413", rec.Code)
	}
}

func TestVerdictStringNamesEveryVerdict(t *testing.T) {
	t.Parallel()

	want := map[ingress.Verdict]string{
		ingress.Stored:     "stored",
		ingress.Duplicate:  "duplicate",
		ingress.Parked:     "parked",
		ingress.Unverified: "unverified",
		ingress.Verdict(0): "invalid",
		ingress.Verdict(9): "invalid",
	}
	for v, s := range want {
		if got := v.String(); got != s {
			t.Fatalf("Verdict(%d).String() = %q, want %q", int(v), got, s)
		}
	}
}

// TestASenderThatHangsUpIsNotAnError. When the client is gone there is nothing to write and
// nothing is wrong on our side, so the edge must not fill the log with errors for a provider that
// closed a connection.
func TestASenderThatHangsUpIsNotAnError(t *testing.T) {
	t.Parallel()

	hub := &testHub{fn: func(ctx context.Context, _ provider.Entry, _ []byte, _ http.Header) (ingress.Verdict, error) {
		return 0, fmt.Errorf("writing the outbox row: %w", ctx.Err())
	}}
	mux, logs := edge(t, hub, ingress.Options{})

	gone, cancel := context.WithCancel(t.Context())
	cancel()
	r := signedRequest(delivery).WithContext(gone)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Body.Len() != 0 {
		t.Fatalf("something was written to a connection that is gone: %q", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "sender left") {
		t.Fatalf("want a debug line about the sender leaving, got: %s", logs.String())
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("a sender hanging up is not an error on our side: %s", logs.String())
	}
}

// TestNewFallsBackToTheDefaultLogger keeps the zero Options usable, since that is what serve will
// pass when the edge is wired up.
func TestNewFallsBackToTheDefaultLogger(t *testing.T) {
	t.Parallel()

	reg, err := provider.NewRegistry(fake.New(fake.DefaultKey))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	h, err := ingress.New(reg, &testHub{}, ingress.Options{})
	if err != nil {
		t.Fatalf("ingress.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, signedRequest(delivery))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
}
