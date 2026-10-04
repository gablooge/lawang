package clickup

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
)

// DefaultBaseURL is where ClickUp's API is. B12 points a Provider at a fake server instead.
const DefaultBaseURL = "https://api.clickup.com"

// apiPrefix is the version segment of every path this package calls. It is a constant and not an
// option: a deployment that could choose it could send a tenant's token to another API.
const apiPrefix = "/api/v2"

// DefaultPerMinute is what the rate limiter allows, which is ClickUp's own documented limit on
// the plans that have the lowest one (Free Forever, Unlimited and Business all allow 100 requests
// a minute per token: https://developer.clickup.com/docs/rate-limits). A deployment on a plan
// with a higher limit raises it; a deployment that leaves it alone cannot be rate limited by
// ClickUp for drain traffic alone.
const DefaultPerMinute = 100

// DefaultBurst is how many requests may be made back to back before the limiter starts pacing.
// One drained delivery costs one or two requests, so ten is a handful of deliveries arriving at
// once, and well under the per-minute allowance.
const DefaultBurst = 10

// DefaultTimeout bounds one request to ClickUp, from dialling to the last byte.
const DefaultTimeout = 30 * time.Second

// maxResponseBytes bounds what this package reads from ClickUp. A task's description is the
// largest thing it fetches, and the record format caps a text at a megabyte of characters, so
// four megabytes is generous. It exists because a response is a remote system's to size.
const maxResponseBytes = 4 << 20

// commentPage is how many comments ClickUp returns per request, and maxCommentPages is how many
// requests this package will spend looking for one comment
// (https://developer.clickup.com/reference/gettaskcomments: the newest 25, then older ones with
// start and start_id). A comment that was just posted is on the first page; an edit to an old one
// may not be, and the search stops rather than walking a task with ten thousand comments.
const (
	commentPage     = 25
	maxCommentPages = 4
)

// ErrRateLimited reports a request the rate limiter would not make. It is not a failure of
// ClickUp's and not a dead letter: the delivery goes back on the retry ladder, which is what
// paces the drain. See limiter.
var ErrRateLimited = errors.New("clickup: the rate limiter is at capacity")

// ErrAPI reports a call to ClickUp that did not come back with what was asked for. Its message
// carries the status and this package's own words, never the response body and never the request
// URL: an error reaches a log line and the outbox's last_error column, and a remote system's text
// does not belong in either (architecture 3.2).
var ErrAPI = errors.New("clickup: the API call failed")

// ErrNotFound reports a task or a comment the API does not have, or will not show this token. It
// is separate from ErrAPI because it is the one answer an operator reads differently: the
// delivery is for something that is gone, or for something the connected account cannot see.
var ErrNotFound = errors.New("clickup: the API does not have it")

// api is the direct ClickUp client: one HTTP client, one rate limiter and the base URL.
type api struct {
	base    string
	client  *http.Client
	tokens  Tokens
	limiter *limiter
}

func newAPI(opts Options) (*api, error) {
	if opts.Tokens == nil {
		// Fail closed at construction. A provider with no way to fetch a credential would accept
		// every delivery and fail every hydration, which is a retry ladder spent on a wiring
		// mistake.
		return nil, errors.New("clickup: New needs a Tokens")
	}
	base := opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimSuffix(base, "/")
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" ||
		u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		// The base URL is the prefix of every request this package makes, and a tenant's token
		// goes on every one of them, so a base URL that is not an absolute http or https origin
		// is refused at start-up rather than sending a credential somewhere unintended.
		return nil, errors.New("clickup: BaseURL must be an absolute http or https URL with no query, fragment or userinfo")
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: DefaultTimeout,
			// A redirect would be followed with the Authorization header still on the request
			// when it stays on one host, and silently dropped when it does not. Neither is a
			// thing to find out about in production, so a 3xx comes back as itself.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	perMinute := opts.PerMinute
	if perMinute == 0 {
		perMinute = DefaultPerMinute
	}
	burst := opts.Burst
	if burst == 0 {
		burst = DefaultBurst
	}
	lim, err := newLimiter(perMinute, burst, opts.Now)
	if err != nil {
		return nil, err
	}
	return &api{base: base, client: client, tokens: opts.Tokens, limiter: lim}, nil
}

