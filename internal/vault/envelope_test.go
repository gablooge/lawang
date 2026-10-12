package vault_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/gablooge/lawang/migrations"

	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/vault"
)

// The keys these tests seal with: 32 random bytes each, generated for this file and used nowhere
// else. keyA is the vault's own and keyB is somebody else's, which is what the wrong-key cases
// need. No real key is in this repository (CLAUDE.md).
var (
	keyA = []byte("\x8c\x1f\x53\xa7\x02\xd9\x4b\x6e\x11\x7a\xc4\x38\x95\x60\xeb\x2d" +
		"\x7f\x0a\x46\xb8\xd3\x21\x9c\x5e\x68\xf4\x13\xaa\x27\x90\xcd\x54")
	keyB = []byte("\x3e\xb6\x09\x71\xc8\x2a\xf5\x4d\x16\x83\xdb\x60\x2f\x9a\x47\xe1" +
		"\x05\xbc\x38\x92\x6f\xd4\x1b\x7e\xa0\x59\xc3\x26\x84\x0d\xf7\x31")
)

const (
	tenantA = tenancy.ID("tenant_a")
	tenantB = tenancy.ID("tenant_b")
	// providerKey is the provider every test seals for, and otherProvider the one that must not be
	// able to read it. Both satisfy ADR 3's grammar, which checkArgs holds them to.
	providerKey   = "clickup"
	otherProvider = "slack"
)

// sealerA is a sealer under keyA. Every test builds its own, because a sealer is cheap and a
// shared one would let one test's mutation reach another.
func sealerA(t *testing.T) interface {
	Seal(tenancy.ID, string, []byte) []byte
	Open(tenancy.ID, string, []byte) ([]byte, error)
	Overhead() int
} {
	t.Helper()
	s, err := vault.Sealer(keyA)
	if err != nil {
		t.Fatalf("Sealer: %v", err)
	}
	return s
}

// sealedMarker is the envelope of a credential carrying the marker, for tenantA and providerKey.
func sealedMarker(t *testing.T) []byte {
	t.Helper()
	plaintext, err := vault.Encode(testCredential(t))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return sealerA(t).Seal(tenantA, providerKey, plaintext)
}

func TestSealAndOpenRoundTrip(t *testing.T) {
	t.Parallel()
	s := sealerA(t)
	for name, fields := range map[string]map[string]string{
		"one token":          {"token": marker},
		"client credentials": {"client_id": "an-id", "client_secret": marker},
		"the longest value":  {"token": strings.Repeat("x", vault.MaxValueLen)},
		"another script":     {"token": "鍵" + marker},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cred, err := vault.NewCredential(fields)
			if err != nil {
				t.Fatalf("NewCredential: %v", err)
			}
			plaintext, err := vault.Encode(cred)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			env := s.Seal(tenantA, providerKey, plaintext)
			// The envelope must not be the plaintext: this is the one assertion that separates
			// "it was sealed" from "it was copied".
			if bytes.Contains(env, []byte(marker)) {
				t.Fatalf("the envelope carries the credential in the clear: %q", env)
			}
			for want, value := range fields {
				if bytes.Contains(env, []byte(value)) {
					t.Fatalf("the envelope carries field %q in the clear", want)
				}
			}
			opened, err := s.Open(tenantA, providerKey, env)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			got, err := vault.DecodeCredential(opened)
			if err != nil {
				t.Fatalf("DecodeCredential: %v", err)
			}
			for want, value := range fields {
				if v, err := got.Field(want); err != nil || v != value {
					t.Errorf("Field(%q) = %q, %v, want what was sealed", want, v, err)
				}
			}
		})
	}
}

