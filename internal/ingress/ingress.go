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
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gablooge/lawang/internal/config"
	"github.com/gablooge/lawang/internal/provider"
)

// routePath is the path of the route the edge serves, and routeMethod is its method, so anything
// but POST is the mux's 405 and never reaches the edge. Neither is exported, and neither is the
// type that serves them: the only servable thing this package hands out is what New returns, and
// that has the path guard in front of it (see New).
const (
	routeMethod = http.MethodPost
	routePath   = "/ingress/{provider}"
)

// pathValue is the wildcard name inside routePath.
const pathValue = "provider"

// edgePrefix is the path prefix the webhook route owns: every path the edge answers begins with
// it. It is derived from routePath rather than written down a second time, so the two cannot
// drift apart. Route.check refuses a caller's route under it, because such a route is more
// specific than routePath and would answer deliveries in the edge's place.
var edgePrefix = routePath[:strings.LastIndex(routePath, "/")+1]

// notFoundBody is the only thing this package says to a request it refuses before the hub: an
// unknown provider, a registered provider that is not a webhook source, a path the mux would have
// redirected, and a route that exists only to stop a redirect all answer these same bytes. A
// refusal that differed between them would tell a stranger which providers are registered.
const notFoundBody = "no such provider\n"

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

// Route is one more route for the server to answer next to the webhook endpoint: GET /healthz
// now, the operator API when it lands. Routes are given to New rather than registered by the
// caller on a mux of its own, because both of the redirects net/http.ServeMux answers have to be
// stopped where the routing table is built, and New is where it is built (see New).
type Route struct {
	// Method is the one method the route answers, spelled as net/http spells it ("GET"). Empty
	// means every method. Method matching is case sensitive, so a lower case spelling would
	// register a route nothing can ever reach, and New refuses one.
	Method string
	// Path is the pattern's path and must start with a slash: "/healthz", "/v1/tenants/{id}",
	// or "/v1/" for a subtree. A host is not part of it, because everything here is served on
	// whatever host reaches the listener. A path under the webhook endpoint's own prefix
	// ("/ingress/") is refused, whatever it is spelled as: such a route is more specific than
	// the webhook pattern, so it would answer deliveries in the edge's place.
	Path string
	// Handler answers the route. It is served behind the path guard, like the edge itself.
	Handler http.Handler
}

// pattern is the net/http.ServeMux pattern for this route.
func (r Route) pattern() string {
	if r.Method == "" {
		return r.Path
	}
	return r.Method + " " + r.Path
}

// check refuses a route New could not register, one it could register into something that can
// never match, and one that would answer in the edge's place. The messages never quote anything a
// sender wrote: a Route is this program's own wiring.
func (r Route) check() error {
	if r.Handler == nil {
		return errors.New("the handler is nil")
	}
	if !strings.HasPrefix(r.Path, "/") {
		return errors.New(`the path must start with a slash, and must not carry a host or a method (write Path: "/healthz", Method: "GET")`)
	}
	if strings.ContainsAny(r.Path, " \t") {
		return errors.New("the path must not contain a space")
	}
	// A literal under the webhook prefix is more specific than routePath, so net/http.ServeMux
	// would hand it every delivery for that provider and the edge would never see one. The
	// duplicate-pattern check in newMux catches routePath spelled exactly and nothing else, so
	// POST /ingress/fake used to be accepted and to answer in the edge's place. Nothing
	// legitimate lives under this prefix, so refusing the whole of it costs a caller nothing.
	if strings.HasPrefix(r.Path, edgePrefix) {
		return fmt.Errorf("the path is under %s, which is the webhook endpoint this package serves itself (%s)", edgePrefix, routePath)
	}
	for i := 0; i < len(r.Method); i++ {
		if r.Method[i] < 'A' || r.Method[i] > 'Z' {
			return errors.New("the method must be an upper case method name such as GET, or empty for every method")
		}
	}
	return nil
}

// Options tunes the edge. The zero Options is the documented default of every field.
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
	// Routes are the other routes the server answers. They are served behind the same path
	// guard as the webhook endpoint, which is the reason they are given here: see New.
	Routes []Route
}

// edge serves the webhook route. It is unexported, and so is its ServeHTTP, so that nothing
// outside this package can put it on a mux of its own and lose the guard New puts in front of it.
type edge struct {
	reg           *provider.Registry
	hub           Hub
	maxBody       int64
	acceptTimeout time.Duration
	log           *slog.Logger
	publicBaseURL string
}

