package acceptance

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// The tenant the fake-server half routes to, and a second one that exists only so that "routed to
// the right tenant" is a claim with something to be wrong about.
const (
	tenantA = tenancy.ID("t_acceptance_a")
	tenantB = tenancy.ID("t_acceptance_b")
)

// The identifiers the fixtures under internal/provider/clickup/testdata carry. They are read out
// of the fixtures at setup rather than written here, so that a recording that replaces those files
// moves these with it; the constants below are only what the fixtures are expected to agree about,
// and fixtureIDs fails the test if they have drifted.
const (
	wantTask    = "86a1b2"
	wantComment = "648893191"
	wantList    = "901100"
)

// The secrets the fake-server half signs with. They are test material: no provider ever sees them
// and nothing here reads a credential from anybody's machine.
var (
	secretA = []byte("the-webhook-secret-of-tenant-a")
	secretB = []byte("the-webhook-secret-of-tenant-b")
)

// discard is where every component in the suite logs, so that a failing test prints its own
// assertion and not a page of structured log lines.
func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fixture reads one of B11's ClickUp payloads.
//
// It is the one place that reaches out of this package, and the reason is in the package comment:
// the suite runs against the fixtures the live run replaces, so the replacement needs no change
// here. A missing file is a hard failure and never a skip, because a suite that silently stops
// demonstrating M1 is worse than a red build.
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "provider", "clickup", "testdata", name)
	b, err := os.ReadFile(path) //nolint:gosec // a fixture of this repository, named by a constant
	if err != nil {
		t.Fatalf("read the ClickUp fixture %s: %v", name, err)
	}
	return b
}

