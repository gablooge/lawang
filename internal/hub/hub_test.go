package hub_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/hub"
	"github.com/gablooge/lawang/internal/ingress"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/testdb"
)

// The dead_reason texts the outbox writes. They are spelled out here rather than taken from the
// outbox package, so that a change to one of them fails a test instead of passing silently: B25
// re-resolves rows by what is in this column.
const (
	reasonNoOwner      = "unattributable: no owner"
	reasonAmbiguous    = "unattributable: ambiguous owner"
	reasonUnreadable   = "unattributable: unreadable delivery"
	reasonUnverifiable = "unattributable: the provider's verification panicked"
	reasonUnstorable   = "poison: the delivery cannot be stored"
)

// TestAForgedSignatureIsRefusedAndStoresNothing is the first "Done when" line. The workspace IS
// registered, so there are candidates to verify and the delivery fails on its signature alone.
func TestAForgedSignatureIsRefusedAndStoresNothing(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "W1", "W1", "S1", secretA)
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})

	body := delivery("W1", "S1", "1")
	for name, req := range map[string]provider.Request{
		"signed with the wrong secret": signed(body, []byte("not the secret")),
		"no signature header at all":   signed(body, nil),
		"a signature over other bytes": signed(delivery("W1", "S1", "2"), secretA),
	} {
		t.Run(name, func(t *testing.T) {
			req.Body = body
			if got := e.accept(h, entry, req); got != ingress.Unverified {
				t.Errorf("verdict = %s, want unverified (401)", got)
			}
		})
	}
	e.wantNothingStored("a forged signature must store nothing, not even a parked row")
}

// TestAnUnknownWorkspaceIsParkedUnderTheSentinelTenant is the second "Done when" line. Nobody has
// registered the workspace, so there is nothing to verify against and no signature claim to
// reject: the delivery is parked and answered 2xx, because a provider must not be made to retry.
func TestAnUnknownWorkspaceIsParkedUnderTheSentinelTenant(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "W1", "W1", "S1", secretA)
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})

	// Correctly signed with tenant A's secret, and for a workspace tenant A has not connected.
	// It must not be routed to tenant A: the keys narrow, they do not decide.
	body := delivery("W9", "S9", "1")
	if got := e.accept(h, entry, signed(body, secretA)); got != ingress.Parked {
		t.Fatalf("verdict = %s, want parked (200)", got)
	}
	row := e.wantParked(reasonNoOwner, "an unknown workspace is parked under the sentinel tenant")
	if row.body != string(body) {
		t.Errorf("the parked row holds %q, want the exact bytes that arrived", row.body)
	}
	if row.provider != fake.DefaultKey {
		t.Errorf("the parked row's provider is %q, want the registry's own key", row.provider)
	}
}

// TestTwoTenantsWhoseSecretsBothVerifyAreParkedNeverRouted is the third "Done when" line, and
// principle 2 itself: routing to the first match is how data crossed tenants in the predecessor.
func TestTwoTenantsWhoseSecretsBothVerifyAreParkedNeverRouted(t *testing.T) {
	t.Parallel()
	e := setup(t)
	// Two tenants who connected the same provider workspace and, by accident or by copy and paste,
	// share a signing secret. Both rows verify the same bytes, and neither can be shown to own it.
	shared := []byte("a secret two tenants both hold")
	e.register(tenantA, "W1", "W1", "S1", shared)
	e.register(tenantB, "W1", "W1", "S1", shared)
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})

	if got := e.accept(h, entry, signed(delivery("W1", "S1", "1"), shared)); got != ingress.Parked {
		t.Fatalf("verdict = %s, want parked (200)", got)
	}
	e.wantParked(reasonAmbiguous, "two verifying tenants must produce a parked row and nothing else")
}

