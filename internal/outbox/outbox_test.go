package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/store"
	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/testdb"
)

const (
	tenantA = tenancy.ID("tenant_a")
	tenantB = tenancy.ID("tenant_b")
	lease   = time.Minute
)

// The failures the tests record.
var (
	badShape    = outbox.NewCause(outbox.ClassNormalizer)
	sink503     = outbox.NewCause(outbox.ClassSinkUnavailable).WithStatus(503)
	sinkTimeout = outbox.NewCause(outbox.ClassSinkUnavailable)
)

type env struct {
	t   *testing.T
	ctx context.Context
	db  *store.DB
	ob  *outbox.Outbox
	tdb testdb.Database
}

// setup gives a test its own migrated database. plannerOff names planner settings (enable_nestloop
// and the like) to turn off for every session of that database, for the tests that must hold
// whatever plan the claim gets.
func setup(t *testing.T, plannerOff ...string) *env {
	t.Helper()
	settings := make([]string, len(plannerOff))
	for i, setting := range plannerOff {
		settings[i] = setting + " = off"
	}
	return setupWith(t, settings...)
}

// setupWith is setup for a database with settings of its own, each written as in ALTER DATABASE
// ... SET: "name = value". The database is the test's own, so this changes nothing for any other.
func setupWith(t *testing.T, settings ...string) *env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	tdb := testdb.New(t)
	for _, setting := range settings { // before the pool opens: a session reads these when it starts
		testdb.Exec(t, tdb.AdminURL, fmt.Sprintf(
			`DO $do$ BEGIN EXECUTE format($f$ALTER DATABASE %%I SET %s$f$, current_database()); END $do$`, setting))
	}
	db, err := store.Open(ctx, tdb.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Migrate(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return &env{t: t, ctx: ctx, db: db, ob: outbox.New(db), tdb: tdb}
}

// accept stores a delivery whose body is unique to (key, version).
func (e *env) accept(tenant tenancy.ID, key string, version int) string {
	e.t.Helper()
	id, fresh, err := e.ob.Accept(e.ctx, tenant, outbox.Delivery{
		Provider: "fake", OrderingKey: key, RawBody: fmt.Appendf(nil, `{"key":%q,"v":%d}`, key, version),
	})
	if err != nil || !fresh {
		e.t.Fatalf("Accept(%s, %s, v%d) = fresh %v, err %v", tenant, key, version, fresh, err)
	}
	return id
}

func (e *env) claim() []outbox.Claimed {
	e.t.Helper()
	got, err := e.ob.Claim(e.ctx, 10, lease)
	if err != nil {
		e.t.Fatalf("Claim: %v", err)
	}
	return got
}

func (e *env) claimOne(wantID string) outbox.Claimed {
	e.t.Helper()
	got := e.claim()
	if len(got) != 1 || got[0].ID() != wantID {
		e.t.Fatalf("Claim = %v, want exactly row %s", claimedIDs(got), wantID)
	}
	return got[0]
}

func (e *env) claimNone(why string) {
	e.t.Helper()
	if got := e.claim(); len(got) != 0 {
		e.t.Fatalf("Claim = %v, want nothing: %s", claimedIDs(got), why)
	}
}

// admin changes rows behind the application's back, to move time along.
func (e *env) admin(sql string) { testdb.Exec(e.t, e.tdb.AdminURL, sql) }

func claimedIDs(cs []outbox.Claimed) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID()
	}
	return out
}

func TestAcceptIsIdempotentPerTenant(t *testing.T) {
	e := setup(t)
	d := outbox.Delivery{Provider: "fake", OrderingKey: "task:1", RawBody: []byte(`{"a":1}`)}

	id, fresh, err := e.ob.Accept(e.ctx, tenantA, d)
	if err != nil || !fresh || id == "" {
		t.Fatalf("first Accept = %q, %v, %v", id, fresh, err)
	}
	again, fresh, err := e.ob.Accept(e.ctx, tenantA, d)
	if err != nil || fresh || again != "" {
		t.Fatalf("re-sent Accept = %q, fresh %v, err %v, want a no-op", again, fresh, err)
	}

	// Two tenants may connect the same provider workspace. The second one's identical delivery is
	// its own, not a duplicate of the first tenant's.
	if _, fresh, err := e.ob.Accept(e.ctx, tenantB, d); err != nil || !fresh {
		t.Fatalf("same bytes for another tenant: fresh %v, err %v, want accepted", fresh, err)
	}

	if got := e.claim(); len(got) != 2 {
		t.Errorf("claimable rows = %d, want 2 (one per tenant)", len(got))
	}
}

func TestAcceptRefusesBadInput(t *testing.T) {
	e := setup(t)
	for name, d := range map[string]outbox.Delivery{
		"no provider":     {OrderingKey: "k", RawBody: []byte("x")},
		"no ordering key": {Provider: "fake", RawBody: []byte("x")},
		// The provider is the connector's own constant, so these are bugs and have no sentinel.
		// They are still refused by this package, in words, and not by the database in bytes.
		"a NUL in the provider":         {Provider: "fa\x00ke", OrderingKey: "k", RawBody: []byte("x")},
		"invalid UTF-8 in the provider": {Provider: "fa\xffke", OrderingKey: "k", RawBody: []byte("x")},
	} {
		_, _, err := e.ob.Accept(e.ctx, tenantA, d)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if sqlState(err) != "" {
			t.Errorf("%s: the refusal came from the database (%v), want it made before the database is asked", name, err)
		}
	}
	if _, _, err := e.ob.Accept(e.ctx, tenancy.ID(""), outbox.Delivery{Provider: "fake", OrderingKey: "k"}); err == nil {
		t.Error("accepted a delivery with no tenant")
	}
}

