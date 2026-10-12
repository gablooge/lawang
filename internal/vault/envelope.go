package vault

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/gablooge/lawang/internal/tenancy"
)

// The envelope the local vault stores, and the only shape that table ever holds:
//
//	byte 0        the format, 1
//	bytes 1..8    the id of the key it was sealed under (keyIDLen bytes)
//	bytes 9..     what AES-256-GCM produced: a 12 byte random nonce, the ciphertext, a 16 byte tag
//
// The format byte comes first so that a value which is NOT an envelope is refused as such rather
// than as a decryption failure: a JSON credential written here by hand, by a restore from an older
// deployment, or by a future version that forgot to seal starts with "{" and is turned away by
// name (ErrNotSealed). Migration 00007 refuses the same shape on the way in, with the same two
// numbers, so the rule holds for what is written and for what is read.
//
// The key id is NOT a key. It is eight bytes of a SHA-256 over a label and the key, so it tells
// two keys apart without saying anything about either, and it is what makes "this row was sealed
// under a key this process does not hold" a refusal of its own instead of a bare authentication
// failure (see Local.open, and ADR 16 on what rotation would need).
const (
	formatV1  = 1
	keyIDLen  = 8
	headerLen = 1 + keyIDLen

	// gcmOverhead is what cipher.NewGCMWithRandomNonce adds to a plaintext: a 12 byte nonce it
	// generates itself and prepends, and GCM's own 16 byte authentication tag. It is written down
	// because migration 00007's CHECK carries headerLen + gcmOverhead as a number, and
	// TestTheShortestEnvelopeIsTheNumberTheMigrationCarries holds the two together.
	gcmOverhead = 28

	// minEnvelopeLen is the shortest envelope that can exist: the header, and GCM's overhead over
	// an empty plaintext. Nothing this package seals is empty (a credential is JSON), so this is
	// the floor of "could be an envelope" and not of "is one".
	minEnvelopeLen = headerLen + gcmOverhead

	// maxEnvelopeLen bounds what is read back out of the row before anything is done with it. The
	// column is bounded by nothing but Postgres, and a credential of sixteen 8 KiB fields seals to
	// a little over 128 KiB, so a megabyte is generous and still a bound.
	maxEnvelopeLen = 1 << 20
)

// keyIDLabel domain-separates the key id from any other hash of the key. It ends in 0x1F, the
// separator internal/ids uses, so the label cannot run into the key.
const keyIDLabel = "lawang vault key id v1\x1f"

// keyIDOf is the id of a master key: the first keyIDLen bytes of SHA-256 over the label and the
// key. The key is written into the hash and never copied into a buffer of this function's own.
func keyIDOf(key []byte) [keyIDLen]byte {
	h := sha256.New()
	_, _ = io.WriteString(h, keyIDLabel)
	_, _ = h.Write(key)
	var id [keyIDLen]byte
	copy(id[:], h.Sum(nil))
	return id
}

// aadLabel domain-separates this program's additional authenticated data from anything else that
// might one day be sealed with the same key.
const aadLabel = "lawang.vault.v1"

// aad is what the envelope is authenticated with besides its own bytes: the label, the tenant, the
// provider key, and the envelope's own header.
//
// Binding the tenant and the provider is the point. Without it, the key alone would be enough to
// move a credential between tenants by editing one row, and a row copied from one tenant to
// another would decrypt and be handed out as that tenant's token. With it, such a row fails to
// decrypt at all.
//
// The parts are joined with 0x1F and the encoding is unambiguous because of what the parts can
// be: checkArgs has already held the tenant to ^[A-Za-z0-9_-]{1,64}$ and the provider key to ADR
// 3's grammar, so neither can contain the separator and no two different pairs can produce the
// same bytes.
//
// The header is appended as read, and that part changes nothing today, deliberately and
// knowingly. A rewritten key id is refused by open before the tag is checked, and a rewritten
// format byte before that, so no input this package can be given tells the two apart: the
// mutation run of item B13 took the header out of this function and every test still passed. It
// is kept because the key id is only a reliable name for a key while it is authenticated, and the
// first reader that accepts more than one key (a rotation, ADR 16) has to be able to trust it.
// A reader should not conclude from this comment that a ciphertext sealed under another key and
// relabelled is accepted: it is refused, by the key id, which
// TestACiphertextValidUnderAnotherKeyIsRefused shows.
func aad(t tenancy.ID, providerKey string, header []byte) []byte {
	b := make([]byte, 0, len(aadLabel)+len(t)+len(providerKey)+len(header)+3)
	b = append(b, aadLabel...)
	b = append(b, 0x1F)
	b = append(b, t...)
	b = append(b, 0x1F)
	b = append(b, providerKey...)
	b = append(b, 0x1F)
	b = append(b, header...)
	return b
}

