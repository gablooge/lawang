// Package ids mints every identifier Sluiceway uses. The hashed key recipes live here and nowhere
// else, so they cannot drift between the accept path, the worker and reconciliation.
//
// The recipes are a compatibility contract: changing one re-keys every record already delivered.
// testdata/golden.json pins them, and another implementation can verify itself against that file.
package ids

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
	"github.com/zeebo/blake3"
)

// sep joins hashed parts. It is the ASCII unit separator, so ("ab","c") never hashes like
// ("a","bc").
const sep = 0x1F

// RecordPrefix starts every record id.
const RecordPrefix = "rec_"

// recordHexLen is how much of the hash a record id keeps: 32 hex characters, 128 bits.
const recordHexLen = 32

// ErrEmptyPart reports a missing key part. A missing tenant or provider is a refusal, never a
// default, because a defaulted part makes unrelated things share an id.
var ErrEmptyPart = errors.New("ids: empty key part")

// ErrSeparatorInPart reports a part containing the separator byte, which would let two different
// part lists produce the same hash input.
var ErrSeparatorInPart = errors.New("ids: key part contains the 0x1F separator")

// New returns a new ULID: unique, and sortable by creation time. It is safe for concurrent use.
//
// The result is an identifier, never a secret. It is guessable: the first 48 bits are the clock,
// and ulid.Make draws the other 80 from math/rand, seeded once from the process start time in
// nanoseconds. Whoever can estimate when the process started can search the seeds (about 10^9 of
// them for a one second guess, not 2^80), and one observed id confirms the right one. Ids minted
// in the same millisecond differ only by an increment of at most 32 bits.
//
// So never use New for a token, a nonce, an OAuth state or anything else whose value must be
// unpredictable. Those come from crypto/rand.
func New() string {
	return ulid.Make().String()
}

// DeliveryID is the accept-path dedupe key: blake3(provider, raw_body), as 64 hex characters. An
// identical re-send from a provider maps to the same id and becomes an accept no-op.
//
// rawBody is hashed exactly as received. It is the last part, so it may contain any byte.
func DeliveryID(provider string, rawBody []byte) (string, error) {
	if err := checkPart("provider", provider); err != nil {
		return "", err
	}
	h := blake3.New()
	_, _ = h.WriteString(provider)
	_, _ = h.Write([]byte{sep})
	_, _ = h.Write(rawBody)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RecordID is the end-to-end idempotency key:
//
//	"rec_" + hex(blake3(provider, external_id, version, scope, tenant))[:32]
//
// provider is the internal provider key, never a sink's wire name, so renaming a source for a sink
// does not re-key its records. tenant is part of the hash on purpose: two tenants may connect the
// same provider workspace, and without it the second tenant's records would dedupe away.
//
// scope is the record's visibility.scope, the one thing access is decided on (ADR 4). It is hashed
// so that an entity which moves to another scope is a new record even when the provider's own
// version did not change with the move. Otherwise the moved record would keep its id, the ledger
// would skip it as already delivered, and the sink would go on deciding access on the old scope.
//
// record.Seal is the only caller, and it hashes the scope the record carries and no other. That
// is enforced, not only said. The guard is in record: a Record whose id, external id, version or
// scope is not what Seal left there cannot be marshalled, whatever minted the id. The tripwire
// is TestRecordIDHasNoCallerOutsideRecord, which fails when a file outside internal/record
// refers to this function, when this package names it anywhere but here (so do not wrap it),
// and on a go:linkname directive that reaches this package.
func RecordID(provider, externalID, version, scope, tenant string) (string, error) {
	parts := [...]struct{ name, value string }{
		{"provider", provider},
		{"external_id", externalID},
		{"version", version},
		{"scope", scope},
		{"tenant", tenant},
	}
	h := blake3.New()
	for i, p := range parts {
		if err := checkPart(p.name, p.value); err != nil {
			return "", err
		}
		if i > 0 {
			_, _ = h.Write([]byte{sep})
		}
		_, _ = h.WriteString(p.value)
	}
	return RecordPrefix + hex.EncodeToString(h.Sum(nil))[:recordHexLen], nil
}

func checkPart(name, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s", ErrEmptyPart, name)
	}
	if strings.IndexByte(value, sep) >= 0 {
		return fmt.Errorf("%w: %s", ErrSeparatorInPart, name)
	}
	return nil
}
