package vault_test

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/vault"
)

// TestStoreWritesASealedRowAndFetchReadsItBack is the round trip, and it is deliberately not
// Store-then-Fetch alone: a vault that wrote nothing at all and kept the credential in a map
// would pass that. So the row is read as the superuser, held to the envelope's shape, opened with
// a sealer of the test's own, and only then is the credential asked for back.
func TestStoreWritesASealedRowAndFetchReadsItBack(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.storeMarkerA()

	if got := e.rowCount(); got != 1 {
		t.Fatalf("the table holds %d rows, want exactly one", got)
	}
	env := e.ciphertext(tenantA)
	switch {
	case len(env) < vault.MinEnvelopeLen:
		t.Fatalf("the stored value is %d bytes, which is not an envelope", len(env))
	case env[0] != vault.FormatV1:
		t.Fatalf("the stored value starts with %d, not the envelope format %d", env[0], vault.FormatV1)
	case bytes.Contains(env, []byte(marker)):
		t.Fatalf("the stored value carries the credential in the clear: %q", env)
	}
	// The row really holds this credential, sealed: the test opens it itself, so "Fetch returns
	// what Store was given" cannot be true of a vault that never wrote the row.
	plaintext, err := sealerA(t).Open(tenantA, providerKey, env)
	if err != nil {
		t.Fatalf("the stored envelope does not open under the vault's key: %v", err)
	}
	cred, err := vault.DecodeCredential(plaintext)
	if err != nil {
		t.Fatalf("the stored envelope does not hold a credential: %v", err)
	}
	if got, err := cred.Field("token"); err != nil || got != marker {
		t.Fatalf("the stored envelope holds %q, %v, want the credential", got, err)
	}

	e.wantFetchedA(e.v, marker, "the credential that was just stored")
}

func TestFetchOfSomethingThatWasNeverStoredIsARefusal(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.storeMarkerA()

	for name, tc := range map[string]struct {
		tenant   tenancy.ID
		provider string
	}{
		"a tenant with nothing stored":           {tenantB, providerKey},
		"another provider of a connected tenant": {tenantA, otherProvider},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := e.v.Fetch(e.ctx, tc.tenant, tc.provider)
			if !errors.Is(err, vault.ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
			if !got.IsZero() {
				t.Errorf("a refusal also returned a credential: %v", got)
			}
		})
	}
}

// TestOneTenantCannotFetchAnothersCredential is the isolation property, and the row is proven to
// exist first: "not found" is only an isolation result when there was something to find.
func TestOneTenantCannotFetchAnothersCredential(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.storeMarkerA()
	if got := e.rowCount(); got != 1 {
		t.Fatalf("the table holds %d rows, want the one that was stored", got)
	}

	got, err := e.v.Fetch(e.ctx, tenantB, providerKey)
	if !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("tenant B fetched tenant A's row: %v, %v", got, err)
	}
	// And the refusal did not remove anything, so the row is still tenant A's.
	e.wantFetchedA(e.v, marker, "tenant A's own credential after tenant B asked for it")
}

func TestStoreReplacesTheRowInPlace(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.storeMarkerA()
	first := e.ciphertext(tenantA)

	const second = marker + "-rotated"
	e.store(tenantA, providerKey, map[string]string{"token": second})

	if got := e.rowCount(); got != 1 {
		t.Fatalf("storing again left %d rows, want one: connecting a provider twice must update the row", got)
	}
	if bytes.Equal(first, e.ciphertext(tenantA)) {
		t.Error("the row still holds the first envelope")
	}
	if !queryAdmin[bool](e,
		"SELECT updated_at > created_at FROM lawang.vault_credentials WHERE tenant_id = $1 AND provider = $2",
		tenantA.String(), providerKey) {
		t.Error("updated_at did not move when the credential was replaced")
	}
	e.wantFetchedA(e.v, second, "the credential as replaced")
}

