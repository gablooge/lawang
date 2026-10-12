//go:build live

// The live half of B12. It is behind the `live` build tag AND the LAWANG_LIVE_CLICKUP variable, so
// it is in neither `make check` nor CI, which have none of these secrets and must stay that way
// (CLAUDE.md, "Credentials for live verification").
//
// What it is for: a committed signature fixture can never be a recorded one, because the real
// webhook secret may not be committed, so the signature beside a recorded payload is still one the
// tests signed themselves. This is the only thing in the repository that can show that Verify
// agrees with ClickUp, because here ClickUp does the signing (issue #12, and
// internal/provider/clickup/testdata/README.md).

package acceptance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/hub"
	"github.com/gablooge/lawang/internal/ingress"
	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/pipeline"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/clickup"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/sink"
	"github.com/gablooge/lawang/internal/store"
	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/testdb"
	"github.com/gablooge/lawang/internal/worker"
)

// liveTenant owns the live run's subscription and records. It is a tenant of the throwaway
// database the run creates, not of anything that outlives it.
const liveTenant = tenancy.ID("t_live_clickup")

// The variables the live half reads. Everything secret comes out of a file in the credentials
// directory; the two ids are not secrets but they are the maintainer's own, so they travel in the
// environment of the one command and are never written into this repository.
const (
	envEnabled   = "LAWANG_LIVE_CLICKUP"
	envCredsDir  = "LAWANG_CREDENTIALS_DIR"
	envTeam      = "LAWANG_LIVE_CLICKUP_TEAM"
	envTask      = "LAWANG_LIVE_CLICKUP_TASK"
	envListen    = "LAWANG_LIVE_LISTEN"
	envRecordDir = "LAWANG_LIVE_RECORD_DIR"
	envMove      = "LAWANG_LIVE_CLICKUP_MOVE"
	envSecond    = "LAWANG_LIVE_CLICKUP_SECOND_WEBHOOK"
	envHostname  = "LAWANG_TUNNEL_HOSTNAME"

	// The names inside the credentials files. They are read by name and their values are never
	// printed, never put in an error, and never put on a command line.
	varToken = "CLICKUP_TOKEN"
)

// defaultListen is where Cloudflare forwards ^/ingress/[a-z][a-z0-9_]{0,31}$ to
// (http://host.docker.internal:8080, see CLAUDE.md), so it is where the live run has to listen.
const defaultListen = ":8080"

// liveConfig is everything one live run needs. No field is a secret except token, and nothing
// prints token.
type liveConfig struct {
	token    string
	hostname string
	team     string
	task     string
	listen   string
}

// liveSkip reports why this run cannot happen, in words that name the file or the variable that is
// missing and never a value. It is a skip and not a failure: the live half is opt-in, and a
// developer or CI without the credentials has to be able to run `go test ./...` with the tag by
// accident and get a clear message rather than a red build.
func liveSkip(t *testing.T) liveConfig {
	t.Helper()
	if os.Getenv(envEnabled) == "" {
		t.Skipf("the live ClickUp check is opt-in: set %s=1 to run it. It creates one webhook and "+
			"posts one comment on a real workspace, and removes both afterwards", envEnabled)
	}
	dir := os.Getenv(envCredsDir)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home directory, so the default credentials directory cannot be found: set %s", envCredsDir)
		}
		dir = filepath.Join(home, ".config", "lawang")
	}
	creds := readEnvFile(t, filepath.Join(dir, "clickup.env"))
	token := creds[varToken]
	if token == "" {
		t.Skipf("%s is not set in %s", varToken, filepath.Join(dir, "clickup.env"))
	}
	// The hostname lives beside the credentials (issue #12), and the environment is accepted too,
	// because CLAUDE.md says only that it is in the maintainer's own notes next to them.
	hostname := os.Getenv(envHostname)
	if hostname == "" {
		hostname = readEnvFile(t, filepath.Join(dir, "tunnel.env"))[envHostname]
	}
	cfg := liveConfig{
		token:    token,
		hostname: hostname,
		team:     os.Getenv(envTeam),
		task:     os.Getenv(envTask),
		listen:   os.Getenv(envListen),
	}
	if cfg.listen == "" {
		cfg.listen = defaultListen
	}
	for _, missing := range []struct{ what, value string }{
		{envHostname + " (in the environment or in " + filepath.Join(dir, "tunnel.env") + ")", cfg.hostname},
		{envTeam + " (the test workspace, or team, id)", cfg.team},
		{envTask + " (a task in the designated test list, the one a comment may be posted on)", cfg.task},
	} {
		if missing.value == "" {
			t.Skipf("%s is not set, so the live ClickUp check cannot run", missing.what)
		}
	}
	// The hostname goes into a URL and into what is registered with ClickUp, so it is held to a
	// host name here rather than concatenated and hoped for.
	if strings.ContainsAny(cfg.hostname, "/:? #") {
		t.Fatalf("%s is not a bare host name, and this test builds a URL out of it", envHostname)
	}
	return cfg
}

