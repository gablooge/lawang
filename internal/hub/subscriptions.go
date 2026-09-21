package hub

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/hub/hubdb"
	"github.com/gablooge/lawang/internal/ids"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/store"
	"github.com/gablooge/lawang/internal/tenancy"
)

// maxResourceLen bounds what a subscription may say it covers. Like the delivery keys, it is the
// provider's own text, and unlike them it never reaches the accept path.
const maxResourceLen = 512

// maxIDLen bounds a subscription's own id, which is a 26 character ULID unless a caller supplies
// one. It is half of the ordering key of every delivery the subscription receives.
const maxIDLen = 64

// ErrInvalidSubscription reports a subscription that could never resolve a delivery. Its message
// names the field and never its value: a subscription carries a secret, and a caller that passed
// one into the wrong field must not have it echoed into a log.
var ErrInvalidSubscription = errors.New("hub: invalid subscription")

// Subscriptions is the subscription table: what the accept path resolves owners in, and what a
// Registrar's results are written to.
//
// Writing is the operator's path (B14's /v1 and "lawang connect"), bound to one tenant under
// row-level security, as the application role. It is deliberately not the resolver's: that role
// may only read, and only on the accept path.
type Subscriptions struct{ db *store.DB }

// NewSubscriptions returns the subscription table on db.
func NewSubscriptions(db *store.DB) *Subscriptions { return &Subscriptions{db: db} }

// Register stores a subscription, or updates in place the one already registered for the same
// (tenant, provider, resource), and returns it as stored (architecture section 5: re-registering
// updates, never duplicates). The stored row keeps its id, because the id is what the outbox
// orders that subscription's deliveries by.
//
// It refuses anything that could resolve NO delivery at all: a tenant id that is not one, a
// provider key that is not one (the same rule the registry and the scope id use, ADR 3), a
// resource that is empty or unstorable, a delivery key over what the table stores, a row with
// neither delivery key, and an empty secret. Refusing here rather than storing is what keeps
// "every delivery is a 401" from being a thing an operator can configure by accident.
//
// It does not refuse a row that carries only one of the two delivery keys, and it could not: which
// keys a provider puts on a delivery is the provider's business, and the candidate lookup asks
// every row either key could select (see Hub.candidates). A row with a workspace id alone is found
// by a delivery that names that workspace, a row with a registration id alone by a delivery that
// names that registration, and a row with both by either.
//
// The returned subscription carries the caller's own secret. Nothing reads that column back out of
// the table here; see the field.
func (s *Subscriptions) Register(ctx context.Context, sub provider.Subscription) (provider.Subscription, error) {
	if err := validate(sub); err != nil {
		return provider.Subscription{}, err
	}
	id := sub.ID
	if id == "" {
		id = ids.New()
	}
	var stored provider.Subscription
	err := s.db.TenantTx(ctx, sub.Tenant, func(tx pgx.Tx) error {
		row, err := hubdb.New(tx).UpsertSubscription(ctx, hubdb.UpsertSubscriptionParams{
			ID:          id,
			TenantID:    sub.Tenant.String(),
			Provider:    sub.Provider,
			Resource:    sub.Resource,
			WorkspaceID: sub.Workspace,
			ExternalID:  sub.External,
			Secret:      sub.Secret,
		})
		if err != nil {
			return err
		}
		stored = provider.Subscription{
			ID: row.ID,
			// Not row.TenantID re-parsed: the transaction is bound to sub.Tenant, and the table's
			// WITH CHECK refuses a row of any other tenant, so the column can only hold the id
			// validate has already been through. Parsing it again would be a branch nothing can
			// reach and no test can cover.
			Tenant:    sub.Tenant,
			Provider:  row.Provider,
			Resource:  row.Resource,
			Workspace: row.WorkspaceID,
			External:  row.ExternalID,
			// The caller's own secret, never one read back out of the table. The two candidate
			// queries are the only readers of that column in the program, and that is what makes
			// encrypting it (B13) a change to them and to one function; a RETURNING list with the
			// secret in it would be a third reader, and its value would travel on to the operator
			// API (B14). The upsert writes this value or nothing, so it is what is stored.
			Secret: sub.Secret,
		}
		return nil
	})
	if err != nil {
		return provider.Subscription{}, fmt.Errorf("hub: register subscription: %w", err)
	}
	return stored, nil
}

// validate holds a subscription to what the table stores and the accept path can use. Every
// message names a field and no value.
func validate(sub provider.Subscription) error {
	if _, err := tenancy.Parse(string(sub.Tenant)); err != nil {
		return fmt.Errorf("%w: tenant: %w", ErrInvalidSubscription, err)
	}
	if !record.ValidProviderKey(sub.Provider) {
		return fmt.Errorf("%w: provider is not a provider key", ErrInvalidSubscription)
	}
	if sub.Resource == "" || len(sub.Resource) > maxResourceLen || !storable(sub.Resource) {
		return fmt.Errorf("%w: resource must be 1 to %d bytes of storable text", ErrInvalidSubscription, maxResourceLen)
	}
	if sub.Workspace == "" && sub.External == "" {
		return fmt.Errorf("%w: a subscription needs a workspace id or an external id, or no delivery could ever select it", ErrInvalidSubscription)
	}
	for _, k := range []struct{ name, key string }{
		{"workspace", sub.Workspace},
		{"external", sub.External},
	} {
		if len(k.key) > maxDeliveryKeyLen || !storable(k.key) {
			return fmt.Errorf("%w: the %s id must be at most %d bytes of storable text", ErrInvalidSubscription, k.name, maxDeliveryKeyLen)
		}
	}
	if len(sub.Secret) == 0 {
		// An empty secret verifies nothing (principle 1), so a row with one would answer 401 for
		// every delivery it was made for, and only the provider's dashboard would show it.
		return fmt.Errorf("%w: the secret is empty", ErrInvalidSubscription)
	}
	// An id is Lawang's own ULID, 26 characters. It is bounded here because it is half of the
	// ordering key of every delivery this subscription ever receives, and the outbox refuses an
	// ordering key over 512 bytes: a subscription registered with a longer id would be resolved
	// happily and then have every one of its deliveries parked as poison.
	if sub.ID != "" && (len(sub.ID) > maxIDLen || !storable(sub.ID)) {
		return fmt.Errorf("%w: the id must be at most %d bytes of storable text", ErrInvalidSubscription, maxIDLen)
	}
	return nil
}

// storable reports whether Postgres takes s as a text value: it refuses a NUL byte and anything
// that is not valid UTF-8 with SQLSTATE 22021, which a caller cannot tell from an outage.
func storable(s string) bool {
	return utf8.ValidString(s) && strings.IndexByte(s, 0) < 0
}
