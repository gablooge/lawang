package hub_test

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/hub"
	"github.com/gablooge/lawang/internal/hub/hubdb"
	"github.com/gablooge/lawang/internal/ingress"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/store"
	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/testdb"
)

// TestRegisteringTheSameResourceAgainUpdatesInPlace is architecture section 5: a subscription is
// unique on (tenant, provider, resource), so re-registering never duplicates. The id is kept,
// because it is what the outbox orders that subscription's deliveries by: a new id would put the
// next delivery in a new queue, beside the one still in flight in the old one.
func TestRegisteringTheSameResourceAgainUpdatesInPlace(t *testing.T) {
	t.Parallel()
	e := setup(t)
	first := e.register(tenantA, "W1", "W1", "S1", secretA)
	again := e.register(tenantA, "W1", "W1", "S2", secretB)

	if again.ID != first.ID {
		t.Errorf("the re-registered subscription has id %q, want the original %q", again.ID, first.ID)
	}
	if again.External != "S2" {
		t.Errorf("the row was not updated: %+v", again)
	}
	// The secret the row now holds is not asserted on the returned value: Register hands back the
	// caller's own, because the upsert does not return the column (nothing but the two candidate
	// queries reads it, which is what makes B13's encryption a change to them). The column is
	// proved by the only thing that reads it, which is the accept path, and that is the thing an
	// operator rotating a secret is actually asking for.
	h, entry := e.hub(fake.New(fake.DefaultKey), hub.Options{})
	if got := e.accept(h, entry, signed(delivery("W1", "S2", "1"), secretA)); got != ingress.Unverified {
		t.Errorf("the old secret still verifies: verdict = %s", got)
	}
	if got := e.accept(h, entry, signed(delivery("W1", "S2", "2"), secretB)); got != ingress.Stored {
		t.Errorf("the new secret does not verify: verdict = %s", got)
	}
}

// TestTwoTenantsMayRegisterTheSameResource. One provider workspace connected by two tenants is the
// case the whole design is built for (architecture section 5), so the uniqueness is per tenant.
func TestTwoTenantsMayRegisterTheSameResource(t *testing.T) {
	t.Parallel()
	e := setup(t)
	a := e.register(tenantA, "W1", "W1", "S1", secretA)
	b := e.register(tenantB, "W1", "W1", "S1", secretB)
	if a.ID == b.ID {
		t.Fatal("two tenants' subscriptions share a row")
	}
}

// TestRegisterRefusesASubscriptionThatCouldNotResolve. Each of these would be stored happily by a
// table with no rules and then answer 401, or nothing at all, for every delivery it was made for,
// and the only sign of it would be on the provider's own dashboard.
func TestRegisterRefusesASubscriptionThatCouldNotResolve(t *testing.T) {
	t.Parallel()
	e := setup(t)
	good := provider.Subscription{
		Tenant: tenantA, Provider: fake.DefaultKey, Resource: "W1",
		Workspace: "W1", External: "S1", Secret: secretA,
	}
	cases := map[string]func(s *provider.Subscription){
		"no tenant":                     func(s *provider.Subscription) { s.Tenant = "" },
		"a tenant that is not one":      func(s *provider.Subscription) { s.Tenant = tenancy.ID("not a tenant") },
		"no provider":                   func(s *provider.Subscription) { s.Provider = "" },
		"a hyphen in the provider key":  func(s *provider.Subscription) { s.Provider = "ms-graph" },
		"no resource":                   func(s *provider.Subscription) { s.Resource = "" },
		"a resource with a NUL":         func(s *provider.Subscription) { s.Resource = "W\x001" },
		"no delivery key at all":        func(s *provider.Subscription) { s.Workspace, s.External = "", "" },
		"a workspace over the bound":    func(s *provider.Subscription) { s.Workspace = strings.Repeat("w", 257) },
		"an external id over the bound": func(s *provider.Subscription) { s.External = strings.Repeat("s", 257) },
		"a workspace with a NUL":        func(s *provider.Subscription) { s.Workspace = "W\x001" },
		"no secret":                     func(s *provider.Subscription) { s.Secret = nil },
		"an empty secret":               func(s *provider.Subscription) { s.Secret = []byte{} },
		"an id with a NUL":              func(s *provider.Subscription) { s.ID = "01\x00SUB" },
		"an id over the bound":          func(s *provider.Subscription) { s.ID = strings.Repeat("i", 65) },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sub := good
			damage(&sub)
			if _, err := e.sub.Register(e.ctx, sub); !errors.Is(err, hub.ErrInvalidSubscription) {
				t.Fatalf("Register = %v, want a refusal", err)
			}
		})
	}
	// The control: the same subscription with nothing broken is accepted, so every refusal above
	// is about the one thing it changed.
	if _, err := e.sub.Register(e.ctx, good); err != nil {
		t.Fatalf("Register refused a good subscription: %v", err)
	}
}