// readEnvFile reads a KEY=VALUE file, as the credentials directory holds them. A file that is not
// there is not an error: the caller turns a missing name into a skip that says which name.
//
// Nothing it returns is ever printed. The error paths deliberately name the path and never a line
// of the file, because a line of one of these files is a credential.
func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Skipf("cannot read %s", path)
	}
	defer func() { _ = f.Close() }()
	out := map[string]string{}
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	if err := scan.Err(); err != nil {
		t.Skipf("cannot read %s to the end", path)
	}
	return out
}

// liveAPI is the smallest ClickUp client that can set a live run up and take it down again. It is
// here and not in internal/provider/clickup on purpose: the provider's client is read-only by
// design (hydration), the registrar is B14's, and nothing the program ships needs to post a
// comment.
//
// The token is in the Authorization header of each request and nowhere else: not in a URL, not in
// a log line, and not in an error. Every error from here names the call and the status.
type liveAPI struct {
	token  string
	client *http.Client
}

func newLiveAPI(token string) *liveAPI {
	return &liveAPI{token: token, client: &http.Client{Timeout: 30 * time.Second}}
}

func (a *liveAPI) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("clickup %s %s: the request body could not be built", method, path)
		}
		reader = bytes.NewReader(raw)
	}
	u := "https://api.clickup.com/api/v2" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return fmt.Errorf("clickup %s %s: the request could not be built", method, path)
	}
	req.Header.Set("Authorization", a.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		// Never the error itself: a *url.Error quotes the request URL.
		return fmt.Errorf("clickup %s %s: the request did not complete", method, path)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("clickup %s %s: the answer could not be read", method, path)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The status and this package's own words. A ClickUp error body echoes the request.
		return fmt.Errorf("clickup %s %s: status %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("clickup %s %s: the answer is not the JSON this call expects", method, path)
	}
	return nil
}

// endpointFor is the URL the webhook is registered against, and shownEndpoint is how the run may
// refer to it in anything a person might read or paste.
//
// The tunnel hostname is deliberately not written down in this repository, because publishing it
// invites traffic to a developer machine (CLAUDE.md), and a test's own output is one paste away
// from a pull request. So nothing here prints it: the operator knows their own hostname, and what
// they need from a message is the webhook id to remove.
func endpointFor(cfg liveConfig) string {
	return "https://" + cfg.hostname + "/ingress/" + clickup.Key
}

const shownEndpoint = "https://<the tunnel hostname>/ingress/" + clickup.Key

// liveWebhook is what ClickUp returns when a webhook is created. The secret is the whole point of
// it, and it is never printed.
type liveWebhook struct {
	ID     string
	Secret string
}

// createWebhook registers the one webhook this run needs, for the events the provider package
// says it can turn into records.
//
// ClickUp's webhook object has no name field, so there is nothing to put "a Lawang test" into: the
// endpoint is the only marker, which is why the run deregisters by the id it was given and why the
// package comment tells the operator to check ClickUp's own webhook list afterwards.
func (a *liveAPI) createWebhook(ctx context.Context, team, endpoint string) (liveWebhook, error) {
	var answer struct {
		ID      string `json:"id"`
		Webhook struct {
			ID     string `json:"id"`
			Secret string `json:"secret"`
		} `json:"webhook"`
	}
	err := a.do(ctx, http.MethodPost, "/team/"+url.PathEscape(team)+"/webhook", nil,
		map[string]any{"endpoint": endpoint, "events": clickup.Events()}, &answer)
	if err != nil {
		return liveWebhook{}, err
	}
	id := answer.Webhook.ID
	if id == "" {
		id = answer.ID
	}
	if id == "" || answer.Webhook.Secret == "" {
		return liveWebhook{}, errors.New("clickup: the created webhook came back with no id or no secret")
	}
	return liveWebhook{ID: id, Secret: answer.Webhook.Secret}, nil
}

func (a *liveAPI) deleteWebhook(ctx context.Context, id string) error {
	return a.do(ctx, http.MethodDelete, "/webhook/"+url.PathEscape(id), nil, nil, nil)
}