// The refusals the sealed form has, which are the local vault's and no other backend's.
var (
	// ErrNotSealed reports a stored value that is not a sealed envelope: too short to be one, or
	// carrying a format byte that is not this format's. A plaintext credential is exactly that
	// value, and this is the refusal that makes "the local vault never hands out something it did
	// not seal" true of what is READ, next to migration 00007's CHECK which makes it true of what
	// is written.
	ErrNotSealed = errors.New("vault: the stored value is not a sealed credential")

	// ErrWrongKey reports a row sealed under a key this process does not hold, by the key id in
	// its header. It is separate from ErrDecrypt because the operator's answer is a different
	// one: the credential is intact and LAWANG_VAULT_KEY is not the key it was stored under, so
	// restoring the right key brings it back, while a tampered row never comes back.
	ErrWrongKey = errors.New("vault: the stored credential was sealed under a different key")

	// ErrDecrypt reports an envelope that did not authenticate: a flipped bit anywhere in the
	// nonce, the ciphertext or the tag, a row moved to another tenant or another provider, or a
	// header made to look local. AES-GCM cannot tell those apart and neither does this error.
	ErrDecrypt = errors.New("vault: the stored credential did not decrypt")
)

// seal returns the envelope for plaintext under this vault's key, bound to the tenant and the
// provider.
//
// The nonce is not this function's to choose, on purpose. cipher.NewGCMWithRandomNonce draws one
// from crypto/rand per Seal and prepends it, which is the standard library's hardened answer to
// the one catastrophic mistake AES-GCM has: a nonce used twice under one key reveals the XOR of
// two plaintexts and the authentication subkey. Nothing here counts, and nothing here derives a
// nonce from the row, so no restore from a backup and no second writer can repeat one; the
// 96 bit random nonce's own collision bound (about 2^-33 after 2^32 seals under one key) is far
// beyond a deployment that writes one envelope per connected provider per tenant.
// TestSealNeverRepeatsANonce holds the property, and crypto/rand cannot fail without panicking
// (Go 1.24 and later), so there is no failure path here to get wrong either.
func (s *sealer) seal(t tenancy.ID, providerKey string, plaintext []byte) []byte {
	header := []byte{formatV1}
	header = append(header, s.keyID[:]...)
	// The header is Seal's destination, so the envelope is the header followed by what GCM
	// produced, in one allocation, and the header is also in the authenticated data.
	env := make([]byte, 0, headerLen+len(plaintext)+gcmOverhead)
	env = append(env, header...)
	return s.aead.Seal(env, nil, plaintext, aad(t, providerKey, header))
}