// TestARefusalNamesNoValue. A caller that pasted a secret into the wrong field must not have it
// echoed into a log: every message here ends up in one.
func TestARefusalNamesNoValue(t *testing.T) {
	t.Parallel()
	e := setup(t)
	secret := "a-secret-that-must-not-be-echoed"
	_, err := e.sub.Register(e.ctx, provider.Subscription{
		Tenant: tenantA, Provider: fake.DefaultKey, Resource: secret + "-resource",
		Workspace: secret + "-workspace", External: "", Secret: nil,
	})
	if err == nil {
		t.Fatal("Register accepted a subscription with no secret")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the refusal echoes what it was given: %v", err)
	}
}

// TestASubscriptionBelongsToItsTenantOnly. The table has row-level security forced, so a tenant's
// transaction sees its own rows and nobody else's, and the resolver is the one role that sees
// across tenants (and only to read the resolution columns).
func TestASubscriptionBelongsToItsTenantOnly(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "W1", "W1", "S1", secretA)
	e.register(tenantB, "W1", "W1", "S1", secretB)

	for _, tenant := range []tenancy.ID{tenantA, tenantB} {
		var seen int
		err := e.db.TenantTx(e.ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(e.ctx, "SELECT count(*) FROM subscriptions").Scan(&seen)
		})
		if err != nil {
			t.Fatalf("count as %s: %v", tenant, err)
		}
		if seen != 1 {
			t.Errorf("%s sees %d subscriptions, want only its own", tenant, seen)
		}
	}
	// With no tenant bound, the table reads as empty: the policy compares against a setting that
	// is NULL, and a comparison with NULL matches nothing.
	var seen int
	err := e.db.Tx(e.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, "SELECT count(*) FROM subscriptions").Scan(&seen)
	})
	if err != nil {
		t.Fatalf("count with no tenant bound: %v", err)
	}
	if seen != 0 {
		t.Errorf("a transaction with no tenant bound sees %d subscriptions, want none", seen)
	}
}

// TestTheResolverRoleReadsNoMoreThanItNeeds. The role exists because deriving a tenant cannot be
// done inside one, which makes it the widest reach in the program: it must be granted the six
// columns the two candidate queries touch, and nothing else at all.
func TestTheResolverRoleReadsNoMoreThanItNeeds(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.register(tenantA, "W1", "W1", "S1", secretA)

	granted := []string{"id", "tenant_id", "provider", "workspace_id", "external_id", "secret"}
	for _, column := range granted {
		var n int
		err := e.db.RoleTx(e.ctx, store.RoleResolver, func(tx pgx.Tx) error {
			return tx.QueryRow(e.ctx, "SELECT count("+column+") FROM subscriptions").Scan(&n)
		})
		if err != nil {
			t.Errorf("the resolver cannot read %s, which a candidate lookup needs: %v", column, err)
		}
	}
	for _, column := range []string{"resource", "created_at"} {
		err := e.db.RoleTx(e.ctx, store.RoleResolver, func(tx pgx.Tx) error {
			var n int
			return tx.QueryRow(e.ctx, "SELECT count("+column+") FROM subscriptions").Scan(&n)
		})
		if err == nil {
			t.Errorf("the resolver can read %s, which no candidate lookup reads", column)
		}
	}
	// And it can change nothing at all, in either table it can reach.
	writes := map[string]string{
		"insert a subscription": `INSERT INTO subscriptions (id, tenant_id, provider, resource, workspace_id, external_id, secret)
		                          VALUES ('01X', 'tenant_a', 'fake', 'W2', 'W2', 'S2', 'x')`,
		"change a secret":       `UPDATE subscriptions SET secret = 'x'`,
		"delete a subscription": `DELETE FROM subscriptions`,
		"read the outbox":       `SELECT count(*) FROM outbox`,
		"read a tenant":         `SELECT count(*) FROM tenants`,
	}
	for name, sql := range writes {
		err := e.db.RoleTx(e.ctx, store.RoleResolver, func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, sql)
			return err
		})
		if err == nil {
			t.Errorf("the resolver role can %s", name)
		}
	}
}

