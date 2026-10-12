package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gablooge/lawang/internal/tenancy"
)

// DefaultAzureAuthority is where Microsoft's identity platform issues tokens. A deployment in a
// national cloud points this somewhere else (for example https://login.microsoftonline.us).
const DefaultAzureAuthority = "https://login.microsoftonline.com"

// DefaultGraphScope is the scope a client-credentials token is asked for: every application
// permission the app registration has been granted, which is what .default means
// (https://learn.microsoft.com/entra/identity-platform/v2-oauth2-client-creds-grant).
const DefaultGraphScope = "https://graph.microsoft.com/.default"

// DefaultAzureTimeout bounds one request to the identity platform.
const DefaultAzureTimeout = 15 * time.Second

// azureTokenPath is appended to the authority and the directory id. It is a constant and not an
// option: a deployment that could choose it could post a client secret to a path of its own.
const azureTokenPath = "/oauth2/v2.0/token" //nolint:gosec // G101: a URL path of Microsoft's, not a credential

// maxAzureResponse bounds what is read from the identity platform. A token response is a few
// kilobytes, and an access token with a long claim set is larger, so 256 KiB is generous.
const maxAzureResponse = 256 << 10

// azureExpiryLeeway is taken off a token's lifetime before it is cached. A token that expires
// while it is in flight is a 401 the caller cannot distinguish from a revoked permission, so the
// cache gives it up well before the issuer does.
const azureExpiryLeeway = 5 * time.Minute

// maxAzureTokenLifetime bounds what the authority is allowed to say a token is good for. Entra
// issues client-credentials tokens with an hour on them, so a day is generous and still a bound.
//
// Without a ceiling the authority chooses how long this process keeps a secret in memory, because
// the leeway only ever subtracts five minutes from whatever the answer said: an expires_in of a
// year would be a token handed out for a year, long after it had been revoked. The arithmetic is
// the other half. time.Duration counts nanoseconds in an int64, so expires_in * time.Second wraps
// above 9223372036 seconds (about 292 years), and the wrap is modulo 2^64, so the product can land
// negative (math.MaxInt64 gives -1s) or positive again: neither the sign nor the size of what
// would be cached follows from what was answered. Everything else this vault reads from a remote
// system is bounded (the response size, the token type, the connection it is about), and this was
// the one number that was not.
const maxAzureTokenLifetime = 24 * time.Hour

// maxAzureLifetimeSeconds is maxAzureTokenLifetime in the unit expires_in is written in, so the
// comparison is against the same constant the documentation and the tests name.
const maxAzureLifetimeSeconds = int64(maxAzureTokenLifetime / time.Second)

// AzureRegistration is one tenant's app registration for one provider: the client credentials
// Lawang authenticates to Microsoft Graph with on that tenant's behalf.
//
// Every member is required but Scope. The secret is secret material: it goes in one place, the
// form body of a request to the authority, and it is never in an error, a log line or a URL.
type AzureRegistration struct {
	// Tenant is the Lawang tenant this registration belongs to.
	Tenant tenancy.ID
	// Provider is the Lawang provider key the registration serves (the Microsoft Graph providers,
	// which are backlog items B16 and B17).
	Provider string
	// Directory is the Entra directory the app lives in: its id (a GUID) or its verified domain.
	// It is one path segment of the token URL, so it is held to what a single segment can be.
	Directory string
	// ClientID is the app registration's application (client) id.
	ClientID string
	// ClientSecret is the app registration's client secret.
	//
	//nolint:gosec // G117: it is a secret on purpose. This struct is configuration and is never
	// marshalled to JSON. The secret goes in one place, the form body of the token request, and
	// no error or log line in this package carries it, which
	// TestAzureAppNeverPutsTheClientSecretInTheURLOrAnError holds it to.
	ClientSecret string
	// Scope is what the token is asked for. Empty means DefaultGraphScope.
	Scope string
}

// AzureAppOptions configure an [AzureApp] vault.
type AzureAppOptions struct {
	// Apps is one entry per (tenant, provider) this vault answers for. A vault with none answers
	// nothing, so NewAzureApp refuses an empty list.
	Apps []AzureRegistration
	// Authority is where tokens are issued, without a trailing slash. Empty means
	// DefaultAzureAuthority.
	Authority string
	// HTTPClient talks to the authority. Nil means a client of this package's own, with
	// DefaultAzureTimeout and redirects turned off.
	HTTPClient *http.Client
	// Now is the clock the token cache reads. Nil means time.Now.
	Now func() time.Time
}