// TestAcceptRefusesAnOrderingKeyItCannotStore: a key is derived from what a sender sent. One that
// is too long for an index tuple would fail the INSERT, or not, depending on how well it
// compresses, and the caller could not tell that from the database being down. It is refused by
// name instead, so that the ingress can park the delivery as poison and answer 2xx.
func TestAcceptRefusesAnOrderingKeyItCannotStore(t *testing.T) {
	e := setup(t)
	accept := func(key string) error {
		_, _, err := e.ob.Accept(e.ctx, tenantA, outbox.Delivery{Provider: "fake", OrderingKey: key, RawBody: []byte(key)})
		return err
	}
	if err := accept(strings.Repeat("k", outbox.MaxOrderingKeyLen)); err != nil {
		t.Errorf("a key of MaxOrderingKeyLen bytes: %v, want accepted", err)
	}
	unstorable := map[string]string{
		"one byte too long": strings.Repeat("k", outbox.MaxOrderingKeyLen+1),
		// Bytes, not characters: 256 two-byte runes are 512 bytes, 257 are not.
		"one rune too long": strings.Repeat("é", outbox.MaxOrderingKeyLen/2+1),
		// 100,000 of one character compress into an index tuple, and used to be accepted.
		"long but compressible": strings.Repeat("k", 100_000),
		"empty":                 "",
		// Postgres stores neither in text. A NUL is one JSON escape away: encoding/json decodes
		// the escape for U+0000 inside an entity id into this byte without complaint.
		"a NUL":                "task:\x001-secret",
		"only a NUL":           "\x00",
		"a NUL at the end":     "task:12-secret\x00",
		"invalid UTF-8":        "task:\xff\xfe-secret",
		"a truncated rune":     "task:12-secret\xc3",
		"a surrogate, encoded": "task:\xed\xa0\x80-secret",
	}
	for name, key := range unstorable {
		err := accept(key)
		if !errors.Is(err, outbox.ErrBadOrderingKey) {
			t.Errorf("%s: err = %v, want ErrBadOrderingKey", name, err)
		}
		if sqlState(err) != "" {
			t.Errorf("%s: the refusal came from the database (%v), want it made before the database is asked", name, err)
		}
		if err != nil && (len(key) >= 8 && strings.Contains(err.Error(), key[:8]) || strings.Contains(err.Error(), "secret")) {
			t.Errorf("%s: the error quotes the key: %v", name, err)
		}
	}
	// Nothing of the refused deliveries is there, and the queue is as usable as before.
	var stored int
	if err := e.adminConn().QueryRow(e.ctx, "SELECT count(*) FROM lawang.outbox").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Errorf("%d rows stored, want only the one key that was accepted", stored)
	}
	// What is merely unusual is a key like any other: valid UTF-8 of any script, and control
	// characters other than NUL.
	for name, key := range map[string]string{
		"four-byte runes":   "task:\U0001F600\U0001F600",
		"a control byte":    "task:\x01\x1f\x7f",
		"U+FFFD, as itself": "task:\uFFFD",
	} {
		if err := accept(key); err != nil {
			t.Errorf("%s: %v, want accepted", name, err)
		}
	}
	if err := accept(strings.Repeat("é", outbox.MaxOrderingKeyLen/2)); err != nil {
		t.Errorf("256 two-byte runes, which are 512 bytes: %v, want accepted", err)
	}

	// The table says the same, for a writer that does not come through Accept.
	err := e.adminErr(fmt.Sprintf(`INSERT INTO lawang.outbox (id, tenant_id, provider, delivery_id, ordering_key, raw_body)
	                               VALUES ('x', 'tenant_a', 'fake', 'x', repeat('k', %d), '')`, outbox.MaxOrderingKeyLen+1))
	if sqlState(err) != "23514" {
		t.Errorf("a raw INSERT of a key one byte too long: err = %v, want a check violation", err)
	}
}

func TestSecondVersionIsNotClaimableWhileTheFirstIsInFlight(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)
	v2 := e.accept(tenantA, "task:1", 2)

	c1 := e.claimOne(v1)
	e.claimNone("v1 is leased, so v2 must wait behind it")

	if err := e.ob.MarkPrepared(e.ctx, c1); err != nil {
		t.Fatal(err)
	}
	e.claimNone("v1 is prepared but not delivered, so v2 must still wait")

	if err := e.ob.MarkDelivered(e.ctx, c1); err != nil {
		t.Fatal(err)
	}
	c2 := e.claimOne(v2)
	if err := e.ob.MarkDelivered(e.ctx, c2); err != nil {
		t.Fatal(err)
	}
	e.claimNone("everything is delivered")
}

func TestOrderingKeysAreIndependentAcrossEntitiesAndTenants(t *testing.T) {
	e := setup(t)
	e.accept(tenantA, "task:1", 1)
	e.accept(tenantA, "task:1", 2) // behind the head
	e.accept(tenantA, "task:2", 1)
	e.accept(tenantB, "task:1", 1) // same key, other tenant: its own queue

	if got := e.claim(); len(got) != 3 {
		t.Errorf("claimed %d rows, want the 3 heads", len(got))
	}
}