// New returns the handler the server serves, or says what is missing. A nil registry or a nil hub
// is refused rather than defaulted: an edge with no hub would answer a provider without storing
// anything.
//
// # Why this returns the whole handler and not a route to mount
//
// net/http.ServeMux answers two kinds of redirect before any handler runs, and both of them turn a
// delivery into a 401 that is indistinguishable from a forgery (see canonicalPathOnly for why).
// Neither can be stopped by the caller remembering something:
//
//  1. It cleans the path and answers 307 with a Location. A guard in front of the mux stops that,
//     because it depends only on the request.
//  2. It answers 307 from /x to /x/ when /x/ is a registered pattern and /x is not. That one
//     depends on the routing table rather than on the request, so nothing in front of the mux can
//     see it coming. It is stopped where the table is built, by registering /x as well.
//
// So this package builds the mux, registers every route on it, and never hands the bare mux out.
// An earlier shape returned the wrapped handler from a Mount(mux) method, and it had the failure
// mode that a caller who wrote mux.Handle(pattern, h) themselves, or who dropped the return value,
// got the unguarded mux and all three redirects back, with nothing in go build, go vet or
// golangci-lint to say so. Other routes go in Options.Routes, so they are behind the guard too.
func New(reg *provider.Registry, hub Hub, opts Options) (http.Handler, error) {
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
	h := &edge{
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
	if h.publicBaseURL == "" {
		// The one signal an operator gets for a variable whose absence is otherwise silent. With
		// no public base URL the edge serves normally, and every delivery of a provider whose
		// signature covers the URL answers 401: the same status the contract reserves for a
		// forged signature, on a provider's own dashboard, with nothing in this process to say
		// which of the two it is. Refusing to start instead would be wrong here, because the edge
		// does not know which providers a deployment has registered or which of their schemes
		// sign the URL. B07 does know, and issue #7 carries the ask to refuse there.
		h.log.Warn("ingress: LAWANG_PUBLIC_BASE_URL is not set, so a provider whose signature covers the request URL (HubSpot v3) will answer 401 for every delivery")
	}
	mux, err := newMux(h, opts.Routes)
	if err != nil {
		return nil, err
	}
	return canonicalPathOnly(mux), nil
}

// newMux builds the routing table: the webhook route, the caller's routes, and one route per
// path the mux would have answered with a trailing-slash redirect, which exists only to take that
// redirect away from it.
//
// Whether the mux redirects is a property of the whole routing table and not of any one pattern,
// so it is not predicted here: the table is built first, and then the mux itself is asked (see
// redirects). An earlier shape compared path strings, and it was wrong in both directions. It
// buried a caller's route, because POST /v1/{resource} already answers /v1/tenants exactly and
// the redirect it added a route for was never going to fire, so its 404 shadowed the wildcard for
// that one path. And it refused a table net/http accepts, because GET /v1/{id}/ plus
// GET /v1/{name} made it register GET /v1/{id}, which conflicts with a pattern the caller wrote.
//
// It never panics. net/http.ServeMux panics on a pattern it cannot parse and on one that
// conflicts with another, and a constructor that took the process down at start for a wiring
// mistake would be a worse answer than an error that describes it.
func newMux(h *edge, routes []Route) (_ *http.ServeMux, err error) {
	for i, rt := range routes {
		if err := rt.check(); err != nil {
			return nil, fmt.Errorf("ingress: Options.Routes[%d]: %w", i, err)
		}
	}
	all := make([]Route, 0, len(routes)+1)
	all = append(all, Route{Method: routeMethod, Path: routePath, Handler: http.HandlerFunc(h.serveHTTP)})
	all = append(all, routes...)

	// This changes no answer, only the message. net/http.ServeMux panics on the second
	// registration of a pattern and the recover below turns that into an error naming the same
	// pattern, so no input can tell this branch from its absence; it is here because "two routes
	// for X" is what the caller needs to read, and deleting it would cost that and nothing else.
	registered := make(map[string]bool, len(all))
	for _, rt := range all {
		if registered[rt.pattern()] {
			return nil, fmt.Errorf("ingress: two routes for %q", rt.pattern())
		}
		registered[rt.pattern()] = true
	}

	mux := http.NewServeMux()
	// probe carries the same patterns as mux, and a handler that does nothing. Asking a routing
	// table what it would answer means serving a request into it, and serving one into mux would
	// run a caller's handler or the edge for a request nobody made. The two are registered
	// together, shadow routes included, so they route identically for as long as probe lives,
	// which is until this function returns.
	probe := http.NewServeMux()
	nothing := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	// guarding names the route this package is adding on its own account while it is being
	// added, so that a conflict with a caller's route is reported as what it is rather than as
	// net/http's message about a pattern the caller never wrote.
	guarding := ""
	defer func() {
		if r := recover(); r != nil {
			// The text is net/http's and quotes the pattern, which is this program's own wiring
			// and never anything a sender wrote.
			if guarding != "" {
				err = fmt.Errorf("ingress: %q is needed so net/http.ServeMux cannot answer that path "+
					"with a redirect, and net/http.ServeMux refused it: %v", guarding, r)
				return
			}
			err = fmt.Errorf("ingress: net/http.ServeMux refused a route: %v", r)
		}
	}()
	for _, rt := range all {
		mux.Handle(rt.pattern(), rt.Handler)
		probe.Handle(rt.pattern(), nothing)
	}
	for _, rt := range all {
		root, ok := redirectRoot(rt.Path)
		if !ok || !redirects(probe, rt.Method, root) {
			continue
		}
		guarding = Route{Method: rt.Method, Path: root}.pattern()
		mux.Handle(guarding, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			respond(w, http.StatusNotFound, notFoundBody)
		}))
		// probe learns it too, so a second route with the same root sees an exact match here and
		// asks for no route of its own. That is the whole of the deduplication.
		probe.Handle(guarding, nothing)
		guarding = ""
	}
	return mux, nil
}