// azureKey addresses one app registration, and one cache entry. It is a (tenant, provider) pair
// and not a tenant alone: one tenant may have an app per provider, and a cache keyed by tenant
// would hand a Teams token to an Outlook lookup, with a scope that does not cover it.
type azureKey struct {
	tenant   tenancy.ID
	provider string
}

// azureToken is a cached access token and the moment it stops being handed out.
type azureToken struct {
	value   string
	expires time.Time
}

// AzureApp is the vault for Microsoft Graph: the credential is an app registration's client
// credentials, which live in this deployment's configuration, and what the vault hands out is an
// access token it fetched with them (the client-credentials grant).
//
// It stores nothing and it is therefore not a place a credential can be put: Store and Revoke
// refuse by name. What it does hold is a cache of access tokens, in memory, per (tenant,
// provider), given up before the issuer expires them.
//
// # It has no consumer in v0.1.0
//
// The items that use it (B16 Teams and B17 Outlook) are the ones the scope cut of 2026-10-03
// deferred, so this vault ships with nothing behind it. Whether it ships at all is the
// maintainer's call; item B13 carries it unchanged until that call is made (docs/backlog.md, the
// cut list). Nothing else in the program imports it, and its tests run against a fake authority.
type AzureApp struct {
	apps      map[azureKey]AzureRegistration
	authority string
	client    *http.Client
	now       func() time.Time

	mu     sync.Mutex
	tokens map[azureKey]azureToken
}

// NewAzureApp returns the Microsoft Graph vault, or refuses an option it cannot use.
func NewAzureApp(opts AzureAppOptions) (*AzureApp, error) {
	authority := strings.TrimSuffix(opts.Authority, "/")
	if authority == "" {
		authority = DefaultAzureAuthority
	}
	u, err := url.Parse(authority)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" ||
		u.Fragment != "" || u.User != nil {
		// https only, unlike Nango's base URL: the identity platform is on the public internet,
		// this is the one request in the program that carries a client secret in its body, and no
		// deployment reaches login.microsoftonline.com over plain HTTP. A test points it at a
		// local https server.
		return nil, errors.New("vault: NewAzureApp needs an Authority that is an absolute https URL with no query, fragment or credentials")
	}
	if len(opts.Apps) == 0 {
		return nil, errors.New("vault: NewAzureApp needs at least one app registration")
	}
	apps := make(map[azureKey]AzureRegistration, len(opts.Apps))
	for i, app := range opts.Apps {
		if err := checkArgs(app.Tenant, app.Provider); err != nil {
			return nil, fmt.Errorf("vault: NewAzureApp: app %d: %w", i, err)
		}
		if err := app.check(); err != nil {
			return nil, fmt.Errorf("vault: NewAzureApp: app %d: %w", i, err)
		}
		key := azureKey{tenant: app.Tenant, provider: app.Provider}
		if _, dup := apps[key]; dup {
			// Two registrations for one tenant and provider: there would be no answer to which
			// one a lookup means, and the map would silently keep the last.
			return nil, fmt.Errorf("vault: NewAzureApp: app %d: a second registration for tenant %q and provider %q",
				i, app.Tenant.String(), app.Provider)
		}
		if app.Scope == "" {
			app.Scope = DefaultGraphScope
		}
		apps[key] = app
	}
	client := &http.Client{Timeout: DefaultAzureTimeout}
	if opts.HTTPClient != nil {
		// A copy, for the reason NewNango copies one: the redirect policy below is this
		// package's, and the caller's client is left as it was.
		copied := *opts.HTTPClient
		client = &copied
	}
	// A redirect on a request whose body carries a client secret is never followed, whoever built
	// the client (TestAzureAppDoesNotFollowARedirect).
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &AzureApp{
		apps:      apps,
		authority: authority,
		client:    client,
		now:       now,
		tokens:    make(map[azureKey]azureToken),
	}, nil
}

// AzureApp is a Vault, checked at compile time.
var _ Vault = (*AzureApp)(nil)

