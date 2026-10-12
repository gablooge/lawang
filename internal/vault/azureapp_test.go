package vault_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/vault"
)

// The app registration these tests configure. Every value is a throwaway string of this file's
// own: no value from ~/.config/lawang is read here, and nothing reaches Microsoft (CLAUDE.md).
const (
	azureDirectory    = "11111111-2222-3333-4444-555555555555"
	azureClientID     = "66666666-7777-8888-9999-000000000000"
	azureClientSecret = "lawang-test-client-secret-4f2a8d1c"
	// graphProvider and otherGraphProvider stand for the two Microsoft Graph providers of items
	// B16 and B17. They are provider keys like any other.
	graphProvider      = "teams"
	otherGraphProvider = "outlook"
)

// fakeAuthority stands in for Microsoft's identity platform. It answers the client-credentials
// grant, and it refuses what the real one refuses: the wrong method, a request that is not form
// encoded, a missing or wrong client secret, and a grant type it does not issue.
type fakeAuthority struct {
	t *testing.T

	mu       sync.Mutex
	requests []recordedRequest
	forms    []url.Values
	issued   int
	// token, when not empty, replaces the access token it issues. Otherwise the token names the
	// client id and the scope it was asked for, so a test can tell one registration's token from
	// another's.
	token string
	// lifetime is what it says expires_in is. Zero means an hour.
	lifetime int64
	// status and body, when set, replace the answer: the refusal cases.
	status int
	body   string
	// tokenType, when not empty, replaces "Bearer".
	tokenType string
}

func newFakeAuthority(t *testing.T) *fakeAuthority { return &fakeAuthority{t: t} }

func (f *fakeAuthority) seen() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func (f *fakeAuthority) seenForms() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.forms...)
}

