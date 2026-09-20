// Package ingress is the webhook edge: the HTTP handler behind POST /ingress/{provider}.
//
// It does four things and deliberately no more (architecture 3.1):
//
//  1. It turns the path segment, which is text a stranger sent, into a registered provider, and
//     answers an unknown one before anything else happens.
//  2. It captures the request body once, under a size cap, and hands those exact bytes on.
//     Nothing here parses, decodes or re-serializes them: a provider's HMAC is over the bytes it
//     sent, and re-serialized JSON never matches it (principle 1).
//  3. It runs the provider's handshake hook, which answers a challenge and stops, before any
//     tenant exists to resolve.
//  4. It hands the delivery to the Hub and turns the Hub's verdict into an HTTP status.
//
// Resolving the owner, verifying the signature against owned subscription rows and storing the
// outbox row are the Hub's, in internal/hub (backlog item B07). The seam is deliberate: this
// package touches no database and holds no tenant, so everything in it can be tested without one.
//
// # The response codes are a contract
//
// A provider retries anything that is not 2xx, so a status is an instruction to a machine that
// will act on it thousands of times:
//
//	202  stored
//	200  a re-send of something already stored, a delivery nobody owns, or poison that was parked
//	401  ONLY a signature that did not verify
//	404  no such provider, and nothing was read or stored
//	413  the body is over the cap
//	400  the body could not be read (a Content-Length that lies, a connection that stopped)
//	503  the accept path did not finish in time, for example a saturated connection pool
//	500  a bug or an outage on our side
//
// The 404 is the one deliberate exception to "2xx for everything else". It is safe because
// nothing has happened by the time it is answered: no body has been read, no tenant resolved,
// nothing stored. And no provider can be storming, because a provider only ever posts to the URL
// Lawang gave it, which names a registered provider. What reaches this branch is a scanner.
//
// # The URL a signature covers
//
// A provider that signs the request URL (HubSpot v3) signed the PUBLIC one, which behind a tunnel
// or a reverse proxy is not what this process sees. The edge builds it from LAWANG_PUBLIC_BASE_URL
// plus the request's own path and query, and never from Host, X-Forwarded-Host or
// X-Forwarded-Proto: a sender that picks part of its own signed input is not being checked. With
// no base URL configured the edge still serves and provider.Request.URL is empty, and a scheme
// that needs it refuses (architecture 4).
//
// # What is never logged
//
// No body, no header value and no path segment reaches a log line or a response. The provider key
// in a log line is the registry's own constant, never the segment that was looked up, even though
// the two hold the same bytes on the path that succeeds.
package ingress

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gablooge/lawang/internal/config"
	"github.com/gablooge/lawang/internal/provider"
)

// Pattern is the route this handler serves. The method is part of it, so anything but POST is the
// mux's 405 and never reaches the handler.
const Pattern = "POST /ingress/{provider}"

// pathValue is the wildcard name inside Pattern.
const pathValue = "provider"

// DefaultMaxBody is the cap on a request body, and the reason a 500 MB POST costs this process a
// megabyte and not half a gigabyte. Webhook bodies are small: a few kilobytes from Slack and
// ClickUp, up to tens of kilobytes from a batched Microsoft Graph notification. The cap is far
// above all of them and far below anything that would hurt.
const DefaultMaxBody int64 = 1 << 20

// DefaultAcceptTimeout bounds everything after the body is captured. Architecture 3.1 targets an
// accept under 200 ms, so this is an order of magnitude of headroom and still an answer rather
// than a hang.
//
// It is what makes a saturated connection pool visible instead of silent. Every accept holds a
// pool connection for its resolve and its insert, and pgxpool's default pool is max(4, NumCPU)
// connections shared by every in-flight webhook. Without a bound, the requests over that number
// queue inside the pool until the provider's own client gives up, and the provider sees a timeout,
// which many treat as an outage. With it they get a 503 and a Retry-After, which is a retry
// instruction the provider already understands. An operator who needs more concurrency raises
// pool_max_conns in LAWANG_DATABASE_URL.
const DefaultAcceptTimeout = 2 * time.Second

// retryAfter is the Retry-After of a 503, in seconds. It is short because the condition it
// reports (a full pool, a slow database) usually clears in about that long.
const retryAfter = 5

