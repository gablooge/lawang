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
const setting = "sluiceway.tenant"

const maxIDLen = 64

// ErrInvalidID reports a tenant id that is empty or malformed.
var ErrInvalidID = errors.New("tenancy: invalid tenant id")

// ErrNoTenant reports a context with no tenant in it. Missing identity is a denial, never a
// default.
var ErrNoTenant = errors.New("tenancy: no tenant in context")

// ID identifies a tenant. The zero value is invalid; build one with Parse.
type ID string

// Parse validates s as a tenant id: 1 to 64 characters of A-Z, a-z, 0-9, underscore and hyphen.
// The tenant_id domain in the database enforces the same rule.
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

type ctxKey struct{}

// NewContext returns a context carrying the tenant.
func NewContext(ctx context.Context, id ID) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the tenant carried by ctx, or ErrNoTenant.
func FromContext(ctx context.Context) (ID, error) {
	id, ok := ctx.Value(ctxKey{}).(ID)
	if !ok || id == "" {
		return "", ErrNoTenant
	}
	return id, nil
}

// Bind scopes tx to the tenant. The setting is transaction-local: it is gone at commit or
// rollback, so a pooled connection never carries one tenant into the next transaction.
//
// Binding again inside the same transaction replaces the tenant, which is how the worker moves
// from claiming rows across tenants to working on one row's tenant.
func Bind(ctx context.Context, tx pgx.Tx, id ID) error {
	// Re-validate: ID is a string type, so a caller can build one without Parse.
	if _, err := Parse(string(id)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", setting, string(id)); err != nil {
		return fmt.Errorf("tenancy: bind: %w", err)
	}
	return nil
}
