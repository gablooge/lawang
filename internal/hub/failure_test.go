package hub_test

import (
	"testing"

	"github.com/gablooge/lawang/internal/hub"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/testdb"
)

// TestADatabaseFailureIsSaidToBeOne. A failure of ours is an error, which the edge answers 500 or
// 503 to, and never a verdict. The distinction is the whole contract: a verdict tells the provider
// that Lawang has dealt with the delivery, so a database that is not there must not be answered
// with "parked" or "stored", which would lose the delivery for good.
//
// Each case removes the one table the step under test uses. That is also the only way to reach
// these branches: every other path through the hub works.
func TestADatabaseFailureIsSaidToBeOne(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		drop     string
		provider func() provider.Provider
		why      string
	}{
		"the candidate lookup by workspace": {
			drop:     "DROP TABLE lawang.subscriptions",
			provider: func() provider.Provider { return fake.New(fake.DefaultKey) },
			why:      "a lookup that failed found no candidates, which is not the same as there being none",
		},
		"the candidate lookup by registration id": {
			drop: "DROP TABLE lawang.subscriptions",
			provider: func() provider.Provider {
				return keyed{fake.New(fake.DefaultKey), provider.DeliveryKeys{Subscription: "S1"}}
			},
			why: "the second lookup fails the same way as the first",
		},
		"the accept of a resolved delivery": {
			drop:     "DROP TABLE lawang.outbox",
			provider: func() provider.Provider { return fake.New(fake.DefaultKey) },
			why:      "an owner was found and the row could not be written, which is not poison",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := setup(t)
			e.register(tenantA, "W1", "W1", "S1", secretA)
			h, entry := e.hub(tc.provider(), hub.Options{})
			testdb.Exec(t, e.tdb.AdminURL, tc.drop)

			verdict, err := h.Accept(e.ctx, entry, signed(delivery("W1", "S1", "1"), secretA))
			if err == nil {
				t.Fatalf("Accept = %s with no error: %s", verdict, tc.why)
			}
			if verdict != 0 {
				t.Errorf("Accept returned the verdict %s beside its error, which the edge would answer with", verdict)
			}
		})
	}
}

// TestParkingThatFailsIsAnErrorAndNotAVerdict. Parking is what the hub does when it cannot
// attribute a delivery, and it is still a write: a park that did not happen must not be reported
// as one, or an unowned delivery is answered 200 and is nowhere.
func TestParkingThatFailsIsAnErrorAndNotAVerdict(t *testing.T) {
	t.Parallel()
	e := setup(t)
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})
	testdb.Exec(t, e.tdb.AdminURL, "DROP TABLE lawang.outbox")

	verdict, err := h.Accept(e.ctx, entry, signed(delivery("W9", "S9", "1"), secretA))
	if err == nil {
		t.Fatalf("Accept = %s with no error, although nothing was parked", verdict)
	}
}

// TestRegisterSaysWhenItCouldNotStore. The operator API (B14) has to be able to tell "stored" from
// "refused" from "the database is not there", because only the first two are the operator's to fix.
func TestRegisterSaysWhenItCouldNotStore(t *testing.T) {
	t.Parallel()
	e := setup(t)
	testdb.Exec(t, e.tdb.AdminURL, "DROP TABLE lawang.subscriptions")

	_, err := e.sub.Register(e.ctx, provider.Subscription{
		Tenant: tenantA, Provider: fake.DefaultKey, Resource: "W1", Workspace: "W1", Secret: secretA,
	})
	if err == nil {
		t.Fatal("Register reported success with no table to store in")
	}
}