// TestTheDeliveryKeyBoundMatchesTheColumn holds the hub's own bound and the table's CHECK
// together. If they drifted apart, a key the hub accepted could fail the INSERT (a 500 for a
// delivery that is not ours) or a key the table stores could never be looked up.
func TestTheDeliveryKeyBoundMatchesTheColumn(t *testing.T) {
	t.Parallel()
	e := setup(t)
	atTheBound := strings.Repeat("w", hub.MaxDeliveryKeyLen)
	if _, err := e.sub.Register(e.ctx, provider.Subscription{
		Tenant: tenantA, Provider: fake.DefaultKey, Resource: "at the bound",
		Workspace: atTheBound, Secret: secretA,
	}); err != nil {
		t.Fatalf("the table refused a delivery key of exactly %d bytes, which the hub accepts: %v", hub.MaxDeliveryKeyLen, err)
	}
	// One byte more is refused by the table as well as by the hub, which is what makes the two
	// numbers one number.
	err := e.db.TenantTx(e.ctx, tenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO subscriptions (id, tenant_id, provider, resource, workspace_id, external_id, secret)
		                          VALUES ($1, $2, 'fake', 'over the bound', $3, '', 'x')`,
			"01OVER", tenantA.String(), atTheBound+"w")
		return err
	})
	if err == nil {
		t.Fatalf("the table stored a delivery key of %d bytes, which the hub refuses before it ever looks one up", hub.MaxDeliveryKeyLen+1)
	}
}

// TestASubscriptionWithNoDeliveryKeyIsRefusedByTheTable. Such a row can never be a candidate, so
// it would sit in the table unreachable, and the deliveries it was made for would all be parked.
func TestASubscriptionWithNoDeliveryKeyIsRefusedByTheTable(t *testing.T) {
	t.Parallel()
	e := setup(t)
	err := e.db.TenantTx(e.ctx, tenantA, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO subscriptions (id, tenant_id, provider, resource, workspace_id, external_id, secret)
		                          VALUES ('01NOKEY', $1, 'fake', 'W1', '', '', 'x')`, tenantA.String())
		return err
	})
	if err == nil {
		t.Fatal("the table stored a subscription no delivery could ever select")
	}
}

// TestTheCandidateLookupUsesAnIndex. The lookup runs once per delivery, on the accept path, so it
// must not read the table. The plan is asked for on a table with enough rows that a sequential
// scan would be the cheaper plan if the index were missing or unusable.
//
// It explains the statement the hub actually sends. The first round of review broke that: the test
// ran EXPLAIN on a SQL string typed into the test, so it agreed with a copy while the checked-in
// query went to a sequential scan. Rewriting the workspace equality in internal/hub/queries.sql
// into a concatenation, which no index can serve, left the whole suite green and the accept path
// reading 515 buffers per delivery instead of 3.
func TestTheCandidateLookupUsesAnIndex(t *testing.T) {
	t.Parallel()
	e := setup(t)
	// Autovacuum is turned off for this table first, and only then are the rows written. A bulk
	// insert of this size makes the autovacuum launcher analyze the table, and its own ANALYZE and
	// the one below both update the table's pg_class row: whichever loses says "tuple concurrently
	// updated" (XX000), which CI found before this line existed. The table belongs to one test's
	// own database, so nothing else is affected.
	testdb.Exec(t, e.tdb.AdminURL, `
		ALTER TABLE lawang.subscriptions SET (autovacuum_enabled = false);
		INSERT INTO lawang.subscriptions (id, tenant_id, provider, resource, workspace_id, external_id, secret)
		SELECT 'sub' || i, 'tenant_' || (i % 500), 'fake', 'r' || i, 'W' || i, 'S' || i, 'x'
		  FROM generate_series(1, 50000) AS i;`)
	e.analyze("lawang.subscriptions")

	// The one row every lookup below goes looking for, and the limit the hub asks for.
	const (
		providerKey  = "fake"
		workspace    = "W42"
		subscription = "S42"
		limit        = hub.DefaultMaxCandidates + 1
	)
	lookups := map[string]struct {
		// query is the sqlc name of the statement, which is how it is found among everything else
		// this connection has prepared. run is the generated method internal/hub itself calls, and
		// args are values of the right types in the statement's own parameter order.
		query string
		run   func(q *hubdb.Queries) error
		args  string
		index string
	}{
		"by workspace": {
			query: "CandidatesByWorkspace",
			run: func(q *hubdb.Queries) error {
				_, err := q.CandidatesByWorkspace(e.ctx, hubdb.CandidatesByWorkspaceParams{
					Provider:      providerKey,
					Workspace:     workspace,
					Subscription:  subscription,
					MaxCandidates: limit,
				})
				return err
			},
			args:  "'fake', 'W42', 'S42', 33",
			index: "subscriptions_by_workspace",
		},
		"by subscription": {
			query: "CandidatesBySubscription",
			run: func(q *hubdb.Queries) error {
				_, err := q.CandidatesBySubscription(e.ctx, hubdb.CandidatesBySubscriptionParams{
					Provider:      providerKey,
					Workspace:     workspace,
					Subscription:  subscription,
					MaxCandidates: limit,
				})
				return err
			},
			args:  "'fake', 'S42', 'W42', 33",
			index: "subscriptions_by_external",
		},
	}
	for name, lookup := range lookups {
		plan := e.explainAsSent(lookup.query, lookup.args, lookup.run)
		// The index by name, not merely "an index scan": ORDER BY id LIMIT means the planner can
		// walk the primary key and filter, which is an Index Scan in the plan and a read of the
		// whole table in fact. That is what dropping the index this lookup needs actually produces,
		// and a test that accepted it would not notice.
		if !strings.Contains(plan, lookup.index) {
			t.Errorf("%s does not use %s, on a path that runs once per delivery:\n%s", name, lookup.index, plan)
		}
		if strings.Contains(plan, "Seq Scan") {
			t.Errorf("%s has a sequential scan in its plan:\n%s", name, plan)
		}
		// The probe found the row it went looking for. Without this the arguments could sit in the
		// wrong parameters, match nothing, and still descend the right index in two buffers.
		if got := actualRows(plan); got != 1 {
			t.Errorf("%s returned %d rows, want the one subscription it names:\n%s", name, got, plan)
		}
		// And the cost is the matching rows, not the table. 50,000 rows are about 500 pages; a
		// lookup that reads the whole thing shows up here whatever the plan is called.
		if buffers := readBuffers(plan); buffers < 1 || buffers > 100 {
			t.Errorf("%s read %d buffers for at most %d rows, which is not an index probe:\n%s",
				name, buffers, limit, plan)
		}
	}
}