// get makes one GET request and decodes its body into out.
//
// path is built from segments this package escaped, and query from url.Values, so nothing a
// delivery carries can reach the URL as anything but one path segment or one parameter value.
func (a *api) get(ctx context.Context, token, path string, query url.Values, out any) error {
	if !a.limiter.allow() {
		return ErrRateLimited
	}
	u := a.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return fmt.Errorf("%w: the request could not be built", ErrAPI)
	}
	// ClickUp takes the token in the Authorization header as it stands, with no scheme word
	// (https://developer.clickup.com/docs/authentication). This is the only place in the program
	// a ClickUp token is written anywhere.
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")

	// G704 (SSRF) is answered by construction rather than suppressed blindly: every byte of u
	// comes from a.base, which newAPI parsed and held to an absolute http or https origin with
	// no query, fragment or userinfo, from apiPrefix, which is a constant, from identifiers
	// validID has held to [A-Za-z0-9_-] AND url.PathEscape has escaped, and from url.Values,
	// which escapes what it encodes. Nothing a delivery or an API answer carries can add a
	// segment, a host or a scheme.
	resp, err := a.client.Do(req) //nolint:gosec // G704: see above
	if err != nil {
		// Never the error itself: an *url.Error quotes the request URL, and a sibling of this
		// client somewhere else may one day carry a credential in a query string. The context's
		// own error is kept, because the worker's shutdown is told apart by it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: %w", ErrAPI, ctxErr)
		}
		return fmt.Errorf("%w: the request did not complete", ErrAPI)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return ErrNotFound
	default:
		// The status and nothing else. A ClickUp error body echoes the request, and a 401 body
		// is the least useful place to go looking for a secret that might be in it.
		return fmt.Errorf("%w: status %d", ErrAPI, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%w: the response could not be read", ErrAPI)
	}
	if len(body) > maxResponseBytes {
		return fmt.Errorf("%w: the response is over %d bytes", ErrAPI, maxResponseBytes)
	}
	if err := json.Unmarshal(body, out); err != nil {
		// The decoder's message quotes the body.
		return fmt.Errorf("%w: the response is not the JSON this call expects", ErrAPI)
	}
	return nil
}

// task fetches one task (https://developer.clickup.com/reference/gettask).
func (a *api) task(ctx context.Context, token, taskID string) (task, error) {
	var t task
	err := a.get(ctx, token, apiPrefix+"/task/"+url.PathEscape(taskID), nil, &t)
	return t, err
}

// comment finds one comment of a task (https://developer.clickup.com/reference/gettaskcomments).
//
// There is no endpoint for a single comment, so this reads the task's comments newest first and
// pages back with start and start_id until it finds the one it wants, for at most maxCommentPages
// pages. A comment it does not reach is ErrNotFound, which walks the retry ladder and ends as a
// visible dead letter rather than as a record with somebody else's text in it.
func (a *api) comment(ctx context.Context, token, taskID, commentID string) (comment, error) {
	path := apiPrefix + "/task/" + url.PathEscape(taskID) + "/comment"
	var query url.Values
	for page := 0; page < maxCommentPages; page++ {
		var answer struct {
			Comments []comment `json:"comments"`
		}
		if err := a.get(ctx, token, path, query, &answer); err != nil {
			return comment{}, err
		}
		for _, c := range answer.Comments {
			if c.ID == commentID {
				return c, nil
			}
		}
		if len(answer.Comments) < commentPage {
			break // the last page: there are no older comments to ask for
		}
		oldest := answer.Comments[len(answer.Comments)-1]
		if oldest.ID == "" || oldest.Date == "" {
			break // nothing to page back from, and a repeat of the same page is not a plan
		}
		query = url.Values{"start": {oldest.Date}, "start_id": {oldest.ID}}
	}
	return comment{}, fmt.Errorf("%w: the comment is not in the %d newest of its task",
		ErrNotFound, commentPage*maxCommentPages)
}

// limiter is a token bucket that REFUSES a request it cannot allow, rather than waiting for one.
//
// Waiting is the usual shape and it is the wrong one here. A hydration call runs inside a drain
// goroutine that holds a claimed outbox row on a lease, so a limiter that slept would hold the
// row (and its entity's whole queue) for as long as it slept, and a backlog of deliveries would
// turn into a pile of sleeping goroutines each sitting on a lease that may expire under it. The
// retry ladder is already the thing that paces a provider that cannot keep up: a refusal puts the
// delivery back with a backoff, the worker gets its goroutine back at once, and nothing is lost.
//
// It is a bucket rather than a counter per minute so that a burst after a quiet period is allowed
// (which is what a drain catching up looks like) without ever exceeding the rate over any window
// longer than the burst.
type limiter struct {
	mu       sync.Mutex
	tokens   float64
	last     time.Time
	burst    float64
	perToken time.Duration
	now      func() time.Time
}

func newLimiter(perMinute, burst int, now func() time.Time) (*limiter, error) {
	if perMinute <= 0 || burst <= 0 {
		return nil, errors.New("clickup: PerMinute and Burst must be positive")
	}
	if now == nil {
		now = time.Now
	}
	return &limiter{
		tokens:   float64(burst),
		last:     now(),
		burst:    float64(burst),
		perToken: time.Minute / time.Duration(perMinute),
		now:      now,
	}, nil
}

// allow takes one token and reports whether there was one.
func (l *limiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.last = now
		l.tokens = min(l.burst, l.tokens+float64(elapsed)/float64(l.perToken))
	}
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}