// open returns the plaintext of an envelope, or says which of the three ways it is not one.
//
// The order of the checks is the order of the answers an operator needs: this is not an envelope
// at all, this is somebody else's key, this does not authenticate. The last one is the only
// branch that depends on the key material, and it is reached for a row whose header is this
// vault's.
func (s *sealer) open(t tenancy.ID, providerKey string, env []byte) ([]byte, error) {
	switch {
	case len(env) > maxEnvelopeLen:
		return nil, fmt.Errorf("%w: it is longer than %d bytes", ErrNotSealed, maxEnvelopeLen)
	case len(env) < minEnvelopeLen:
		return nil, fmt.Errorf("%w: it is shorter than the %d bytes an envelope takes", ErrNotSealed, minEnvelopeLen)
	case env[0] != formatV1:
		// A plaintext credential arrives here: JSON starts with "{" (0x7B). The message says the
		// format and not the byte, because the byte is the first byte of whatever was stored.
		return nil, fmt.Errorf("%w: its first byte is not the format of a sealed credential", ErrNotSealed)
	}
	header := env[:headerLen]
	if !bytes.Equal(header[1:], s.keyID[:]) {
		return nil, ErrWrongKey
	}
	// Nothing from the AEAD's error is kept. "cipher: message authentication failed" carries no
	// value, but it also says nothing this sentinel does not, and an error that reaches a log line
	// is better one this package wrote.
	plaintext, err := s.aead.Open(nil, nil, env[headerLen:], aad(t, providerKey, header))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// credentialFormatV1 is the version of the JSON inside an envelope. It is inside the sealed bytes
// and not in the header, so it is authenticated, and a reader that does not know a later format
// refuses rather than guessing which fields it is looking at.
const credentialFormatV1 = 1

// wireCredential is what a credential looks like inside an envelope. It exists because
// Credential.MarshalJSON refuses to serialize a credential anywhere, deliberately, so the one
// encoder that writes one writes this instead and seals the result before it returns.
type wireCredential struct {
	Format int               `json:"format"`
	Fields map[string]string `json:"fields"`
}

// encode is the plaintext of an envelope. Its result is sealed by the caller and goes nowhere
// else: it is the credential in the clear, and the only bytes in this program that ever are.
func encode(c Credential) ([]byte, error) {
	if c.IsZero() {
		// Storing nothing under a tenant's name would answer "connected" for a tenant that is
		// not, and hand out an empty token later (see the Vault doc comment).
		return nil, fmt.Errorf("%w: a credential needs at least one field", ErrInvalidCredential)
	}
	b, err := json.Marshal(wireCredential{Format: credentialFormatV1, Fields: c.fields})
	if err != nil {
		// Unreachable for a credential NewCredential built: the fields are valid UTF-8 strings
		// with no NUL. It is still an error and not a panic, because encode is the function a
		// later field type would be added to.
		return nil, fmt.Errorf("%w: it cannot be encoded", ErrInvalidCredential)
	}
	return b, nil
}

// decodeCredential reads the plaintext of an envelope back.
//
// It is strict, and every rule here is about a value that decrypted and is still not a credential:
// a later format, a member nothing wrote, trailing bytes. Such a value cannot come from a
// tampered row, because tampering fails the authentication tag, so it comes from this program
// (a format change, a rollback to an older binary after a newer one wrote a row), and guessing
// which fields it holds is how a credential gets handed to the wrong provider. Everything that
// survives the shape checks goes through NewCredential, so a value that no longer satisfies the
// credential rules is a refusal on the way out as well as on the way in.
func decodeCredential(b []byte) (Credential, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var w wireCredential
	if err := dec.Decode(&w); err != nil {
		// The decoder's message quotes the JSON it could not read, which is the credential in the
		// clear, so none of it is kept.
		return Credential{}, fmt.Errorf("%w: it is not a stored credential", ErrDecrypt)
	}
	if dec.More() {
		return Credential{}, fmt.Errorf("%w: it carries more than one document", ErrDecrypt)
	}
	if w.Format != credentialFormatV1 {
		return Credential{}, fmt.Errorf(
			"%w: it is stored in format %d and this version reads format %d",
			ErrDecrypt, w.Format, credentialFormatV1)
	}
	c, err := NewCredential(w.Fields)
	if err != nil {
		// NewCredential's messages name a field and never a value, so this one may be wrapped.
		return Credential{}, fmt.Errorf("%w: %w", ErrDecrypt, err)
	}
	return c, nil
}