func (f *fakeAuthority) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, url: r.URL.String(),
		authorization: r.Header.Get("Authorization"),
	})
	f.forms = append(f.forms, r.PostForm)
	f.mu.Unlock()

	switch {
	case r.Method != http.MethodPost:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	case r.Header.Get("Content-Type") != "application/x-www-form-urlencoded":
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	case r.PostForm.Get("grant_type") != "client_credentials",
		r.PostForm.Get("client_id") == "",
		r.PostForm.Get("scope") == "":
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
		return
	case r.PostForm.Get("client_secret") != azureClientSecret:
		// What the real one does with a wrong or missing secret, and the reason this double
		// exists: a vault that forgot to send it would otherwise pass.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		return
	}
	if f.status != 0 {
		if f.status >= 300 && f.status <= 399 {
			// A redirect with somewhere to go, so that a client which followed one would really
			// make a second request. Without the Location header, Go would not follow it and the
			// case below would pass for a client that does follow them.
			w.Header().Set("Location", "/another-directory/oauth2/v2.0/token")
		}
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
		return
	}

	f.mu.Lock()
	f.issued++
	f.mu.Unlock()
	token := f.token
	if token == "" {
		token = marker + "-for-" + r.PostForm.Get("client_id") + "-" + r.PostForm.Get("scope")
	}
	tokenType := f.tokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	lifetime := f.lifetime
	if lifetime == 0 {
		lifetime = 3600
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"token_type":%q,"expires_in":%d,"ext_expires_in":%d,"access_token":%q}`,
		tokenType, lifetime, lifetime, token)
}

// clock is a time a test moves by hand, so the cache's expiry is a decision and not a wait.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// serving starts the fake authority over TLS, because this vault accepts no other scheme, and
// returns a vault configured with apps.
func (f *fakeAuthority) serving(t *testing.T, now func() time.Time, apps ...vault.AzureRegistration) *vault.AzureApp {
	t.Helper()
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	if len(apps) == 0 {
		apps = []vault.AzureRegistration{{
			Tenant: tenantA, Provider: graphProvider, Directory: azureDirectory,
			ClientID: azureClientID, ClientSecret: azureClientSecret,
		}}
	}
	v, err := vault.NewAzureApp(vault.AzureAppOptions{
		Apps:       apps,
		Authority:  srv.URL,
		HTTPClient: srv.Client(),
		Now:        now,
	})
	if err != nil {
		t.Fatalf("NewAzureApp: %v", err)
	}
	return v
}

func TestAzureAppFetchIssuesAnAccessTokenWithTheAppsCredentials(t *testing.T) {
	t.Parallel()
	f := newFakeAuthority(t)
	v := f.serving(t, nil)

	cred, err := v.Fetch(context.Background(), tenantA, graphProvider)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	token, err := cred.Field("access_token")
	if err != nil {
		t.Fatalf("Field: %v", err)
	}
	if !strings.HasPrefix(token, marker) {
		t.Errorf("the credential does not carry the issued token")
	}
	if names := cred.Names(); len(names) != 1 || names[0] != "access_token" {
		t.Errorf("the credential holds %v, want access_token alone", names)
	}

	seen := f.seen()
	if len(seen) != 1 {
		t.Fatalf("the authority saw %d requests, want one", len(seen))
	}
	if want := "/" + azureDirectory + "/oauth2/v2.0/token"; seen[0].path != want {
		t.Errorf("path = %q, want %q", seen[0].path, want)
	}
	if seen[0].rawQuery != "" {
		t.Errorf("the request carries a query: %q", seen[0].rawQuery)
	}
	form := f.seenForms()[0]
	if got := form.Get("grant_type"); got != "client_credentials" {
		t.Errorf("grant_type = %q", got)
	}
	if got := form.Get("client_id"); got != azureClientID {
		t.Errorf("client_id = %q", got)
	}
	if got := form.Get("scope"); got != vault.DefaultGraphScope {
		t.Errorf("scope = %q, want the default %q", got, vault.DefaultGraphScope)
	}
}

func TestAzureAppUsesTheScopeTheRegistrationAsksFor(t *testing.T) {
	t.Parallel()
	const scope = "https://graph.microsoft.com/Mail.Read"
	f := newFakeAuthority(t)
	v := f.serving(t, nil, vault.AzureRegistration{
		Tenant: tenantA, Provider: graphProvider, Directory: azureDirectory,
		ClientID: azureClientID, ClientSecret: azureClientSecret, Scope: scope,
	})
	if _, err := v.Fetch(context.Background(), tenantA, graphProvider); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := f.seenForms()[0].Get("scope"); got != scope {
		t.Errorf("scope = %q, want %q", got, scope)
	}
}

// TestAzureAppCachesPerTenantAndProvider is the guard against the mistake a cache invites: one
// tenant's token answering another tenant's lookup. The cache is keyed by the pair, so four
// lookups over two tenants and two providers produce four distinct tokens and four requests, and
// the second round produces none.
func TestAzureAppCachesPerTenantAndProvider(t *testing.T) {
	t.Parallel()
	c := &clock{now: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	f := newFakeAuthority(t)
	apps := []vault.AzureRegistration{
		{Tenant: tenantA, Provider: graphProvider, Directory: azureDirectory, ClientID: "client-a-teams", ClientSecret: azureClientSecret},
		{Tenant: tenantA, Provider: otherGraphProvider, Directory: azureDirectory, ClientID: "client-a-outlook", ClientSecret: azureClientSecret},
		{Tenant: tenantB, Provider: graphProvider, Directory: azureDirectory, ClientID: "client-b-teams", ClientSecret: azureClientSecret},
	}
	v := f.serving(t, c.Now, apps...)
	ctx := context.Background()

	tokens := map[string]string{}
	for _, app := range apps {
		cred, err := v.Fetch(ctx, app.Tenant, app.Provider)
		if err != nil {
			t.Fatalf("Fetch(%s, %s): %v", app.Tenant, app.Provider, err)
		}
		token, err := cred.Field("access_token")
		if err != nil {
			t.Fatalf("Field: %v", err)
		}
		if !strings.Contains(token, app.ClientID) {
			t.Errorf("the token for %s/%s was not issued for %s", app.Tenant, app.Provider, app.ClientID)
		}
		tokens[app.Tenant.String()+"|"+app.Provider] = token
	}
	if len(tokens) != len(apps) {
		t.Fatalf("%d distinct tokens for %d registrations", len(tokens), len(apps))
	}
	if f.issued != len(apps) {
		t.Fatalf("the authority issued %d tokens, want %d", f.issued, len(apps))
	}

	// The second round is served from the cache, and each pair still gets its own token.
	for _, app := range apps {
		cred, err := v.Fetch(ctx, app.Tenant, app.Provider)
		if err != nil {
			t.Fatalf("Fetch(%s, %s): %v", app.Tenant, app.Provider, err)
		}
		token, _ := cred.Field("access_token")
		if want := tokens[app.Tenant.String()+"|"+app.Provider]; token != want {
			t.Errorf("the cached token for %s/%s is another one's", app.Tenant, app.Provider)
		}
	}
	if f.issued != len(apps) {
		t.Errorf("the authority issued %d tokens, want the cache to have answered the second round", f.issued)
	}
}

// TestAzureAppGivesUpACachedTokenBeforeItExpires moves the clock rather than waiting. A token
// that expires in flight is a 401 the caller cannot tell from a revoked permission, so the cache
// gives it up a leeway early.
func TestAzureAppGivesUpACachedTokenBeforeItExpires(t *testing.T) {
	t.Parallel()
	c := &clock{now: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	f := newFakeAuthority(t)
	f.lifetime = int64(time.Hour / time.Second)
	v := f.serving(t, c.Now)
	ctx := context.Background()

	if _, err := v.Fetch(ctx, tenantA, graphProvider); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if f.issued != 1 {
		t.Fatalf("the authority issued %d tokens, want one", f.issued)
	}

	// Still inside the lifetime, and more than the leeway from the end of it: the cache answers.
	c.advance(time.Hour - vault.AzureExpiryLeeway - time.Second)
	if _, err := v.Fetch(ctx, tenantA, graphProvider); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if f.issued != 1 {
		t.Errorf("the authority issued %d tokens, want the cache to have answered", f.issued)
	}

	// Inside the leeway, so the token is given up before the issuer expires it.
	c.advance(2 * time.Second)
	if _, err := v.Fetch(ctx, tenantA, graphProvider); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if f.issued != 2 {
		t.Errorf("the authority issued %d tokens, want a second one once the leeway was reached", f.issued)
	}
}

func TestAzureAppDoesNotCacheATokenThatIsAlreadyInsideTheLeeway(t *testing.T) {
	t.Parallel()
	c := &clock{now: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	f := newFakeAuthority(t)
	// A lifetime shorter than the leeway: caching it would hand out a token the cache itself
	// considers expired, so nothing is cached and every call fetches.
	f.lifetime = int64(vault.AzureExpiryLeeway/time.Second) - 1
	v := f.serving(t, c.Now)
	ctx := context.Background()

	for i := range 3 {
		if _, err := v.Fetch(ctx, tenantA, graphProvider); err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
	}
	if f.issued != 3 {
		t.Errorf("the authority issued %d tokens, want one per call", f.issued)
	}
}

func TestAzureAppFetchOfATenantWithNoRegistrationIsNotFound(t *testing.T) {
	t.Parallel()
	f := newFakeAuthority(t)
	v := f.serving(t, nil)
	ctx := context.Background()

	for name, tc := range map[string]struct {
		tenant   tenancy.ID
		provider string
	}{
		"another tenant":                  {tenantB, graphProvider},
		"another provider of this tenant": {tenantA, otherGraphProvider},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := v.Fetch(ctx, tc.tenant, tc.provider)
			if !errors.Is(err, vault.ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
			if !got.IsZero() {
				t.Errorf("a refusal also returned a credential: %v", got)
			}
		})
	}
	if got := f.seen(); len(got) != 0 {
		t.Errorf("a tenant with no registration still reached the authority: %v", got)
	}
}

func TestAzureAppRefusesAnAnswerThatIsNotAUsableToken(t *testing.T) {
	t.Parallel()
	for name, prepare := range map[string]func(*fakeAuthority){
		"a status that is not 200": func(f *fakeAuthority) {
			f.status, f.body = http.StatusBadRequest, `{"error":"invalid_scope","error_description":`+
				`"AADSTS70011: `+marker+`","correlation_id":"a-correlation-id"}`
		},
		"a body that is not JSON": func(f *fakeAuthority) {
			f.status, f.body = http.StatusOK, "<html>a proxy answered</html>"
		},
		"a token of another type": func(f *fakeAuthority) { f.tokenType = "mac" },
		"no lifetime":             func(f *fakeAuthority) { f.lifetime = -1 },
		"an empty access token":   func(f *fakeAuthority) { f.token = " " },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeAuthority(t)
			prepare(f)
			v := f.serving(t, nil)

			got, err := v.Fetch(context.Background(), tenantA, graphProvider)
			if err == nil {
				t.Fatalf("it was accepted: %v", got)
			}
			if !errors.Is(err, vault.ErrUnavailable) {
				t.Errorf("err = %v, want ErrUnavailable", err)
			}
			if !got.IsZero() {
				t.Errorf("a refusal also returned a credential: %v", got)
			}
			// Nothing of the answer's body: an error from the identity platform describes the
			// request that was made, and that request carries the client secret.
			for _, leak := range []string{marker, "AADSTS70011", "correlation", "a proxy answered", azureClientSecret} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("the refusal carries %q: %v", leak, err)
				}
			}
		})
	}
}

// TestAzureAppNeverPutsTheClientSecretInTheURLOrAnError is the rule for the credential this vault
// authenticates with. It travels in the form body, never in the URL.
func TestAzureAppNeverPutsTheClientSecretInTheURLOrAnError(t *testing.T) {
	t.Parallel()
	f := newFakeAuthority(t)
	v := f.serving(t, nil)
	ctx := context.Background()

	if _, err := v.Fetch(ctx, tenantA, graphProvider); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	for _, seen := range f.seen() {
		if strings.Contains(seen.url, azureClientSecret) {
			t.Errorf("the client secret reached the URL: %s", seen.url)
		}
		if seen.authorization != "" {
			t.Errorf("the client secret went in a header as well: %q", seen.authorization)
		}
	}
	if got := f.seenForms()[0].Get("client_secret"); got != azureClientSecret {
		t.Errorf("the client secret did not reach the form body: %q", got)
	}

	// And an outage, so there is an error to inspect.
	f.status, f.body = http.StatusServiceUnavailable, azureClientSecret
	_, err := v.Fetch(ctx, tenantB, graphProvider)
	if err == nil {
		t.Fatal("a tenant with no registration was served")
	}
	if strings.Contains(err.Error(), azureClientSecret) {
		t.Errorf("the refusal carries the client secret: %v", err)
	}
}

// TestAzureAppDoesNotFollowARedirect is about the one request in the program whose body carries a
// client secret. Go would send that body on to the new location when it stays on one host and
// drop the request when it does not, and this vault answers the 3xx as itself instead.
//
// The client here is the test server's, passed in, which is the case that matters: a redirect
// policy only set on the default client would leave this to whoever hands one in.
func TestAzureAppDoesNotFollowARedirect(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			f := newFakeAuthority(t)
			f.status = status
			v := f.serving(t, nil)

			got, err := v.Fetch(context.Background(), tenantA, graphProvider)
			if err == nil {
				t.Fatalf("a %d was accepted: %v", status, got)
			}
			if !errors.Is(err, vault.ErrUnavailable) {
				t.Errorf("err = %v, want ErrUnavailable", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(status)) {
				t.Errorf("the refusal does not name the status: %v", err)
			}
			if seen := f.seen(); len(seen) != 1 {
				t.Errorf("the redirect was followed: %d requests", len(seen))
			}
		})
	}
}

func TestAzureAppStoreAndRevokeAreRefusals(t *testing.T) {
	t.Parallel()
	f := newFakeAuthority(t)
	v := f.serving(t, nil)
	ctx := context.Background()

	if err := v.Store(ctx, tenantA, graphProvider, testCredential(t)); !errors.Is(err, vault.ErrUnsupported) {
		t.Errorf("Store: %v, want ErrUnsupported", err)
	}
	// Revoke says what revoking actually takes. Dropping the cached token would read as a
	// revocation and be none: the client secret would still work.
	err := v.Revoke(ctx, tenantA, graphProvider)
	if !errors.Is(err, vault.ErrUnsupported) {
		t.Errorf("Revoke: %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "rotating") {
		t.Errorf("the refusal does not say what revoking takes: %v", err)
	}
	if got := f.seen(); len(got) != 0 {
		t.Errorf("a refusal still talked to the authority: %v", got)
	}
}

func TestNewAzureAppRefusesOptionsItCannotUse(t *testing.T) {
	t.Parallel()
	good := vault.AzureRegistration{
		Tenant: tenantA, Provider: graphProvider, Directory: azureDirectory,
		ClientID: azureClientID, ClientSecret: azureClientSecret,
	}
	with := func(f func(*vault.AzureRegistration)) []vault.AzureRegistration {
		app := good
		f(&app)
		return []vault.AzureRegistration{app}
	}
	for name, opts := range map[string]vault.AzureAppOptions{
		"no registrations":              {},
		"an http authority":             {Authority: "http://login.example.test", Apps: []vault.AzureRegistration{good}},
		"a relative authority":          {Authority: "/common", Apps: []vault.AzureRegistration{good}},
		"an authority with a query":     {Authority: "https://login.example.test?a=1", Apps: []vault.AzureRegistration{good}},
		"an authority with credentials": {Authority: "https://user:pass@login.example.test", Apps: []vault.AzureRegistration{good}},
		"no tenant":                     {Apps: with(func(a *vault.AzureRegistration) { a.Tenant = "" })},
		"the sentinel tenant":           {Apps: with(func(a *vault.AzureRegistration) { a.Tenant = tenancy.Sentinel })},
		"no provider":                   {Apps: with(func(a *vault.AzureRegistration) { a.Provider = "" })},
		"a provider in capitals":        {Apps: with(func(a *vault.AzureRegistration) { a.Provider = "Teams" })},
		"no directory":                  {Apps: with(func(a *vault.AzureRegistration) { a.Directory = "" })},
		// The directory is one path segment of the URL a client secret is posted to. Every shape
		// that could move that request somewhere else is refused rather than escaped.
		"a directory with a slash":                      {Apps: with(func(a *vault.AzureRegistration) { a.Directory = "a/b" })},
		"a directory with a parent segment":             {Apps: with(func(a *vault.AzureRegistration) { a.Directory = "../common" })},
		"a directory that is a parent segment":          {Apps: with(func(a *vault.AzureRegistration) { a.Directory = ".." })},
		"a directory with an escape":                    {Apps: with(func(a *vault.AzureRegistration) { a.Directory = "a%2Fb" })},
		"a directory with a query":                      {Apps: with(func(a *vault.AzureRegistration) { a.Directory = "a?b=1" })},
		"a directory with a fragment":                   {Apps: with(func(a *vault.AzureRegistration) { a.Directory = "a#b" })},
		"a directory with a space":                      {Apps: with(func(a *vault.AzureRegistration) { a.Directory = "a b" })},
		"a directory with a newline":                    {Apps: with(func(a *vault.AzureRegistration) { a.Directory = "a\nb" })},
		"a directory that is too long":                  {Apps: with(func(a *vault.AzureRegistration) { a.Directory = strings.Repeat("a", 129) })},
		"no client id":                                  {Apps: with(func(a *vault.AzureRegistration) { a.ClientID = "" })},
		"no client secret":                              {Apps: with(func(a *vault.AzureRegistration) { a.ClientSecret = "" })},
		"a scope with a space":                          {Apps: with(func(a *vault.AzureRegistration) { a.Scope = "a b" })},
		"a scope with a newline":                        {Apps: with(func(a *vault.AzureRegistration) { a.Scope = "a\nb" })},
		"a scope that is too long":                      {Apps: with(func(a *vault.AzureRegistration) { a.Scope = strings.Repeat("a", 257) })},
		"two registrations for one tenant and provider": {Apps: []vault.AzureRegistration{good, good}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			v, err := vault.NewAzureApp(opts)
			if err == nil {
				t.Fatal("the options were accepted")
			}
			if v != nil {
				t.Error("a refusal also returned a vault")
			}
			// No message carries a value: one of these members is a client secret, and an
			// authority pasted from the wrong place can be a URL with a password in it.
			for _, leak := range []string{azureClientSecret, "user:pass"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("the refusal echoes %q: %v", leak, err)
				}
			}
		})
	}

	// And the registration all of those were built from is accepted, so each refusal above is
	// about the one thing it changed.
	if _, err := vault.NewAzureApp(vault.AzureAppOptions{Apps: []vault.AzureRegistration{good}}); err != nil {
		t.Errorf("the registration the cases are built from is refused: %v", err)
	}
	// Two registrations for one tenant under DIFFERENT providers are legitimate: one app per
	// provider is how Microsoft Graph permissions are usually split.
	second := good
	second.Provider = otherGraphProvider
	if _, err := vault.NewAzureApp(vault.AzureAppOptions{Apps: []vault.AzureRegistration{good, second}}); err != nil {
		t.Errorf("two providers of one tenant were refused: %v", err)
	}
}

func TestAzureAppRefusesWhatItCannotAddress(t *testing.T) {
	t.Parallel()
	f := newFakeAuthority(t)
	v := f.serving(t, nil)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		tenant   tenancy.ID
		provider string
	}{
		"no tenant":              {"", graphProvider},
		"the sentinel tenant":    {tenancy.Sentinel, graphProvider},
		"no provider":            {tenantA, ""},
		"a provider in capitals": {tenantA, "Teams"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Fetch(ctx, tc.tenant, tc.provider); !errors.Is(err, vault.ErrInvalidArgument) {
				t.Errorf("Fetch: %v, want ErrInvalidArgument", err)
			}
			if err := v.Store(ctx, tc.tenant, tc.provider, testCredential(t)); !errors.Is(err, vault.ErrInvalidArgument) {
				t.Errorf("Store: %v, want ErrInvalidArgument", err)
			}
			if err := v.Revoke(ctx, tc.tenant, tc.provider); !errors.Is(err, vault.ErrInvalidArgument) {
				t.Errorf("Revoke: %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestAzureAppRefusesACancelledLookup(t *testing.T) {
	t.Parallel()
	f := newFakeAuthority(t)
	v := f.serving(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := v.Fetch(ctx, tenantA, graphProvider)
	if err == nil {
		t.Fatalf("a cancelled lookup returned a credential: %v", got)
	}
	if !errors.Is(err, vault.ErrUnavailable) || !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want ErrUnavailable and context.Canceled", err)
	}
}

// TestTheFakeAuthorityRefusesWhatTheIdentityPlatformRefuses holds the double to CLAUDE.md's rule.
// The client-secret case is the one that matters: without it, a vault that forgot to send the
// secret would pass every test in this file.
func TestTheFakeAuthorityRefusesWhatTheIdentityPlatformRefuses(t *testing.T) {
	t.Parallel()
	f := newFakeAuthority(t)
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	endpoint := srv.URL + "/" + azureDirectory + "/oauth2/v2.0/token"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	full := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {azureClientID},
		"client_secret": {azureClientSecret},
		"scope":         {vault.DefaultGraphScope},
	}
	without := func(key string) url.Values {
		out := url.Values{}
		for k, v := range full {
			if k != key {
				out[k] = v
			}
		}
		return out
	}
	for name, tc := range map[string]struct {
		method      string
		contentType string
		form        url.Values
		want        int
	}{
		"the right request":       {http.MethodPost, "application/x-www-form-urlencoded", full, http.StatusOK},
		"a GET":                   {http.MethodGet, "application/x-www-form-urlencoded", full, http.StatusMethodNotAllowed},
		"not form encoded":        {http.MethodPost, "application/json", full, http.StatusUnsupportedMediaType},
		"no client secret":        {http.MethodPost, "application/x-www-form-urlencoded", without("client_secret"), http.StatusUnauthorized},
		"the wrong client secret": {http.MethodPost, "application/x-www-form-urlencoded", url.Values{"grant_type": {"client_credentials"}, "client_id": {azureClientID}, "client_secret": {"wrong"}, "scope": {vault.DefaultGraphScope}}, http.StatusUnauthorized},
		"no grant type":           {http.MethodPost, "application/x-www-form-urlencoded", without("grant_type"), http.StatusBadRequest},
		"no client id":            {http.MethodPost, "application/x-www-form-urlencoded", without("client_id"), http.StatusBadRequest},
		"no scope":                {http.MethodPost, "application/x-www-form-urlencoded", without("scope"), http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(ctx, tc.method, endpoint, strings.NewReader(tc.form.Encode()))
			if err != nil {
				t.Fatalf("build the request: %v", err)
			}
			req.Header.Set("Content-Type", tc.contentType)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}