// check holds one app registration to what can be used. Its messages name a member and never a
// value: one of them is a client secret, and a caller that put a secret in the wrong member must
// not have it echoed into a starting process's log.
func (a AzureRegistration) check() error {
	switch {
	case a.Directory == "":
		return errors.New("a registration needs a Directory (the Entra directory id or domain)")
	case !validAzureDirectory(a.Directory):
		return errors.New("the Directory must be one URL path segment: letters, digits, dots and hyphens, at most 128 of them")
	case a.ClientID == "":
		return errors.New("a registration needs a ClientID")
	case a.ClientSecret == "":
		// Fail closed at construction: the client-credentials grant has nothing else to prove.
		return errors.New("a registration needs a ClientSecret")
	case a.Scope != "" && !validAzureScope(a.Scope):
		return errors.New("the Scope must be a single scope value: no space, no control character, at most 256 bytes")
	}
	return nil
}

// validAzureDirectory holds the directory to what one path segment of the token URL can be.
//
// It is checked and not escaped, on purpose. The directory goes into the path of the URL this
// package posts a client secret to, so a value carrying a slash, a dot-dot segment, a query or a
// fragment could move that request to another path, or to another host's path under a crafted
// authority. Refusing the shape is one rule an operator can read; escaping would make the
// request go somewhere that merely looks wrong.
func validAzureDirectory(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	if s == "." || s == ".." || strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '.':
		default:
			return false
		}
	}
	return true
}

// validAzureScope holds the scope to one scope value. It reaches the form body, not the URL, so
// the rule is narrower than it has to be on purpose: a space would make it two scopes, and a
// control character has no business in configuration.
func validAzureScope(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7F {
			return false
		}
	}
	return true
}

// Store refuses: the credential this vault uses is an app registration's client secret, which
// lives in configuration and in Entra, and there is nowhere here to write one. An operator
// changes it by rotating the secret in Entra and in the configuration.
func (a *AzureApp) Store(_ context.Context, t tenancy.ID, providerKey string, _ Credential) error {
	if err := checkArgs(t, providerKey); err != nil {
		return err
	}
	return fmt.Errorf("%w: a Microsoft Graph app registration's client secret lives in this deployment's configuration", ErrUnsupported)
}

// Revoke refuses, for the same reason Store does, and says what revoking actually takes. Dropping
// the cached access token here would read as a revocation and be none: the secret would still
// work, and the next fetch would get another token.
func (a *AzureApp) Revoke(_ context.Context, t tenancy.ID, providerKey string) error {
	if err := checkArgs(t, providerKey); err != nil {
		return err
	}
	return fmt.Errorf("%w: revoking a Microsoft Graph credential means rotating the app registration's client secret in Entra and in this deployment's configuration", ErrUnsupported)
}

