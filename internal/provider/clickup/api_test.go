package clickup_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/clickup"
	"github.com/gablooge/lawang/internal/tenancy"
)

const testTenant = tenancy.ID("t_clickup")

// fixedToken is the vault seam B13 will fill, as a constant. A test double must refuse what the
// real one refuses, so the empty tenant gets a refusal here too.
type fixedToken string

func (f fixedToken) Token(_ context.Context, t tenancy.ID) (string, error) {
	if t == "" {
		return "", errors.New("no tenant")
	}
	return string(f), nil
}

// noToken stands for a tenant the vault has nothing for.
type noToken struct{}

func (noToken) Token(context.Context, tenancy.ID) (string, error) {
	return "", errors.New("nothing stored for this tenant")
}

// fakeAPI is a stand-in for ClickUp's API: it answers the two calls this package makes, counts
// them, and records what it was asked. It refuses a request with no Authorization header, which
// is what ClickUp does, so a provider that forgot the credential fails here rather than passing.
type fakeAPI struct {
	t *testing.T

	mu       sync.Mutex
	requests []*http.Request
	calls    int

	taskBody     []byte
	commentsBody []byte
	status       int
	// pages, when set, is served one per request to the comments endpoint, oldest last.
	pages [][]byte
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	page := 0
	for _, prev := range f.requests {
		if strings.HasSuffix(prev.URL.Path, "/comment") {
			page++
		}
	}
	f.requests = append(f.requests, r.Clone(context.Background()))
	f.mu.Unlock()

	if r.Header.Get("Authorization") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.status != 0 && f.status != http.StatusOK {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"err":"a body nothing should read","ECODE":"OAUTH_025"}`))
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/comment"):
		body := f.commentsBody
		if f.pages != nil {
			if page >= len(f.pages) {
				page = len(f.pages) - 1
			}
			body = f.pages[page]
		}
		_, _ = w.Write(body)
	default:
		_, _ = w.Write(f.taskBody)
	}
}

func (f *fakeAPI) seen() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.requests...)
}

// newHydrating returns a provider pointed at a fake ClickUp API, and the fake.
func newHydrating(t *testing.T, opts ...func(*clickup.Options)) (*clickup.Provider, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{
		t:            t,
		taskBody:     fixture(t, "api_task.json"),
		commentsBody: fixture(t, "api_comments.json"),
	}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	o := clickup.Options{Tokens: fixedToken("placeholder-token"), BaseURL: srv.URL}
	for _, f := range opts {
		f(&o)
	}
	p, err := clickup.New(o)
	if err != nil {
		t.Fatal(err)
	}
	return p, api
}

// hydrate is the two steps a drain takes before it normalizes.
func hydrate(t *testing.T, p *clickup.Provider, body []byte) (provider.Change, provider.Hydrated) {
	t.Helper()
	changes, err := p.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(changes))
	}
	h, err := p.Hydrate(context.Background(), testTenant, changes[0])
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	return changes[0], h
}

// Hydration asks ClickUp for exactly what the change is about, with the tenant's token on the
// request, and nowhere else.
func TestHydrateCallsTheDocumentedEndpoints(t *testing.T) {
	for _, tc := range []struct {
		file  string
		paths []string
	}{
		{"webhook_task_updated.json", []string{"/api/v2/task/86a1b2"}},
		{"webhook_comment_posted.json", []string{"/api/v2/task/86a1b2", "/api/v2/task/86a1b2/comment"}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			p, api := newHydrating(t)
			hydrate(t, p, fixture(t, tc.file))
			seen := api.seen()
			if len(seen) != len(tc.paths) {
				t.Fatalf("made %d requests, want %d", len(seen), len(tc.paths))
			}
			for i, want := range tc.paths {
				if seen[i].URL.Path != want {
					t.Errorf("request %d went to %q, want %q", i, seen[i].URL.Path, want)
				}
				if got := seen[i].Header.Get("Authorization"); got != "placeholder-token" {
					t.Errorf("request %d carried Authorization %q", i, got)
				}
				if seen[i].Method != http.MethodGet {
					t.Errorf("request %d was a %s", i, seen[i].Method)
				}
			}
		})
	}
}

// A tenant with no token is a refusal, and no request is made. Fail closed: an anonymous request
// to ClickUp would be answered 401 and the delivery would spend its ladder on it.
func TestHydrateRefusesATenantWithNoToken(t *testing.T) {
	p, api := newHydrating(t, func(o *clickup.Options) { o.Tokens = noToken{} })
	changes, err := p.Parse(fixture(t, "webhook_task_updated.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Hydrate(context.Background(), testTenant, changes[0])
	if !errors.Is(err, clickup.ErrNoToken) {
		t.Fatalf("Hydrate: %v, want ErrNoToken", err)
	}
	if n := len(api.seen()); n != 0 {
		t.Errorf("made %d requests without a token", n)
	}
}

// No tenant at all is the same refusal, for the same reason, and it is the one the vault cannot
// make for us.
func TestHydrateRefusesAnEmptyTenant(t *testing.T) {
	p, api := newHydrating(t)
	changes, err := p.Parse(fixture(t, "webhook_task_updated.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Hydrate(context.Background(), "", changes[0]); err == nil {
		t.Fatal("hydrated with no tenant")
	}
	if n := len(api.seen()); n != 0 {
		t.Errorf("made %d requests with no tenant", n)
	}
}

// The payload a change carries must be the change's own. A stored row whose payload names
// another entity is a bug or a tampered table, and hydrating it would label one entity's content
// with another's id.
func TestHydrateRefusesAPayloadThatIsNotTheChangesOwn(t *testing.T) {
	p, api := newHydrating(t)
	c := provider.Change{ExternalID: "clickup:task:somethingelse", Payload: fixture(t, "webhook_task_updated.json")}
	if _, err := p.Hydrate(context.Background(), testTenant, c); !errors.Is(err, clickup.ErrBadObject) {
		t.Fatalf("Hydrate: %v, want ErrBadObject", err)
	}
	if n := len(api.seen()); n != 0 {
		t.Errorf("made %d requests for a payload that was not the change's", n)
	}
}

// What the API answers with, other than the answer.
func TestHydrateOnAnAPIThatRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   error
	}{
		{"unauthorized", http.StatusUnauthorized, clickup.ErrAPI},
		{"forbidden", http.StatusForbidden, clickup.ErrAPI},
		{"not found", http.StatusNotFound, clickup.ErrNotFound},
		{"rate limited by clickup", http.StatusTooManyRequests, clickup.ErrAPI},
		{"server error", http.StatusInternalServerError, clickup.ErrAPI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, api := newHydrating(t)
			api.status = tc.status
			changes, err := p.Parse(fixture(t, "webhook_task_updated.json"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Hydrate(context.Background(), testTenant, changes[0])
			if !errors.Is(err, tc.want) {
				t.Fatalf("Hydrate: %v, want %v", err, tc.want)
			}
			// The fake answers a body with an error code in it, the way ClickUp does. None of
			// it may reach the error, which ends up in last_error.
			if strings.Contains(err.Error(), "OAUTH_025") || strings.Contains(err.Error(), "a body nothing should read") {
				t.Errorf("the error carries the response body: %v", err)
			}
		})
	}
}

// Nothing a hydration failure produces carries the tenant's token. The marker is the token
// itself, and it is searched for in every error this path can produce.
func TestNoErrorCarriesTheToken(t *testing.T) {
	const marker = "pk_markervalue_THIS_IS_A_TOKEN"
	for _, tc := range []struct {
		name  string
		setup func(*fakeAPI)
	}{
		{"a refused call", func(f *fakeAPI) { f.status = http.StatusUnauthorized }},
		{"an answer that is not JSON", func(f *fakeAPI) { f.taskBody = []byte("<html>" + marker + "</html>") }},
		{"an answer with no list", func(f *fakeAPI) { f.taskBody = []byte(`{"id":"86a1b2"}`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, api := newHydrating(t, func(o *clickup.Options) { o.Tokens = fixedToken(marker) })
			tc.setup(api)
			changes, err := p.Parse(fixture(t, "webhook_task_updated.json"))
			if err != nil {
				t.Fatal(err)
			}
			h, err := p.Hydrate(context.Background(), testTenant, changes[0])
			if err == nil {
				_, err = p.Normalize(h, changes[0])
			}
			if err == nil {
				t.Fatal("nothing failed, so there is no error to check")
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("the error carries the token: %v", err)
			}
		})
	}
}

// A cancelled context stops the drain, and the cancellation has to be recognisable: the worker
// tells its own shutdown from a provider outage by it.
func TestHydrateHonoursTheContext(t *testing.T) {
	p, _ := newHydrating(t)
	changes, err := p.Parse(fixture(t, "webhook_task_updated.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.Hydrate(ctx, testTenant, changes[0])
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Hydrate: %v, want a context.Canceled", err)
	}
}

// The comment a delivery names is found on a later page, and the search stops rather than
// walking a task with a very long history.
func TestHydrateFindsACommentOnALaterPage(t *testing.T) {
	// A full page of comments that are not the one wanted, then the one wanted.
	var filler strings.Builder
	filler.WriteString(`{"comments":[`)
	for i := range 25 {
		if i > 0 {
			filler.WriteByte(',')
		}
		filler.WriteString(`{"id":"900` + string(rune('0'+i%10)) + string(rune('a'+i)) +
			`","comment_text":"filler","date":"17911000000` + string(rune('0'+i%10)) + `","user":null}`)
	}
	filler.WriteString(`]}`)

	p, api := newHydrating(t)
	api.pages = [][]byte{[]byte(filler.String()), fixture(t, "api_comments.json")}
	_, h := hydrate(t, p, fixture(t, "webhook_comment_posted.json"))
	if h == nil {
		t.Fatal("no object")
	}
	seen := api.seen()
	if len(seen) != 3 {
		t.Fatalf("made %d requests, want 3 (the task and two pages)", len(seen))
	}
	q := seen[2].URL.Query()
	if q.Get("start_id") == "" || q.Get("start") == "" {
		t.Errorf("the second page was asked for without start and start_id: %v", seen[2].URL.RawQuery)
	}
}

// A comment that is not in the pages the search will read is ErrNotFound, never a record built
// out of the wrong comment.
func TestHydrateRefusesACommentItCannotFind(t *testing.T) {
	p, api := newHydrating(t)
	api.commentsBody = []byte(`{"comments":[{"id":"someoneelse","comment_text":"not it","date":"1791100600000"}]}`)
	changes, err := p.Parse(fixture(t, "webhook_comment_posted.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Hydrate(context.Background(), testTenant, changes[0]); !errors.Is(err, clickup.ErrNotFound) {
		t.Fatalf("Hydrate: %v, want ErrNotFound", err)
	}
}

// The rate limiter refuses a request above its capacity rather than waiting for one, and says so
// by name, so that the drain puts the delivery back on the ladder and gets its goroutine back.
func TestTheRateLimiterRefusesAboveItsCapacity(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	p, api := newHydrating(t, func(o *clickup.Options) {
		o.PerMinute = 60 // one token a second
		o.Burst = 2
		o.Now = clock
	})
	changes, err := p.Parse(fixture(t, "webhook_task_updated.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, err := p.Hydrate(context.Background(), testTenant, changes[0]); err != nil {
			t.Fatalf("hydration %d of the burst: %v", i, err)
		}
	}
	if _, err := p.Hydrate(context.Background(), testTenant, changes[0]); !errors.Is(err, clickup.ErrRateLimited) {
		t.Fatalf("the third hydration: %v, want ErrRateLimited", err)
	}
	if n := len(api.seen()); n != 2 {
		t.Errorf("the API saw %d requests, want 2: a refused request must not be made", n)
	}
	// It refuses, it does not wait: the clock has not moved, and the call came back.
	now = now.Add(time.Second)
	if _, err := p.Hydrate(context.Background(), testTenant, changes[0]); err != nil {
		t.Fatalf("after a second of refill: %v", err)
	}
}

// The bucket never gives out more than its burst, however long it has been idle.
func TestTheRateLimiterDoesNotHoardTokens(t *testing.T) {
	now := time.Now()
	p, api := newHydrating(t, func(o *clickup.Options) {
		o.PerMinute = 60
		o.Burst = 2
		o.Now = func() time.Time { return now }
	})
	changes, err := p.Parse(fixture(t, "webhook_task_updated.json"))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	for i := range 2 {
		if _, err := p.Hydrate(context.Background(), testTenant, changes[0]); err != nil {
			t.Fatalf("hydration %d after an idle hour: %v", i, err)
		}
	}
	if _, err := p.Hydrate(context.Background(), testTenant, changes[0]); !errors.Is(err, clickup.ErrRateLimited) {
		t.Fatalf("a third hydration after an idle hour: %v, want ErrRateLimited", err)
	}
	if n := len(api.seen()); n != 2 {
		t.Errorf("the API saw %d requests, want 2", n)
	}
}

// An answer bigger than the client will read is refused rather than held in memory. The size of
// a response is the remote system's to choose, and a drain works several deliveries at once.
func TestAnOversizeAnswerIsRefused(t *testing.T) {
	p, api := newHydrating(t)
	// Valid JSON, and over the four megabyte cap.
	api.taskBody = append(append([]byte(`{"id":"86a1b2","name":"`), bytes.Repeat([]byte("a"), 5<<20)...),
		[]byte(`","date_updated":"1791100000000","list":{"id":"901100"}}`)...)
	changes, err := p.Parse(fixture(t, "webhook_task_updated.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Hydrate(context.Background(), testTenant, changes[0])
	if !errors.Is(err, clickup.ErrAPI) {
		t.Fatalf("Hydrate: %v, want ErrAPI", err)
	}
	if !strings.Contains(err.Error(), "over") {
		t.Errorf("the error does not say the answer was too large: %v", err)
	}
}

// New refuses an Options that cannot work, at start-up, rather than on the first delivery.
func TestNewRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts clickup.Options
	}{
		{"no tokens", clickup.Options{BaseURL: "https://example.invalid"}},
		{"a relative base url", clickup.Options{Tokens: fixedToken("t"), BaseURL: "/api"}},
		{"a base url with no host", clickup.Options{Tokens: fixedToken("t"), BaseURL: "https://"}},
		{"a base url that is not http", clickup.Options{Tokens: fixedToken("t"), BaseURL: "file:///etc"}},
		{"a base url with a query", clickup.Options{Tokens: fixedToken("t"), BaseURL: "https://example.invalid?a=b"}},
		{"a base url with userinfo", clickup.Options{Tokens: fixedToken("t"), BaseURL: "https://u:p@example.invalid"}},
		{"a negative rate", clickup.Options{Tokens: fixedToken("t"), PerMinute: -1}},
		{"a negative burst", clickup.Options{Tokens: fixedToken("t"), Burst: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := clickup.New(tc.opts); err == nil {
				t.Fatal("accepted it")
			}
		})
	}
}

// The defaults are the documented ones, and a provider built with nothing but a Tokens works.
func TestNewDefaults(t *testing.T) {
	p, err := clickup.New(clickup.Options{Tokens: fixedToken("t")})
	if err != nil {
		t.Fatal(err)
	}
	if p.Key() != clickup.Key {
		t.Errorf("key %q", p.Key())
	}
	if _, err := url.Parse(clickup.DefaultBaseURL); err != nil {
		t.Errorf("the default base URL does not parse: %v", err)
	}
	if clickup.DefaultPerMinute != 100 {
		t.Errorf("the default rate is %d, and ClickUp's documented floor is 100 a minute", clickup.DefaultPerMinute)
	}
}
