package outbox_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/tenancy"
)

// countRows is every row of the outbox, whoever owns it, read as the superuser so that nothing
// about row-level security can hide one from an assertion.
func (e *env) countRows() int {
	e.t.Helper()
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		e.t.Fatalf("connect as the superuser: %v", err)
	}
	defer func() { _ = conn.Close(e.ctx) }()
	var n int
	if err := conn.QueryRow(e.ctx, "SELECT count(*) FROM lawang.outbox").Scan(&n); err != nil {
		e.t.Fatalf("count the outbox: %v", err)
	}
	return n
}

// park stores an unattributable delivery and fails the test if it could not.
func (e *env) park(body string, reason outbox.ParkReason) (id string, fresh bool) {
	e.t.Helper()
	id, fresh, err := e.ob.Park(e.ctx, "fake", []byte(body), reason)
	if err != nil {
		e.t.Fatalf("Park(%q, %v): %v", body, reason, err)
	}
	return id, fresh
}

// TestAParkedDeliveryIsDeadUnderTheSentinelTenant. Everything about a parked row is what keeps it
// out of the delivery path: it is owned by nobody real, it is already finished, and it is not the
// head of its key, so no worker will ever lease it.
func TestAParkedDeliveryIsDeadUnderTheSentinelTenant(t *testing.T) {
	t.Parallel()
	e := setup(t)

	id, fresh := e.park(`{"nobody":"owns this"}`, outbox.ParkNoOwner)
	if !fresh {
		t.Fatal("the first park of a delivery reports it was already parked")
	}
	row, err := e.ob.Get(e.ctx, tenancy.Sentinel, id)
	if err != nil {
		t.Fatalf("Get as the sentinel tenant: %v", err)
	}
	switch {
	case row.TenantID != tenancy.Sentinel.String():
		t.Errorf("the parked row belongs to %q, want %q", row.TenantID, tenancy.Sentinel)
	case row.State != outbox.StateDead:
		t.Errorf("state = %q, want %q: a parked row must never be drained", row.State, outbox.StateDead)
	case row.IsHead:
		t.Error("the parked row is the head of its key, so a worker would claim it")
	case row.DeadReason != outbox.ParkNoOwner.String():
		t.Errorf("dead_reason = %q, want %q", row.DeadReason, outbox.ParkNoOwner)
	case row.FinishedAt == nil:
		t.Error("the parked row is not finished, so retention and re-resolution cannot tell how old it is")
	case string(row.RawBody) != `{"nobody":"owns this"}`:
		t.Errorf("raw_body = %q, want the exact bytes that arrived", row.RawBody)
	}
	// And no worker ever sees it. The claim walks the heads, and this row is not one.
	e.claimNone("a parked row is not claimable")
}

// TestParkingTheSameDeliveryAgainIsANoOp. A provider that retries a delivery nobody owns must not
// grow the table one row per retry, so the sentinel tenant dedupes on delivery_id like any other.
func TestParkingTheSameDeliveryAgainIsANoOp(t *testing.T) {
	t.Parallel()
	e := setup(t)

	first, fresh := e.park(`{"same":"bytes"}`, outbox.ParkNoOwner)
	if !fresh {
		t.Fatal("the first park reports it was already parked")
	}
	if _, fresh := e.park(`{"same":"bytes"}`, outbox.ParkNoOwner); fresh {
		t.Error("parking the same delivery again stored a second row")
	}
	// A different reason does not make it a different delivery either: the key is the bytes.
	if _, fresh := e.park(`{"same":"bytes"}`, outbox.ParkAmbiguousOwner); fresh {
		t.Error("parking the same delivery under another reason stored a second row")
	}
	if got := e.countRows(); got != 1 {
		t.Errorf("the outbox holds %d rows, want 1", got)
	}
	if _, err := e.ob.Get(e.ctx, tenancy.Sentinel, first); err != nil {
		t.Errorf("the first parked row is gone: %v", err)
	}
}