// TestTheShortestEnvelopeIsTheNumberTheMigrationCarries holds three places together: the
// constant, what AES-GCM really adds, and the number written into migration 00007's CHECK. The
// constraint is the database's half of "this table never holds a plaintext credential", and a
// number that drifted would either refuse a legitimate envelope or accept a short value.
func TestTheShortestEnvelopeIsTheNumberTheMigrationCarries(t *testing.T) {
	t.Parallel()
	s := sealerA(t)
	if got := vault.HeaderLen + s.Overhead(); got != vault.MinEnvelopeLen {
		t.Errorf("the header plus GCM's overhead is %d and MinEnvelopeLen is %d", got, vault.MinEnvelopeLen)
	}
	if s.Overhead() != vault.GCMOverhead {
		t.Errorf("GCM's overhead is %d and the constant says %d", s.Overhead(), vault.GCMOverhead)
	}
	// The shortest envelope that can exist, built rather than asserted: an empty plaintext.
	if got := len(s.Seal(tenantA, providerKey, nil)); got != vault.MinEnvelopeLen {
		t.Errorf("sealing nothing produced %d bytes, want %d", got, vault.MinEnvelopeLen)
	}

	sql, err := migrations.FS.ReadFile("00007_vault.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	// The whitespace is normalized first: the CHECK is wrapped over two lines in the file, so a
	// literal substring search would miss it and report a clean result for the wrong reason.
	oneLine := regexp.MustCompile(`\s+`).ReplaceAllString(string(sql), " ")
	want := fmt.Sprintf("CHECK (octet_length(ciphertext) >= %d AND get_byte(ciphertext, 0) = %d)",
		vault.MinEnvelopeLen, vault.FormatV1)
	if !strings.Contains(oneLine, want) {
		t.Errorf("migration 00007 does not carry %q", want)
	}
	// The control: the same search for a number that is NOT the constant must fail, so a
	// normalization that destroyed the text cannot read as a pass.
	wrong := fmt.Sprintf("CHECK (octet_length(ciphertext) >= %d AND get_byte(ciphertext, 0) = %d)",
		vault.MinEnvelopeLen+1, vault.FormatV1)
	if strings.Contains(oneLine, wrong) {
		t.Errorf("the control matched: the search would pass for any number")
	}
}

// TestTamperedCiphertextFailsToDecrypt is the second "Done when" clause of item B13.
//
// It flips a bit in EVERY byte of an envelope, one at a time, so no region is covered by accident
// and none is left out: the format byte, the key id, the nonce AES-GCM prepended, the ciphertext
// and the authentication tag. Each refusal is checked against the reason that region should give,
// because "it failed" is also what a broken test gives.
func TestTamperedCiphertextFailsToDecrypt(t *testing.T) {
	t.Parallel()
	s := sealerA(t)
	env := sealedMarker(t)

	// The control: the envelope these cases start from really does open, so every refusal below
	// is the tampering's doing and not the envelope's.
	if _, err := s.Open(tenantA, providerKey, env); err != nil {
		t.Fatalf("the untampered envelope does not open, so this test proves nothing: %v", err)
	}

	for i := range env {
		tampered := bytes.Clone(env)
		tampered[i] ^= 1
		got, err := s.Open(tenantA, providerKey, tampered)
		if err == nil {
			t.Fatalf("byte %d of %d: a tampered envelope opened and returned %q", i, len(env), got)
		}
		if got != nil {
			t.Errorf("byte %d: a refusal also returned plaintext", i)
		}
		// Which refusal belongs to which region. The format byte and the key id are checked by
		// name before anything is decrypted; everything after them is the authentication tag's
		// to catch, and it cannot tell which byte moved.
		var want error
		switch {
		case i == 0:
			want = vault.ErrNotSealed
		case i < vault.HeaderLen:
			want = vault.ErrWrongKey
		default:
			want = vault.ErrDecrypt
		}
		if !errors.Is(err, want) {
			t.Errorf("byte %d: err = %v, want %v", i, err, want)
		}
		if strings.Contains(err.Error(), marker) {
			t.Errorf("byte %d: the refusal carries the credential: %v", i, err)
		}
	}
}

// TestAnEnvelopeThatIsTheWrongLengthIsRefused covers what the bit-flipping cannot: bytes added,
// bytes taken away, and the empty value. A truncated row is what a half-written restore leaves.
func TestAnEnvelopeThatIsTheWrongLengthIsRefused(t *testing.T) {
	t.Parallel()
	s := sealerA(t)
	env := sealedMarker(t)

	for name, tc := range map[string]struct {
		env  []byte
		want error
	}{
		"nothing":                 {nil, vault.ErrNotSealed},
		"the header alone":        {env[:vault.HeaderLen], vault.ErrNotSealed},
		"one byte short":          {env[:len(env)-1], vault.ErrDecrypt},
		"the shortest possible":   {env[:vault.MinEnvelopeLen-1], vault.ErrNotSealed},
		"one byte too many":       {append(bytes.Clone(env), 0), vault.ErrDecrypt},
		"two envelopes":           {append(bytes.Clone(env), env...), vault.ErrDecrypt},
		"longer than the maximum": {append(bytes.Clone(env), make([]byte, vault.MaxEnvelopeLen)...), vault.ErrNotSealed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := s.Open(tenantA, providerKey, tc.env)
			if err == nil {
				t.Fatalf("it opened and returned %q", got)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestACiphertextValidUnderAnotherKeyIsRefused is the case a naive implementation accepts: the
// bytes are a perfectly good envelope, sealed by somebody, just not under this vault's key.
//
// Both ways in are covered. Left alone, the envelope's header names a key this vault does not
// hold, which is refused by name. Rewritten to name this vault's key, which is what an attacker
// with the row would do, it is refused by the authentication tag, because the header is part of
// the authenticated data.
func TestACiphertextValidUnderAnotherKeyIsRefused(t *testing.T) {
	t.Parallel()
	a := sealerA(t)
	b, err := vault.Sealer(keyB)
	if err != nil {
		t.Fatalf("Sealer: %v", err)
	}
	plaintext, err := vault.Encode(testCredential(t))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	foreign := b.Seal(tenantA, providerKey, plaintext)

	// The control: it is a valid envelope, and the vault that owns the key reads it. Without
	// this, both refusals below could be a malformed value rather than the wrong key.
	if _, err := b.Open(tenantA, providerKey, foreign); err != nil {
		t.Fatalf("the foreign envelope does not open under its own key, so this test proves nothing: %v", err)
	}

	if _, err := a.Open(tenantA, providerKey, foreign); !errors.Is(err, vault.ErrWrongKey) {
		t.Errorf("err = %v, want ErrWrongKey", err)
	}

	// The same bytes with the header made to look local. The key id of this vault is taken from
	// an envelope it sealed itself, so the test does not recompute the derivation.
	local := sealedMarker(t)
	disguised := bytes.Clone(foreign)
	copy(disguised[:vault.HeaderLen], local[:vault.HeaderLen])
	if !bytes.Equal(disguised[:vault.HeaderLen], local[:vault.HeaderLen]) {
		t.Fatal("the disguise did not take, so the case below is not the one described")
	}
	got, err := a.Open(tenantA, providerKey, disguised)
	if !errors.Is(err, vault.ErrDecrypt) {
		t.Errorf("err = %v, want ErrDecrypt", err)
	}
	if got != nil {
		t.Errorf("a refusal also returned plaintext: %q", got)
	}
}

// TestAnEnvelopeOfAnotherTenantOrProviderDoesNotOpen is what the additional authenticated data
// buys: the key alone is not enough to move a credential between tenants by editing a row.
func TestAnEnvelopeOfAnotherTenantOrProviderDoesNotOpen(t *testing.T) {
	t.Parallel()
	s := sealerA(t)
	env := sealedMarker(t) // sealed for tenantA and providerKey

	if _, err := s.Open(tenantA, providerKey, env); err != nil {
		t.Fatalf("the envelope does not open for its own tenant and provider: %v", err)
	}
	for name, tc := range map[string]struct {
		tenant   tenancy.ID
		provider string
	}{
		"another tenant":         {tenantB, providerKey},
		"another provider":       {tenantA, otherProvider},
		"both":                   {tenantB, otherProvider},
		"a tenant with a prefix": {tenancy.ID("tenant_ax"), providerKey},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := s.Open(tc.tenant, tc.provider, env)
			if !errors.Is(err, vault.ErrDecrypt) {
				t.Errorf("Open(%s, %s): err = %v, want ErrDecrypt", tc.tenant, tc.provider, err)
			}
			if got != nil {
				t.Errorf("a refusal also returned plaintext: %q", got)
			}
		})
	}
}

// TestAPlaintextCredentialIsNotAnEnvelope is the "refuses plaintext" half of item B13, at the
// layer that reads a row.
//
// Every case here is a value a reader could mistake for a credential: the exact bytes this
// package would have written if it had forgotten to seal them, the same bytes padded past the
// minimum length so the refusal cannot be about the length, and the shapes a hand-edited or
// restored row takes. None of them may come back as a credential, and each is refused as "not
// sealed" rather than as a decryption failure, because the two send an operator to different
// places.
func TestAPlaintextCredentialIsNotAnEnvelope(t *testing.T) {
	t.Parallel()
	s := sealerA(t)
	plaintext, err := vault.Encode(testCredential(t))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Contains(plaintext, []byte(marker)) {
		t.Fatal("the encoded credential does not carry the marker, so these cases are not what they say")
	}
	if len(plaintext) <= vault.MinEnvelopeLen {
		t.Fatalf("the encoded credential is %d bytes, which is under the envelope minimum: the "+
			"cases below would be refused by length and prove nothing", len(plaintext))
	}

	padded := append(bytes.Clone(plaintext), bytes.Repeat([]byte(" "), vault.MinEnvelopeLen)...)
	bare, err := json.Marshal(map[string]string{"token": marker})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for name, value := range map[string][]byte{
		"what this package would have written unsealed": plaintext,
		"the same, padded past the minimum length":      padded,
		"a bare JSON object":                            append(bare, bytes.Repeat([]byte(" "), vault.MinEnvelopeLen)...),
		"the token on its own":                          []byte(strings.Repeat(marker, 4)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := s.Open(tenantA, providerKey, value)
			if err == nil {
				t.Fatalf("a plaintext value opened and returned %q", got)
			}
			if !errors.Is(err, vault.ErrNotSealed) {
				t.Errorf("err = %v, want ErrNotSealed", err)
			}
			if got != nil {
				t.Errorf("a refusal also returned %q", got)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("the refusal carries the credential: %v", err)
			}
		})
	}
}

// TestSealNeverRepeatsANonce is the property AES-GCM lives or dies by: a nonce used twice under
// one key reveals the XOR of two plaintexts and the authentication subkey.
//
// Nothing here chooses the nonce. cipher.NewGCMWithRandomNonce draws one from crypto/rand per
// Seal and prepends it, so what this test can show is that the nonces really are distinct over
// many seals of the SAME plaintext under the SAME key, which is the case a counter, a fixed
// nonce, or a nonce derived from the row would all fail.
func TestSealNeverRepeatsANonce(t *testing.T) {
	t.Parallel()
	s := sealerA(t)
	plaintext, err := vault.Encode(testCredential(t))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	const seals = 20000
	nonces := make(map[string]int, seals)
	for i := range seals {
		env := s.Seal(tenantA, providerKey, plaintext)
		// The nonce is the GCM output's first 12 bytes, which is what NonceSize would be if this
		// AEAD reported one: it is the overhead minus the 16 byte tag.
		nonce := string(env[vault.HeaderLen : vault.HeaderLen+vault.GCMOverhead-16])
		if first, dup := nonces[nonce]; dup {
			t.Fatalf("seal %d repeated the nonce of seal %d", i, first)
		}
		nonces[nonce] = i
	}
	if len(nonces) != seals {
		t.Fatalf("%d distinct nonces out of %d seals", len(nonces), seals)
	}
}

func TestSealerRefusesAKeyThatIsNotOne(t *testing.T) {
	t.Parallel()
	for name, key := range map[string][]byte{
		"none":           nil,
		"empty":          {},
		"one byte":       {1},
		"AES-128":        bytes.Repeat([]byte{7}, 16),
		"one byte short": bytes.Repeat([]byte{7}, vault.MasterKeyLen-1),
		"one byte long":  bytes.Repeat([]byte{7}, vault.MasterKeyLen+1),
		"all zeros":      make([]byte, vault.MasterKeyLen),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, err := vault.Sealer(key)
			if err == nil {
				t.Fatal("the key was accepted")
			}
			if !errors.Is(err, vault.ErrNoMasterKey) {
				t.Errorf("err = %v, want ErrNoMasterKey", err)
			}
			if s != nil {
				t.Error("a refusal also returned a sealer")
			}
			// The key is never in the message, whatever is wrong with it.
			if bytes.Contains([]byte(err.Error()), key) && len(key) > 0 {
				t.Errorf("the refusal echoes the key: %v", err)
			}
		})
	}
}