func TestBackoffHoldsTheWholeKey(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)
	e.accept(tenantA, "task:1", 2)

	c1 := e.claimOne(v1)
	if err := e.ob.Fail(e.ctx, c1, outbox.Ladder{time.Hour}, sink503); err != nil {
		t.Fatal(err)
	}
	e.claimNone("v1 is waiting out its backoff, and v2 may not overtake it")

	e.admin("UPDATE lawang.outbox SET next_attempt_at = now() - interval '1 second'")
	c1 = e.claimOne(v1)
	if c1.Attempt() != 2 {
		t.Errorf("Attempt = %d, want 2", c1.Attempt())
	}
	row, err := e.ob.Get(e.ctx, tenantA, v1)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastError != "sink unavailable (status 503)" || row.State != outbox.StatePending {
		t.Errorf("row = state %q, last_error %q", row.State, row.LastError)
	}
}

func TestRetryKeepsThePreparedState(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)
	c := e.claimOne(v1)
	if err := e.ob.MarkPrepared(e.ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := e.ob.Fail(e.ctx, c, outbox.Ladder{time.Hour}, sinkTimeout); err != nil {
		t.Fatal(err)
	}
	row, _ := e.ob.Get(e.ctx, tenantA, v1)
	if row.State != outbox.StatePrepared {
		t.Errorf("state after a delivery retry = %q, want prepared (it must not prepare twice)", row.State)
	}
	e.admin("UPDATE lawang.outbox SET next_attempt_at = now() - interval '1 second'")
	e.claimOne(v1)
}

func TestExhaustedLadderParksTheRowAndReplayRevivesIt(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)
	v2 := e.accept(tenantA, "task:1", 2)

	c1 := e.claimOne(v1)
	if err := e.ob.Fail(e.ctx, c1, outbox.Ladder{}, sink503); err != nil {
		t.Fatal(err)
	}
	row, _ := e.ob.Get(e.ctx, tenantA, v1)
	if row.State != outbox.StateDead || row.DeadReason == "" || row.FinishedAt == nil {
		t.Errorf("row = state %q, reason %q, finished %v, want a dead letter", row.State, row.DeadReason, row.FinishedAt)
	}

	// A dead letter does not hold its entity hostage.
	c2 := e.claimOne(v2)
	if err := e.ob.MarkDelivered(e.ctx, c2); err != nil {
		t.Fatal(err)
	}

	if err := e.ob.Replay(e.ctx, tenantB, v1); !errors.Is(err, outbox.ErrNotFound) {
		t.Errorf("another tenant replaying the row: err = %v, want ErrNotFound", err)
	}
	if err := e.ob.Replay(e.ctx, tenantA, v1); err != nil {
		t.Fatal(err)
	}
	c1 = e.claimOne(v1)
	if c1.Attempt() != 1 {
		t.Errorf("Attempt after replay = %d, want 1", c1.Attempt())
	}
	if err := e.ob.Replay(e.ctx, tenantA, v1); !errors.Is(err, outbox.ErrNotFound) {
		t.Errorf("replaying a row that is not dead: err = %v, want ErrNotFound", err)
	}
}

func TestAnExpiredLeaseIsTakenOverAndTheOldHolderIsShutOut(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)

	crashed := e.claimOne(v1)
	e.claimNone("the lease is still running")

	e.admin("UPDATE lawang.outbox SET lease_until = now() - interval '1 second'")
	takeover := e.claimOne(v1)
	if takeover.Attempt() != 2 {
		t.Errorf("Attempt = %d, want 2", takeover.Attempt())
	}

	// The first worker was only slow, not dead, and now comes back.
	for name, err := range map[string]error{
		"MarkPrepared":  e.ob.MarkPrepared(e.ctx, crashed),
		"MarkDelivered": e.ob.MarkDelivered(e.ctx, crashed),
		"Fail":          e.ob.Fail(e.ctx, crashed, outbox.DefaultLadder, sink503),
		"MarkDead":      e.ob.MarkDead(e.ctx, crashed, badShape),
	} {
		if !errors.Is(err, outbox.ErrLeaseLost) {
			t.Errorf("%s by the old holder: err = %v, want ErrLeaseLost", name, err)
		}
	}
	if err := e.ob.MarkDelivered(e.ctx, takeover); err != nil {
		t.Errorf("the current holder: %v", err)
	}
	if err := e.ob.MarkDelivered(e.ctx, takeover); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Errorf("delivering twice: err = %v, want ErrLeaseLost", err)
	}
	if err := e.ob.MarkDelivered(e.ctx, outbox.Claimed{}); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Errorf("a Claimed that never came from Claim: err = %v, want ErrLeaseLost", err)
	}
}