func TestRevokeRemovesTheRowAndRevokingNothingIsARefusal(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.storeMarkerA()

	if err := e.v.Revoke(e.ctx, tenantA, providerKey); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got := e.rowCount(); got != 0 {
		t.Fatalf("the table still holds %d rows after a revoke", got)
	}
	if _, err := e.v.Fetch(e.ctx, tenantA, providerKey); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("Fetch after Revoke: %v, want ErrNotFound", err)
	}
	// Revoking again is a refusal and not a silent success: "done" for a credential that is
	// still somewhere is the answer an operator acts on when a token has leaked.
	if err := e.v.Revoke(e.ctx, tenantA, providerKey); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("revoking nothing: %v, want ErrNotFound", err)
	}
}

func TestOneTenantCannotRevokeAnothersCredential(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.storeMarkerA()

	if err := e.v.Revoke(e.ctx, tenantB, providerKey); !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("tenant B revoking tenant A's credential: %v, want ErrNotFound", err)
	}
	if got := e.rowCount(); got != 1 {
		t.Fatalf("the table holds %d rows: tenant B's revoke reached tenant A's row", got)
	}
	e.wantFetchedA(e.v, marker, "tenant A's credential after tenant B tried to revoke it")
}

// TestTheTableItselfRefusesAPlaintextRow is the database's half of "no plaintext credential is
// ever in this table". It is the layer that holds even for a writer that is not this package: a
// psql session, a restore, or a later version that forgot to seal.
func TestTheTableItselfRefusesAPlaintextRow(t *testing.T) {
	t.Parallel()
	e := setup(t)
	plaintext, err := vault.Encode(testCredential(t))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	const insert = "INSERT INTO lawang.vault_credentials (tenant_id, provider, ciphertext) VALUES ($1, $2, $3)"
	err = e.tryAdmin(insert, tenantA.String(), providerKey, plaintext)
	if err == nil {
		t.Fatal("the table accepted a plaintext credential")
	}
	if !isCheckViolation(err, vault.SealedConstraint) {
		t.Errorf("err = %v, want a violation of %s", err, vault.SealedConstraint)
	}
	if got := e.rowCount(); got != 0 {
		t.Errorf("the table holds %d rows after the refusal", got)
	}

	// The control: the same statement, with the same credential sealed, is accepted. Without it
	// the refusal above could be anything about the statement rather than about the value.
	sealed := sealerA(t).Seal(tenantA, providerKey, plaintext)
	e.admin(insert, tenantA.String(), providerKey, sealed)
	if got := e.rowCount(); got != 1 {
		t.Fatalf("the table holds %d rows, want the sealed one", got)
	}
	e.wantFetchedA(e.v, marker, "a row inserted as a sealed envelope")
}

// TestFetchRefusesAPlaintextRowThatReachedTheTable is the Go half of the same rule, and the one
// that matters for a row which predates the constraint or arrived through a restore. The
// constraint is dropped first, because with it in place no such row can exist, and a guard that
// cannot be reached is a guard nothing proves.
func TestFetchRefusesAPlaintextRowThatReachedTheTable(t *testing.T) {
	t.Parallel()
	e := setup(t)
	plaintext, err := vault.Encode(testCredential(t))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	e.admin("ALTER TABLE lawang.vault_credentials DROP CONSTRAINT " + vault.SealedConstraint)
	e.admin("INSERT INTO lawang.vault_credentials (tenant_id, provider, ciphertext) VALUES ($1, $2, $3)",
		tenantA.String(), providerKey, plaintext)
	if got := e.rowCount(); got != 1 {
		t.Fatalf("the plaintext row is not in the table, so this test proves nothing (rows: %d)", got)
	}

	got, err := e.v.Fetch(e.ctx, tenantA, providerKey)
	if err == nil {
		t.Fatalf("Fetch handed out a plaintext row: %v", got)
	}
	if !errors.Is(err, vault.ErrNotSealed) {
		t.Errorf("err = %v, want ErrNotSealed", err)
	}
	if !got.IsZero() {
		t.Errorf("a refusal also returned a credential: %v", got)
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("the refusal carries the credential: %v", err)
	}
}

