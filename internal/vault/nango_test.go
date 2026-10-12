package vault_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/vault"
)

// nangoSecret is the secret key the fake Nango demands, and the one the vault is configured with.
// It is a throwaway string of this file's own.
const nangoSecret = "lawang-test-nango-secret-9d4c1b7e"

// fakeNango stands in for a self-hosted Nango. It answers the two calls this package makes, and
// it refuses what the real one refuses, because a double that accepts more than the real system
// lets a missing credential or a wrong path pass as a working vault (CLAUDE.md).
type fakeNango struct {
	t *testing.T

	mu       sync.Mutex
	requests []recordedRequest
	// connections is what it holds, keyed by connection id and provider config key.
	connections map[string]string
	// status and body, when set, replace the answer for every request: the outage cases.
	status int
	body   string
}

// recordedRequest is what a fake server saw, kept so a test can assert about the URL and the
// headers. The body is not kept: on these paths there is none.
type recordedRequest struct {
	method, path, rawQuery, authorization, url string
}

func newFakeNango(t *testing.T) *fakeNango {
	return &fakeNango{t: t, connections: map[string]string{}}
}

// hold makes the fake answer for one connection of providerKey with the given credentials object.
func (f *fakeNango) hold(connectionID, credentials string) {
	providerConfigKey := providerKey
	f.connections[connectionID+"|"+providerConfigKey] = fmt.Sprintf(
		`{"id":7,"connection_id":%q,"provider_config_key":%q,"provider":"clickup",`+
			`"created_at":"2026-10-12T00:00:00.000Z","credentials":%s}`,
		connectionID, providerConfigKey, credentials)
}

