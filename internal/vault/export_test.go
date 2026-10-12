package vault

import "github.com/gablooge/lawang/internal/tenancy"

// Sealer is the cryptography without a database, for the tests of the envelope: the round trip,
// every way a stored value can fail to be one, and the nonce property. newSealer is what
// NewLocal calls, so these tests drive the production path and not a copy of it.
var Sealer = newSealer

// Seal and Open are the envelope functions, exported for the tests that build a value by hand and
// then take it apart: a tampered byte, a header rewritten to look local, a row of another tenant,
// and a plaintext credential.
func (s *sealer) Seal(t tenancy.ID, providerKey string, plaintext []byte) []byte {
	return s.seal(t, providerKey, plaintext)
}

// Open is open.
func (s *sealer) Open(t tenancy.ID, providerKey string, env []byte) ([]byte, error) {
	return s.open(t, providerKey, env)
}

// Overhead is what AES-GCM adds to a plaintext, read from the AEAD itself. The test of the
// shortest envelope compares it with the number migration 00007's CHECK carries, so the
// constraint and the code cannot drift.
func (s *sealer) Overhead() int { return s.aead.Overhead() }

// Encode and DecodeCredential are the plaintext of an envelope, exported for the tests that
// show what a strict reader refuses: a later format, a member nothing wrote, a second document.
var (
	Encode           = encode
	DecodeCredential = decodeCredential
)

// The envelope's own numbers, so that a test asserts against the constant and never against a
// number somebody wrote twice.
const (
	FormatV1           = formatV1
	KeyIDLen           = keyIDLen
	HeaderLen          = headerLen
	GCMOverhead        = gcmOverhead
	MinEnvelopeLen     = minEnvelopeLen
	MaxEnvelopeLen     = maxEnvelopeLen
	CredentialFormatV1 = credentialFormatV1
	MaxFields          = maxFields
	MaxFieldNameLen    = maxFieldNameLen
	MaxValueLen        = maxValueLen
	MaxNangoResponse   = maxNangoResponse
	SealedConstraint   = sealedConstraint
	AzureExpiryLeeway  = azureExpiryLeeway
	// MaxAzureTokenLifetime is the ceiling on what the authority may say expires_in is, so the
	// test that drives the boundary names the constant and not a number of its own.
	MaxAzureTokenLifetime = maxAzureTokenLifetime
)