// redirectRoot returns the only path from which net/http.ServeMux could answer a 307 redirect to
// a pattern whose path is p, and whether there is one.
//
// The redirect (matchOrRedirect, net/http/server.go:2731 in Go 1.26.7) fires for a path that does
// not end in a slash when the same path plus a slash is an exact match for some pattern. That
// path plus a slash has one segment more than the path, so the only candidate is p without its
// last segment, whatever that segment is written as. How it is written does not matter here and
// deliberately is not read: an earlier version enumerated the spellings that can match the empty
// segment a trailing slash leaves (a trailing slash, {name...}, {$}) and missed a fourth, since
// net/http stores a literal segment written %2F as the same segment as {$}, so POST /a answered
// 307 to /a/ for a route the enumeration said had no root.
//
// Whether the redirect really fires is for redirects to answer, because that depends on every
// other pattern in the table. A pattern with a single segment ("/healthz", "/", "/{$}",
// "/{rest...}") has no candidate at all: the mux cleans an empty path to "/" before it matches
// anything.
func redirectRoot(p string) (string, bool) {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "", false
	}
	return p[:i], true
}

// redirects reports whether mux answers method and path with a redirect, by putting a request to
// the mux rather than predicting the answer from the pattern strings. Every handler in the mux it
// is given does nothing (see newMux), so asking cannot run a caller's handler, cannot reach the
// edge, and cannot touch anything outside this call.
func redirects(mux *http.ServeMux, method, path string) bool {
	u := &url.URL{Path: path}
	if decoded, err := url.PathUnescape(path); err == nil {
		// The mux matches on EscapedPath, which returns RawPath only when RawPath is a valid
		// encoding of Path, so a path carrying a percent-escape needs both fields set. When it
		// cannot be decoded, Path alone is right: net/http's own pattern parser leaves an
		// undecodable segment as it is written, and so does EscapedPath.
		u.Path, u.RawPath = decoded, path
	}
	var answered statusOnly
	mux.ServeHTTP(&answered, &http.Request{Method: method, URL: u, Header: make(http.Header)})
	return answered.code >= 300 && answered.code < 400
}

// statusOnly is the http.ResponseWriter a redirect probe answers into. It keeps the status and
// throws the rest away, because nothing written to it is ever sent anywhere.
type statusOnly struct {
	head http.Header
	code int
}

func (s *statusOnly) Header() http.Header {
	if s.head == nil {
		s.head = make(http.Header)
	}
	return s.head
}

func (s *statusOnly) Write(b []byte) (int, error) { return len(b), nil }

func (s *statusOnly) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
}