// maxReplyBody bounds a handshake reply. A challenge echo is tens of bytes; this is the guard
// that a provider implementation cannot turn the edge into something that serves content.
const maxReplyBody = 8 << 10

// maxPrealloc bounds how much a sender's Content-Length may make the edge allocate before a
// single byte of body has arrived. It is deliberately not tied to MaxBody, which an operator may
// raise for a provider with large payloads: the cap on what is read is one thing, and what an
// unproven claim may reserve is another.
const maxPrealloc = 1 << 20

const (
	contentTypeText = "text/plain; charset=utf-8"
	nosniffHeader   = "X-Content-Type-Options"
	nosniff         = "nosniff"
)

// Hub is everything the edge hands a delivery to: candidate subscriptions, per-candidate
// verification against the exact bytes, owner resolution, and the outbox insert. internal/hub
// implements it (backlog item B07).
type Hub interface {
	// Accept takes the delivery and returns what the provider should be told.
	//
	// p is the registry's entry, so p.Key() is this program's own constant and the string that
	// belongs in outbox.Delivery.Provider. req is the delivery as a signature scheme sees it, and
	// it is what the hub hands to WebhookSource.Verify once per candidate subscription: the exact
	// request bytes (an implementation must not re-serialize them, and must not modify the slice,
	// which the edge does not copy), the headers, the method, and the public URL the provider
	// posted to, which is built from configuration and never from a header.
	//
	// It returns an error only for a failure on our side. Everything a sender can cause is a
	// Verdict: a forged signature, an unknown workspace, an ordering key that cannot be stored.
	Accept(ctx context.Context, p provider.Entry, req provider.Request) (Verdict, error)
}

// Verdict is what the Hub decided, and the edge's only input for the status it answers. The zero
// value is not a verdict: a Hub that returns it with no error has a bug, and the edge answers 500.
type Verdict int

// The verdicts, in the order of architecture 3.1.
const (
	// Stored is a new delivery in the outbox: 202.
	Stored Verdict = iota + 1
	// Duplicate is a delivery this tenant already has, so the re-send was a no-op: 200.
	Duplicate
	// Parked is a delivery that was stored under the sentinel tenant because nobody owns it,
	// more than one tenant could, or it is poison that can never be delivered: 200. A provider
	// must not retry any of those, so none of them is an error status.
	Parked
	// Unverified is the one thing that answers 401: no candidate's secret verified the bytes.
	Unverified
)

// String names the verdict for a log line.
func (v Verdict) String() string {
	switch v {
	case Stored:
		return "stored"
	case Duplicate:
		return "duplicate"
	case Parked:
		return "parked"
	case Unverified:
		return "unverified"
	default:
		return "invalid"
	}
}

// status is the HTTP status of a verdict, and 0 for one that is not a verdict.
func (v Verdict) status() int {
	switch v {
	case Stored:
		return http.StatusAccepted
	case Duplicate, Parked:
		return http.StatusOK
	case Unverified:
		return http.StatusUnauthorized
	default:
		return 0
	}
}

// Options tunes a Handler. The zero Options is the documented default of every field.
type Options struct {
	// MaxBody is the cap on a request body in bytes, and 0 means DefaultMaxBody.
	MaxBody int64
	// AcceptTimeout bounds the Hub, and 0 means DefaultAcceptTimeout.
	AcceptTimeout time.Duration
	// Logger is where the edge logs, and nil means slog.Default.
	Logger *slog.Logger
	// PublicBaseURL is LAWANG_PUBLIC_BASE_URL: the absolute URL a provider reaches this
	// deployment at, which is what provider.Request.URL is built from. Empty means the
	// deployment configured none, and provider.Request.URL is then empty too. New checks it with
	// config.NormalizePublicBaseURL, the same function config.Load uses, and refuses what that
	// refuses.
	PublicBaseURL string
}

// Handler serves Pattern.
type Handler struct {
	reg           *provider.Registry
	hub           Hub
	maxBody       int64
	acceptTimeout time.Duration
	log           *slog.Logger
	publicBaseURL string
}