// TestAClaimedAimedAtAnotherTenantChangesNothing is the isolation property of the transitions: they
// run bound to the tenant the Claimed names, so a genuine lease whose tenant was swapped for
// another one meets row-level security and finds no row. Outside this package a Claimed cannot be
// altered at all. This takes the one way in that a test has, to show what would happen if a later
// change ran the transitions under anything wider than the row's own tenant.
func TestAClaimedAimedAtAnotherTenantChangesNothing(t *testing.T) {
	e := setup(t)
	// Tenant B has a row of the same entity, leased by the same Claim and so under the same token.
	// It must not move either: a transition names one row.
	other := e.accept(tenantB, "task:1", 1)
	v1 := e.accept(tenantA, "task:1", 1)
	var genuine outbox.Claimed
	for _, c := range e.claim() {
		if c.ID() == v1 {
			genuine = c
		}
	}
	if genuine.ID() != v1 || genuine.Tenant() != tenantA {
		t.Fatalf("claimed %s for %s, want %s for %s", genuine.ID(), genuine.Tenant(), v1, tenantA)
	}
	before, err := e.ob.Get(e.ctx, tenantA, v1)
	if err != nil {
		t.Fatal(err)
	}

	swapped := outbox.WithTenant(genuine, tenantB)
	for name, err := range map[string]error{
		"MarkPrepared":  e.ob.MarkPrepared(e.ctx, swapped),
		"MarkDelivered": e.ob.MarkDelivered(e.ctx, swapped),
		"Fail":          e.ob.Fail(e.ctx, swapped, outbox.Ladder{time.Hour}, sink503),
		"MarkDead":      e.ob.MarkDead(e.ctx, swapped, badShape),
	} {
		if !errors.Is(err, outbox.ErrLeaseLost) {
			t.Errorf("%s with the tenant swapped: err = %v, want ErrLeaseLost", name, err)
		}
	}
	after, err := e.ob.Get(e.ctx, tenantA, v1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("tenant A's row changed:\nbefore %+v\nafter  %+v", before, after)
	}
	if row, err := e.ob.Get(e.ctx, tenantB, other); err != nil || row.State != outbox.StatePending || row.LeaseUntil == nil {
		t.Errorf("tenant B's own row = state %q, lease %v, err %v, want pending and still leased", row.State, row.LeaseUntil, err)
	}
	// The lease itself is untouched: its real holder still finishes the row.
	if err := e.ob.MarkDelivered(e.ctx, genuine); err != nil {
		t.Errorf("the genuine holder afterwards: %v", err)
	}
}

// TestAnErrorIsNotALostLease: ErrLeaseLost tells a worker to stop working on the row, and any other
// error tells it that the transition may be tried again while the lease lasts. A database or
// context error must therefore never read as a lost lease, and must change nothing.
func TestAnErrorIsNotALostLease(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)
	dead := e.accept(tenantA, "task:2", 1)
	cancelled, cancel := context.WithCancel(e.ctx)
	cancel()

	if got, err := e.ob.Claim(cancelled, 10, lease); err == nil || len(got) != 0 {
		t.Errorf("Claim with a cancelled context = %v, %v, want an error and no rows", claimedIDs(got), err)
	}
	claimed := e.claim()
	if len(claimed) != 2 {
		t.Fatalf("claimed %d rows, want both: the failed Claim must not have leased any", len(claimed))
	}
	var c, toKill outbox.Claimed
	for _, got := range claimed {
		if got.Attempt() != 1 {
			t.Errorf("row %s is on attempt %d, want 1: the failed Claim used one up", got.ID(), got.Attempt())
		}
		if got.ID() == v1 {
			c = got
		} else {
			toKill = got
		}
	}
	before, err := e.ob.Get(e.ctx, tenantA, v1)
	if err != nil {
		t.Fatal(err)
	}

	for name, err := range map[string]error{
		"MarkPrepared":  e.ob.MarkPrepared(cancelled, c),
		"MarkDelivered": e.ob.MarkDelivered(cancelled, c),
		"Fail":          e.ob.Fail(cancelled, c, outbox.Ladder{time.Hour}, sink503),
		"Fail to dead":  e.ob.Fail(cancelled, c, outbox.Ladder{}, sink503),
		"MarkDead":      e.ob.MarkDead(cancelled, c, badShape),
	} {
		if err == nil || errors.Is(err, outbox.ErrLeaseLost) {
			t.Errorf("%s with a cancelled context: err = %v, want an error that is not ErrLeaseLost", name, err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s with a cancelled context: err = %v, want it to wrap context.Canceled", name, err)
		}
	}
	after, err := e.ob.Get(e.ctx, tenantA, v1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the row changed:\nbefore %+v\nafter  %+v", before, after)
	}
	if err := e.ob.MarkDelivered(e.ctx, c); err != nil {
		t.Errorf("the same lease, once the context is good again: %v", err)
	}

	// The two that are not lease-guarded say the same: a failure is not "no such row".
	if err := e.ob.MarkDead(e.ctx, toKill, badShape); err != nil {
		t.Fatal(err)
	}
	if err := e.ob.Replay(cancelled, tenantA, dead); err == nil || errors.Is(err, outbox.ErrNotFound) {
		t.Errorf("Replay with a cancelled context: err = %v, want an error that is not ErrNotFound", err)
	}
	if _, _, err := e.ob.Accept(cancelled, tenantA, outbox.Delivery{Provider: "fake", OrderingKey: "k", RawBody: []byte("x")}); err == nil {
		t.Error("Accept with a cancelled context: no error")
	}
	// "Nothing is stranded" is an answer too, and a failure must not read as that one.
	if got, err := e.ob.StrandedKeys(cancelled, tenantA, 10); err == nil {
		t.Errorf("StrandedKeys with a cancelled context = %q and no error", got)
	}
	if _, err := e.ob.Get(cancelled, tenantA, v1); err == nil || errors.Is(err, outbox.ErrNotFound) {
		t.Errorf("Get with a cancelled context: err = %v, want an error that is not ErrNotFound", err)
	}
}

// TestClaimRefusesABatchOrLeaseThatIsNotPositive: the guard is load-bearing. A lease of zero would
// hand out rows that are claimable again at once, every poll would use up one attempt, and the
// ladder would be gone in seconds with no delivery ever tried.
func TestClaimRefusesABatchOrLeaseThatIsNotPositive(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)
	for name, call := range map[string]struct {
		batch int
		lease time.Duration
	}{
		"batch 0":          {0, lease},
		"batch -1":         {-1, lease},
		"lease 0":          {10, 0},
		"a negative lease": {10, -time.Second},
	} {
		if got, err := e.ob.Claim(e.ctx, call.batch, call.lease); err == nil || len(got) != 0 {
			t.Errorf("Claim with %s = %v, %v, want an error and no rows", name, claimedIDs(got), err)
		}
	}
	row, err := e.ob.Get(e.ctx, tenantA, v1)
	if err != nil {
		t.Fatal(err)
	}
	if row.Attempts != 0 || row.LeaseUntil != nil {
		t.Errorf("after the refused claims the row has attempts %d and lease %v, want untouched", row.Attempts, row.LeaseUntil)
	}
	if c := e.claimOne(v1); c.Attempt() != 1 {
		t.Errorf("Attempt = %d, want 1", c.Attempt())
	}
}

