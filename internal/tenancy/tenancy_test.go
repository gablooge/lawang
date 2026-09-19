package tenancy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestParse(t *testing.T) {
	for _, s := range []string{"tenant_a", "01JZXA8Q2KTENANT0000000000", "a", "A-b_9", strings.Repeat("x", 64)} {
		if id, err := Parse(s); err != nil || id.String() != s {
			t.Errorf("Parse(%q) = %q, %v", s, id, err)
		}
	}
	for _, s := range []string{"", " ", "tenant a", "tenant\x1fa", "tenant'a", "ténant", "a;b", strings.Repeat("x", 65)} {
		if id, err := Parse(s); !errors.Is(err, ErrInvalidID) || id != "" {
			t.Errorf("Parse(%q) = %q, %v, want ErrInvalidID", s, id, err)
		}
	}
}

// markedTx stands in for the transaction store.RoleTx hands out. Its embedded pgx.Tx is nil, so
// reaching the database would panic: the refusal has to come first.
type markedTx struct{ pgx.Tx }

func (markedTx) CrossTenant() {}

func TestBindRefusesACrossTenantTransaction(t *testing.T) {
	if err := Bind(context.Background(), markedTx{}, ID("tenant_a")); !errors.Is(err, ErrCrossTenantTx) {
		t.Errorf("err = %v, want ErrCrossTenantTx", err)
	}
}

func TestBindRefusesAnInvalidIDBeforeTouchingTheDatabase(t *testing.T) {
	// A nil transaction proves the refusal happens first: reaching the database would panic.
	if err := Bind(context.Background(), nil, ID("")); !errors.Is(err, ErrInvalidID) {
		t.Errorf("err = %v, want ErrInvalidID", err)
	}
	if err := Bind(context.Background(), nil, ID("a b")); !errors.Is(err, ErrInvalidID) {
		t.Errorf("err = %v, want ErrInvalidID", err)
	}
}