// TestTwoTenantsOnOneWorkspaceWithDifferentSecretsRouteToTheOwner is the other half of the pair:
// sharing a workspace is legitimate and must still deliver, as long as exactly one secret verifies.
func TestTwoTenantsOnOneWorkspaceWithDifferentSecretsRouteToTheOwner(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "W1", "W1", "S1", secretA)
	subB := e.register(tenantB, "W1", "W1", "S1", secretB)
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})

	if got := e.accept(h, entry, signed(delivery("W1", "S1", "1"), secretB)); got != ingress.Stored {
		t.Fatalf("verdict = %s, want stored (202)", got)
	}
	row := e.wantStored(tenantB, "the tenant is the one whose secret verified, not the first candidate")
	// The queue a delivery joins is named by our own two constants, never by anything a sender
	// wrote: if the ordering key carried a payload field, one sender could put another tenant's
	// deliveries behind its own.
	if want := fake.DefaultKey + ":" + subB.ID; row.orderingKey != want {
		t.Errorf("ordering key = %q, want %q", row.orderingKey, want)
	}
}

// TestAnIdenticalResendIsANoOp is the fourth "Done when" line.
func TestAnIdenticalResendIsANoOp(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "W1", "W1", "S1", secretA)
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})
	req := signed(delivery("W1", "S1", "1"), secretA)

	if got := e.accept(h, entry, req); got != ingress.Stored {
		t.Fatalf("first delivery: verdict = %s, want stored (202)", got)
	}
	for i := range 2 {
		if got := e.accept(h, entry, req); got != ingress.Duplicate {
			t.Fatalf("re-send %d: verdict = %s, want duplicate (200)", i+1, got)
		}
	}
	e.wantStored(tenantA, "three identical deliveries are one row")
}

// TestAResentUnownedDeliveryParksOnce. The sentinel tenant dedupes like any other: a provider that
// retries a delivery nobody owns must not grow the table one row per retry.
func TestAResentUnownedDeliveryParksOnce(t *testing.T) {
	t.Parallel()
	e := setup(t)
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})
	req := signed(delivery("W9", "S9", "1"), secretA)

	for i := range 3 {
		if got := e.accept(h, entry, req); got != ingress.Parked {
			t.Fatalf("delivery %d: verdict = %s, want parked (200)", i+1, got)
		}
	}
	e.wantParked(reasonNoOwner, "three identical unowned deliveries are one parked row")
}

// TestTheKeysNarrowAndTheSecretDecides. The delivery names tenant B's workspace and is signed with
// tenant A's secret, which verifies nothing that was asked. Nothing may reach either tenant: the
// payload chooses which rows are looked at and never which tenant owns the result (principle 2).
func TestTheKeysNarrowAndTheSecretDecides(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "WA", "WA", "", secretA)
	e.register(tenantB, "WB", "WB", "", secretB)
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})

	if got := e.accept(h, entry, signed(delivery("WB", "", "1"), secretA)); got != ingress.Unverified {
		t.Fatalf("verdict = %s, want unverified (401): tenant A's secret verifies nothing in tenant B's workspace", got)
	}
	e.wantNothingStored("a secret that belongs to another workspace must not route or park anything")
}

// TestASubscriptionKeyNarrowsWithinAWorkspace. Two subscriptions of one tenant on one workspace,
// each with its own registration id and secret: a delivery naming one of them must verify against
// that one, and the other's secret must not be enough.
func TestASubscriptionKeyNarrowsWithinAWorkspace(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "list-1", "W1", "S1", secretA)
	subTwo := e.register(tenantA, "list-2", "W1", "S2", secretB)
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})

	if got := e.accept(h, entry, signed(delivery("W1", "S2", "1"), secretA)); got != ingress.Unverified {
		t.Fatalf("verdict = %s, want unverified: the other registration's secret must not verify this one", got)
	}
	if got := e.accept(h, entry, signed(delivery("W1", "S2", "2"), secretB)); got != ingress.Stored {
		t.Fatalf("verdict = %s, want stored", got)
	}
	row := e.wantStored(tenantA, "the delivery belongs to the registration it names")
	if want := fake.DefaultKey + ":" + subTwo.ID; row.orderingKey != want {
		t.Errorf("ordering key = %q, want the second subscription's %q", row.orderingKey, want)
	}
}

// TestASubscriptionThatRecordedNoRegistrationIdStillMatches. A provider that learns the id of its
// own registration only after it was made would otherwise have every delivery parked as unowned.
func TestASubscriptionThatRecordedNoRegistrationIdStillMatches(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "W1", "W1", "", secretA)
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})

	if got := e.accept(h, entry, signed(delivery("W1", "S1", "1"), secretA)); got != ingress.Stored {
		t.Fatalf("verdict = %s, want stored (202)", got)
	}
	e.wantStored(tenantA, "a delivery that names a registration id matches a row that has none")
}