// canonicalPathOnly answers 404 for a request whose path net/http.ServeMux would have cleaned,
// and passes everything else through untouched.
//
// It exists because the mux does that cleaning before any handler runs, and answers 307 with a
// Location header: over a real socket, //ingress/fake, /ingress//fake and /ingress/fake/../fake
// all redirect to /ingress/fake. A 307 preserves the method and the body, so a well-behaved
// provider re-POSTs the delivery to the cleaned path, the edge then builds provider.Request.URL
// from the cleaned path, and the provider signed the path it was given. Every delivery verifies
// against the wrong string and answers 401, which the contract at the top of this file reserves
// for exactly one thing: a signature that did not verify. A misconfiguration would be
// indistinguishable from a forgery.
//
// Refusing is safe because a provider only ever posts to the URL Lawang gave it, which has one
// spelling, so nothing legitimate arrives here needing to be cleaned. The answer is the 404 an
// unknown provider gets, since the guard sits in front of every route and not only this one, and
// a request that is refused for its path shape must not say whether the provider behind it
// exists. The Cloudflare Tunnel in front of a development machine already refuses these shapes
// (CLAUDE.md), and this is the same rule for a deployment that has no tunnel in front of it.
//
// The guard cannot live in the handler: the mux redirects before dispatching, so the handler is
// never called. In front of the mux is the only place it works, which is why New returns the
// wrapped handler and never the mux.
//
// This is the cleaning redirect only. The mux's other redirect, from /x to a registered /x/,
// depends on the routing table rather than on the request, so this guard cannot see it coming;
// newMux takes that one away from the mux instead.
func canonicalPathOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !canonicalPath(r.URL.EscapedPath()) {
			respond(w, http.StatusNotFound, notFoundBody)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// canonicalPath reports whether escaped is what net/http.ServeMux would route without cleaning it
// first. It is net/http's own (unexported) cleanPath compared against its input: path.Clean with
// a trailing slash put back, an empty path becoming "/" and a path with no leading slash getting
// one.
//
// What it must be given is r.URL.EscapedPath(), which is the string findHandler cleans
// (net/http/server.go:2662 and 2680 in Go 1.26.7), and never the decoded r.URL.Path. The two
// differ, and reading the decoded one refuses requests the mux would have delivered:
// /ingress/fake%2f%2fx decodes to /ingress/fake//x, which is not canonical, while the escaped
// form is, and the mux routes it to the handler with no redirect at all. It costs nothing today,
// because that request is a 404 from the registry either way, and it would cost a route the day
// one has a wildcard segment that may carry an encoded slash.
//
// One request shape is refused that the mux would not have cleaned: a CONNECT, which findHandler
// exempts from cleaning. This edge answers POST and nothing else, and a CONNECT is not something
// a provider sends, so it is refused with everything else rather than exempted.
func canonicalPath(escaped string) bool {
	// Reachable over a real socket, both of them: an absolute-form request line with no path
	// ("POST http://host HTTP/1.1") arrives with an empty path, and the asterisk form
	// ("POST * HTTP/1.1") arrives with "*". Through the mux the first is a 307 to "/".
	if escaped == "" || escaped[0] != '/' {
		return false
	}
	clean := path.Clean(escaped)
	if escaped[len(escaped)-1] == '/' && clean != "/" {
		clean += "/"
	}
	return clean == escaped
}

// serveHTTP is the accept path. Its order is the security argument: the path segment is resolved
// to a registered provider before a single byte of the body is read.
func (h *edge) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// r.PathValue gives the segment percent-decoded, so it can hold any byte at all: a NUL from
	// %00, a newline, a slash from %2F, 500 bytes of anything. It is only ever used as a map key
	// here, and never reaches a log, a response, an error or the outbox.
	entry, ok := h.reg.Lookup(r.PathValue(pathValue))
	if !ok {
		// Nothing read, nothing stored, nothing said about what is registered.
		respond(w, http.StatusNotFound, notFoundBody)
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
		respond(w, http.StatusNotFound, notFoundBody)
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
		Header: provider.NewHeader(r.Header),
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
func (h *edge) publicURL(r *http.Request) string {
	if h.publicBaseURL == "" {
		return ""
	}
	// RequestURI is the escaped path plus "?" and the raw query when there is one, which is the
	// form every signing scheme that covers a URL uses. It is the path exactly as it arrived,
	// never a cleaned one: canonicalPathOnly, in front of the mux, has already answered 404 for
	// anything the mux would otherwise have cleaned and redirected, so nothing reaches here whose
	// spelling the sender would not recognize.
	return h.publicBaseURL + r.URL.RequestURI()
}

// readBody captures the request body under the cap, and reports whether the caller may go on. It
// answers the sender itself when it does not.
//
// The cap is enforced by http.MaxBytesReader, which stops at one byte past it, so an oversize body
// is never accumulated: a 500 MB POST costs this process the cap plus one byte and the connection
// is then closed. The reader is in front of everything that touches the body, hashing included
// (ids.DeliveryID hashes whatever it is given).
func (h *edge) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
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
func (h *edge) writeReply(w http.ResponseWriter, key string, reply provider.Reply) {
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

// compile-time check that the edge can be served, which is all newMux needs of it. It is
// deliberately not an http.Handler: an exported ServeHTTP would let a caller put the edge on a
// mux of its own and lose both of the guards New puts around it.
var _ = http.HandlerFunc((*edge)(nil).serveHTTP)