// explainAsSent is the plan of the statement the hub actually sends for one sqlc query, and never
// of a copy of it living in this file.
//
// run drives the generated method internal/hub itself calls, which makes pgx prepare the statement
// on this connection. pg_prepared_statements then holds Postgres's own copy of the text, found by
// the "-- name: <Name> :many" line sqlc writes at the top of every query it generates, and the plan
// comes from EXECUTE of that statement. So a change to internal/hub/queries.sql is a change to what
// is explained here, which is the whole point: the previous version of this test could not see one.
//
// plan_cache_mode = force_generic_plan is the other half of it. pgx caches the statement, so the
// plan a delivery runs after the first few executions is the parameterized one, made without
// knowing the values. That is the plan a partial index, or a wrapper around an indexed column,
// quietly stops using, and a statement explained with its values written in would not show it: the
// planner constant-folds a literal and cannot fold a parameter.
func (e *env) explainAsSent(query, args string, run func(*hubdb.Queries) error) string {
	e.t.Helper()
	var plan strings.Builder
	err := e.db.RoleTx(e.ctx, store.RoleResolver, func(tx pgx.Tx) error {
		if err := run(hubdb.New(tx)); err != nil {
			return fmt.Errorf("run %s: %w", query, err)
		}
		var name string
		err := tx.QueryRow(e.ctx,
			"SELECT name FROM pg_prepared_statements WHERE statement LIKE $1",
			"%-- name: "+query+" :%").Scan(&name)
		if err != nil {
			return fmt.Errorf("find the statement pgx prepared for %s: %w", query, err)
		}
		if _, err := tx.Exec(e.ctx, "SET LOCAL plan_cache_mode = force_generic_plan"); err != nil {
			return err
		}
		rows, err := tx.Query(e.ctx,
			"EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) EXECUTE "+pgx.Identifier{name}.Sanitize()+"("+args+")")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			plan.WriteString(line + "\n")
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatalf("explain %s as sent: %v", query, err)
	}
	return plan.String()
}

// actualRows is the row count of the outermost node of an EXPLAIN (ANALYZE) plan: the "rows=N" of
// its "(actual time=... rows=N loops=1)", which is what the statement returned and not an estimate.
func actualRows(plan string) int {
	for _, line := range strings.Split(plan, "\n") {
		i := strings.Index(line, "(actual ")
		if i < 0 {
			continue
		}
		for _, field := range strings.Fields(line[i:]) {
			if n, err := strconv.Atoi(strings.TrimPrefix(field, "rows=")); err == nil && strings.HasPrefix(field, "rows=") {
				return n
			}
		}
	}
	return -1
}

// readBuffers is the "shared hit=N read=M" total of the execution half of an
// EXPLAIN (ANALYZE, BUFFERS) plan. Everything from the "Planning:" line on is left out: planning
// reads the catalogue, once per connection's statement cache, and is not what a delivery costs.
func readBuffers(plan string) int {
	total := 0
	for _, line := range strings.Split(plan, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Planning") {
			break
		}
		i := strings.Index(line, "Buffers: shared")
		if i < 0 {
			continue
		}
		for _, field := range strings.Fields(line[i:]) {
			for _, prefix := range []string{"hit=", "read="} {
				if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(field, prefix), ",")); err == nil && strings.HasPrefix(field, prefix) {
					total += n
				}
			}
		}
	}
	return total
}