// TestADeliveryFoundByItsRegistrationIdAlone is the Microsoft Graph shape: a notification that
// carries the subscription's own id and no workspace. The strict fake always sends a workspace, so
// the keys come from a double that reports only the one a Graph notification has.
func TestADeliveryFoundByItsRegistrationIdAlone(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "mailbox-1", "", "S1", secretA)
	graph := keyed{fake.New(fake.DefaultKey), provider.DeliveryKeys{Subscription: "S1"}}
	h, entry := e.hub(graph, hub.Options{})

	if got := e.accept(h, entry, signed(delivery("W1", "S1", "1"), secretA)); got != ingress.Stored {
		t.Fatalf("verdict = %s, want stored (202)", got)
	}
	e.wantStored(tenantA, "a subscription id alone resolves an owner")
}

// TestADeliveryTheProviderCannotReadIsParked. A body the provider does not recognize resolves
// nothing, and it is not a signature failure: 401 is reserved for one, and a provider that is
// answered anything but 2xx retries.
func TestADeliveryTheProviderCannotReadIsParked(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "W1", "W1", "S1", secretA)
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})

	body := []byte(`{"type":"event","workspace":"W1","surprise":true}`) // an unknown field: the double is strict
	if got := e.accept(h, entry, signed(body, secretA)); got != ingress.Parked {
		t.Fatalf("verdict = %s, want parked (200)", got)
	}
	e.wantParked(reasonUnreadable, "a body the provider cannot read its own keys out of is parked")
}

// TestADeliveryWithNoUsableKeysIsParkedWithoutALookup. A delivery that narrows nothing must not be
// answered by verifying every subscription there is: that is work a stranger could ask for.
func TestADeliveryWithNoUsableKeysIsParkedWithoutALookup(t *testing.T) {
	t.Parallel()
	for name, p := range map[string]provider.Provider{
		"no keys at all":       keyless{fake.New(fake.DefaultKey)},
		"a key with a NUL":     keyed{fake.New(fake.DefaultKey), nulKey},
		"a key over the bound": keyed{fake.New(fake.DefaultKey), longKey},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := setup(t)
			// A subscription that would verify, if the delivery could ever be looked up.
			e.register(tenantA, "W1", "W1", "S1", secretA)
			h, entry := e.hub(p, hub.Options{})

			if got := e.accept(h, entry, signed(delivery("W1", "S1", "1"), secretA)); got != ingress.Parked {
				t.Fatalf("verdict = %s, want parked (200)", got)
			}
			e.wantParked(reasonUnreadable, "a delivery key that cannot select a row resolves nothing")
		})
	}
}

// TestMoreCandidatesThanTheHubWillVerifyAreParked. The bound on candidates is a bound on the work
// one delivery can ask for, and reaching it must not be a truncation: a candidate set cut short
// could hide the second tenant that verifies and route what should have been parked.
func TestMoreCandidatesThanTheHubWillVerifyAreParked(t *testing.T) {
	t.Parallel()
	e := setup(t)
	// Two tenants is the limit here, and three subscriptions match the delivery's keys. The first
	// two hold secrets that do not verify, so a hub that simply cut the set short would answer 401
	// and a hub that verified them all would route to the third.
	e.register(tenantA, "one", "W1", "S1", secretA)
	e.register(tenantB, "two", "W1", "S1", secretB)
	e.register(tenantB, "three", "W1", "S1", []byte("the third secret"))
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{MaxCandidates: 2})

	if got := e.accept(h, entry, signed(delivery("W1", "S1", "1"), []byte("the third secret"))); got != ingress.Parked {
		t.Fatalf("verdict = %s, want parked (200)", got)
	}
	e.wantParked(reasonAmbiguous, "more candidates than the hub will verify is an unresolvable owner")
}