// New returns a Handler, or says what is missing. A nil registry or a nil hub is refused rather
// than defaulted: an edge with no hub would answer a provider without storing anything.
func New(reg *provider.Registry, hub Hub, opts Options) (*Handler, error) {
	if reg == nil {
		return nil, errors.New("ingress: a provider registry is required")
	}
	if hub == nil {
		return nil, errors.New("ingress: a hub is required")
	}
	if opts.MaxBody < 0 || opts.AcceptTimeout < 0 {
		return nil, errors.New("ingress: MaxBody and AcceptTimeout cannot be negative")
	}
	// An unusable base URL is refused at start rather than carried into a signature base string,
	// where it would show up as every delivery failing to verify with nothing to see. The check is
	// config's own, called and not copied, because the two spellings end up compared byte for byte
	// inside an HMAC.
	base := ""
	if opts.PublicBaseURL != "" {
		normalized, err := config.NormalizePublicBaseURL(opts.PublicBaseURL)
		if err != nil {
			// The error never quotes the value, for the reason config gives.
			return nil, fmt.Errorf("ingress: PublicBaseURL: %w", err)
		}
		base = normalized
	}
	h := &Handler{
		reg:           reg,
		hub:           hub,
		maxBody:       opts.MaxBody,
		acceptTimeout: opts.AcceptTimeout,
		log:           opts.Logger,
		publicBaseURL: base,
	}
	if h.maxBody == 0 {
		h.maxBody = DefaultMaxBody
	}
	if h.acceptTimeout == 0 {
		h.acceptTimeout = DefaultAcceptTimeout
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	return h, nil
}

// Mount adds the route to mux, so the pattern is written in one place.
func (h *Handler) Mount(mux *http.ServeMux) { mux.Handle(Pattern, h) }

// ServeHTTP is the accept path. Its order is the security argument: the path segment is resolved
// to a registered provider before a single byte of the body is read.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// r.PathValue gives the segment percent-decoded, so it can hold any byte at all: a NUL from
	// %00, a newline, a slash from %2F, 500 bytes of anything. It is only ever used as a map key
	// here, and never reaches a log, a response, an error or the outbox.
	entry, ok := h.reg.Lookup(r.PathValue(pathValue))
	if !ok {
		// Nothing read, nothing stored, nothing said about what is registered.
		respond(w, http.StatusNotFound, "no such provider\n")
		return
	}
	source, ok := entry.WebhookSource()
	if !ok {
		// A provider that receives no webhooks has no endpoint here, and saying so would be a
		// different answer for a registered provider than for an unregistered one.
		//
		// The answer stays byte-identical, but this branch is logged and the one above is not.
		// What reaches the branch above is a scanner, and the segment is unloggable text from a
		// stranger. What reaches this one is a key that IS in the registry, so it can only be a
		// mistake in this program's own wiring (a method renamed in a refactor, a value receiver
		// where a pointer receiver was meant, a provider registered that never implemented
		// WebhookSource), and the consequence is that every real delivery is dropped with a 404
		// and the loss is visible only on the provider's own dashboard. The key logged is the
		// registry's own constant, never the path segment.
		h.log.Warn("ingress: a registered provider is not a webhook source, so its deliveries are dropped",
			"provider", entry.Key())
		respond(w, http.StatusNotFound, "no such provider\n")
		return
	}

	body, ok := h.readBody(w, r)
	if !ok {
		return
	}

	// The handshake runs before anything is resolved or verified, because a challenge arrives
	// when no subscription exists yet. The provider is handed the request with its body already
	// consumed, so it cannot read a second, different copy of it.
	handshakeReq := *r
	handshakeReq.Body = http.NoBody
	if reply, handled := source.Handshake(&handshakeReq, body); handled {
		h.writeReply(w, entry.Key(), reply)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.acceptTimeout)
	defer cancel()
	verdict, err := h.hub.Accept(ctx, entry, provider.Request{
		Method: r.Method,
		URL:    h.publicURL(r),
		Header: r.Header,
		Body:   body,
	})
	switch {
	case err != nil && r.Context().Err() != nil:
		// The sender hung up. Nothing can be written to a closed connection, and this is not a
		// failure worth an error line.
		h.log.Debug("ingress: sender left before the accept finished", "provider", entry.Key())
	case errors.Is(err, context.DeadlineExceeded):
		// The accept path did not finish in time. A saturated connection pool arrives here,
		// because a pool that is full makes every acquire wait for this context.
		h.log.Warn("ingress: accept timed out", "provider", entry.Key(), "timeout", h.acceptTimeout)
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		respond(w, http.StatusServiceUnavailable, "try again\n")
	case err != nil:
		// The hub's errors are this program's own, so the text is safe to log. It is never sent
		// to the sender.
		h.log.Error("ingress: accept failed", "provider", entry.Key(), "error", err)
		respond(w, http.StatusInternalServerError, "error\n")
	default:
		status := verdict.status()
		if status == 0 {
			h.log.Error("ingress: hub returned no verdict and no error", "provider", entry.Key())
			respond(w, http.StatusInternalServerError, "error\n")
			return
		}
		h.log.Debug("ingress: accepted", "provider", entry.Key(), "verdict", verdict.String())
		respond(w, status, verdict.String()+"\n")
	}
}

