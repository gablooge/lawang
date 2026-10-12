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
	"time"

	"github.com/gablooge/lawang/internal/tenancy"
)

// DefaultNangoTimeout bounds one request to Nango, from dialling to the last byte. A vault lookup
// is on the drain's path, and a vault that does not answer has to fail closed rather than hold a
// worker (architecture section 11).
const DefaultNangoTimeout = 15 * time.Second

// maxNangoResponse bounds what is read from Nango. A connection document is a few kilobytes of
// JSON; 256 KiB is generous and still a bound, because the size of a remote system's answer is
// the remote system's to choose.
const maxNangoResponse = 256 << 10

// nangoConnectionPath is the API path a connection is read at and removed at. It is a constant
// and not an option, for the reason internal/provider/clickup's apiPrefix is one: a deployment
// that could choose it could send its Nango secret key somewhere else.
const nangoConnectionPath = "/connection/"

// NangoOptions configure a [Nango]. BaseURL and SecretKey are required.
type NangoOptions struct {
	// BaseURL is where the self-hosted Nango is, without a trailing slash: an absolute http or
	// https URL with no query, no fragment and no credentials. http is accepted because a
	// self-hosted Nango is often reached inside a cluster, and refusing it would push a
	// deployment to terminate TLS somewhere it cannot.
	BaseURL string
	// SecretKey is Nango's own secret key, which authenticates this deployment to it. It is
	// secret material: it goes in one place, the Authorization header of a request to BaseURL,
	// and it is never in an error, a log line or a URL.
	SecretKey string
	// HTTPClient talks to Nango. Nil means a client of this package's own, with
	// DefaultNangoTimeout and redirects turned off.
	HTTPClient *http.Client
}

// Nango is the vault for a deployment that already runs a self-hosted Nango
// (https://docs.nango.dev): it stores nothing locally and asks Nango for a connection's
// credentials over HTTP.
//
// # What it assumes about Nango, and what is not proven
//
// A tenant's connection in Nango is addressed by a connection id and a provider config key, and
// this vault maps them without any configuration of its own: the connection id is the Lawang
// tenant id, and the provider config key is the Lawang provider key. A deployment therefore names
// its Nango connections the way Lawang names its tenants and providers, and nothing in the middle
// can decide that a lookup for one tenant answers with another's: Fetch refuses an answer whose
// connection id or provider config key is not the one it asked for.
//
// The credential shapes come from Nango's connection API as its documentation describes it
// (OAUTH2, API_KEY and BASIC), and a type this package does not know is a refusal by name and
// never a guess at which member holds the secret. What is NOT proven is the agreement between
// this client and a running Nango: the tests here run against a fake server of this package's
// own, and no live check against a self-hosted instance has been run (see the pull request of
// item B13).
type Nango struct {
	base   string
	secret string
	client *http.Client
}

