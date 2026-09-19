// Package tenancy carries the tenant identity and binds it to a database transaction so row-level
// security applies.
//
// A tenant only ever comes from one of the three trust roots in docs/architecture.md section 4.
// It is never read from a payload.
package tenancy

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// setting is the transaction-local Postgres setting every row-level security policy reads.
const setting = "lawang.tenant"

const maxIDLen = 64

// ErrInvalidID reports a tenant id that is empty or malformed.
var ErrInvalidID = errors.New("tenancy: invalid tenant id")

// ErrCrossTenantTx reports a Bind on a transaction that runs under a helper role. Binding would
// not narrow such a transaction, so it is refused instead of giving a false sense of isolation.
var ErrCrossTenantTx = errors.New("tenancy: this transaction runs under a cross-tenant helper role and a bind would not narrow it, do the tenant's work in a second transaction under store.TenantTx")

// CrossTenantTx marks a transaction that runs under a helper database role, whose policies admit
// rows of every tenant. store.RoleTx hands out transactions that implement it.
type CrossTenantTx interface {
	pgx.Tx
	CrossTenant()
}

// ID identifies a tenant. The zero value is invalid; build one with Parse.
type ID string

// Parse validates s as a tenant id: 1 to 64 characters of A-Z, a-z, 0-9, underscore and hyphen.
// The tenant_id domain in the database enforces the same rule, and the two are held together by
// TestTheGoRuleAndTheDomainAgree in internal/store: change one and that test names the other.
func Parse(s string) (ID, error) {
	if s == "" || len(s) > maxIDLen {
		return "", ErrInvalidID
	}
	for i := range len(s) {
		c := s[i]
		ok := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
		if !ok {
			return "", ErrInvalidID
		}
	}
	return ID(s), nil
}

// String returns the id as stored.
func (id ID) String() string { return string(id) }

// Bind scopes tx to the tenant. The setting is transaction-local: it is gone at commit or
// rollback, so a pooled connection never carries one tenant into the next transaction. Callers
// outside internal/store want store.TenantTx, which binds before anything else runs.
//
// Binding again inside the same transaction replaces the tenant.
//
// A bind narrows only a transaction of the application role. Under a helper role
// (store.RoleTx) the role's own cross-tenant policies keep applying, because Postgres ORs
// permissive policies together, so Bind refuses such a transaction with ErrCrossTenantTx. The
// worker therefore claims rows in one transaction under its role and does each row's work in a
// second one under store.TenantTx.
func Bind(ctx context.Context, tx pgx.Tx, id ID) error {
	// Re-validate: ID is a string type, so a caller can build one without Parse.
	if _, err := Parse(string(id)); err != nil {
		return err
	}
	if _, ok := tx.(CrossTenantTx); ok {
		return ErrCrossTenantTx
	}
	if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", setting, string(id)); err != nil {
		return fmt.Errorf("tenancy: bind: %w", err)
	}
	return nil
}