// TestFetchRefusesATamperedRow is the second "Done when" clause where it happens in production: a
// byte of a row changed by whoever can write the table.
func TestFetchRefusesATamperedRow(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.storeMarkerA()
	// The control: it reads before the row is touched.
	e.wantFetchedA(e.v, marker, "before the row was tampered with")

	// The last byte of the row is part of the authentication tag. set_byte takes a zero-based
	// index, so the last byte is octet_length - 1, and the XOR keeps the length the same: this is
	// a tampered row and not a truncated one.
	e.admin(`UPDATE lawang.vault_credentials
	            SET ciphertext = set_byte(ciphertext, octet_length(ciphertext) - 1,
	                                      get_byte(ciphertext, octet_length(ciphertext) - 1) # 1)
	          WHERE tenant_id = $1 AND provider = $2`, tenantA.String(), providerKey)

	got, err := e.v.Fetch(e.ctx, tenantA, providerKey)
	if err == nil {
		t.Fatalf("Fetch accepted a tampered row: %v", got)
	}
	if !errors.Is(err, vault.ErrDecrypt) {
		t.Errorf("err = %v, want ErrDecrypt", err)
	}
	if !got.IsZero() {
		t.Errorf("a refusal also returned a credential: %v", got)
	}
	// A tampered row is still removable: that is the row an operator is trying to get rid of.
	if err := e.v.Revoke(e.ctx, tenantA, providerKey); err != nil {
		t.Errorf("Revoke of a tampered row: %v", err)
	}
}

// TestFetchRefusesARowSealedUnderAnotherKey is the operator case: the key in the environment is
// not the one the rows were written under (a restore into a new deployment, a rotated secret).
// The refusal has to say that, because the credential is intact and the right key brings it back.
func TestFetchRefusesARowSealedUnderAnotherKey(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.storeMarkerA()

	other := e.vaultUnder(keyB)
	got, err := other.Fetch(e.ctx, tenantA, providerKey)
	if err == nil {
		t.Fatalf("a vault with the wrong key read the row: %v", got)
	}
	if !errors.Is(err, vault.ErrWrongKey) {
		t.Errorf("err = %v, want ErrWrongKey", err)
	}
	// The row is not damaged: the vault that owns the key still reads it.
	e.wantFetchedA(e.v, marker, "after a vault with the wrong key asked for it")
}

// TestARowCopiedToAnotherTenantDoesNotDecrypt is what the envelope's authenticated data buys in
// the database: whoever can write this table cannot move a credential between tenants, because
// the tenant is part of what the tag covers.
func TestARowCopiedToAnotherTenantDoesNotDecrypt(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.storeMarkerA()

	// The row of tenant A, copied verbatim under tenant B, as only a superuser or a bug could.
	e.admin(`INSERT INTO lawang.vault_credentials (tenant_id, provider, ciphertext)
	         SELECT $1, provider, ciphertext FROM lawang.vault_credentials
	          WHERE tenant_id = $2 AND provider = $3`, tenantB.String(), tenantA.String(), providerKey)
	if got := e.rowCount(); got != 2 {
		t.Fatalf("the table holds %d rows, want the original and the copy", got)
	}
	if !bytes.Equal(e.ciphertext(tenantA), e.ciphertext(tenantB)) {
		t.Fatal("the copy is not byte-identical, so this test is not the case it describes")
	}

	got, err := e.v.Fetch(e.ctx, tenantB, providerKey)
	if err == nil {
		t.Fatalf("tenant B read a copy of tenant A's credential: %v", got)
	}
	if !errors.Is(err, vault.ErrDecrypt) {
		t.Errorf("err = %v, want ErrDecrypt", err)
	}
	e.wantFetchedA(e.v, marker, "tenant A's own row, which is the same bytes")
}