// NewNango returns the Nango vault, or refuses an option it cannot use. The refusals are at
// construction, not at the first fetch, because every one of them would otherwise be a credential
// lookup that fails on the drain's path for a reason an operator cannot see from there.
func NewNango(opts NangoOptions) (*Nango, error) {
	base := strings.TrimSuffix(opts.BaseURL, "/")
	if base == "" {
		return nil, errors.New("vault: NewNango needs a BaseURL")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" ||
		u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		// No value in the message: a base URL pasted from the wrong place can be a URL with a
		// password in it, and this error reaches the log of a starting process.
		return nil, errors.New("vault: NewNango needs a BaseURL that is an absolute http or https URL with no query, fragment or credentials")
	}
	if opts.SecretKey == "" {
		// Fail closed at construction. A Nango vault with no secret key would send an
		// unauthenticated request per lookup and spend a retry ladder on a wiring mistake.
		return nil, errors.New("vault: NewNango needs a SecretKey")
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: DefaultNangoTimeout,
			// A redirect would be followed with the Authorization header still on the request
			// when it stays on one host, and silently dropped when it does not. Neither belongs
			// on a path that carries the secret key, so a 3xx comes back as itself.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Nango{base: base, secret: opts.SecretKey, client: client}, nil
}

// Nango is a Vault, checked at compile time.
var _ Vault = (*Nango)(nil)

// Store refuses: a Nango connection is created by Nango's own connect flow, which is where the
// end user authorizes it, and this vault holds nothing it could write to. Answering anything else
// would tell an operator a credential is somewhere it is not.
// The context is unused and still a parameter: it is the Vault interface's, and a refusal needs no
// round trip. The tenant and the provider are checked even so, because a caller that addressed
// nothing valid should hear that first, whichever vault it is talking to.
func (n *Nango) Store(_ context.Context, t tenancy.ID, providerKey string, _ Credential) error {
	if err := checkArgs(t, providerKey); err != nil {
		return err
	}
	return fmt.Errorf("%w: a Nango connection is created in Nango, by its own connect flow", ErrUnsupported)
}

// nangoConnection is the part of Nango's connection document this package reads. It is
// deliberately not the whole document: the rest of it (metadata, the provider's raw token
// response) is not needed, and the raw response is another copy of the credential.
//
// It is decoded leniently, because the document carries members this package does not name and a
// Nango release may add more. What is NOT lenient is the credentials type (see nangoCredentials)
// and the two identifiers, which Fetch holds to what it asked for.
type nangoConnection struct {
	ConnectionID      string           `json:"connection_id"`
	ProviderConfigKey string           `json:"provider_config_key"`
	Credentials       nangoCredentials `json:"credentials"`
}

// nangoCredentials is a connection's credentials as Nango's API returns them. The member names are
// Nango's own spelling, which is not uniform (api_key credentials carry "apiKey"), so each one is
// written out rather than derived.
//
// and is only ever unmarshalled FROM Nango's answer; what leaves this package is a Credential,
// which cannot be marshalled at all.
//
//nolint:gosec // G117: these are the secrets this type exists to read. The struct is unexported
type nangoCredentials struct {
	Type        string `json:"type"`
	AccessToken string `json:"access_token"`
	APIKey      string `json:"apiKey"`
	Username    string `json:"username"`
	Password    string `json:"password"`
}

// The Nango credential types this vault knows, and the field names a credential of each type is
// handed out under. A type that is not here is refused by name: guessing which member of an
// unknown shape holds the secret is how a credential reaches a provider as the wrong thing.
const (
	nangoOAuth2 = "OAUTH2"
	nangoAPIKey = "API_KEY"
	nangoBasic  = "BASIC"
)

// credential turns what Nango returned into a Credential, or says why it is not one.
func (c nangoCredentials) credential() (Credential, error) {
	var fields map[string]string
	switch strings.ToUpper(c.Type) {
	case nangoOAuth2:
		fields = map[string]string{"access_token": c.AccessToken}
	case nangoAPIKey:
		fields = map[string]string{"api_key": c.APIKey}
	case nangoBasic:
		fields = map[string]string{"username": c.Username, "password": c.Password}
	default:
		// The type is named. It is a short constant of Nango's vocabulary, not a secret, and an
		// operator needs to know which connection to look at. A type longer than a word of that
		// vocabulary is cut, because it is still a remote system's text.
		return Credential{}, fmt.Errorf("%w: it holds credentials of type %q, which this vault does not know",
			ErrUnavailable, truncate(c.Type, 32))
	}
	// NewCredential is what refuses an empty value, which is how a connection that Nango has but
	// has no usable credentials for (a failed refresh, a revoked grant) arrives here. Its message
	// names the field and never the value.
	cred, err := NewCredential(fields)
	if err != nil {
		return Credential{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return cred, nil
}

// Fetch asks Nango for this tenant's connection for this provider.
//
// It refuses an answer that is not about what it asked for. Nango is a system in the middle, and a
// mix-up there (a connection renamed, a proxy answering from a cache, a provider config key
// reused) would otherwise hand one tenant another tenant's token, which is the one failure this
// whole program is built not to have.
func (n *Nango) Fetch(ctx context.Context, t tenancy.ID, providerKey string) (Credential, error) {
	if err := checkArgs(t, providerKey); err != nil {
		return Credential{}, err
	}
	body, status, err := n.do(ctx, http.MethodGet, t, providerKey)
	if err != nil {
		return Credential{}, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return Credential{}, fmt.Errorf("%w: %s", ErrNotFound, providerKey)
	default:
		return Credential{}, nangoStatusError(status)
	}
	var doc nangoConnection
	if err := json.Unmarshal(body, &doc); err != nil {
		// Nothing from the decoder's message: it quotes the JSON it could not read, and that
		// document carries the token.
		return Credential{}, fmt.Errorf("%w: its answer is not a connection document", ErrUnavailable)
	}
	if doc.ConnectionID != t.String() || doc.ProviderConfigKey != providerKey {
		// Both values in the message are this program's own: the tenant it asked for and the
		// provider key it asked for. What came back is not quoted, because a value chosen
		// somewhere else is not this error's to print.
		return Credential{}, fmt.Errorf(
			"%w: its answer is for another connection than the one asked for (tenant %q, provider %q)",
			ErrUnavailable, t.String(), providerKey)
	}
	return doc.Credentials.credential()
}

// Revoke removes the connection in Nango, which is where the credential is. Removing one that is
// not there is ErrNotFound, as it is everywhere else in this package.
func (n *Nango) Revoke(ctx context.Context, t tenancy.ID, providerKey string) error {
	if err := checkArgs(t, providerKey); err != nil {
		return err
	}
	_, status, err := n.do(ctx, http.MethodDelete, t, providerKey)
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, providerKey)
	case status >= 200 && status <= 299:
		return nil
	default:
		return nangoStatusError(status)
	}
}

// do makes one request to Nango's connection endpoint and returns its body and status.
//
// The tenant is one escaped path segment and the provider key one query parameter, both already
// held to their grammars by checkArgs, so neither can reach the URL as anything else. The secret
// key travels in a header and never in the URL: a URL reaches an access log, a proxy and the text
// of a transport error.
func (n *Nango) do(ctx context.Context, method string, t tenancy.ID, providerKey string) ([]byte, int, error) {
	u := n.base + nangoConnectionPath + url.PathEscape(t.String()) +
		"?" + url.Values{"provider_config_key": {providerKey}}.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u, http.NoBody)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: the request could not be built", ErrUnavailable)
	}
	req.Header.Set("Authorization", "Bearer "+n.secret)
	req.Header.Set("Accept", "application/json")
	// G704 (SSRF) is answered by construction rather than suppressed blindly: every byte of u
	// comes from n.base, which NewNango parsed and held to an absolute http or https origin with
	// no query, fragment or userinfo, from nangoConnectionPath, which is a constant, from the
	// tenant, which checkArgs held to [A-Za-z0-9_-] and url.PathEscape escaped, and from
	// url.Values, which escapes what it encodes.
	resp, err := n.client.Do(req) //nolint:gosec // G704: see above
	if err != nil {
		// The error of an HTTP client quotes the request URL, and this one has the tenant in its
		// path and the provider in its query. Neither is secret, but the base URL is configuration
		// and a *url.Error on a redirected request can carry a URL this deployment did not
		// choose, so the classification is kept and the text is not (internal/sink does the same).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, fmt.Errorf("%w: %w", ErrUnavailable, ctxErr)
		}
		return nil, 0, fmt.Errorf("%w: it could not be reached", ErrUnavailable)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxNangoResponse))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: its answer could not be read", ErrUnavailable)
	}
	return body, resp.StatusCode, nil
}

// nangoStatusError is every answer that is not the one asked for: the status and nothing else.
//
// A 401 or a 403 is an operator's job and an outage is not, and the status is what tells them
// apart, so it is in the message. The body never is: it is a remote system's text, on a path
// where that text is a token's neighbour.
func nangoStatusError(status int) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("%w: it refused this deployment's secret key (status %d)", ErrUnavailable, status)
	}
	return fmt.Errorf("%w: it answered status %d", ErrUnavailable, status)
}

// truncate bounds a remote system's word before it reaches an error message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
