package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gablooge/lawang/internal/store"
	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/vault/vaultdb"
)

// MasterKeyLen is the length of the local vault's master key in bytes. It is AES-256, so 32.
const MasterKeyLen = 32

// ErrNoMasterKey reports a local vault asked for without a key.
//
// It is the second half of "the local vault refuses to start without a key", and the first half is
// config.Load, which refuses to produce a configuration at all when LAWANG_VAULT_KEY is unset in
// production. This one is what holds for every other caller: a test, a later command, or a
// deployment whose key arrives from somewhere else one day. There is no fallback behind it. A
// local vault with no key could only store credentials in the clear, and a vault that stores
// credentials in the clear is not one (ADR 16).
var ErrNoMasterKey = errors.New("vault: the local vault needs a master key")

// LocalOptions configure a [Local]. Both members are required.
type LocalOptions struct {
	// Key is the master key: exactly MasterKeyLen bytes, from the environment
	// (config.Config.VaultKey, LAWANG_VAULT_KEY). NewLocal reads it and does not keep it: what a
	// Local holds afterwards is the AES key schedule and the key's id, so the key itself is in
	// one place, the configuration, and not in a struct that might be printed.
	Key []byte
}

// Local is the dependency-free vault: one tenant's credential for one provider, sealed with
// AES-256-GCM under a master key from the environment, in this deployment's own Postgres
// (migration 00007, ADR 16).
//
// It is safe for concurrent use: everything in it is immutable after NewLocal, and a
// cipher.AEAD may be used from several goroutines at once.
type Local struct {
	*sealer
	db *store.DB
}

// sealer is the cryptography on its own, with no database behind it: a key schedule, the id of
// the key, and the two functions that turn a credential into an envelope and back (envelope.go).
//
// It is a type of its own so that the sealing can be tested without a Postgres, which is what
// makes the tamper, the wrong-key and the plaintext cases a unit test that runs everywhere rather
// than an integration test that is skipped on a machine with no Docker.
type sealer struct {
	aead cipher.AEAD
	// keyID identifies the key every envelope this vault writes was sealed under, so a row sealed
	// under another key is refused by name rather than as an authentication failure.
	keyID [keyIDLen]byte
}

// NewLocal returns the local vault on db, sealing with opts.Key. It refuses a nil database, and
// every key newSealer refuses.
func NewLocal(db *store.DB, opts LocalOptions) (*Local, error) {
	if db == nil {
		return nil, errors.New("vault: NewLocal needs a database")
	}
	s, err := newSealer(opts.Key)
	if err != nil {
		return nil, err
	}
	return &Local{sealer: s, db: db}, nil
}

// newSealer builds the cryptography for one master key, or refuses the key.
//
// It refuses, rather than defaulting, every way of arriving without usable encryption: no key, a
// key of the wrong length, and the all-zero key, which is what an uninitialized buffer, a cleared
// secret manager entry and a base64 run of A's all produce. None of those is a deployment that
// should get as far as storing a credential.
func newSealer(key []byte) (*sealer, error) {
	if len(key) == 0 {
		return nil, ErrNoMasterKey
	}
	if len(key) != MasterKeyLen {
		// The length and never the value. A key of the wrong length is usually a value pasted
		// from the wrong place, and the wrong place may be another secret.
		return nil, fmt.Errorf("%w: it must be exactly %d bytes", ErrNoMasterKey, MasterKeyLen)
	}
	// Constant time, so that a caller cannot learn how much of a key is zero from how long this
	// takes. It costs nothing on a once-per-process path and it is the right habit on any
	// comparison that touches key material.
	if subtle.ConstantTimeCompare(key, make([]byte, MasterKeyLen)) == 1 {
		return nil, fmt.Errorf("%w: the all-zero key is not a key", ErrNoMasterKey)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		// Unreachable: aes.NewCipher only refuses a length, and the length is checked above.
		return nil, fmt.Errorf("%w: it is not an AES key", ErrNoMasterKey)
	}
	// The nonce is the standard library's to generate, one per Seal, from crypto/rand. See seal
	// for why that matters more than anything else in this file.
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		// Unreachable for AES: NewGCMWithRandomNonce only refuses a block size that is not 16.
		return nil, fmt.Errorf("%w: it cannot be used for AES-GCM", ErrNoMasterKey)
	}
	return &sealer{aead: aead, keyID: keyIDOf(key)}, nil
}