// publicURL is the absolute URL the provider posted to, as the provider itself would have written
// it, and the empty string when the deployment configured no public base URL.
//
// Not one byte of it comes from the request's headers. Host, X-Forwarded-Host and
// X-Forwarded-Proto are all written by whoever sent the request (the Cloudflare Tunnel in front of
// a development machine passes Host straight through), so a signature base string built from them
// would let the sender choose part of what it is proving, which makes the signature check theatre.
// The scheme, the host and any stripped prefix therefore come from configuration, and only the
// path and the query come from the request, which the sender signed as well.
//
// When the base is empty this returns empty rather than a relative URL, because a scheme that
// signs the URL must refuse rather than verify against something invented (fail closed, see
// provider.Request.URL). The edge still serves: every other scheme is unaffected, and a handshake
// never needs it.
func (h *Handler) publicURL(r *http.Request) string {
	if h.publicBaseURL == "" {
		return ""
	}
	// RequestURI is the escaped path plus "?" and the raw query when there is one, which is the
	// form every signing scheme that covers a URL uses. The mux has already refused anything that
	// needed cleaning, so this is the path as it arrived.
	return h.publicBaseURL + r.URL.RequestURI()
}

// readBody captures the request body under the cap, and reports whether the caller may go on. It
// answers the sender itself when it does not.
//
// The cap is enforced by http.MaxBytesReader, which stops at one byte past it, so an oversize body
// is never accumulated: a 500 MB POST costs this process the cap plus one byte and the connection
// is then closed. The reader is in front of everything that touches the body, hashing included
// (ids.DeliveryID hashes whatever it is given).
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	// An honest Content-Length over the cap is refused without reading anything at all. A lying
	// one is caught below, by the reader, which is the check that actually holds.
	if r.ContentLength > h.maxBody {
		respond(w, http.StatusRequestEntityTooLarge, "body too large\n")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBody)

	// One allocation of about the right size when the sender announced a length, instead of
	// doubling a buffer from 512 bytes eleven times for a megabyte. An announced length is a
	// claim and not a promise, so it sizes nothing beyond maxPrealloc: otherwise a deployment
	// that raised MaxBody would let one header make this process allocate that much per request
	// with no body ever sent. What bounds the read is the reader above, not this.
	var buf bytes.Buffer
	if n := r.ContentLength; n > 0 && n <= maxPrealloc {
		buf.Grow(int(n) + bytes.MinRead) // MinRead too, so the last empty read does not regrow
	}
	_, err := buf.ReadFrom(r.Body)
	switch {
	case err == nil:
		return buf.Bytes(), true
	case errors.As(err, new(*http.MaxBytesError)):
		respond(w, http.StatusRequestEntityTooLarge, "body too large\n")
	default:
		// A Content-Length that promised more than arrived, a chunked body that stopped, a read
		// timeout. The error is not logged: it can quote what was read.
		respond(w, http.StatusBadRequest, "body could not be read\n")
	}
	return nil, false
}