// fixtureIDs holds the fixtures to what the suite assumes about them, before any test runs.
//
// Without it, a recording that replaces the fixtures with a different task or comment id would
// make every assertion below compare two strings that are both wrong in the same way, and the
// suite would stay green while demonstrating nothing. The ids are read from the payloads
// themselves and compared with the constants, so a drift names itself.
func fixtureIDs(t testing.TB) {
	t.Helper()
	var webhook struct {
		TaskID  string `json:"task_id"`
		History []struct {
			Comment *struct {
				ID string `json:"id"`
			} `json:"comment"`
		} `json:"history_items"`
	}
	if err := json.Unmarshal(fixture(t, "webhook_comment_posted.json"), &webhook); err != nil {
		t.Fatalf("the comment webhook fixture is not readable: %v", err)
	}
	var task struct {
		ID   string `json:"id"`
		List struct {
			ID string `json:"id"`
		} `json:"list"`
	}
	if err := json.Unmarshal(fixture(t, "api_task.json"), &task); err != nil {
		t.Fatalf("the task fixture is not readable: %v", err)
	}
	// The comment id of the API answer, which is the one id hydration has to match ACROSS two
	// fixtures: the webhook names the comment and the comments endpoint is where it is read back.
	// Held with its own minimal struct, on comments[].id specifically, because the file carries a
	// numeric id on a nested user object as well.
	var comments struct {
		Comments []struct {
			ID string `json:"id"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(fixture(t, "api_comments.json"), &comments); err != nil {
		t.Fatalf("the comments fixture is not readable: %v", err)
	}
	var apiComment string
	if len(comments.Comments) > 0 {
		apiComment = comments.Comments[0].ID
	}
	var comment string
	for _, h := range webhook.History {
		if h.Comment != nil && h.Comment.ID != "" {
			comment = h.Comment.ID
			break
		}
	}
	for _, c := range []struct{ what, got, want string }{
		{"the webhook's task id", webhook.TaskID, wantTask},
		{"the API task's id", task.ID, wantTask},
		{"the webhook's comment id", comment, wantComment},
		{"the API comments' comment id", apiComment, wantComment},
		{"the API task's list id", task.List.ID, wantList},
	} {
		if c.got != c.want {
			t.Fatalf("%s is %q and this suite expects %q: the fixtures have been replaced, so update "+
				"the constants in harness_test.go rather than letting the assertions compare two wrong strings",
				c.what, c.got, c.want)
		}
	}
}

// fakeClickUp is ClickUp's API, as this suite serves it: the two endpoints the provider calls, the
// payloads of B11's testdata behind them, and a refusal for everything else.
//
// It refuses what the real API refuses, which is the rule a test double is held to (CLAUDE.md): no
// Authorization header is a 401, an unknown task is a 404, and a path this provider does not call
// is a 404. Without those, a provider that lost the credential or built the wrong path would be
// demonstrated working.
type fakeClickUp struct {
	taskBody     []byte
	commentsBody []byte

	mu    sync.Mutex
	calls int
	paths []string
}

func (f *fakeClickUp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()

	if r.Header.Get("Authorization") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v2/task/" + wantTask:
		_, _ = w.Write(f.taskBody)
	case "/api/v2/task/" + wantTask + "/comment":
		_, _ = w.Write(f.commentsBody)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// seen is how many requests the provider has made, and to what.
func (f *fakeClickUp) seen() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append([]string(nil), f.paths...)
}

// sinks is the per-tenant sink the drain is given: the strict stub, with the one failure this
// suite needs in front of it.
//
// Everything it does not break goes through to the stub, so no assertion in this suite is made
// against a lenient double (architecture principle 5). A refusal by the strict stub fails the test
// where it happens, rather than looking like a record that did not arrive.
//
// Sink, though, deliberately answers for EVERY tenant, including one with no subscription and the
// sentinel, where the real factory halts instead (architecture section 8: "No sink can be built
// for the tenant ... halt: nothing is delivered"). That is the one place this double is looser than
// what it stands in for, and it is looser on purpose. Clause 5 proves "never routed" by showing
// that tenant B's and the sentinel's sinks are empty. If this factory refused those tenants, a
// delivery that crossed into one of them would be refused by the FACTORY, and the clause would see
// an empty sink, which is exactly what it sees when nothing crossed at all: the one failure it
// exists to catch would be hidden behind a halt. Answering for everybody is what makes a
// wrong-tenant delivery land somewhere an assertion can see it. The fail-closed path is B14's,
// where the per-tenant sink configuration arrives and can be refused on its own terms.
type sinks struct {
	t     testing.TB
	inner *sink.Stub

	mu sync.Mutex
	// panics is how many more Deliver calls die, which is what a worker killed between its two
	// commits looks like from the outside.
	panics int
	calls  int
	// offered is the record ids of every batch, in order, including the batch of a call that
	// died. It is here because the documents the stub holds cannot answer "was this record
	// offered twice": a Sink is idempotent on the record id, so a batch carrying one id twice
	// leaves the receiver holding exactly what one copy would have, and the duplicate is
	// invisible from the outside. This is the only place it is visible.
	offered [][]string
}

func (s *sinks) Sink(context.Context, tenancy.ID) (sink.Sink, error) { return s, nil }

func (s *sinks) Deliver(ctx context.Context, t tenancy.ID, recs []record.Record) (sink.DeliveryResult, error) {
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	s.mu.Lock()
	s.calls++
	s.offered = append(s.offered, ids)
	if s.panics > 0 {
		s.panics--
		s.mu.Unlock()
		panic("the sink call died, as a worker killed between prepare and deliver does")
	}
	s.mu.Unlock()
	result, err := s.inner.Deliver(ctx, t, recs)
	if err == nil && len(result.Rejected) > 0 {
		s.t.Errorf("the strict stub refused %d records this suite meant to deliver: %+v",
			len(result.Rejected), result.Rejected)
	}
	return result, err
}

func (s *sinks) dieOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.panics = 1
}

func (s *sinks) deliverCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// batches is the record ids of every batch the sink was offered, in order.
func (s *sinks) batches() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, 0, len(s.offered))
	for _, b := range s.offered {
		out = append(out, append([]string(nil), b...))
	}
	return out
}

// env is one test's whole deployment: a migrated Postgres, the edge on a real socket, the hub, the
// ClickUp provider pointed at the fake API, the pipeline, the drain and the strict stub sink.
type env struct {
	t   *testing.T
	ctx context.Context
	db  *store.DB
	tdb testdb.Database

	adm *adminDB

	api   *fakeClickUp
	edge  *httptest.Server
	stub  *sink.Stub
	sinks *sinks
	drain *worker.Drain
	ob    *outbox.Outbox
}

// setup builds the deployment. It is the composition cmd/lawang will make once a vault can hand out
// a token per tenant (B13) and a sink can be built per tenant (B14); until then this is the only
// place the whole of M1 is wired together, which is what makes it the acceptance suite.
func setup(t *testing.T) *env {
	t.Helper()
	fixtureIDs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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

	api := &fakeClickUp{
		taskBody:     fixture(t, "api_task.json"),
		commentsBody: fixture(t, "api_comments.json"),
	}
	apiSrv := httptest.NewServer(api)
	t.Cleanup(apiSrv.Close)

	cu, err := clickup.New(clickup.Options{Tokens: vault{}, BaseURL: apiSrv.URL})
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
	// The webhook edge alone. The rest of the routing table (and the property that every route
	// of it is behind the path guard) belongs to the command that builds the table, and
	// cmd/lawang's TestEveryRouteIsBehindTheIngressGuard asserts it there.
	handler, err := ingress.New(reg, h, ingress.Options{Logger: discard()})
	if err != nil {
		t.Fatalf("ingress.New: %v", err)
	}
	// A real socket, not an httptest.ResponseRecorder: two of the three redirects net/http.ServeMux
	// answers are only visible over one (the B06 log row in docs/backlog.md), and a redirect turns
	// every delivery into a 401 that reads as a forgery.
	edge := httptest.NewServer(handler)
	t.Cleanup(edge.Close)

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
		Pool:  1,
		Batch: 5,
		// Short, because this suite moves a lease along itself rather than waiting for one. The
		// row timeout has to stay under the lease, which NewDrain refuses otherwise.
		Lease:      10 * time.Second,
		RowTimeout: 5 * time.Second,
		Poll:       10 * time.Millisecond,
		Logger:     discard(),
	})
	if err != nil {
		t.Fatalf("NewDrain: %v", err)
	}

	e := &env{t: t, ctx: ctx, db: db, tdb: tdb, adm: newAdminDB(ctx, t, tdb.AdminURL),
		api: api, edge: edge, stub: stub, sinks: s, drain: drain, ob: outbox.New(db)}
	e.register(tenantA, secretA)
	return e
}

// vault is the credential seam B13 fills, as a constant.
//
// It refuses what the real one refuses: a tenant that is not one gets nothing. Without that, a
// hydration that reached the API with no tenant would be demonstrated working.
type vault struct{}

func (vault) Token(_ context.Context, t tenancy.ID) (string, error) {
	if _, err := tenancy.Parse(t.String()); err != nil {
		return "", fmt.Errorf("no token for this tenant: %w", err)
	}
	return "a-token-of-this-test-and-nothing-else", nil
}

// subscriptions is the subscription table, which is where an operator's registrar will write
// (B14) and the only place the accept path resolves an owner in.
func (e *env) subscriptions() *hub.Subscriptions { return hub.NewSubscriptions(e.db) }

// register stores the ClickUp subscription the fixtures' deliveries arrive on, built through
// clickup.SubscriptionFor so that the row carries the webhook id as its external key, which is the
// only key a ClickUp delivery has.
func (e *env) register(tenant tenancy.ID, secret []byte) {
	e.t.Helper()
	sub, err := clickup.SubscriptionFor(tenant, fixtureWorkspace(e.t), fixtureWebhook(e.t), secret)
	if err != nil {
		e.t.Fatalf("SubscriptionFor: %v", err)
	}
	if _, err := e.subscriptions().Register(e.ctx, sub); err != nil {
		e.t.Fatalf("Register(%s): %v", tenant, err)
	}
}

// fixtureWorkspace and fixtureWebhook are the ids the fixtures carry, read from the payloads so
// that a recording brings its own.
func fixtureWorkspace(t testing.TB) string {
	t.Helper()
	var task struct {
		TeamID string `json:"team_id"`
	}
	if err := json.Unmarshal(fixture(t, "api_task.json"), &task); err != nil {
		t.Fatalf("the task fixture is not readable: %v", err)
	}
	if task.TeamID == "" {
		t.Fatal("the task fixture carries no team_id, so no subscription can name the workspace it covers")
	}
	return task.TeamID
}

func fixtureWebhook(t testing.TB) string {
	t.Helper()
	var d struct {
		WebhookID string `json:"webhook_id"`
	}
	if err := json.Unmarshal(fixture(t, "webhook_comment_posted.json"), &d); err != nil {
		t.Fatalf("the comment webhook fixture is not readable: %v", err)
	}
	if d.WebhookID == "" {
		t.Fatal("the comment webhook fixture carries no webhook_id, so nothing could resolve its owner")
	}
	return d.WebhookID
}

// sign is ClickUp's signature over body: the hex of HMAC-SHA256 with the webhook's own secret
// (https://developer.clickup.com/docs/webhooksignature). It is written here rather than taken from
// internal/provider/clickup, which deliberately exports no signer: a provider that signed and
// verified with one function would verify its own arithmetic and nothing else.
func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// deliver posts a body to the webhook edge the way ClickUp does, with the signature it names, and
// returns the status the provider would see.
//
// signature is written out rather than derived here so that a test can send one that is over
// different bytes, which is the whole of the forgery case.
func (e *env) deliver(body []byte, signature string) int {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost,
		e.edge.URL+"/ingress/"+clickup.Key, bytes.NewReader(body))
	if err != nil {
		e.t.Fatalf("build the delivery: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set(clickup.SignatureHeader, signature)
	}
	resp, err := e.edge.Client().Do(req)
	if err != nil {
		e.t.Fatalf("post the delivery: %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return resp.StatusCode
}

// deliverSigned posts a body signed with secret, which is the honest case.
func (e *env) deliverSigned(body, secret []byte) int {
	e.t.Helper()
	return e.deliver(body, sign(secret, body))
}

// deliverWithSignatures posts a body carrying several signature headers, which is several claims
// where the scheme allows one. It is a separate helper because net/http joins repeated Set calls
// into one value and only Add makes two header fields, and two fields is the shape the edge has to
// pass through unchanged for the provider to be able to refuse them.
func (e *env) deliverWithSignatures(body []byte, signatures ...string) int {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.ctx, http.MethodPost,
		e.edge.URL+"/ingress/"+clickup.Key, bytes.NewReader(body))
	if err != nil {
		e.t.Fatalf("build the delivery: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, s := range signatures {
		req.Header.Add(clickup.SignatureHeader, s)
	}
	resp, err := e.edge.Client().Do(req)
	if err != nil {
		e.t.Fatalf("post the delivery: %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return resp.StatusCode
}

// run starts the drain and returns the function that stops it and waits for it to finish.
//
// The drain is the real one, polling, and not a step function: Drain.Once is exported only to
// internal/worker's own tests, and a suite that drove the drain a step at a time would not be
// running what a deployment runs. A test therefore waits for a condition (see wait) instead of
// counting turns.
func (e *env) run() (stop func()) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := e.drain.Run(ctx); err != nil {
			e.t.Errorf("the drain returned an error: %v", err)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// wait polls until cond holds, and fails with what the deployment looked like when it did not.
//
// The bound is short on purpose: a condition that has not come true in a few seconds, with a
// drain polling every ten milliseconds and nothing waiting on a network, is not slow but wrong,
// and a test that is expected to fail must fail fast (.claude/agents/implementer.md).
func (e *env) wait(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("waited for %s and it never happened.\n  outbox: %+v\n  documents at the sink: %d\n  ledger rows: %d\n  calls to the ClickUp API: %d",
				what, e.outboxRows(), len(e.documents(tenantA)), e.ledgerCount(), e.apiCalls())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// documents is what reached the stub for a tenant, in arrival order.
func (e *env) documents(t tenancy.ID) [][]byte { return e.stub.Documents(t) }

func (e *env) apiCalls() int {
	n, _ := e.api.seen()
	return n
}

// outboxRow is what the suite asserts about a row.
//
// Attempts is read for two reasons. It is the only thing that tells a row which is simply slow
// from one the drain keeps claiming and failing, which is a defect that never reaches a final
// state and so can never satisfy a wait; and it shows up in wait's expiry dump, where it is the
// first thing worth knowing.
type outboxRow struct {
	Tenant      string
	Provider    string
	State       string
	IsHead      bool
	Attempts    int
	Leased      bool
	DeadReason  string
	OrderingKey string
}

// outboxRows reads every row of the outbox, in queue order, as the superuser, so that nothing
// about row-level security can hide a row from an assertion that says there is none.
func (e *env) outboxRows() []outboxRow {
	e.t.Helper()
	return adminQuery(e.ctx, e.adm,
		`SELECT tenant_id, provider, state, is_head, attempts, lease_token IS NOT NULL, dead_reason, ordering_key
		   FROM lawang.outbox ORDER BY seq`,
		func(r pgx.CollectableRow) (outboxRow, error) {
			var o outboxRow
			err := r.Scan(&o.Tenant, &o.Provider, &o.State, &o.IsHead, &o.Attempts, &o.Leased,
				&o.DeadReason, &o.OrderingKey)
			return o, err
		})
}

// settled is true of a state a row will not leave again: delivered, or dead-lettered.
//
// It exists so that a wait can end on the WRONG outcome as well as the right one. A wait that ends
// only on the state the test wants can be satisfied by nothing but that state, so a defect which
// parks or dead-letters the row instead leaves the wait to run out, and the assertions written to
// name that defect never run at all: the test fails as a bare twenty second timeout. Clause 3's
// second wait says the same thing at the line, and this is that lesson as a predicate rather than
// as one fix in one place.
func settled(r outboxRow) bool {
	return r.State == outbox.StateDelivered || r.State == outbox.StateDead
}

// retrying is a row the drain has claimed and handed back without finishing: pending again, its
// lease gone, waiting for its next attempt.
//
// This is the case a final state cannot cover, and it is the one the review of #62 found. A defect
// that makes a claim fail leaves the row to be retried for as long as the test is willing to wait,
// so it never settles at all and a wait watching only for a final state runs out. Reading it here,
// rather than waiting for a second attempt to appear, is also what makes such a wait end at once
// instead of sitting through the retry backoff first. Measured against the second spelling of the
// ledger survivor (if false && known, which lets the re-insert run and violates the ledger's
// primary key): the clause failed as a 23.3 second timeout before this change, as a 9.3 second
// named failure when the condition was "attempted twice", and as a 2.4 second named failure once
// it became this.
//
// In a correct run of this suite no row is ever in this state. A row the drain is working on holds
// a lease, and one it has finished with is delivered or dead. The one deliberate retry is clause
// 3's crash, which moves the lease along itself and has its own two-sided waits.
func retrying(r outboxRow) bool {
	return r.State == outbox.StatePending && r.Attempts > 0 && !r.Leased
}

// done is a row that has stopped: settled, or stuck in retry, or past a first attempt it should
// not have needed. Any of the three is an outcome a wait may end on, and the last two are wrong.
func done(r outboxRow) bool { return settled(r) || retrying(r) || r.Attempts > 1 }

// oneRowDone is the wait condition of a test that has sent one delivery and expects its one row to
// be delivered.
//
// More than one row is a wrong outcome too, and ending on it is what turns a defect that stores a
// second row into a named assertion instead of a timeout.
func (e *env) oneRowDone() bool {
	rows := e.outboxRows()
	if len(rows) != 1 {
		return len(rows) > 1
	}
	return done(rows[0])
}

// doneFor is true when some row of this tenant is done. It is the form the parked test needs: a
// dead row is the normal state of the row IT parked, so "any row is dead" says nothing there, and
// the tenant whose delivery is being waited for is what has to be watched.
func (e *env) doneFor(tenant tenancy.ID) bool {
	for _, r := range e.outboxRows() {
		if r.Tenant == tenant.String() && done(r) {
			return true
		}
	}
	return false
}

// ledgerCount is how many records have been prepared, over every tenant. A re-drain that prepared
// a delivery twice shows here and nowhere else.
func (e *env) ledgerCount() int {
	e.t.Helper()
	return adminCount(e.ctx, e.adm, `SELECT count(*) FROM lawang.record_ledger`)
}

// preparedRecordCount is how many records have been prepared into a delivery, over every tenant:
// one row of outbox_record per record a delivery is going to offer a sink.
//
// It is the count that tells a record the ledger held back from one it offered again. The sink
// cannot: a Sink is idempotent on the record id and the strict stub answers a repeat whose content
// matches with a silent no-op, which is correct of it and which makes "the sink still holds two
// documents" true whether the ledger skipped the records or re-delivered them. A mutation of the
// ledger's skip survived a version of the re-send test that only counted documents.
func (e *env) preparedRecordCount() int {
	e.t.Helper()
	return adminCount(e.ctx, e.adm, `SELECT count(*) FROM lawang.outbox_record`)
}

// redactionCount is how many distinct values the masker has mapped. It is what tells "the text at
// the sink carries no address" apart from "nothing ever looked", which is the way that assertion
// passes for the wrong reason.
func (e *env) redactionCount() int {
	e.t.Helper()
	return adminCount(e.ctx, e.adm, `SELECT count(*) FROM lawang.redaction_map`)
}

// admin changes rows behind the application's back, to move time along.
func (e *env) admin(sql string) { testdb.Exec(e.t, e.tdb.AdminURL, sql) }

// leaseExpired is what a worker that died leaves behind: its rows keep their lease until it runs
// out, and then another worker may take them.
func (e *env) leaseExpired() {
	e.t.Helper()
	e.admin("UPDATE lawang.outbox SET lease_until = now() - interval '1 second' WHERE lease_token IS NOT NULL")
}

// adminDB is one superuser connection, kept open for the test that holds it.
//
// Every assertion in this suite reads as the superuser, so that nothing about row-level security
// can hide a row from an assertion that says there is none. It is one connection and not one per
// read because a wait polls its condition every few milliseconds: a connection per read was a few
// thousand logins per waiting test, which is churn a reviewer should not have to find.
type adminDB struct {
	mu   sync.Mutex
	t    testing.TB
	url  string
	conn *pgx.Conn
}

func newAdminDB(ctx context.Context, t testing.TB, url string) *adminDB {
	t.Helper()
	a := &adminDB{t: t, url: url}
	t.Cleanup(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.conn != nil {
			_ = a.conn.Close(ctx)
		}
	})
	return a
}

// at returns the connection, opening it on first use. A connection the server has closed is
// reopened, because a test that waits for a condition outlives any one idle timeout.
func (a *adminDB) at(ctx context.Context) *pgx.Conn {
	a.t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn != nil && !a.conn.IsClosed() {
		return a.conn
	}
	conn, err := pgx.Connect(ctx, a.url)
	if err != nil {
		a.t.Fatal("connect as the superuser") // not wrapped: the error quotes the URL
	}
	a.conn = conn
	return conn
}

func adminQuery[T any](ctx context.Context, a *adminDB, sql string, scan func(pgx.CollectableRow) (T, error)) []T {
	a.t.Helper()
	rows, err := a.at(ctx).Query(ctx, sql)
	if err != nil {
		a.t.Fatalf("query: %v", err)
	}
	got, err := pgx.CollectRows(rows, scan)
	if err != nil {
		a.t.Fatalf("read the rows: %v", err)
	}
	return got
}

func adminCount(ctx context.Context, a *adminDB, sql string) int {
	a.t.Helper()
	var n int
	if err := a.at(ctx).QueryRow(ctx, sql).Scan(&n); err != nil {
		a.t.Fatalf("count: %v", err)
	}
	return n
}

// doc is the part of a delivered document the suite asserts about. It is read back out of the
// bytes the sink received, not out of a record.Record this process still holds, because the bytes
// are what a receiver gets.
type doc struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	Kind       string `json:"kind"`
	ExternalID string `json:"external_id"`
	Version    string `json:"version"`
	Supersedes string `json:"supersedes"`
	Title      string `json:"title"`
	Text       string `json:"text"`
	Visibility struct {
		Scope    string `json:"scope"`
		Audience string `json:"audience"`
	} `json:"visibility"`
	Container struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"container"`
	Edges struct {
		ReplyParent string `json:"reply_parent"`
	} `json:"edges"`
}

// docsAt decodes what the stub holds for a tenant.
func (e *env) docsAt(t tenancy.ID) []doc {
	e.t.Helper()
	raw := e.documents(t)
	out := make([]doc, 0, len(raw))
	for i, b := range raw {
		var d doc
		if err := json.Unmarshal(b, &d); err != nil {
			e.t.Fatalf("document %d at the sink is not readable: %v", i, err)
		}
		out = append(out, d)
	}
	return out
}

// wantNoRows fails when anything at all reached the outbox.
func (e *env) wantNoRows(why string) {
	e.t.Helper()
	if got := e.outboxRows(); len(got) != 0 {
		e.t.Fatalf("the outbox holds %d rows (%+v), want none: %s", len(got), got, why)
	}
}

// wantNoDocuments fails when anything reached the sink, for either tenant.
func (e *env) wantNoDocuments(why string) {
	e.t.Helper()
	for _, t := range []tenancy.ID{tenantA, tenantB, tenancy.Sentinel} {
		if got := e.documents(t); len(got) != 0 {
			e.t.Fatalf("%d documents reached the sink for %s, want none: %s", len(got), t, why)
		}
	}
}