// Local is a Vault, checked at compile time.
var _ Vault = (*Local)(nil)

// checkViolation is SQLSTATE 23514, which is what a CHECK constraint refuses a row with.
const checkViolation = "23514"

// sealedConstraint is the constraint in migration 00007 that refuses a row which does not look
// like a sealed envelope. The name lives here, next to the one statement that can trip it.
const sealedConstraint = "vault_credentials_ciphertext_is_sealed"

// Store seals the credential and writes it, replacing whatever this tenant had for this provider.
//
// The transaction is bound to the tenant and runs as the application role, so row-level security
// decides which row may be written, and the envelope's authenticated data decides which row can
// later be read: a credential written under the wrong tenant by a bug in a caller would be
// refused by the policy, and one moved between tenants by hand no longer decrypts.
func (l *Local) Store(ctx context.Context, t tenancy.ID, providerKey string, c Credential) error {
	if err := checkArgs(t, providerKey); err != nil {
		return err
	}
	plaintext, err := encode(c)
	if err != nil {
		return err
	}
	ciphertext := l.seal(t, providerKey, plaintext)
	err = l.db.TenantTx(ctx, t, func(tx pgx.Tx) error {
		return vaultdb.New(tx).UpsertCredential(ctx, vaultdb.UpsertCredentialParams{
			TenantID:   t.String(),
			Provider:   providerKey,
			Ciphertext: ciphertext,
		})
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == checkViolation && pgErr.ConstraintName == sealedConstraint {
			// The server says what was written is not an envelope, which means the sealing above
			// is broken rather than that the caller did anything wrong. It is its own refusal so
			// that it cannot be read as a database outage and retried for ever. Nothing from the
			// driver's message is kept: PgError.Error() leaves Detail out, and Detail is where
			// the failing row would be.
			return fmt.Errorf("%w: the database refused what this vault sealed", ErrNotSealed)
		}
		return fmt.Errorf("vault: store credential: %w", err)
	}
	return nil
}

// Fetch reads this tenant's credential for this provider and opens it.
//
// Every way the row can fail to be a credential is a refusal with its own name: nothing stored
// (ErrNotFound), something stored that is not an envelope (ErrNotSealed, which is what a
// plaintext row is), an envelope sealed under another key (ErrWrongKey), and an envelope that
// does not authenticate (ErrDecrypt). None of them is an empty credential.
func (l *Local) Fetch(ctx context.Context, t tenancy.ID, providerKey string) (Credential, error) {
	if err := checkArgs(t, providerKey); err != nil {
		return Credential{}, err
	}
	var ciphertext []byte
	err := l.db.TenantTx(ctx, t, func(tx pgx.Tx) error {
		var err error
		ciphertext, err = vaultdb.New(tx).FetchCredential(ctx, vaultdb.FetchCredentialParams{
			TenantID: t.String(),
			Provider: providerKey,
		})
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No row of this tenant's, or a row of another tenant's that row-level security does not
		// show this one. Both are "nothing is stored for you", which is the only answer either
		// may get.
		return Credential{}, fmt.Errorf("%w: %s", ErrNotFound, providerKey)
	case err != nil:
		return Credential{}, fmt.Errorf("vault: fetch credential: %w", err)
	}
	plaintext, err := l.open(t, providerKey, ciphertext)
	if err != nil {
		return Credential{}, err
	}
	return decodeCredential(plaintext)
}

// Revoke removes this tenant's credential for this provider, and reports ErrNotFound when there
// was none. Nothing is decrypted on the way: a row that cannot be read still has to be removable,
// which is exactly the row an operator is trying to get rid of after a key has changed.
func (l *Local) Revoke(ctx context.Context, t tenancy.ID, providerKey string) error {
	if err := checkArgs(t, providerKey); err != nil {
		return err
	}
	var rows int64
	err := l.db.TenantTx(ctx, t, func(tx pgx.Tx) error {
		var err error
		rows, err = vaultdb.New(tx).DeleteCredential(ctx, vaultdb.DeleteCredentialParams{
			TenantID: t.String(),
			Provider: providerKey,
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("vault: revoke credential: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, providerKey)
	}
	return nil
}