// TestClaimNeverLeasesMoreThanMaxBatch: a caller that asks for more gets MaxBatch, and the rest
// stays claimable.
func TestClaimNeverLeasesMoreThanMaxBatch(t *testing.T) {
	e := setup(t)
	const extra = 5
	e.admin(fmt.Sprintf(`
		INSERT INTO lawang.outbox (id, tenant_id, provider, delivery_id, ordering_key, raw_body, is_head)
		SELECT 'r' || g, 'tenant_a', 'fake', 'r' || g, 'entity:' || g, '', true
		  FROM generate_series(1, %d) g`, outbox.MaxBatch+extra))

	got, err := e.ob.Claim(e.ctx, 5*outbox.MaxBatch, lease)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != outbox.MaxBatch {
		t.Errorf("a batch of %d leased %d rows, want MaxBatch = %d", 5*outbox.MaxBatch, len(got), outbox.MaxBatch)
	}
	if rest := e.claim(); len(rest) != extra {
		t.Errorf("the next claim leased %d rows, want the %d that were left", len(rest), extra)
	}
}

func TestTheWorkerRoleNeverReadsAPayload(t *testing.T) {
	e := setup(t)
	e.accept(tenantA, "task:1", 1)

	for _, stmt := range []string{
		"SELECT raw_body FROM outbox",
		"SELECT * FROM outbox",
		"SELECT provider, delivery_id FROM outbox",
		"SELECT lease_token FROM outbox", // it writes the token, and never reads one back
		// The claim reads is_head and due_at, and nothing they are made from: not which entity a
		// row belongs to, not its state, and not the two times due_at is computed from.
		"SELECT ordering_key FROM outbox",
		"SELECT state FROM outbox",
		"SELECT next_attempt_at FROM outbox",
		"SELECT lease_until FROM outbox",
		// It can lease a head. It can never make one, or make a row due.
		"UPDATE outbox SET is_head = true",
		"UPDATE outbox SET next_attempt_at = now()",
		"UPDATE outbox SET state = 'delivered'",
		"UPDATE outbox SET tenant_id = 'tenant_b'",
		"DELETE FROM outbox",
		"INSERT INTO outbox (id, tenant_id, provider, delivery_id, ordering_key, raw_body) VALUES ('x','tenant_a','p','d','k','')",
	} {
		err := e.db.RoleTx(e.ctx, store.RoleWorker, func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, stmt)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("worker role ran %q: err = %v, want permission denied", stmt, err)
		}
	}

	// What it may see is the scheduling columns, across tenants.
	err := e.db.RoleTx(e.ctx, store.RoleWorker, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(e.ctx, "SELECT count(id) FROM outbox").Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("worker role sees %d rows, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// And the application role with no tenant bound sees nothing at all.
	err = e.db.Tx(e.ctx, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(e.ctx, "SELECT count(*) FROM outbox").Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("unbound application role sees %d rows, want 0", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGetIsTenantScoped(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	if _, err := e.ob.Get(e.ctx, tenantB, id); !errors.Is(err, outbox.ErrNotFound) {
		t.Errorf("another tenant's Get: err = %v, want ErrNotFound", err)
	}
}

// repairKey is the repair statement of ADR 10, as written there: under the key's lock, hand the
// marker to the earliest unfinished row. Where the key has a head already it changes nothing, or is
// refused by the unique index if that head is not the earliest row.
const repairKey = `
	BEGIN;
	SELECT pg_advisory_xact_lock(hashtextextended('%[1]s' || chr(31) || '%[2]s', 0));
	UPDATE lawang.outbox SET is_head = true
	 WHERE id = (SELECT id FROM lawang.outbox
	              WHERE tenant_id = '%[1]s' AND ordering_key = '%[2]s' AND state IN ('pending', 'prepared')
	              ORDER BY seq LIMIT 1);
	COMMIT;`

// TestStrandedKeysFindsAKeyWithWorkAndNoHead: a key whose unfinished rows have no head among them
// is never claimed and never fails, so the only way to learn of it is to look. The keys here are
// broken on purpose, by the superuser and behind the package's back, which is the way it happens.
func TestStrandedKeysFindsAKeyWithWorkAndNoHead(t *testing.T) {
	e := setup(t)
	stranded := func(tenant tenancy.ID, limit int) []string {
		t.Helper()
		got, err := e.ob.StrandedKeys(e.ctx, tenant, limit)
		if err != nil {
			t.Fatalf("StrandedKeys(%s): %v", tenant, err)
		}
		return got
	}

	// Everything a healthy queue contains, none of which is stranded: a head with rows behind it,
	// a head in flight, a head backing off, a key with nothing left, a dead letter, and a row
	// replayed behind a newer version.
	first := map[string]string{"healthy:queue": e.accept(tenantA, "healthy:queue", 1)}
	second := map[string]string{"healthy:queue": e.accept(tenantA, "healthy:queue", 2)}
	e.accept(tenantA, "healthy:queue", 3)
	done := e.accept(tenantA, "healthy:done", 1)
	dead := e.accept(tenantA, "healthy:dead", 1)
	e.accept(tenantA, "healthy:dead", 2)
	backoff := e.accept(tenantA, "healthy:backoff", 1)
	e.accept(tenantA, "healthy:backoff", 2)
	for _, c := range e.claim() {
		var err error
		switch c.ID() {
		case done:
			err = e.ob.MarkDelivered(e.ctx, c)
		case dead:
			err = e.ob.MarkDead(e.ctx, c, badShape)
		case backoff:
			err = e.ob.Fail(e.ctx, c, outbox.Ladder{time.Hour}, sink503)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := e.ob.Replay(e.ctx, tenantA, dead); err != nil {
		t.Fatal(err)
	}
	if got := stranded(tenantA, 10); len(got) != 0 {
		t.Fatalf("StrandedKeys of a healthy queue = %q, want none", got)
	}

	// Three broken keys of tenant A, and one of tenant B under a name tenant A also has.
	for _, key := range []string{"lost:b", "lost:a", "lost:c"} {
		first[key] = e.accept(tenantA, key, 1)
		second[key] = e.accept(tenantA, key, 2)
		e.admin("UPDATE lawang.outbox SET is_head = false WHERE id = '" + first[key] + "'")
	}
	e.accept(tenantA, "theirs", 1) // healthy for A
	e.admin("UPDATE lawang.outbox SET is_head = false WHERE id = '" + e.accept(tenantB, "theirs", 1) + "'")

	if got, want := stranded(tenantA, 10), []string{"lost:a", "lost:b", "lost:c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("StrandedKeys(tenant A) = %q, want %q: each key once, in key order", got, want)
	}
	if got, want := stranded(tenantB, 10), []string{"theirs"}; !reflect.DeepEqual(got, want) {
		t.Errorf("StrandedKeys(tenant B) = %q, want %q", got, want)
	}
	if got, want := stranded(tenantA, 2), []string{"lost:a", "lost:b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("StrandedKeys with a limit of 2 = %q, want %q", got, want)
	}
	for _, limit := range []int{0, -1} {
		if got, err := e.ob.StrandedKeys(e.ctx, tenantA, limit); err == nil {
			t.Errorf("StrandedKeys with a limit of %d = %q, want it refused", limit, got)
		}
	}

	// However many there are and whatever is asked for, one call returns at most MaxBatch.
	const tenantC = tenancy.ID("tenant_c")
	e.admin(fmt.Sprintf(`
		INSERT INTO lawang.outbox (id, tenant_id, provider, delivery_id, ordering_key, raw_body)
		SELECT 's' || g, '%s', 'fake', 's' || g, 'stranded:' || g, ''
		  FROM generate_series(1, %d) g`, tenantC, outbox.MaxBatch+5))
	if got := stranded(tenantC, 5*outbox.MaxBatch); len(got) != outbox.MaxBatch {
		t.Errorf("StrandedKeys with a limit of %d returned %d keys, want MaxBatch = %d", 5*outbox.MaxBatch, len(got), outbox.MaxBatch)
	}

	// The documented repair makes a key deliverable again, in queue order, and takes it off the list.
	e.admin(fmt.Sprintf(repairKey, tenantA, "lost:a"))
	if got, want := stranded(tenantA, 10), []string{"lost:b", "lost:c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after the repair of lost:a, StrandedKeys = %q, want %q", got, want)
	}
	// Run on a key that was never broken, it changes nothing.
	e.admin(fmt.Sprintf(repairKey, tenantA, "healthy:queue"))
	for _, key := range []string{"lost:a", "healthy:queue"} {
		if !e.isHead(first[key]) || e.isHead(second[key]) {
			t.Errorf("%s after the repair: the first row is a head = %v, the second = %v, want the first and only the first",
				key, e.isHead(first[key]), e.isHead(second[key]))
		}
	}
	// And the repaired key delivers, in queue order. (This claim also leases the other heads that
	// are due, so the one after it can only return what the delivery below makes claimable.)
	var claimed []string
	for _, c := range e.claim() {
		if c.ID() == first["lost:a"] {
			if err := e.ob.MarkDelivered(e.ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		claimed = append(claimed, c.ID())
	}
	if !slices.Contains(claimed, first["lost:a"]) || slices.Contains(claimed, second["lost:a"]) {
		t.Errorf("Claim = %v, want the first row of the repaired key (%s) and not the second", claimed, first["lost:a"])
	}
	if got := claimedIDs(e.claim()); !reflect.DeepEqual(got, []string{second["lost:a"]}) {
		t.Errorf("Claim after the first row was delivered = %v, want the second row (%s)", got, second["lost:a"])
	}
}

// TestAnIDNoRowCanHaveIsNotFound: an id reaches Get and Replay from whoever operates the dead
// letters. One that Postgres cannot even compare with (a NUL, invalid UTF-8) names no row, and the
// answer to that is ErrNotFound, not the database's complaint about a byte sequence, which a caller
// would have to take for an outage.
func TestAnIDNoRowCanHaveIsNotFound(t *testing.T) {
	e := setup(t)
	id := e.accept(tenantA, "task:1", 1)
	for name, bad := range map[string]string{"a NUL": id + "\x00", "invalid UTF-8": id + "\xff"} {
		if _, err := e.ob.Get(e.ctx, tenantA, bad); !errors.Is(err, outbox.ErrNotFound) {
			t.Errorf("Get of an id with %s: err = %v, want ErrNotFound", name, err)
		}
		if err := e.ob.Replay(e.ctx, tenantA, bad); !errors.Is(err, outbox.ErrNotFound) {
			t.Errorf("Replay of an id with %s: err = %v, want ErrNotFound", name, err)
		}
	}
}

// TestConcurrentClaimersNeverShareARow hammers Claim from many goroutines, several times over.
func TestConcurrentClaimersNeverShareARow(t *testing.T) {
	e := setup(t)
	admin := e.adminConn()
	const (
		rounds   = 5
		rows     = 120
		claimers = 8
	)
	for round := range rounds {
		want := make(map[string]bool, rows)
		for i := range rows {
			tenant := tenantA
			if i%2 == 1 {
				tenant = tenantB
			}
			want[e.accept(tenant, fmt.Sprintf("r%d:entity:%d", round, i), 1)] = true
		}

		var mu sync.Mutex
		seen := make(map[string]int)
		var wg sync.WaitGroup
		for range claimers {
			wg.Go(func() {
				for {
					got, err := e.ob.Claim(e.ctx, 7, lease)
					if err != nil {
						t.Errorf("Claim: %v", err)
						return
					}
					if len(got) == 0 {
						return
					}
					mu.Lock()
					again := false
					for _, c := range got {
						seen[c.ID()]++
						again = again || seen[c.ID()] > 1
					}
					mu.Unlock()
					if again {
						return // reported below. A claim that hands rows out twice never runs dry.
					}
				}
			})
		}
		wg.Wait()

		for id, n := range seen {
			if n != 1 {
				t.Errorf("round %d: row %s was claimed %d times", round, id, n)
			}
			if !want[id] {
				t.Errorf("round %d: claimed unexpected row %s", round, id)
			}
		}
		if len(seen) != rows {
			t.Errorf("round %d: %d rows claimed, want %d", round, len(seen), rows)
		}
		e.checkHeads(admin)
		e.admin("UPDATE lawang.outbox SET state = 'delivered', is_head = false, lease_until = NULL, lease_token = NULL, finished_at = now() WHERE state <> 'delivered'")
	}
}

// TestConcurrentWorkersDeliverEachEntityInOrder is the FIFO guarantee under load: no two versions
// of one entity are ever in flight together, and each entity's versions finish in arrival order.
func TestConcurrentWorkersDeliverEachEntityInOrder(t *testing.T) {
	e := setup(t)
	const (
		entities = 25
		versions = 6
		workers  = 8
	)
	// Versions of one entity sit next to each other in the queue, which is the worst case: a claim
	// that ignored the head-of-key rule would pick up several of them in a single batch.
	for k := range entities {
		tenant := tenantA
		if k%2 == 1 {
			tenant = tenantB
		}
		for v := 1; v <= versions; v++ {
			e.accept(tenant, fmt.Sprintf("entity:%d", k), v)
		}
	}

	var (
		mu        sync.Mutex
		inFlight  = map[string]string{} // tenant/key -> row id
		lastSeq   = map[string]int64{}
		delivered int
		wg        sync.WaitGroup
	)
	for range workers {
		wg.Go(func() {
			idle := 0
			for idle < 20 {
				got, err := e.ob.Claim(e.ctx, 3, lease)
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				if len(got) == 0 {
					// Heads may all be leased by other workers right now. Look again shortly.
					idle++
					time.Sleep(5 * time.Millisecond)
					continue
				}
				idle = 0

				// Everything in the batch is in flight from the moment it is claimed, not from
				// the moment its turn comes.
				batch := make([]outbox.Row, len(got))
				for i, c := range got {
					row, err := e.ob.Get(e.ctx, c.Tenant(), c.ID())
					if err != nil {
						t.Errorf("Get: %v", err)
						return
					}
					batch[i] = row
					key := row.TenantID + "/" + row.OrderingKey
					mu.Lock()
					if other, busy := inFlight[key]; busy {
						t.Errorf("%s: row %s claimed while %s is still in flight", key, c.ID(), other)
					}
					if row.Seq <= lastSeq[key] {
						t.Errorf("%s: seq %d claimed after seq %d was delivered", key, row.Seq, lastSeq[key])
					}
					inFlight[key] = c.ID()
					mu.Unlock()
				}

				for i, c := range got {
					key := batch[i].TenantID + "/" + batch[i].OrderingKey
					time.Sleep(time.Millisecond) // the work
					// The bookkeeping is cleared BEFORE the commit that finishes the row, and
					// that order matters. MarkDelivered hands the head marker to the next version
					// in the same transaction, so the moment it commits another worker may
					// legitimately claim that version; clearing afterwards leaves a window in
					// which this goroutine has not been scheduled yet and the other worker's
					// perfectly correct claim reads as a violation. CI failed that way on a loaded
					// runner. Nothing is weakened by the swap: the next version cannot be claimed
					// until this one finishes, so no real overlap can hide in the new window,
					// while the defect this test exists to catch (a claim that ignores the
					// head-of-key rule) still trips the check, because a batch is marked in flight
					// at claim time and no row of a key is ever delivered before it is claimed.
					mu.Lock()
					lastSeq[key] = batch[i].Seq
					delete(inFlight, key)
					delivered++
					mu.Unlock()
					if err := e.ob.MarkDelivered(e.ctx, c); err != nil {
						t.Errorf("MarkDelivered: %v", err)
						return
					}
				}
			}
		})
	}
	wg.Wait()

	if delivered != entities*versions {
		t.Errorf("delivered %d rows, want %d", delivered, entities*versions)
	}
	e.checkHeads(e.adminConn())
}

// TestReplayNeverOvertakesAVersionInFlight: v1 is a dead letter, v2 is being delivered, and the
// operator replays v1. The replayed row joins the back of its entity's queue, so it is claimable
// only once v2 has finished.
func TestReplayNeverOvertakesAVersionInFlight(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)
	v2 := e.accept(tenantA, "task:1", 2)

	c1 := e.claimOne(v1)
	if err := e.ob.MarkDead(e.ctx, c1, badShape); err != nil {
		t.Fatal(err)
	}
	c2 := e.claimOne(v2)

	if err := e.ob.Replay(e.ctx, tenantA, v1); err != nil {
		t.Fatal(err)
	}
	e.claimNone("v2 is in flight, so the replayed v1 must wait behind it")

	// Not merely held back while v2 is leased: it is behind v2 in the queue, so no claim that
	// races v2's can ever see it as the head.
	first, err := e.ob.Get(e.ctx, tenantA, v1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.ob.Get(e.ctx, tenantA, v2)
	if err != nil {
		t.Fatal(err)
	}
	if first.Seq <= second.Seq {
		t.Errorf("replayed v1 has seq %d, v2 has %d: a replay must go to the back of its queue", first.Seq, second.Seq)
	}

	if err := e.ob.MarkDelivered(e.ctx, c2); err != nil {
		t.Fatal(err)
	}
	e.claimOne(v1)
}

func TestReplayRestoresThePreparedState(t *testing.T) {
	e := setup(t)
	v1 := e.accept(tenantA, "task:1", 1)
	c := e.claimOne(v1)
	if err := e.ob.MarkPrepared(e.ctx, c); err != nil {
		t.Fatal(err)
	}
	// The sink stays down until the ladder is used up: the commonest dead letter there is.
	if err := e.ob.Fail(e.ctx, c, outbox.Ladder{}, sink503); err != nil {
		t.Fatal(err)
	}
	if err := e.ob.Replay(e.ctx, tenantA, v1); err != nil {
		t.Fatal(err)
	}
	row, err := e.ob.Get(e.ctx, tenantA, v1)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != outbox.StatePrepared {
		t.Errorf("state after replaying a row that died prepared = %q, want prepared (it must not prepare twice)", row.State)
	}
	c = e.claimOne(v1)
	if err := e.ob.MarkPrepared(e.ctx, c); !errors.Is(err, outbox.ErrLeaseLost) {
		t.Errorf("preparing the replayed row again: err = %v, want ErrLeaseLost", err)
	}
	if err := e.ob.MarkDelivered(e.ctx, c); err != nil {
		t.Error(err)
	}
}

// TestWhateverARemoteSystemSendsTheFailureIsRecordedAndNothingOfItIsStored: the one text of a
// Cause that comes from outside is the remote system's error code, so that is where a secret, or
// text Postgres cannot store, would have to get in. It replaces TestAnyCauseCanBeRecorded, which
// passed such text as the cause itself, when a cause was still a string. Both of its properties
// are kept: a failure can always be recorded (one that cannot is a row that loops at lease cadence
// with no backoff and no dead letter), and what is stored is bounded, valid text.
func TestWhateverARemoteSystemSendsTheFailureIsRecordedAndNothingOfItIsStored(t *testing.T) {
	e := setup(t)
	const secret = "not-a-real-key-0123456789" // stands in for an API key in a query string
	codes := map[string]string{
		"the error of an HTTP client": `Post "https://sink.example/hook?api_key=` + secret + `": context deadline exceeded`,
		"a URL":                       "https://sink.example/hook?api_key=" + secret,
		"a header":                    "Bearer " + secret,
		"JSON":                        `{"token":"` + secret + `"}`,
		"invalid UTF-8":               "sink said \xff\xfe\xc3 " + secret,
		"NUL":                         "bad\x00byte=" + secret,
		"long":                        secret + strings.Repeat("x", 4000),
		"long multi-byte":             secret + strings.Repeat("\U0001F600", 400),
	}
	for name, code := range codes {
		for how, wantReason := range map[string]string{"Fail": "", "Fail to dead": "retries exhausted", "MarkDead": "not retryable"} {
			id := e.accept(tenantA, name+"/"+how, 1)
			c := e.claimOne(id)
			cause := outbox.NewCause(outbox.ClassSinkUnavailable).WithStatus(503).WithCode(code)
			var err error
			switch how {
			case "Fail":
				err = e.ob.Fail(e.ctx, c, outbox.Ladder{time.Hour}, cause)
			case "Fail to dead":
				err = e.ob.Fail(e.ctx, c, outbox.Ladder{}, cause)
			default:
				err = e.ob.MarkDead(e.ctx, c, cause)
			}
			if err != nil {
				t.Errorf("%s with %s as the code: %v", how, name, err)
				continue
			}
			row, err := e.ob.Get(e.ctx, tenantA, id)
			if err != nil {
				t.Fatal(err)
			}
			if want := "sink unavailable (status 503, code withheld)"; row.LastError != want {
				t.Errorf("%s with %s as the code: last_error = %q, want %q", how, name, row.LastError, want)
			}
			if row.DeadReason != wantReason {
				t.Errorf("%s with %s as the code: dead_reason = %q, want %q", how, name, row.DeadReason, wantReason)
			}
			for field, got := range map[string]string{"last_error": row.LastError, "dead_reason": row.DeadReason} {
				if strings.Contains(got, secret) || len(got) > 1000 || !utf8.ValidString(got) {
					t.Errorf("%s with %s as the code: stored %s = %q", how, name, field, got)
				}
			}
		}
	}
}