// writeReply answers a handshake, and refuses a reply a provider should not be able to ask for.
// A provider is this program's own code, so a refusal here is a bug to fix, which is why it is
// logged at error and answered with a 500.
func (h *Handler) writeReply(w http.ResponseWriter, key string, reply provider.Reply) {
	status := reply.Status
	if status == 0 {
		status = http.StatusOK
	}
	contentType := reply.ContentType
	if contentType == "" {
		contentType = contentTypeText
	}
	switch {
	case !handshakeStatus(status):
		h.log.Error("ingress: handshake reply has a status a handshake may not use",
			"provider", key, "status", status)
	case len(reply.Body) > maxReplyBody:
		h.log.Error("ingress: handshake reply is too large", "provider", key, "bytes", len(reply.Body))
	case !validHandshakeContentType(contentType):
		h.log.Error("ingress: handshake reply has a content type a handshake may not use",
			"provider", key)
	default:
		w.Header().Set("Content-Type", contentType)
		w.Header().Set(nosniffHeader, nosniff)
		w.WriteHeader(status)
		_, _ = w.Write(reply.Body)
		return
	}
	respond(w, http.StatusInternalServerError, "error\n")
}

// handshakeStatus reports whether a provider's handshake may answer with this status: a 2xx, or a
// 4xx that is not 401 or 403.
//
// A 3xx would let a provider redirect whoever sent the challenge, and a 5xx would ask for a retry
// of a handshake that is not going to change its mind. 401 is refused because the response-code
// contract at the top of this file makes it mean exactly one thing, a signature that did not
// verify, and a handshake runs before anything is verified and has no signature to fail: a
// provider that answered a malformed challenge with 401, which is an easy and defensible-looking
// choice, would make the one reserved status ambiguous for everyone reading the table afterwards.
// 403 goes with it, because it is the same claim in a different number and nothing a handshake
// does needs it. A challenge a provider cannot make sense of is a 400.
func handshakeStatus(status int) bool {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return false
	case status >= 200 && status < 300:
		return true
	case status >= 400 && status < 500:
		return true
	default:
		return false
	}
}

// maxContentType bounds a handshake reply's content type before it is parsed. The longest thing
// the allowlist below can spell is "application/json; charset=utf-8", so this is generous and
// still not a header a provider can grow to a kilobyte.
const maxContentType = 64

// handshakeMediaTypes are the media types a handshake reply may use, and the list is closed on
// purpose. A handshake echoes a challenge, which is a stranger's text, so a provider that could
// choose the content type could turn this origin into one that serves what a browser executes:
// text/html with an echoed <script> is reflected script execution, and X-Content-Type-Options:
// nosniff does not help, because nosniff stops a browser guessing a type and not honouring the one
// that was sent. These two are what the shapes in the wild need: Microsoft Graph echoes a
// validationToken as text/plain, Slack echoes a challenge inside a JSON object.
var handshakeMediaTypes = []string{"application/json", "text/plain"}

// validHandshakeContentType reports whether a provider's handshake may answer with this
// Content-Type: one of handshakeMediaTypes, with no parameter but charset, and charset only utf-8.
// A parameter the edge does not understand is a provider bug, not something to pass on.
//
// mime.ParseMediaType also refuses a control character, a newline included, so this is the whole
// guard against a second header smuggled through the content type as well.
func validHandshakeContentType(s string) bool {
	if len(s) > maxContentType {
		return false
	}
	mediaType, params, err := mime.ParseMediaType(s)
	if err != nil || !slices.Contains(handshakeMediaTypes, mediaType) {
		return false
	}
	for name, value := range params {
		// ParseMediaType lower-cases the media type and the parameter names, never the values.
		if name != "charset" || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	return true
}

// respond writes one of this package's own constant answers. Nothing a sender sent is ever in
// one: not the body, not a header, not the path. Every call passes a string literal of this file
// or Verdict.String, which is a closed set, and the answer goes out as text/plain with nosniff,
// so there is nothing for a browser to execute even if one asked for this endpoint.
func respond(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", contentTypeText)
	w.Header().Set(nosniffHeader, nosniff)
	w.Header().Set("Content-Length", strconv.Itoa(len(msg)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg) //nolint:gosec // G705: msg is one of this file's own constants
}

// compile-time check that the handler is one.
var _ http.Handler = (*Handler)(nil)