// postComment is the one write the backlog item authorizes on content: a comment on the task in
// the designated test list. Its text says what it is, so that anybody who sees it in ClickUp knows
// why it is there.
func (a *liveAPI) postComment(ctx context.Context, task, text string) (string, error) {
	var answer struct {
		ID json.RawMessage `json:"id"`
	}
	err := a.do(ctx, http.MethodPost, "/task/"+url.PathEscape(task)+"/comment", nil,
		map[string]any{"comment_text": text, "notify_all": false}, &answer)
	if err != nil {
		return "", err
	}
	id, err := looseID(answer.ID)
	if err != nil {
		return "", fmt.Errorf("clickup: the created comment came back with no usable id: %w", err)
	}
	return id, nil
}

func (a *liveAPI) deleteComment(ctx context.Context, id string) error {
	return a.do(ctx, http.MethodDelete, "/comment/"+url.PathEscape(id), nil, nil, nil)
}

// liveTask is what the live run reads off a task: enough to answer the README's question about
// date_updated and to know which list it is in.
type liveTask struct {
	ID          string `json:"id"`
	DateUpdated string `json:"date_updated"`
	TeamID      string `json:"team_id"`
	List        struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"list"`
}

func (a *liveAPI) task(ctx context.Context, id string) (liveTask, []byte, error) {
	var raw json.RawMessage
	if err := a.do(ctx, http.MethodGet, "/task/"+url.PathEscape(id), nil, nil, &raw); err != nil {
		return liveTask{}, nil, err
	}
	var t liveTask
	if err := json.Unmarshal(raw, &t); err != nil {
		return liveTask{}, raw, errors.New("clickup: the task answer is not the shape this provider reads")
	}
	return t, raw, nil
}

func (a *liveAPI) comments(ctx context.Context, task string, query url.Values) ([]byte, error) {
	var raw json.RawMessage
	err := a.do(ctx, http.MethodGet, "/task/"+url.PathEscape(task)+"/comment", query, nil, &raw)
	return raw, err
}

// looseID reads an id ClickUp may write as a string or as a number. It is here because the live
// half meets both spellings and refusing one of them would stop a run over a detail that is not
// what the run is about; the provider package itself is strict, and a shape it refuses is exactly
// the kind of finding this run exists to produce.
func looseID(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("the field is absent")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return "", errors.New("the field is empty")
		}
		return s, nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String(), nil
	}
	return "", errors.New("the field is neither a string nor a number")
}

// deliveries records every request that reached the live edge, so that the run can say what
// ClickUp actually sent rather than what the documentation says it sends.
type deliveries struct {
	mu   sync.Mutex
	seen [][]byte
	code []int
}

func (d *deliveries) add(body []byte, status int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, bytes.Clone(body))
	d.code = append(d.code, status)
}

func (d *deliveries) all() ([][]byte, []int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]byte(nil), d.seen...), append([]int(nil), d.code...)
}

// liveEnv is the live run's deployment: the same composition the fake-server half uses, with the
// provider pointed at ClickUp itself and the edge on the port the tunnel forwards to.
type liveEnv struct {
	t     *testing.T
	ctx   context.Context
	db    *store.DB
	tdb   testdb.Database
	stub  *sink.Stub
	sinks *sinks
	subs  *hub.Subscriptions
	seen  *deliveries
}

func liveSetup(t *testing.T, cfg liveConfig) *liveEnv {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	tdb := testdb.New(t)
	db, err := store.Open(ctx, tdb.URL)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Migrate(ctx, discard()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// The real API, with the token behind the same Tokens seam B13 will fill.
	cu, err := clickup.New(clickup.Options{Tokens: liveToken(cfg.token)})
	if err != nil {
		t.Fatalf("clickup.New: %v", err)
	}
	reg, err := provider.NewRegistry(cu)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	h, err := hub.New(db, reg, hub.Options{Logger: discard()})
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}
	handler, err := ingress.New(reg, h, ingress.Options{Logger: discard()})
	if err != nil {
		t.Fatalf("ingress.New: %v", err)
	}
	seen := &deliveries{}
	// Everything that arrives through the tunnel is hostile input from the public internet, signed
	// or not (CLAUDE.md). This wrapper only watches: it reads the body, hands the same bytes on,
	// and changes nothing, because the whole question is whether the exact bytes verify.
	watched := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, ingress.DefaultMaxBody+1))
		if err != nil {
			body = nil
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := &statusWatcher{ResponseWriter: w, status: http.StatusOK}
		handler.ServeHTTP(rec, r)
		seen.add(body, rec.status)
	})

	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		t.Fatalf("cannot listen on %s, which is where the tunnel forwards to: %v", cfg.listen, err)
	}
	srv := &http.Server{Handler: watched, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	})

	pl, err := pipeline.New(reg, pipeline.Options{})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	stub, err := sink.NewStub(sink.StubConfig{})
	if err != nil {
		t.Fatalf("NewStub: %v", err)
	}
	s := &sinks{t: t, inner: stub}
	drain, err := worker.NewDrain(db, pl, s, worker.Options{
		Pool: 1, Batch: 5, Lease: 30 * time.Second, RowTimeout: 20 * time.Second,
		Poll: 100 * time.Millisecond, Logger: discard(),
	})
	if err != nil {
		t.Fatalf("NewDrain: %v", err)
	}
	drainCtx, stopDrain := context.WithCancel(ctx)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_ = drain.Run(drainCtx)
	}()
	t.Cleanup(func() {
		stopDrain()
		<-drained
	})

	return &liveEnv{t: t, ctx: ctx, db: db, tdb: tdb, stub: stub, sinks: s,
		subs: hub.NewSubscriptions(db), seen: seen}
}

// statusWatcher records the status the edge answered, which is the one thing the live run has to
// know about its own answer: 202 is "stored", and 401 would be Verify disagreeing with ClickUp.
type statusWatcher struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusWatcher) WriteHeader(code int) {
	if !s.written {
		s.status, s.written = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWatcher) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

// liveToken hands the provider the real token, for the live tenant and nothing else. It refuses
// every other tenant, which is what the real vault will do.
type liveToken string

func (l liveToken) Token(_ context.Context, t tenancy.ID) (string, error) {
	if t != liveTenant {
		return "", fmt.Errorf("no ClickUp token for tenant %s in this run", t)
	}
	return string(l), nil
}

// finding is one of testdata/README.md's open questions and what this run saw. Findings are
// printed, never written into the repository: what to do with them is the maintainer's call, and
// the fixture replacement is a pass of its own.
func finding(t *testing.T, question, answer string) {
	t.Helper()
	t.Logf("FINDING | %s | %s", question, answer)
}

// record0 writes a payload for the maintainer to scrub, and only where they asked for it.
//
// Nothing is written by default. These bytes are a real workspace's: real ids, real names, real
// addresses in an API answer. They are the raw material of the fixture replacement, which is a
// later pass, and they must be scrubbed before any of them is committed (CLAUDE.md).
func record0(t *testing.T, name string, payload []byte) {
	t.Helper()
	dir := os.Getenv(envRecordDir)
	if dir == "" {
		t.Logf("not recording %s: set %s to a directory outside this repository to keep the raw payloads", name, envRecordDir)
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Errorf("cannot create %s: %v", envRecordDir, err)
		return
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Errorf("cannot write %s: %v", name, err)
		return
	}
	t.Logf("wrote %s. IT IS NOT SCRUBBED: real ids, names and addresses are in it, and CLAUDE.md "+
		"requires every one of them to be replaced with an obvious placeholder before it is committed", path)
}

// TestLiveClickUpEndToEnd is the live half of M1's acceptance: one comment, posted by this test on
// a real ClickUp task, delivered by ClickUp through the tunnel, signed by ClickUp with the real
// webhook secret, verified by this program, drained, and landing as exactly two records.
//
// It is the signature evidence for B12, and the only thing in the repository that is.
//
// What it leaves behind if it is interrupted: a ClickUp webhook and a comment. Both are removed in
// a cleanup, and a cleanup that fails says so at error level with the id to remove by hand.
func TestLiveClickUpEndToEnd(t *testing.T) {
	cfg := liveSkip(t)
	api := newLiveAPI(cfg.token)
	e := liveSetup(t, cfg)

	// What the task looked like before anything was written, which is also half of the
	// date_updated question.
	before, beforeRaw, err := api.task(e.ctx, cfg.task)
	if err != nil {
		t.Fatalf("read the test task: %v", err)
	}
	record0(t, "live_api_task_before.json", beforeRaw)
	t.Logf("the test task is in list %q", before.List.ID)
	finding(t, "does a task answer carry team_id at the top level",
		fmt.Sprintf("team_id is %q on the task answer (this is the API, not a webhook body)", before.TeamID))

	endpoint := endpointFor(cfg)
	wh, err := api.createWebhook(e.ctx, cfg.team, endpoint)
	if err != nil {
		t.Fatalf("create the webhook (is the tunnel running, and does the hostname resolve?): %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := api.deleteWebhook(ctx, wh.ID); err != nil {
			t.Errorf("THE WEBHOOK IS STILL REGISTERED. Delete it by hand: id %s, on team %s, pointing at %s. "+
				"A webhook nobody deregistered goes on posting deliveries at a developer machine: %v",
				wh.ID, cfg.team, shownEndpoint, err)
			return
		}
		t.Logf("deregistered the webhook %s", wh.ID)
	})
	t.Logf("registered webhook %s on team %s, pointing at %s", wh.ID, cfg.team, shownEndpoint)

	// The row that makes a delivery somebody's. The secret came from ClickUp and goes nowhere but
	// this throwaway database.
	sub, err := clickup.SubscriptionFor(liveTenant, cfg.team, wh.ID, []byte(wh.Secret))
	if err != nil {
		t.Fatalf("SubscriptionFor: %v", err)
	}
	if _, err := e.subs.Register(e.ctx, sub); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// The one write on content this item authorizes.
	text := "Lawang automated acceptance check (B12). It is safe to delete. " + time.Now().UTC().Format(time.RFC3339)
	commentID, err := api.postComment(e.ctx, cfg.task, text)
	if err != nil {
		t.Fatalf("post the comment: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := api.deleteComment(ctx, commentID); err != nil {
			t.Errorf("THE COMMENT IS STILL ON THE TASK. Delete it by hand: comment %s on task %s: %v",
				commentID, cfg.task, err)
			return
		}
		t.Logf("deleted the comment %s", commentID)
	})
	t.Logf("posted comment %s on task %s", commentID, cfg.task)

	// Now wait for ClickUp to deliver it, and for the drain to carry it to the sink.
	wantTaskID := clickup.Key + ":task:" + cfg.task
	wantCommentID := clickup.Key + ":comment:" + commentID
	deadline := time.Now().Add(3 * time.Minute)
	for len(e.stub.Documents(liveTenant)) < 2 && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
	}

	bodies, codes := e.seen.all()
	for i, body := range bodies {
		record0(t, fmt.Sprintf("live_webhook_%d.json", i), body)
		t.Logf("delivery %d answered %d", i, codes[i])
	}
	if len(bodies) == 0 {
		t.Fatalf("no delivery arrived at %s in three minutes. Either the tunnel container is not running "+
			"(make tunnel), or the hostname does not point at it, or Cloudflare is not forwarding "+
			"/ingress/%s to port 8080", shownEndpoint, clickup.Key)
	}
	// THE signature assertion. Every delivery was signed by ClickUp with the real secret, and a
	// 401 anywhere here is Verify disagreeing with ClickUp.
	for i, code := range codes {
		if code == http.StatusUnauthorized {
			t.Errorf("delivery %d was answered 401: ClickUp signed it with the real webhook secret and "+
				"clickup.Verify did not accept it, which is the one thing this run exists to check", i)
		}
	}

	docs := decodeLive(t, e.stub.Documents(liveTenant))
	if len(docs) != 2 {
		t.Fatalf("%d records reached the sink, want exactly 2 (the comment and its re-hydrated parent task): %+v",
			len(docs), docs)
	}
	if docs[0].Kind != string(record.KindTask) || docs[0].ExternalID != wantTaskID {
		t.Errorf("the first record is %s %s, want a task %s", docs[0].Kind, docs[0].ExternalID, wantTaskID)
	}
	if docs[1].Kind != string(record.KindMessage) || docs[1].ExternalID != wantCommentID {
		t.Errorf("the second record is %s %s, want a message %s", docs[1].Kind, docs[1].ExternalID, wantCommentID)
	}
	if docs[1].Edges.ReplyParent != wantTaskID {
		t.Errorf("the comment's reply parent is %q, want %q", docs[1].Edges.ReplyParent, wantTaskID)
	}
	if docs[0].Visibility.Scope != docs[1].Visibility.Scope {
		t.Errorf("the task is in scope %q and its comment in %q, and a comment is decided on its task's list",
			docs[0].Visibility.Scope, docs[1].Visibility.Scope)
	}
	if rows := liveOutboxStates(t, e); len(rows) != 1 || rows[0] != outbox.StateDelivered {
		t.Errorf("the outbox holds %v, want one delivered row", rows)
	}

	// The README's questions this delivery can answer, from the bytes ClickUp sent.
	answerTheREADME(t, bodies)

	// And one more thing the tunnel makes possible: a forged delivery over the real path, from the
	// public internet, which must be a 401 and must store nothing. It sends nothing to ClickUp.
	forged := bytes.Clone(bodies[0])
	status := postRaw(e.ctx, t, endpoint, forged, sign([]byte("not the webhook secret"), forged))
	if status != http.StatusUnauthorized {
		t.Errorf("a forged delivery over the tunnel answered %d, want 401", status)
	}
	if got := len(e.stub.Documents(liveTenant)); got != 2 {
		t.Errorf("the forged delivery produced records: the sink holds %d documents, want 2", got)
	}

	after, afterRaw, err := api.task(e.ctx, cfg.task)
	if err != nil {
		t.Errorf("read the test task again: %v", err)
	} else {
		record0(t, "live_api_task_after.json", afterRaw)
		finding(t, "does date_updated move when a comment is posted",
			fmt.Sprintf("before %q, after %q, moved=%v", before.DateUpdated, after.DateUpdated,
				before.DateUpdated != after.DateUpdated))
	}
	comments, err := api.comments(e.ctx, cfg.task, nil)
	if err != nil {
		t.Errorf("read the task's comments: %v", err)
	} else {
		record0(t, "live_api_comments.json", comments)
	}
}

// answerTheREADME reads the open questions of internal/provider/clickup/testdata/README.md off the
// bodies ClickUp actually sent. It asserts nothing about them on purpose: a finding that
// contradicts the documentation is a decision for the maintainer and for the fixture-replacement
// pass, and a test that failed on one would stop the run that produced it.
//
// The one exception is the question ADR 15 decision 4's safety argument rests on, whether
// taskUpdated accompanies taskMoved, which no comment delivery can answer. That one needs a move,
// and TestLiveClickUpAMoveSendsTaskUpdated is where it is asked.
func answerTheREADME(t *testing.T, bodies [][]byte) {
	t.Helper()
	type item struct {
		Date    json.RawMessage `json:"date"`
		Field   string          `json:"field"`
		Comment json.RawMessage `json:"comment"`
	}
	var anyTopLevelWorkspace []string
	var events []string
	allDatesDecimalStrings := true
	for _, body := range bodies {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err != nil {
			t.Errorf("a delivery ClickUp sent is not a JSON object: %v", err)
			continue
		}
		for _, key := range []string{"team_id", "workspace_id", "team", "workspace"} {
			if _, ok := top[key]; ok {
				anyTopLevelWorkspace = append(anyTopLevelWorkspace, key)
			}
		}
		var event string
		if raw, ok := top["event"]; ok {
			_ = json.Unmarshal(raw, &event)
		}
		events = append(events, event)
		var items []item
		if raw, ok := top["history_items"]; ok {
			if err := json.Unmarshal(raw, &items); err != nil {
				t.Errorf("history_items is not the shape this provider reads: %v", err)
			}
		}
		for _, it := range items {
			var s string
			if err := json.Unmarshal(it.Date, &s); err != nil || !isDecimalRun(s) {
				allDatesDecimalStrings = false
				finding(t, "is every history_items[].date a decimal string",
					fmt.Sprintf("NO: on a history item with field %q the date is %s", it.Field, string(it.Date)))
			}
			if it.Field == "comment" && len(it.Comment) > 0 {
				finding(t, "the shape of history_items[].comment on taskCommentPosted", string(it.Comment))
			}
		}
	}
	finding(t, "the events ClickUp sent for one comment", strings.Join(events, ", "))
	if len(anyTopLevelWorkspace) == 0 {
		finding(t, "does a task event carry a workspace or team id at the top level",
			"NO, as the documentation says, so DeliveryKeys reading only webhook_id is right")
	} else {
		finding(t, "does a task event carry a workspace or team id at the top level",
			"YES: "+strings.Join(anyTopLevelWorkspace, ", ")+". DeliveryKeys could narrow candidates by it as well")
	}
	if allDatesDecimalStrings {
		finding(t, "is every history_items[].date a decimal string", "YES on every item of every delivery seen here")
	}
}

func isDecimalRun(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil && !strings.ContainsAny(s, "+-")
}

// TestLiveClickUpAMoveSendsTaskUpdated asks the one question ADR 15 decision 4's safety argument
// rests on: when a task is moved between lists, does ClickUp send taskUpdated beside taskMoved?
//
// ClickUp's v2 API has no endpoint that moves a task between lists, so the move is the
// maintainer's to make in the browser while this test waits. That is why it is opt-in and why it
// prints what to do. It writes nothing to ClickUp itself: its only footprint is the webhook, which
// it deregisters.
func TestLiveClickUpAMoveSendsTaskUpdated(t *testing.T) {
	cfg := liveSkip(t)
	if os.Getenv(envMove) == "" {
		t.Skipf("the move probe needs a task moved between two lists by hand while it waits: set %s=1 to run it", envMove)
	}
	api := newLiveAPI(cfg.token)
	e := liveSetup(t, cfg)

	before, _, err := api.task(e.ctx, cfg.task)
	if err != nil {
		t.Fatalf("read the test task: %v", err)
	}
	endpoint := endpointFor(cfg)
	wh, err := api.createWebhook(e.ctx, cfg.team, endpoint)
	if err != nil {
		t.Fatalf("create the webhook: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := api.deleteWebhook(ctx, wh.ID); err != nil {
			t.Errorf("THE WEBHOOK IS STILL REGISTERED. Delete it by hand: id %s, pointing at %s: %v", wh.ID, shownEndpoint, err)
		}
	})
	sub, err := clickup.SubscriptionFor(liveTenant, cfg.team, wh.ID, []byte(wh.Secret))
	if err != nil {
		t.Fatalf("SubscriptionFor: %v", err)
	}
	if _, err := e.subs.Register(e.ctx, sub); err != nil {
		t.Fatalf("Register: %v", err)
	}

	wait := 3 * time.Minute
	t.Logf("MOVE TASK %s OUT OF LIST %q (%s) INTO ANOTHER LIST IN THE SAME WORKSPACE NOW, in the ClickUp "+
		"interface. Waiting %s for the deliveries.", cfg.task, before.List.Name, before.List.ID, wait)
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		bodies, _ := e.seen.all()
		if len(bodies) >= 2 {
			break // taskMoved and something beside it, which is the whole question
		}
	}

	bodies, codes := e.seen.all()
	if len(bodies) == 0 {
		t.Fatal("no delivery arrived, so the move either did not happen or the tunnel is not forwarding")
	}
	var events []string
	for i, body := range bodies {
		record0(t, fmt.Sprintf("live_move_webhook_%d.json", i), body)
		var d struct {
			Event string `json:"event"`
		}
		_ = json.Unmarshal(body, &d)
		events = append(events, d.Event)
		if codes[i] == http.StatusUnauthorized {
			t.Errorf("delivery %d of the move was answered 401, which is Verify disagreeing with ClickUp", i)
		}
	}
	moved, updated := false, false
	for _, ev := range events {
		switch ev {
		case "taskMoved":
			moved = true
		case "taskUpdated":
			updated = true
		}
	}
	finding(t, "the events a move sends", strings.Join(events, ", "))
	finding(t, "does taskUpdated accompany taskMoved (ADR 15 decision 4)",
		fmt.Sprintf("taskMoved=%v, taskUpdated=%v. If taskMoved arrived WITHOUT taskUpdated, ADR 15 "+
			"decision 4's safety argument does not hold and the A to B and back to A case is reachable "+
			"from comment deliveries alone", moved, updated))

	after, _, err := api.task(e.ctx, cfg.task)
	if err != nil {
		t.Errorf("read the task after the move: %v", err)
		return
	}
	finding(t, "does date_updated move on a cross-list move",
		fmt.Sprintf("list %q to %q, date_updated %q to %q, moved=%v",
			before.List.ID, after.List.ID, before.DateUpdated, after.DateUpdated,
			before.DateUpdated != after.DateUpdated))
	if before.List.ID == after.List.ID {
		t.Errorf("the task is still in list %s, so no move happened and the findings above are about nothing",
			after.List.ID)
	}
}

// TestLiveClickUpCommentPaging asks whether GET /task/{id}/comment really pages with start and
// start_id the way internal/provider/clickup's api.comment assumes. It is read-only.
func TestLiveClickUpCommentPaging(t *testing.T) {
	cfg := liveSkip(t)
	api := newLiveAPI(cfg.token)

	first, err := api.comments(context.Background(), cfg.task, nil)
	if err != nil {
		t.Fatalf("read the first page of comments: %v", err)
	}
	type page struct {
		Comments []struct {
			ID   json.RawMessage `json:"id"`
			Date json.RawMessage `json:"date"`
		} `json:"comments"`
	}
	var p page
	if err := json.Unmarshal(first, &p); err != nil {
		t.Fatalf("the comments answer is not the shape this provider reads: %v", err)
	}
	finding(t, "how many comments the first page carries", strconv.Itoa(len(p.Comments)))
	if len(p.Comments) < 25 {
		finding(t, "does GET /task/{id}/comment page with start and start_id",
			"not answerable on this task: it has fewer than one page of comments, so no second page can be asked for")
		return
	}
	oldest := p.Comments[len(p.Comments)-1]
	id, err := looseID(oldest.ID)
	if err != nil {
		t.Fatalf("the oldest comment of the first page has no usable id: %v", err)
	}
	date, err := looseID(oldest.Date)
	if err != nil {
		t.Fatalf("the oldest comment of the first page has no usable date: %v", err)
	}
	second, err := api.comments(context.Background(), cfg.task, url.Values{"start": {date}, "start_id": {id}})
	if err != nil {
		t.Fatalf("read the second page of comments: %v", err)
	}
	var q page
	if err := json.Unmarshal(second, &q); err != nil {
		t.Fatalf("the second page is not the shape this provider reads: %v", err)
	}
	switch {
	case len(q.Comments) == 0:
		finding(t, "does GET /task/{id}/comment page with start and start_id",
			"the second page came back empty, so either the task has exactly one page or the contract differs")
	case bytes.Equal(first, second):
		finding(t, "does GET /task/{id}/comment page with start and start_id",
			"NO: the second request returned the same page, so api.comment would loop on its own first page "+
				"and every comment past the first page is ErrNotFound")
	default:
		finding(t, "does GET /task/{id}/comment page with start and start_id",
			fmt.Sprintf("YES: a second page of %d comments came back, different from the first", len(q.Comments)))
	}
	record0(t, "live_api_comments_page1.json", first)
	record0(t, "live_api_comments_page2.json", second)
}

// TestLiveClickUpSecondWebhookSecret asks whether a webhook secret is issued per workspace or per
// webhook. It is opt-in because the answer costs two webhook registrations on a real workspace,
// and a run that is interrupted leaves them behind.
func TestLiveClickUpSecondWebhookSecret(t *testing.T) {
	cfg := liveSkip(t)
	if os.Getenv(envSecond) == "" {
		t.Skipf("the secret-scope probe registers two webhooks on the real workspace: set %s=1 to run it", envSecond)
	}
	api := newLiveAPI(cfg.token)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	endpoint := endpointFor(cfg)
	var ids []string
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, id := range ids {
			if err := api.deleteWebhook(clean, id); err != nil {
				t.Errorf("THE WEBHOOK IS STILL REGISTERED. Delete it by hand: id %s, pointing at %s: %v", id, shownEndpoint, err)
			}
		}
	})
	var secrets []string
	for range 2 {
		wh, err := api.createWebhook(ctx, cfg.team, endpoint)
		if err != nil {
			t.Fatalf("create a webhook: %v", err)
		}
		ids = append(ids, wh.ID)
		secrets = append(secrets, wh.Secret)
	}
	// The secrets themselves are never printed. Only whether they are the same string.
	same := secrets[0] == secrets[1]
	finding(t, "is one webhook secret issued per workspace or per webhook",
		fmt.Sprintf("two webhooks on one workspace came back with the same secret: %v. "+
			"The same secret means per workspace, which would let one secret verify another "+
			"registration's deliveries; different means per webhook, as the documentation says", same))
	if len(ids) == 2 && ids[0] == ids[1] {
		t.Error("ClickUp returned one webhook id twice, so the second registration replaced the first")
	}
}

// decodeLive reads what the stub holds into the same shape the fake-server half asserts about.
func decodeLive(t *testing.T, raw [][]byte) []doc {
	t.Helper()
	out := make([]doc, 0, len(raw))
	for i, b := range raw {
		var d doc
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatalf("document %d at the sink is not readable: %v", i, err)
		}
		out = append(out, d)
	}
	return out
}

// liveOutboxStates is the state of every outbox row, read as the superuser.
func liveOutboxStates(t *testing.T, e *liveEnv) []string {
	t.Helper()
	return adminQuery(e.ctx, newAdminDB(e.ctx, t, e.tdb.AdminURL),
		`SELECT state FROM lawang.outbox ORDER BY seq`,
		func(r pgx.CollectableRow) (string, error) {
			var s string
			err := r.Scan(&s)
			return s, err
		})
}

// postRaw posts bytes to a URL with one signature header, which is how a forgery reaches the real
// endpoint from the public internet.
func postRaw(ctx context.Context, t *testing.T, endpoint string, body []byte, signature string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build the forged delivery: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(clickup.SignatureHeader, signature)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("post the forged delivery: %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return resp.StatusCode
}