// Fetch hands out an access token for this tenant's app registration, from the cache when one is
// still good for a while, and otherwise from the identity platform.
//
// A tenant with no registration is ErrNotFound and never a token from somewhere else. The cache
// is keyed by tenant and provider together, so one tenant's token is not an answer to another's
// lookup whatever the scopes happen to be.
func (a *AzureApp) Fetch(ctx context.Context, t tenancy.ID, providerKey string) (Credential, error) {
	if err := checkArgs(t, providerKey); err != nil {
		return Credential{}, err
	}
	key := azureKey{tenant: t, provider: providerKey}
	app, ok := a.apps[key]
	if !ok {
		return Credential{}, fmt.Errorf("%w: %s (no app registration is configured for this tenant and provider)",
			ErrNotFound, providerKey)
	}
	if token, ok := a.cached(key); ok {
		return NewCredential(map[string]string{"access_token": token})
	}
	token, lifetime, err := a.issue(ctx, app)
	if err != nil {
		return Credential{}, err
	}
	cred, err := NewCredential(map[string]string{"access_token": token})
	if err != nil {
		// An empty or unstorable access token: the issuer answered 200 with something that is not
		// a token. It is refused rather than handed on, and nothing is cached.
		return Credential{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	a.remember(key, token, lifetime)
	return cred, nil
}

// cached returns a token that is still good for longer than the leeway.
func (a *AzureApp) cached(key azureKey) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	token, ok := a.tokens[key]
	if !ok || !a.now().Before(token.expires) {
		// Not Before, rather than After, so that a token whose moment is exactly now is given up:
		// the boundary belongs on the side that fetches again.
		return "", false
	}
	return token.value, true
}

// remember caches a token until the leeway before the issuer's expiry. A token whose whole
// lifetime is inside the leeway is not cached at all, which is a fetch per call and still correct.
func (a *AzureApp) remember(key azureKey, token string, lifetime time.Duration) {
	if lifetime <= azureExpiryLeeway {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens[key] = azureToken{value: token, expires: a.now().Add(lifetime - azureExpiryLeeway)}
}

// azureTokenResponse is the identity platform's answer to the client-credentials grant. Nothing
// else of it is read: the id token, the refresh token (which this grant does not issue) and the
// extended expiry are not this vault's business.
type azureTokenResponse struct {
	TokenType string `json:"token_type"`
	ExpiresIn int64  `json:"expires_in"`
	//nolint:gosec // G117: it is the secret this type exists to read. The struct is unexported and
	// is only ever unmarshalled FROM the identity platform's answer; what leaves this package is a
	// Credential, which cannot be marshalled at all.
	AccessToken string `json:"access_token"`
}

// issue performs the client-credentials grant and returns the access token and its lifetime.
//
// The secret is in the form body and never in the URL: a URL reaches an access log, a proxy and
// the text of a transport error. Nothing of the answer's body reaches an error either, because an
// error body from the identity platform carries a correlation id, a timestamp and a description
// of what is wrong with the request, and the request is the one with the secret in it.
func (a *AzureApp) issue(ctx context.Context, app AzureRegistration) (string, time.Duration, error) {
	scope := app.Scope
	if scope == "" {
		scope = DefaultGraphScope
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {app.ClientID},
		"client_secret": {app.ClientSecret},
		"scope":         {scope},
	}
	// The directory has been held to one path segment by check, so this concatenation cannot move
	// the request to another path.
	endpoint := a.authority + "/" + app.Directory + azureTokenPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("%w: the request could not be built", ErrUnavailable)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// G704 (SSRF) is answered by construction rather than suppressed blindly: every byte of
	// endpoint comes from a.authority, which NewAzureApp parsed and held to an absolute https
	// origin with no query, fragment or userinfo, from app.Directory, which check held to one
	// path segment of letters, digits, dots and hyphens, and from azureTokenPath, which is a
	// constant. Nothing a request or an answer carries can reach it.
	resp, err := a.client.Do(req) //nolint:gosec // G704: see above
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", 0, fmt.Errorf("%w: %w", ErrUnavailable, ctxErr)
		}
		return "", 0, fmt.Errorf("%w: the identity platform could not be reached", ErrUnavailable)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAzureResponse))
	if err != nil {
		return "", 0, fmt.Errorf("%w: its answer could not be read", ErrUnavailable)
	}
	if resp.StatusCode != http.StatusOK {
		// The status and nothing else. A 400 or a 401 here is a wrong secret, an expired secret or
		// a consent that was never granted, and the status is what an operator starts from.
		return "", 0, fmt.Errorf("%w: the identity platform answered status %d", ErrUnavailable, resp.StatusCode)
	}
	var doc azureTokenResponse
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", 0, fmt.Errorf("%w: its answer is not a token response", ErrUnavailable)
	}
	if !strings.EqualFold(doc.TokenType, "Bearer") {
		// A token of another type cannot be put in an Authorization: Bearer header, and guessing
		// would send a credential a provider will refuse. The type is quoted and cut: it is a
		// remote system's word.
		return "", 0, fmt.Errorf("%w: it issued a token of type %q", ErrUnavailable, truncate(doc.TokenType, 32))
	}
	switch {
	case doc.ExpiresIn <= 0:
		// A lifetime of zero or less is an answer this vault cannot act on: it would cache
		// nothing and could not say how long the token is good for.
		return "", 0, fmt.Errorf("%w: it issued a token with no lifetime", ErrUnavailable)
	case doc.ExpiresIn > maxAzureLifetimeSeconds:
		// And bounded above, for the reasons on maxAzureTokenLifetime. The number is the
		// authority's own and is not secret, so the message carries it: an operator who meets this
		// is looking at an authority that is not the one they think it is.
		return "", 0, fmt.Errorf("%w: it issued a token good for %d seconds, and this vault keeps one for at most %d",
			ErrUnavailable, doc.ExpiresIn, maxAzureLifetimeSeconds)
	}
	return doc.AccessToken, time.Duration(doc.ExpiresIn) * time.Second, nil
}