// TestDecodeCredentialIsStrict covers the values that decrypt and are still not a credential. A
// tampered row cannot produce one of these, because tampering fails the tag; these come from this
// program (a format change, a binary rolled back after a newer one wrote a row), and guessing
// which fields such a value holds is how a credential reaches a provider as the wrong thing.
func TestDecodeCredentialIsStrict(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"a later format":               fmt.Sprintf(`{"format":%d,"fields":{"token":%q}}`, vault.CredentialFormatV1+1, marker),
		"no format":                    fmt.Sprintf(`{"fields":{"token":%q}}`, marker),
		"a member nothing wrote":       fmt.Sprintf(`{"format":%d,"fields":{"token":%q},"extra":1}`, vault.CredentialFormatV1, marker),
		"no fields":                    fmt.Sprintf(`{"format":%d}`, vault.CredentialFormatV1),
		"empty fields":                 fmt.Sprintf(`{"format":%d,"fields":{}}`, vault.CredentialFormatV1),
		"an empty value":               fmt.Sprintf(`{"format":%d,"fields":{"token":""}}`, vault.CredentialFormatV1),
		"a field name that is not one": fmt.Sprintf(`{"format":%d,"fields":{"Token":%q}}`, vault.CredentialFormatV1, marker),
		"two documents":                fmt.Sprintf(`{"format":%d,"fields":{"token":%q}} {"format":1}`, vault.CredentialFormatV1, marker),
		"not JSON at all":              marker,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := vault.DecodeCredential([]byte(body))
			if err == nil {
				t.Fatalf("it was accepted and returned %v", got)
			}
			if !errors.Is(err, vault.ErrDecrypt) {
				t.Errorf("err = %v, want ErrDecrypt", err)
			}
			if !got.IsZero() {
				t.Errorf("a refusal also returned a credential: %v", got)
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("the refusal carries the credential: %v", err)
			}
		})
	}
}

func TestEncodeRefusesTheZeroCredential(t *testing.T) {
	t.Parallel()
	// Storing nothing under a tenant's name would answer "connected" for a tenant that is not,
	// and hand out an empty token at the next fetch.
	var zero vault.Credential
	got, err := vault.Encode(zero)
	if err == nil {
		t.Fatalf("the zero credential encoded to %q", got)
	}
	if !errors.Is(err, vault.ErrInvalidCredential) {
		t.Errorf("err = %v, want ErrInvalidCredential", err)
	}
}
