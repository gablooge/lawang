// Package clickup is the ClickUp provider: the accept path's signature check and parse, the
// direct API client that hydrates a change, and the normalizer that turns what it fetched into
// records (docs/architecture.md, section 7, and docs/adr/0015-clickup-provider.md).
//
// What it implements, and what it deliberately does not:
//
//   - provider.Provider and provider.WebhookSource. The webhook is the only way changes arrive
//     in v0.1; the reconciler is B21 and the registrar is B14.
//   - NOT provider.Degrader. A ClickUp webhook body does not carry the list the task is in, and
//     the list is what the scope is made of, so a record built from the body alone would need a
//     guessed scope, which is a second id for one version of one entity (ADR 4, decision 7).
//     A hydration failure therefore waits for ClickUp's API to come back, which is exactly what
//     provider.Degrader says to do when the body does not carry the scope.
//   - NOT provider.URLSigner. ClickUp signs the body and nothing else.
//
// # Everything here is hostile input
//
// A delivery arrives from the public internet, signed or not, and Verify, DeliveryKeys and Parse
// all run on bytes nobody has vouched for. So no error in this package quotes the delivery, every
// identifier read out of one is held to a grammar before it is used, and nothing is built out of
// a field this package has not checked.
//
// # Fixtures
//
// Every payload under testdata is synthesized from ClickUp's published documentation and not
// recorded from a live workspace (testdata/README.md says what B12 replaces and what to look at).
package clickup

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/tenancy"
)

// Key is the internal provider key, the first segment of every ClickUp scope id and external id
// and part of every ClickUp record id. ADR 3 freezes its grammar and record.ValidProviderKey owns
// the rule; the registry checks this string against it at start-up.
const Key = "clickup"

// SignatureHeader carries the hex HMAC-SHA256 of the exact request body, computed with the
// webhook's own secret (https://developer.clickup.com/docs/webhooksignature).
const SignatureHeader = "X-Signature"

// ContainerKindList and ContainerKindTask are the two container kinds this provider mints. A task
// lives in a list, a comment lives in a task, and both records are decided on the task's list
// (architecture section 6: the container is where the entity lives, and need not be what the
// scope is made of).
const (
	ContainerKindList = "list"
	ContainerKindTask = "task"
)

// ErrNoToken reports a tenant with no ClickUp API token. It is a refusal and never a default:
// hydration without a credential would reach ClickUp as an anonymous request and be answered 401,
// and the delivery would walk the retry ladder with nothing to say.
var ErrNoToken = errors.New("clickup: no API token for this tenant")

// Tokens hands out one tenant's ClickUp API token. It is the seam the vault fills (B13): a
// Credential is opaque at the provider interface, and this package needs exactly one string from
// it, so it asks for that and nothing else.
//
// What it returns is secret material. It never reaches a log line, an error, a record or a
// metric label, and this package puts it in one place only, the Authorization header of a request
// to ClickUp.
type Tokens interface {
	Token(ctx context.Context, t tenancy.ID) (string, error)
}

// Options configure a Provider. Everything but Tokens has a default.
type Options struct {
	// Tokens is required. A provider that cannot fetch a credential can hydrate nothing, so New
	// refuses a nil one rather than failing on every delivery.
	Tokens Tokens
	// BaseURL is where the API is, without a trailing slash. Empty means DefaultBaseURL. B12
	// points it at a fake ClickUp server.
	BaseURL string
	// HTTPClient talks to the API. Nil means a client of this package's own, with a timeout, its
	// own connection pool and redirects turned off.
	HTTPClient *http.Client
	// PerMinute is what the rate limiter allows. Zero means DefaultPerMinute.
	PerMinute int
	// Burst is how many requests may be made back to back. Zero means DefaultBurst.
	Burst int
	// Now is the clock the rate limiter reads. Nil means time.Now.
	Now func() time.Time
}

// Provider is the ClickUp integration. Build one with New and register it like any other
// provider. It is safe for concurrent use: everything in it is either immutable or, in the rate
// limiter's case, guarded by its own mutex.
type Provider struct {
	api *api
}

// New returns a ClickUp provider. It refuses an Options with no Tokens, and a BaseURL that is not
// an absolute http or https URL without a query or a fragment, because that string is the prefix
// of every request this package makes.
func New(opts Options) (*Provider, error) {
	a, err := newAPI(opts)
	if err != nil {
		return nil, err
	}
	return &Provider{api: a}, nil
}

// Key is the provider key.
func (p *Provider) Key() string { return Key }

// VersionOrder says how this provider spells record.Record.Version: one run of decimal digits,
// ordered by the number it spells.
//
// Every time ClickUp reports is a string of epoch milliseconds (a task's date_updated, a
// comment's date, a history item's date), and every version this normalizer mints is one of
// those numbers, so decimal is what they are. Lexical would be wrong here and silently: epoch
// milliseconds are 13 digits today and were 12 until 2001, so two versions of one entity are
// the same width in practice and a fixed-width claim would hold until it did not. Decimal is
// also the one order the pipeline can PROVE rather than take on trust, since it can see whether
// a string is a digit run, so a version this package ever minted wrongly is a dead letter and
// never a wrong answer.
func (p *Provider) VersionOrder() provider.VersionOrder { return provider.VersionOrderDecimal }

// Handshake reports that this is not a handshake, always. ClickUp sends no challenge: a webhook
// is created through the API and starts delivering, so there is nothing to echo, and echoing
// anything would be answering a stranger's text at this origin.
//
// It still has to be cheap, because the edge calls it on every delivery before anything is
// verified.
func (p *Provider) Handshake(*http.Request, []byte) (provider.Reply, bool) {
	return provider.Reply{}, false
}

// Verify reports whether the delivery is signed with secret: the hex of HMAC-SHA256 over the
// exact request bytes, in the X-Signature header, compared in constant time.
//
// It never errors and never panics. A missing secret, a missing signature, two signatures, a
// signature that is not hex and one of the wrong length are all a plain false (architecture
// principle 1). It reads r.Body as it stands and writes to nothing: the hub hands the same
// Request to every candidate subscription, and the bytes are what the outbox will store.
//
// ClickUp signs the body alone, so r.Method and r.URL are not read. That is why this provider is
// not a provider.URLSigner, and why a deployment with no LAWANG_PUBLIC_BASE_URL can serve it.
func (p *Provider) Verify(r provider.Request, secret []byte) bool {
	if len(secret) == 0 {
		return false // fail closed: a missing secret verifies nothing
	}
	sigs := r.Header.Values(SignatureHeader)
	if len(sigs) != 1 {
		// Two signature headers are two claims where the scheme allows one, and picking either
		// is how a smuggled second value gets its chance.
		return false
	}
	// ClickUp digests in lowercase hex, and hex.DecodeString also reads uppercase, so a digest
	// spelled either way verifies. That is deliberate: the two spell the same bytes, and
	// refusing one of them would turn a signature that proves knowledge of the secret into a
	// 401 on a provider's dashboard for no gain. hmac.Equal already refuses a digest of the
	// wrong length, so the length test changes no answer; it is the shape of the refusal
	// written down, and no test can tell it from its absence.
	want, err := hex.DecodeString(sigs[0])
	if err != nil || len(want) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(r.Body)
	return hmac.Equal(mac.Sum(nil), want)
}

// badDelivery is every refusal of a body, and it never quotes one. what says which part of the
// delivery was wrong, in this package's own words.
func badDelivery(what string) error { return fmt.Errorf("%w: %s", ErrBadDelivery, what) }