// TestTheVaultRefusesWhatItCannotAddress covers checkArgs on all three methods: the refusals that
// are about which credential, rather than about the credential itself.
func TestTheVaultRefusesWhatItCannotAddress(t *testing.T) {
	t.Parallel()
	e := setup(t)
	cred := testCredential(t)

	for name, tc := range map[string]struct {
		tenant   tenancy.ID
		provider string
	}{
		"no tenant":                   {"", providerKey},
		"a tenant with a space":       {"tenant a", providerKey},
		"a tenant that is too long":   {tenancy.ID(strings.Repeat("a", 65)), providerKey},
		"the sentinel tenant":         {tenancy.Sentinel, providerKey},
		"no provider":                 {tenantA, ""},
		"a provider in capitals":      {tenantA, "ClickUp"},
		"a provider with a hyphen":    {tenantA, "click-up"},
		"a provider that is too long": {tenantA, strings.Repeat("a", 33)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := e.v.Store(e.ctx, tc.tenant, tc.provider, cred); !errors.Is(err, vault.ErrInvalidArgument) {
				t.Errorf("Store: %v, want ErrInvalidArgument", err)
			}
			if _, err := e.v.Fetch(e.ctx, tc.tenant, tc.provider); !errors.Is(err, vault.ErrInvalidArgument) {
				t.Errorf("Fetch: %v, want ErrInvalidArgument", err)
			}
			if err := e.v.Revoke(e.ctx, tc.tenant, tc.provider); !errors.Is(err, vault.ErrInvalidArgument) {
				t.Errorf("Revoke: %v, want ErrInvalidArgument", err)
			}
			if got := e.rowCount(); got != 0 {
				t.Errorf("the table holds %d rows after a refused Store", got)
			}
		})
	}
}

func TestStoreRefusesACredentialThatCarriesNothing(t *testing.T) {
	t.Parallel()
	e := setup(t)
	var zero vault.Credential
	if err := e.v.Store(e.ctx, tenantA, providerKey, zero); !errors.Is(err, vault.ErrInvalidCredential) {
		t.Fatalf("Store of the zero credential: %v, want ErrInvalidCredential", err)
	}
	if got := e.rowCount(); got != 0 {
		t.Errorf("the table holds %d rows after the refusal", got)
	}
}

func TestNewLocalRefusesNoDatabase(t *testing.T) {
	t.Parallel()
	v, err := vault.NewLocal(nil, vault.LocalOptions{Key: keyA})
	if err == nil {
		t.Fatal("NewLocal accepted a nil database")
	}
	if v != nil {
		t.Error("a refusal also returned a vault")
	}
}

// TestTenantsDoNotSeeEachOtherUnderConcurrency runs the three operations of several tenants at
// once, under the race detector, and holds each tenant to its own credential. The vault is one
// value shared by every goroutine, which is how a deployment uses it.
func TestTenantsDoNotSeeEachOtherUnderConcurrency(t *testing.T) {
	t.Parallel()
	e := setup(t)
	tenants := []tenancy.ID{"t_one", "t_two", "t_three", "t_four", "t_five", "t_six"}

	var wg sync.WaitGroup
	for i, tenant := range tenants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token := marker + "-" + tenant.String()
			cred, err := vault.NewCredential(map[string]string{"token": token})
			if err != nil {
				t.Errorf("%s: NewCredential: %v", tenant, err)
				return
			}
			if err := e.v.Store(e.ctx, tenant, providerKey, cred); err != nil {
				t.Errorf("%s: Store: %v", tenant, err)
				return
			}
			got, err := e.v.Fetch(e.ctx, tenant, providerKey)
			if err != nil {
				t.Errorf("%s: Fetch: %v", tenant, err)
				return
			}
			value, err := got.Field("token")
			if err != nil {
				t.Errorf("%s: Field: %v", tenant, err)
				return
			}
			if value != token {
				t.Errorf("%s: fetched another tenant's token", tenant)
			}
			// Half of them revoke, so a delete runs beside the other tenants' reads.
			if i%2 == 0 {
				if err := e.v.Revoke(e.ctx, tenant, providerKey); err != nil {
					t.Errorf("%s: Revoke: %v", tenant, err)
				}
			}
		}()
	}
	wg.Wait()

	if got, want := e.rowCount(), len(tenants)/2; got != want {
		t.Errorf("the table holds %d rows, want %d: every tenant that revoked removed exactly its own row", got, want)
	}
}