// TestAPanickingVerifyParksTheDeliveryInsteadOfRouting. Architecture 7 promises Verify never
// panics and nothing can enforce it. One that does has answered nothing, so the candidates that
// did answer cannot settle the owner either, and the connection must not be dropped.
func TestAPanickingVerifyParksTheDeliveryInsteadOfRouting(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "W1", "W1", "S1", secretA)
	h, entry := e.hub(panicking{fake.New(fake.DefaultKey)}, hub.Options{})

	verdict, err := h.Accept(e.ctx, entry, signed(delivery("W1", "S1", "1"), secretA))
	if err != nil {
		t.Fatalf("Accept returned an error: %v, want a parked verdict: a provider's panic must not become a 500", err)
	}
	if verdict != ingress.Parked {
		t.Fatalf("verdict = %s, want parked (200)", verdict)
	}
	e.wantParked(reasonUnverifiable, "a delivery whose verification panicked is parked, never routed")
}

// TestAVerifyThatNeverReturnsDoesNotHoldTheRequest. The accept path is bounded (the edge gives it
// ingress.DefaultAcceptTimeout), and a provider bug must not hold a request past that bound: the
// edge then answers 503 with a Retry-After, which every provider understands, rather than nothing.
//
// The test has a deadline of its own and releases the blocked Verify in a Cleanup, so a hub that
// waited for it fails the test instead of hanging it.
func TestAVerifyThatNeverReturnsDoesNotHoldTheRequest(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "W1", "W1", "S1", secretA)
	stuck := &blocking{Provider: fake.New(fake.DefaultKey), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(stuck.release) }) })
	h, entry := e.hub(stuck, hub.Options{})

	ctx, cancel := context.WithTimeout(e.ctx, 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.Accept(ctx, entry, signed(delivery("W1", "S1", "1"), secretA))
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Accept returned %v, want an error the edge answers 503 to", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Accept did not return: a provider's Verify held the request past the accept path's bound")
	}
	e.wantNothingStored("an accept that ran out of time stores nothing")
	once.Do(func() { close(stuck.release) })
}

// TestAProviderThatWritesToTheBodyChangesNothingElse. provider.Request.Body aliases the edge's own
// capture buffer, and the same Request goes to every candidate, so an implementation that
// normalized the bytes in place would change what every later candidate verifies and what is then
// stored. The hub copies per candidate, which turns that rule into a guarantee.
func TestAProviderThatWritesToTheBodyChangesNothingElse(t *testing.T) {
	t.Parallel()
	e := setup(t)
	// The first candidate by id is the one that does not verify, so the mutating Verify runs
	// before the one that has to see the real bytes. Ids are ULIDs and sort by mint time, so
	// registering in this order fixes the order the hub reads them in.
	e.register(tenantA, "first", "W1", "S1", secretA)
	e.register(tenantB, "second", "W1", "S1", secretB)
	h, entry := e.hub(mutating{fake.New(fake.DefaultKey)}, hub.Options{})

	body := delivery("W1", "S1", "1")
	kept := bytes.Clone(body)
	if got := e.accept(h, entry, signed(body, secretB)); got != ingress.Stored {
		t.Fatalf("verdict = %s, want stored: the second candidate must see the bytes that arrived", got)
	}
	if !bytes.Equal(body, kept) {
		t.Errorf("the request's own body was changed by a provider: %q", body)
	}
	row := e.wantStored(tenantB, "the delivery belongs to the tenant whose secret verified the real bytes")
	if row.body != string(kept) {
		t.Errorf("the stored body is %q, want the exact bytes that arrived", row.body)
	}
}

// TestADeliveryThatCannotBeStoredIsParkedAndNotRetried. Poison: an owner was resolved, and the row
// still cannot be written, however often the provider sends it again. Answering anything but 2xx
// would be asking for a retry storm.
//
// The only way in is a subscription row whose id the table did not get from Lawang, since the
// ordering key is the provider key and that id. One is written here behind the package's back,
// which is also what proves the branch is not dead.
func TestADeliveryThatCannotBeStoredIsParkedAndNotRetried(t *testing.T) {
	t.Parallel()
	e := setup(t)
	longID := strings.Repeat("i", 600) // the outbox refuses an ordering key over 512 bytes
	testdb.Exec(t, e.tdb.URL, `
		BEGIN;
		SELECT set_config('lawang.tenant', '`+tenantA.String()+`', true);
		INSERT INTO lawang.subscriptions (id, tenant_id, provider, resource, workspace_id, external_id, secret)
		VALUES ('`+longID+`', '`+tenantA.String()+`', 'fake', 'W1', 'W1', 'S1', 'secret-of-tenant-a');
		COMMIT;`)
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})

	if got := e.accept(h, entry, signed(delivery("W1", "S1", "1"), secretA)); got != ingress.Parked {
		t.Fatalf("verdict = %s, want parked (200)", got)
	}
	e.wantParked(reasonUnstorable, "poison is parked and answered 2xx, never retried")
}