func (f *fakeNango) seen() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func (f *fakeNango) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		method:        r.Method,
		path:          r.URL.Path,
		rawQuery:      r.URL.RawQuery,
		authorization: r.Header.Get("Authorization"),
		url:           r.URL.String(),
	})
	f.mu.Unlock()

	// What the real Nango refuses. The secret key is compared whole: a double that accepted any
	// bearer would let a vault that forgot the header pass.
	if r.Header.Get("Authorization") != "Bearer "+nangoSecret {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, ok := strings.CutPrefix(r.URL.Path, "/connection/")
	if !ok || id == "" || strings.Contains(id, "/") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	key := r.URL.Query().Get("provider_config_key")
	if key == "" {
		// Nango needs to be told which integration the connection belongs to.
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if f.status != 0 {
		if f.status >= 300 && f.status <= 399 {
			// A redirect with somewhere to go, so that a client which followed one would really
			// make a second request. Without the Location header, Go would not follow it and the
			// "a redirect is not followed" case would pass for a client that does follow them.
			w.Header().Set("Location", "/connection/somebody-else?provider_config_key="+otherProvider)
		}
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
		return
	}
	body, held := f.connections[id+"|"+key]
	if !held {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method == http.MethodDelete {
		delete(f.connections, id+"|"+key)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// serving starts the fake and returns a Nango vault pointed at it.
func (f *fakeNango) serving(t *testing.T) *vault.Nango {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	v, err := vault.NewNango(vault.NangoOptions{
		BaseURL:    srv.URL,
		SecretKey:  nangoSecret,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewNango: %v", err)
	}
	return v
}

func TestNangoFetchHandsOutTheConnectionsCredential(t *testing.T) {
	t.Parallel()
	// One case per credential type Nango's connection API returns, with Nango's own spelling of
	// each member, including the one that is not snake case (apiKey).
	for name, tc := range map[string]struct {
		credentials string
		want        map[string]string
	}{
		"oauth2": {
			`{"type":"OAUTH2","access_token":"` + marker + `","expires_at":"2026-10-12T01:00:00.000Z"}`,
			map[string]string{"access_token": marker},
		},
		"api key": {
			`{"type":"API_KEY","apiKey":"` + marker + `"}`,
			map[string]string{"api_key": marker},
		},
		"basic": {
			`{"type":"BASIC","username":"a-user","password":"` + marker + `"}`,
			map[string]string{"username": "a-user", "password": marker},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeNango(t)
			f.hold(tenantA.String(), tc.credentials)
			v := f.serving(t)

			cred, err := v.Fetch(context.Background(), tenantA, providerKey)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			for field, want := range tc.want {
				got, err := cred.Field(field)
				if err != nil || got != want {
					t.Errorf("Field(%q) = %q, %v, want %q", field, got, err, want)
				}
			}
			// The connection id is the tenant and the provider config key is the provider key:
			// the mapping is the whole of this vault's addressing, so it is asserted and not
			// assumed.
			seen := f.seen()
			if len(seen) != 1 {
				t.Fatalf("the fake saw %d requests, want one", len(seen))
			}
			if want := "/connection/" + tenantA.String(); seen[0].path != want {
				t.Errorf("path = %q, want %q", seen[0].path, want)
			}
			if want := "provider_config_key=" + providerKey; seen[0].rawQuery != want {
				t.Errorf("query = %q, want %q", seen[0].rawQuery, want)
			}
			if seen[0].method != http.MethodGet {
				t.Errorf("method = %q, want GET", seen[0].method)
			}
		})
	}
}

// TestNangoRefusesAnAnswerAboutAnotherConnection is the guard that matters most here: Nango is a
// system in the middle, and an answer about another connection would otherwise hand one tenant
// another tenant's token.
func TestNangoRefusesAnAnswerAboutAnotherConnection(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"another tenant": fmt.Sprintf(
			`{"connection_id":%q,"provider_config_key":%q,"credentials":{"type":"OAUTH2","access_token":%q}}`,
			tenantB.String(), providerKey, marker),
		"another provider config key": fmt.Sprintf(
			`{"connection_id":%q,"provider_config_key":%q,"credentials":{"type":"OAUTH2","access_token":%q}}`,
			tenantA.String(), otherProvider, marker),
		"no identifiers at all": fmt.Sprintf(
			`{"credentials":{"type":"OAUTH2","access_token":%q}}`, marker),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeNango(t)
			// The fake holds the connection that was asked for, and answers with the wrong
			// document: the mix-up is Nango's, which is the case being tested.
			f.hold(tenantA.String(), `{"type":"OAUTH2","access_token":"unused"}`)
			f.status, f.body = http.StatusOK, body
			v := f.serving(t)

			got, err := v.Fetch(context.Background(), tenantA, providerKey)
			if err == nil {
				t.Fatalf("Fetch accepted an answer about another connection: %v", got)
			}
			if !errors.Is(err, vault.ErrUnavailable) {
				t.Errorf("err = %v, want ErrUnavailable", err)
			}
			if !got.IsZero() {
				t.Errorf("a refusal also returned a credential: %v", got)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("the refusal carries the token: %v", err)
			}
		})
	}
}

func TestNangoRefusesAnAnswerThatIsNotAUsableCredential(t *testing.T) {
	t.Parallel()
	for name, credentials := range map[string]string{
		"a type this vault does not know": `{"type":"TBA","token":"` + marker + `"}`,
		"no type":                         `{"access_token":"` + marker + `"}`,
		"an empty access token":           `{"type":"OAUTH2","access_token":""}`,
		"no access token at all":          `{"type":"OAUTH2"}`,
		"a basic half":                    `{"type":"BASIC","username":"a-user"}`,
		"an empty api key":                `{"type":"API_KEY","apiKey":""}`,
		// A token with a newline on the end is a value a system in the middle really does hand
		// over: Nango holds what an operator pasted into it. It is refused here rather than
		// handed to a provider's client, where net/http would refuse it as a header value and
		// the failure would read as a provider that could not be reached.
		"an access token with a newline on the end": `{"type":"OAUTH2","access_token":"` + marker + `\n"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeNango(t)
			f.hold(tenantA.String(), credentials)
			v := f.serving(t)

			got, err := v.Fetch(context.Background(), tenantA, providerKey)
			if err == nil {
				t.Fatalf("Fetch accepted it: %v", got)
			}
			if !errors.Is(err, vault.ErrUnavailable) {
				t.Errorf("err = %v, want ErrUnavailable", err)
			}
			if !got.IsZero() {
				t.Errorf("a refusal also returned a credential: %v", got)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("the refusal carries the token: %v", err)
			}
		})
	}
}

func TestNangoFetchOfAConnectionNangoDoesNotHaveIsNotFound(t *testing.T) {
	t.Parallel()
	f := newFakeNango(t)
	f.hold(tenantA.String(), `{"type":"OAUTH2","access_token":"`+marker+`"}`)
	v := f.serving(t)

	// Nothing is held for this tenant, and the connection that IS held is somebody else's: a 404
	// is "nothing is stored for you" and never a fall back to another connection.
	got, err := v.Fetch(context.Background(), tenantB, providerKey)
	if !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if !got.IsZero() {
		t.Errorf("a refusal also returned a credential: %v", got)
	}
}

func TestNangoRevokeRemovesTheConnection(t *testing.T) {
	t.Parallel()
	f := newFakeNango(t)
	f.hold(tenantA.String(), `{"type":"OAUTH2","access_token":"`+marker+`"}`)
	v := f.serving(t)
	ctx := context.Background()

	if err := v.Revoke(ctx, tenantA, providerKey); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := v.Fetch(ctx, tenantA, providerKey); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("Fetch after Revoke: %v, want ErrNotFound", err)
	}
	// Revoking again is a refusal, as it is in every other vault here.
	if err := v.Revoke(ctx, tenantA, providerKey); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("revoking nothing: %v, want ErrNotFound", err)
	}
	seen := f.seen()
	if len(seen) != 3 {
		t.Fatalf("the fake saw %d requests, want three", len(seen))
	}
	if seen[0].method != http.MethodDelete {
		t.Errorf("the revoke was a %s, want DELETE", seen[0].method)
	}
}

func TestNangoStoreIsARefusalAndNotASilentSuccess(t *testing.T) {
	t.Parallel()
	f := newFakeNango(t)
	v := f.serving(t)

	err := v.Store(context.Background(), tenantA, providerKey, testCredential(t))
	if !errors.Is(err, vault.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if got := f.seen(); len(got) != 0 {
		t.Errorf("the refusal still talked to Nango: %v", got)
	}
}

// TestNangoNeverPutsTheSecretKeyInTheURLOrAnError is the rule for the one credential this vault
// holds itself. It goes in a header, because a URL reaches an access log, a proxy and the text of
// a transport error.
func TestNangoNeverPutsTheSecretKeyInTheURLOrAnError(t *testing.T) {
	t.Parallel()
	f := newFakeNango(t)
	f.hold(tenantA.String(), `{"type":"OAUTH2","access_token":"`+marker+`"}`)
	v := f.serving(t)
	ctx := context.Background()

	if _, err := v.Fetch(ctx, tenantA, providerKey); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// An answer that is not the one asked for, so there is an error to inspect as well.
	f.status, f.body = http.StatusInternalServerError, "Nango says: "+nangoSecret+" "+marker
	_, err := v.Fetch(ctx, tenantA, providerKey)
	if err == nil {
		t.Fatal("a 500 was accepted")
	}
	if !errors.Is(err, vault.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	// The status is in the message, because a 401 is an operator's job and an outage is not.
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("the refusal does not say what came back: %v", err)
	}
	// Nothing of the body is, because a remote system's text on this path is a token's neighbour.
	for _, leak := range []string{nangoSecret, marker, "Nango says"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the refusal carries %q: %v", leak, err)
		}
	}
	for _, seen := range f.seen() {
		if strings.Contains(seen.url, nangoSecret) {
			t.Errorf("the secret key reached the URL: %s", seen.url)
		}
		if seen.authorization != "Bearer "+nangoSecret {
			t.Errorf("the secret key did not reach the Authorization header: %q", seen.authorization)
		}
	}
}

func TestNangoRefusesEveryStatusThatIsNotTheOneAskedFor(t *testing.T) {
	t.Parallel()
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest,
		http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusFound,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			f := newFakeNango(t)
			f.hold(tenantA.String(), `{"type":"OAUTH2","access_token":"`+marker+`"}`)
			f.status, f.body = status, `{"error":"a body nothing should read"}`
			v := f.serving(t)

			got, err := v.Fetch(context.Background(), tenantA, providerKey)
			if err == nil {
				t.Fatalf("status %d was accepted: %v", status, got)
			}
			if !errors.Is(err, vault.ErrUnavailable) {
				t.Errorf("err = %v, want ErrUnavailable", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(status)) {
				t.Errorf("the refusal does not name the status: %v", err)
			}
			if strings.Contains(err.Error(), "nothing should read") {
				t.Errorf("the refusal carries the body: %v", err)
			}
			// A 302 is not followed: the Authorization header would travel with it, or be
			// dropped silently, and neither belongs on a path that carries the secret key.
			if status == http.StatusFound && len(f.seen()) != 1 {
				t.Errorf("the redirect was followed: %d requests", len(f.seen()))
			}
		})
	}
}

// TestNangoBoundsWhatItReads keeps a remote system from choosing how much memory a credential
// lookup costs. The answer is cut at the bound, so what is left is not a document, and the
// refusal is a refusal rather than a process that grew.
func TestNangoBoundsWhatItReads(t *testing.T) {
	t.Parallel()
	f := newFakeNango(t)
	f.hold(tenantA.String(), `{"type":"OAUTH2","access_token":"`+marker+`"}`)
	f.status = http.StatusOK
	f.body = `{"connection_id":"` + tenantA.String() + `","padding":"` +
		strings.Repeat("x", vault.MaxNangoResponse+1024) + `"}`
	v := f.serving(t)

	got, err := v.Fetch(context.Background(), tenantA, providerKey)
	if err == nil {
		t.Fatalf("an answer larger than the bound was accepted: %v", got)
	}
	if !errors.Is(err, vault.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	// Which layer refused it: the document was cut at the bound, so it is not a document any
	// more. Without this the test would also pass on the credentials-type guard, which is a
	// different line and would leave the bound itself unproven.
	if !strings.Contains(err.Error(), "not a connection document") {
		t.Errorf("the refusal is not the one the bound produces: %v", err)
	}
}

func TestNangoRefusesACancelledLookupWithoutGuessing(t *testing.T) {
	t.Parallel()
	f := newFakeNango(t)
	f.hold(tenantA.String(), `{"type":"OAUTH2","access_token":"`+marker+`"}`)
	v := f.serving(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := v.Fetch(ctx, tenantA, providerKey)
	if err == nil {
		t.Fatalf("a cancelled lookup returned a credential: %v", got)
	}
	if !errors.Is(err, vault.ErrUnavailable) || !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want ErrUnavailable and context.Canceled", err)
	}
}

func TestNewNangoRefusesOptionsItCannotUse(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]vault.NangoOptions{
		"no base URL":             {SecretKey: nangoSecret},
		"a relative base URL":     {BaseURL: "/connection", SecretKey: nangoSecret},
		"a base URL with no host": {BaseURL: "http://", SecretKey: nangoSecret},
		"another scheme":          {BaseURL: "ftp://nango.example.test", SecretKey: nangoSecret},
		"a query":                 {BaseURL: "https://nango.example.test?a=1", SecretKey: nangoSecret},
		"a fragment":              {BaseURL: "https://nango.example.test#a", SecretKey: nangoSecret},
		"credentials in the URL":  {BaseURL: "https://user:pass@nango.example.test", SecretKey: nangoSecret},
		"no secret key":           {BaseURL: "https://nango.example.test"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			v, err := vault.NewNango(opts)
			if err == nil {
				t.Fatal("the options were accepted")
			}
			if v != nil {
				t.Error("a refusal also returned a vault")
			}
			// A base URL pasted from the wrong place can be a URL with a password in it.
			for _, leak := range []string{"user:pass", "pass", nangoSecret} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("the refusal echoes %q: %v", leak, err)
				}
			}
		})
	}
	// http is accepted on purpose: a self-hosted Nango is usually reached inside a cluster.
	if _, err := vault.NewNango(vault.NangoOptions{BaseURL: "http://nango.internal:3003/", SecretKey: nangoSecret}); err != nil {
		t.Errorf("an http base URL was refused: %v", err)
	}
}

