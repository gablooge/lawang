package tenancy

import (
	"context"
	"errors"
	"strings"
	"testing"
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

func TestContext(t *testing.T) {
	if _, err := FromContext(context.Background()); !errors.Is(err, ErrNoTenant) {
		t.Errorf("empty context: err = %v, want ErrNoTenant", err)
	}
	// A zero ID in the context is still no tenant.
	if _, err := FromContext(NewContext(context.Background(), "")); !errors.Is(err, ErrNoTenant) {
		t.Errorf("zero id in context: err = %v, want ErrNoTenant", err)
	}
	id, err := FromContext(NewContext(context.Background(), ID("tenant_a")))
	if err != nil || id != "tenant_a" {
		t.Errorf("FromContext = %q, %v", id, err)
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