// TestASubscriptionRowThatCouldNotHaveComeFromLawangIsNotAnOwner. The isolation argument is that
// the tenant comes from a row Lawang owns. A row carrying a tenant id the domain would not have
// allowed, or an empty secret, did not come from here: it is refused rather than used, and the
// refusal is our own failure (a 500), not a verdict a sender can ask for.
func TestASubscriptionRowThatCouldNotHaveComeFromLawangIsNotAnOwner(t *testing.T) {
	t.Parallel()
	// Each row here is refused by the database, so writing one means dropping the constraint that
	// refuses it, as the superuser, in a database of the test's own. Nothing in the running system
	// can do any of this. What is under test is that the hub does not lean on the table for it: a
	// candidate is where a tenant comes from, and a candidate that cannot be one is not used.
	cases := map[string]string{
		"a tenant id the domain refuses": `
			ALTER DOMAIN lawang.tenant_id DROP CONSTRAINT tenant_id_check;
			INSERT INTO lawang.subscriptions (id, tenant_id, provider, resource, workspace_id, external_id, secret)
			VALUES ('01SUB', 'not a tenant id', 'fake', 'W1', 'W1', 'S1', 'secret-of-tenant-a');`,
		"a secret that verifies nothing": `
			ALTER TABLE lawang.subscriptions DROP CONSTRAINT subscriptions_secret_is_not_empty;
			INSERT INTO lawang.subscriptions (id, tenant_id, provider, resource, workspace_id, external_id, secret)
			VALUES ('01SUB', '` + tenantA.String() + `', 'fake', 'W1', 'W1', 'S1', '');`,
	}
	for name, write := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := setup(t)
			testdb.Exec(t, e.tdb.AdminURL, write)
			// A second, perfectly good subscription on the same workspace, whose secret DOES
			// verify the delivery. This is what makes the test about the hub's own refusal: with
			// the bad row simply skipped, the delivery would be routed to this tenant and stored,
			// and nothing would ever say that the table holds a row nobody can account for. A
			// candidate set with a row that could not have come from Lawang is a table that cannot
			// be reasoned about, so the whole resolution is refused.
			e.register(tenantB, "W1", "W1", "S1", secretB)
			h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})

			_, err := h.Accept(e.ctx, entry, signed(delivery("W1", "S1", "1"), secretB))
			if err == nil {
				t.Fatal("Accept resolved an owner from a candidate set holding a row that could not have come from Lawang")
			}
			e.wantNothingStored("a resolution that cannot be reasoned about stores nothing")
		})
	}
}

// TestTheHubRefusesAProviderThatIsNotAWebhookSource. The edge answers 404 before this, so it is
// our own wiring and not a sender, which is why it is an error and not a verdict.
func TestTheHubRefusesAProviderThatIsNotAWebhookSource(t *testing.T) {
	t.Parallel()
	e := setup(t)
	h, entry := e.hub(noWebhooks{}, hub.Options{})
	if _, err := h.Accept(e.ctx, entry, signed(delivery("W1", "S1", "1"), secretA)); err == nil {
		t.Fatal("Accept accepted a delivery for a provider that receives none")
	}
	e.wantNothingStored("a provider with no webhook stores nothing")
}