func TestNangoRefusesWhatItCannotAddress(t *testing.T) {
	t.Parallel()
	f := newFakeNango(t)
	v := f.serving(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		tenant   tenancy.ID
		provider string
	}{
		"no tenant":              {"", providerKey},
		"the sentinel tenant":    {tenancy.Sentinel, providerKey},
		"no provider":            {tenantA, ""},
		"a provider in capitals": {tenantA, "ClickUp"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Fetch(ctx, tc.tenant, tc.provider); !errors.Is(err, vault.ErrInvalidArgument) {
				t.Errorf("Fetch: %v, want ErrInvalidArgument", err)
			}
			if err := v.Revoke(ctx, tc.tenant, tc.provider); !errors.Is(err, vault.ErrInvalidArgument) {
				t.Errorf("Revoke: %v, want ErrInvalidArgument", err)
			}
			if err := v.Store(ctx, tc.tenant, tc.provider, testCredential(t)); !errors.Is(err, vault.ErrInvalidArgument) {
				t.Errorf("Store: %v, want ErrInvalidArgument", err)
			}
		})
	}
	if got := f.seen(); len(got) != 0 {
		t.Errorf("a refused lookup still talked to Nango: %v", got)
	}
}

// TestTheFakeNangoRefusesWhatNangoRefuses holds the double to the rule in CLAUDE.md: a test
// double must reject whatever the real system rejects. Without the first case, a vault that
// forgot the Authorization header would pass every test in this file.
func TestTheFakeNangoRefusesWhatNangoRefuses(t *testing.T) {
	t.Parallel()
	f := newFakeNango(t)
	f.hold(tenantA.String(), `{"type":"OAUTH2","access_token":"`+marker+`"}`)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for name, tc := range map[string]struct {
		url, bearer string
		want        int
	}{
		"no bearer":              {srv.URL + "/connection/" + tenantA.String() + "?provider_config_key=" + providerKey, "", http.StatusUnauthorized},
		"the wrong bearer":       {srv.URL + "/connection/" + tenantA.String() + "?provider_config_key=" + providerKey, "Bearer wrong", http.StatusUnauthorized},
		"no provider config key": {srv.URL + "/connection/" + tenantA.String(), "Bearer " + nangoSecret, http.StatusBadRequest},
		"no connection id":       {srv.URL + "/connection/", "Bearer " + nangoSecret, http.StatusNotFound},
		"an unknown connection":  {srv.URL + "/connection/nobody?provider_config_key=" + providerKey, "Bearer " + nangoSecret, http.StatusNotFound},
		"the right request":      {srv.URL + "/connection/" + tenantA.String() + "?provider_config_key=" + providerKey, "Bearer " + nangoSecret, http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, tc.url, http.NoBody)
			if err != nil {
				t.Fatalf("build the request: %v", err)
			}
			if tc.bearer != "" {
				req.Header.Set("Authorization", tc.bearer)
			}
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