// TestEachParkedDeliveryHasItsOwnOrderingKey. Parked rows wait for nothing and nothing waits for
// them, so a flood of unattributable deliveries must not queue up behind one key: the key is the
// delivery's own id.
func TestEachParkedDeliveryHasItsOwnOrderingKey(t *testing.T) {
	t.Parallel()
	e := setup(t)

	seen := map[string]bool{}
	for _, body := range []string{`{"a":1}`, `{"a":2}`, `{"a":3}`} {
		id, _ := e.park(body, outbox.ParkNoOwner)
		row, err := e.ob.Get(e.ctx, tenancy.Sentinel, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if seen[row.OrderingKey] {
			t.Fatalf("two parked deliveries share the ordering key %q, so one waits for the other", row.OrderingKey)
		}
		seen[row.OrderingKey] = true
	}
}

// TestParkingCannotReachARealTenant. The function takes no tenant, which is the argument, and this
// is the evidence: an accepted delivery of the same bytes, for a real tenant, is untouched by
// parking the same bytes, and the parked row is not that tenant's.
func TestParkingCannotReachARealTenant(t *testing.T) {
	t.Parallel()
	e := setup(t)

	body := `{"key":"e1","v":1}`
	acceptedID, fresh, err := e.ob.Accept(e.ctx, tenantA, outbox.Delivery{
		Provider: "fake", OrderingKey: "fake:sub1", RawBody: []byte(body),
	})
	if err != nil || !fresh {
		t.Fatalf("Accept = fresh %v, err %v", fresh, err)
	}
	parkedID, fresh := e.park(body, outbox.ParkNoOwner)
	if !fresh {
		t.Fatal("the bytes were already parked, although they were only ever accepted")
	}

	accepted, err := e.ob.Get(e.ctx, tenantA, acceptedID)
	if err != nil {
		t.Fatalf("Get the accepted row: %v", err)
	}
	if accepted.State != outbox.StatePending || !accepted.IsHead {
		t.Errorf("the tenant's own row changed when a delivery was parked: %+v", accepted)
	}
	// The parked row is not visible to the tenant at all, and the tenant's is not visible to the
	// sentinel: row-level security applies to parking like to everything else.
	if _, err := e.ob.Get(e.ctx, tenantA, parkedID); !errors.Is(err, outbox.ErrNotFound) {
		t.Errorf("a real tenant can see the parked row: %v", err)
	}
	if _, err := e.ob.Get(e.ctx, tenancy.Sentinel, acceptedID); !errors.Is(err, outbox.ErrNotFound) {
		t.Errorf("the sentinel tenant can see a real tenant's row: %v", err)
	}
	// The accepted row is still the only claimable one.
	e.claimOne(acceptedID)
}

// TestParkRefusesWhatItCannotRecord. A parked row whose reason says nothing is a row no sweep can
// re-resolve and no operator can act on, and a provider that is not this program's own constant is
// a bug in the caller.
func TestParkRefusesWhatItCannotRecord(t *testing.T) {
	t.Parallel()
	e := setup(t)

	if _, _, err := e.ob.Park(e.ctx, "fake", []byte(`{}`), outbox.ParkUnreasoned); !errors.Is(err, outbox.ErrNoParkReason) {
		t.Errorf("Park with no reason = %v, want ErrNoParkReason", err)
	}
	if _, _, err := e.ob.Park(e.ctx, "fake", []byte(`{}`), outbox.ParkReason(200)); !errors.Is(err, outbox.ErrNoParkReason) {
		t.Errorf("Park with an unknown reason = %v, want ErrNoParkReason", err)
	}
	if _, _, err := e.ob.Park(e.ctx, "", []byte(`{}`), outbox.ParkNoOwner); err == nil {
		t.Error("Park accepted an empty provider, which would hash into the delivery id as nothing")
	}
	if _, _, err := e.ob.Park(e.ctx, "fa\x00ke", []byte(`{}`), outbox.ParkNoOwner); err == nil {
		t.Error("Park accepted a provider Postgres cannot store")
	}
	if got := e.countRows(); got != 0 {
		t.Errorf("a refused park stored %d rows", got)
	}
}

// TestEveryParkReasonHasItsOwnText. The reason is what B25 re-resolves rows by and what an
// operator reads, so two reasons that read alike would be one reason.
func TestEveryParkReasonHasItsOwnText(t *testing.T) {
	t.Parallel()
	reasons := []outbox.ParkReason{
		outbox.ParkNoOwner, outbox.ParkAmbiguousOwner, outbox.ParkUnreadable,
		outbox.ParkUnverifiable, outbox.ParkUnstorable,
	}
	seen := map[string]bool{}
	for _, r := range reasons {
		text := r.String()
		switch {
		case text == "":
			t.Errorf("reason %d has no text", r)
		case seen[text]:
			t.Errorf("two reasons read alike: %q", text)
		case strings.Contains(text, "no reason given"):
			t.Errorf("reason %d falls through to the zero value's text", r)
		}
		seen[text] = true
	}
	if outbox.ParkUnreasoned.String() == "" {
		t.Error("the zero reason has no text at all, so a log line about it says nothing")
	}
}