// TestNewRefusesWhatCannotWork covers the start-up refusals, including the one this item exists to
// add: a provider whose signature covers the public URL, in a deployment that configured none.
func TestNewRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()
	e := setup(t)
	signer, err := provider.NewRegistry(urlSigning{fake.New(fake.DefaultKey)})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := provider.NewRegistry(fake.New(fake.DefaultKey))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := hub.New(nil, plain, hub.Options{}); err == nil {
		t.Error("hub.New accepted a nil database")
	}
	if _, err := hub.New(e.db, nil, hub.Options{}); err == nil {
		t.Error("hub.New accepted a nil registry")
	}
	if _, err := hub.New(e.db, plain, hub.Options{MaxCandidates: -1}); err == nil {
		t.Error("hub.New accepted a negative candidate bound")
	}
	// A bound past the accept path's own budget is refused too: every candidate is an HMAC over
	// the whole body, so a hub that would verify thousands of them is one that answers 503.
	if _, err := hub.New(e.db, plain, hub.Options{MaxCandidates: 1 << 20}); err == nil {
		t.Error("hub.New accepted a candidate bound no accept could finish inside its deadline")
	}
	if _, err := hub.New(e.db, signer, hub.Options{}); err == nil {
		t.Error("hub.New started with a URL-signing provider and no public base URL: every one of its deliveries would be a 401")
	} else if !strings.Contains(err.Error(), "LAWANG_PUBLIC_BASE_URL") {
		t.Errorf("the refusal does not name the variable to set: %v", err)
	}
	if _, err := hub.New(e.db, signer, hub.Options{PublicBaseURL: "https://lawang.example.com"}); err != nil {
		t.Errorf("hub.New refused a URL-signing provider with a public base URL configured: %v", err)
	}
	// A provider that does not sign the URL is unaffected by the variable, which is most of them.
	if _, err := hub.New(e.db, plain, hub.Options{}); err != nil {
		t.Errorf("hub.New refused a provider whose scheme does not cover the URL: %v", err)
	}
}

// TestTheSentinelCannotBeATenant. A tenants row under the sentinel id would hand whoever holds
// that tenant's operator credential every stranger's parked delivery. The constant and the CHECK
// are held together here, because either alone is only half of the guarantee.
func TestTheSentinelCannotBeATenant(t *testing.T) {
	t.Parallel()
	e := setup(t)
	err := e.db.TenantTx(e.ctx, tenancy.Sentinel, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, "INSERT INTO tenants (id) VALUES ($1)", tenancy.Sentinel.String())
		return err
	})
	if err == nil {
		t.Fatalf("the tenants table accepted %q: parked deliveries would belong to whoever holds it", tenancy.Sentinel)
	}
	// The neighbouring id is a perfectly good tenant, so the CHECK refuses the one string and not
	// a shape.
	err = e.db.TenantTx(e.ctx, tenancy.ID("_parked_x"), func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, "INSERT INTO tenants (id) VALUES ($1)", "_parked_x")
		return err
	})
	if err != nil {
		t.Fatalf("the tenants table refused an ordinary tenant id: %v", err)
	}
}

// TestTheHubLogsNoSecret. Everything the hub logs about a delivery goes to an operator's log and
// every backup of it. A secret, a body or a delivery key has no business there.
func TestTheHubLogsNoSecret(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "W1", "W1", "S1", secretA)
	var logged lockedBuffer
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{
		Logger: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	e.accept(h, entry, signed(delivery("W1", "S1", "1"), secretA))
	e.accept(h, entry, signed(delivery("W9", "S9", "1"), secretA))
	e.accept(h, entry, signed(delivery("W1", "S1", "2"), []byte("wrong")))

	for _, forbidden := range []string{string(secretA), "W9", "fake:task:", "hello"} {
		if strings.Contains(logged.String(), forbidden) {
			t.Errorf("the log contains %q:\n%s", forbidden, logged.String())
		}
	}
}

// TestARowFoundOnlyByItsRegistrationIdIsStillACandidate is the cross-tenant attribution the first
// review found, and the reason the candidate lookup is a union rather than an either/or.
//
// Tenant A registered a mailbox, which the provider names by the registration's own id and not by
// a workspace. Tenant B registered a workspace, whose id the provider does send. A delivery that
// carries both keys belongs to whichever of them signed it, and here both could have: they share a
// secret. Asking only the rows the workspace selects would route it to B without ever learning
// that A had a claim on it.
func TestARowFoundOnlyByItsRegistrationIdIsStillACandidate(t *testing.T) {
	t.Parallel()
	e := setup(t)
	shared := []byte("a secret two tenants both hold")
	e.register(tenantA, "mailbox-1", "", "S1", shared)
	e.register(tenantB, "W1", "W1", "", shared)
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})

	if got := e.accept(h, entry, signed(delivery("W1", "S1", "1"), shared)); got != ingress.Parked {
		t.Fatalf("verdict = %s, want parked (200): both tenants' secrets verify these exact bytes", got)
	}
	e.wantParked(reasonAmbiguous,
		"a delivery two tenants could own is parked whatever mix of key shapes they registered with")
}

// TestEveryRowTheKeysCouldBelongToIsACandidate is the whole table of key shapes: a subscription is
// registered with a workspace id, with a registration id, or with both, and a delivery carries one
// or the other or both.
//
// Every row in a case holds the SAME secret, so the verdict counts the rows the lookup actually
// asked: none is parked as unowned, one is stored for its tenant, and two are parked as ambiguous.
// With different secrets per row a lookup that skipped a row would still store the right delivery
// for the right tenant, and the test would prove nothing about which rows were asked.
//
// The keys come from a double, because the strict fake always reads both of them out of its own
// envelope and half of this table is about a provider that sends only one.
func TestEveryRowTheKeysCouldBelongToIsACandidate(t *testing.T) {
	t.Parallel()
	shared := []byte("a secret every row in this table holds")

	type row struct {
		tenant                        tenancy.ID
		resource, workspace, external string
	}
	cases := map[string]struct {
		rows []row
		// The keys the delivery carries. An empty string is a key the provider did not send.
		workspace, subscription string
		// want is the verdict, and owner is who the row belongs to when it is stored.
		want   ingress.Verdict
		owner  tenancy.ID
		reason string
	}{
		"a workspace on both sides": {
			rows:      []row{{tenantA, "W1", "W1", ""}},
			workspace: "W1",
			want:      ingress.Stored, owner: tenantA,
		},
		"a workspace row and a delivery that also names a registration": {
			rows:      []row{{tenantA, "W1", "W1", ""}},
			workspace: "W1", subscription: "S1",
			want: ingress.Stored, owner: tenantA,
		},
		"a registration row and a delivery that also names a workspace": {
			rows:      []row{{tenantA, "mailbox-1", "", "S1"}},
			workspace: "W1", subscription: "S1",
			want: ingress.Stored, owner: tenantA,
		},
		"a registration id on both sides": {
			rows:         []row{{tenantA, "mailbox-1", "", "S1"}},
			subscription: "S1",
			want:         ingress.Stored, owner: tenantA,
		},
		"both keys on both sides": {
			rows:      []row{{tenantA, "list-1", "W1", "S1"}},
			workspace: "W1", subscription: "S1",
			want: ingress.Stored, owner: tenantA,
		},
		"both keys registered, only the workspace delivered": {
			rows:      []row{{tenantA, "list-1", "W1", "S1"}},
			workspace: "W1",
			want:      ingress.Stored, owner: tenantA,
		},
		"both keys registered, only the registration id delivered": {
			rows:         []row{{tenantA, "list-1", "W1", "S1"}},
			subscription: "S1",
			want:         ingress.Stored, owner: tenantA,
		},
		"the delivery names another registration in the same workspace": {
			rows:      []row{{tenantA, "list-1", "W1", "S2"}},
			workspace: "W1", subscription: "S1",
			want: ingress.Parked, reason: reasonNoOwner,
		},
		"the delivery names another workspace for the same registration": {
			rows:      []row{{tenantA, "list-1", "W2", "S1"}},
			workspace: "W1", subscription: "S1",
			want: ingress.Parked, reason: reasonNoOwner,
		},
		"a workspace-only delivery cannot reach a registration-only row": {
			rows:      []row{{tenantA, "mailbox-1", "", "S1"}},
			workspace: "W1",
			want:      ingress.Parked, reason: reasonNoOwner,
		},
		"a registration-only delivery cannot reach a workspace-only row": {
			rows:         []row{{tenantA, "W1", "W1", ""}},
			subscription: "S1",
			want:         ingress.Parked, reason: reasonNoOwner,
		},
		"one tenant found by its workspace, another by its registration id": {
			rows: []row{
				{tenantA, "mailbox-1", "", "S1"},
				{tenantB, "W1", "W1", ""},
			},
			workspace: "W1", subscription: "S1",
			want: ingress.Parked, reason: reasonAmbiguous,
		},
		"one tenant found by both keys, another by its registration id": {
			rows: []row{
				{tenantA, "mailbox-1", "", "S1"},
				{tenantB, "list-1", "W1", "S1"},
			},
			workspace: "W1", subscription: "S1",
			want: ingress.Parked, reason: reasonAmbiguous,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := setup(t)
			for _, r := range tc.rows {
				e.register(r.tenant, r.resource, r.workspace, r.external, shared)
			}
			source := keyed{fake.New(fake.DefaultKey), provider.DeliveryKeys{
				Workspace:    tc.workspace,
				Subscription: tc.subscription,
			}}
			h, entry := e.hub(source, hub.Options{})

			got := e.accept(h, entry, signed(delivery("W1", "S1", "1"), shared))
			if got != tc.want {
				t.Fatalf("verdict = %s, want %s", got, tc.want)
			}
			if tc.want == ingress.Stored {
				e.wantStored(tc.owner, "the delivery belongs to the one row its keys could have come from")
				return
			}
			e.wantParked(tc.reason, "the keys select the rows, and how many verify decides the verdict")
		})
	}
}

// TestGarbageFromAStrangerDoesNotBecomeMegabytesOfDatabase. /ingress/<provider> answers anyone,
// signed or not, and a body the provider cannot read its own keys out of is parked without ever
// touching a tenant. That is right, and the first round of review pointed out what it cost: twenty
// POSTs of distinct garbage were twenty rows holding twenty bodies, at up to the edge's 1 MiB cap
// each, for a reason no sweep can ever re-resolve.
func TestGarbageFromAStrangerDoesNotBecomeMegabytesOfDatabase(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "W1", "W1", "S1", secretA)
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})

	const posts = 20
	for i := range posts {
		// Unsigned, unparseable, and distinct, so nothing dedupes it away.
		body := fmt.Appendf(nil, `not json at all, delivery %d, %s`, i, strings.Repeat("x", 4096))
		if got := e.accept(h, entry, signed(body, nil)); got != ingress.Parked {
			t.Fatalf("delivery %d: verdict = %s, want parked (200)", i, got)
		}
	}
	rows := e.outboxRows()
	if len(rows) != posts {
		t.Fatalf("the outbox holds %d rows, want %d: each distinct delivery is still its own row", len(rows), posts)
	}
	total := 0
	for _, row := range rows {
		if strings.Contains(row.body, "xxxx") || strings.Contains(row.body, "not json at all") {
			t.Fatalf("a parked row holds the bytes a stranger sent: %.120q", row.body)
		}
		if row.deadReason != reasonUnreadable {
			t.Fatalf("dead_reason = %q, want %q", row.deadReason, reasonUnreadable)
		}
		total += len(row.body)
	}
	// Twenty deliveries of 4 KiB each. The bound here is not the interesting number, the ratio is:
	// what is kept is a note per delivery and not the delivery.
	if total > posts*256 {
		t.Errorf("%d unreadable deliveries kept %d bytes of body, want a note each", posts, total)
	}
}

// TestAPanickingVerifyLogsWhereItPanicked. Parking is the right answer (the candidates that did
// answer cannot settle an owner on their own), but it is silent: every delivery of that shape goes
// to the sentinel and the tenant simply stops receiving them. The log line is the only sign, so it
// has to name the line that panicked and not just the provider.
//
// What it must NOT carry is the panic value. A provider that panics is one whose promises are
// already not being kept, and the value is whatever it passed to panic, which can be built out of
// the body or the secret it was handed.
func TestAPanickingVerifyLogsWhereItPanicked(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "W1", "W1", "S1", secretA)
	var logged lockedBuffer
	h, entry := e.hub(panickingWithTheSecret{fake.New(fake.DefaultKey)}, hub.Options{
		Logger: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})

	if got := e.accept(h, entry, signed(delivery("W1", "S1", "1"), secretA)); got != ingress.Parked {
		t.Fatalf("verdict = %s, want parked (200)", got)
	}
	out := logged.String()
	for _, want := range []string{
		"panickingWithTheSecret", // the frame that panicked, by name
		"fakes_test.go",          // and the file it is in
		"panic_type=string",      // the kind of value, which a type name cannot leak
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the log does not say %q, so nobody can find the bug:\n%s", want, out)
		}
	}
	if strings.Contains(out, string(secretA)) {
		t.Errorf("the log carries the panic value, which this provider built out of the secret:\n%s", out)
	}
}
