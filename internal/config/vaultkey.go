package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode"
)

// masterKeyLen is the length of the vault's master key in bytes: AES-256, so 32.
//
// It is vault.MasterKeyLen, written out here because this package reads the environment and
// depends on nothing else of this program, not even on the package that uses the key.
// TestTheVaultKeyLengthIsTheOneTheVaultRequires holds the two numbers together, from a test, so
// the duplication cannot drift without a failure.
const masterKeyLen = 32

// MasterKey is the vault's master key as it arrives from the environment: the one secret Lawang
// holds itself, rather than on behalf of a tenant (ADR 16, open decision 7).
//
// The key is an array and not a slice, so a Config stays comparable and no caller shares the
// backing bytes with it, and it is unexported, so the only ways to a key are ParseMasterKey, which
// validates, and Bytes, which copies. Every way of printing one is redacted: String covers %v,
// %+v and %s, GoString covers %#v, which would otherwise print the array, and LogValue covers
// slog. A key reaching a log line is the one mistake that turns an encrypted table back into a
// plain one, because the backup that carries the table also carries the log.
type MasterKey struct {
	key [masterKeyLen]byte
	// set tells the zero value (no key at all) from a key that happens to be all zeros, which
	// ParseMasterKey refuses and which therefore cannot be stored here anyway.
	set bool
	// generated marks a key this process made up for itself because it is running in development.
	// It is reported by LogValue and by String, because a developer has to be able to see that
	// what they stored under it is gone at the next restart.
	generated bool
}

// ParseMasterKey reads LAWANG_VAULT_KEY: 32 bytes in standard base64 with padding, which is 44
// characters and what "openssl rand -base64 32" prints.
//
// One spelling, on purpose. Hex was considered and refused: 64 hex characters are also 64 valid
// base64 characters, so a value could be read two ways and the wrong reading is a key nothing was
// encrypted under, found out at the first fetch. A hex key still gets a clear refusal, because it
// decodes as base64 to 48 bytes and the length rule names what the spelling is.
//
// No message ever carries the value. Load cannot know which variable a secret was pasted into
// (the package comment on Load), and this is the variable whose value is a key.
func ParseMasterKey(encoded string) (MasterKey, error) {
	if encoded == "" {
		return MasterKey{}, errors.New("it is empty")
	}
	if strings.ContainsFunc(encoded, unicode.IsSpace) {
		// A value read out of a file usually keeps the newline at the end of it. base64 decoding
		// skips newlines, so such a value would be accepted and the same key would have two
		// spellings; a space anywhere else is a copying mistake. Both are said out loud rather
		// than quietly tolerated.
		return MasterKey{}, errors.New("it contains whitespace (a value read from a file keeps the newline at the end of it)")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		// The decoder's error quotes the offending byte of the input, so none of it is kept.
		// Strict() also refuses a final character whose unused bits are not zero, which is a
		// value that would otherwise have had two spellings.
		return MasterKey{}, fmt.Errorf("it must be %d bytes in standard base64 with padding (%d characters, as printed by: openssl rand -base64 %d)",
			masterKeyLen, base64.StdEncoding.EncodedLen(masterKeyLen), masterKeyLen)
	}
	if len(raw) != masterKeyLen {
		// The length of what was decoded is in the message and the bytes are not. A length is
		// what tells an operator that they pasted a hex key, a 16 byte key or something else
		// entirely, and it says nothing about the key itself.
		return MasterKey{}, fmt.Errorf("it decodes to %d bytes and the key is %d bytes (a 64 character hex key decodes to 48)",
			len(raw), masterKeyLen)
	}
	var k MasterKey
	copy(k.key[:], raw)
	k.set = true
	return k, nil
}

// newGeneratedMasterKey is the development convenience: a key this process makes up for itself,
// so that a developer can run Lawang without having one.
//
// It is unreachable in production. Load calls it on exactly one branch, under cfg.Env ==
// Development, and cfg.Env is Development only when LAWANG_ENV is the string "development":
// anything else, including an unrecognized value, leaves it Production and adds an error
// (TestTheDevelopmentVaultKeyCannotBeReachedInProduction drives the spellings).
//
// What it costs is written into the variable's documentation: the key is gone when the process
// ends, so a credential stored under it cannot be read again after a restart. That is the right
// trade for a developer and the wrong one for a deployment, which is why the other branch is a
// refusal and not a second convenience.
func newGeneratedMasterKey() MasterKey {
	var k MasterKey
	// crypto/rand.Read fills the whole buffer and never returns an error (Go 1.24 and later it
	// panics if the operating system's source fails, rather than handing back short or
	// predictable bytes), so there is no failure path here to get wrong.
	_, _ = rand.Read(k.key[:])
	k.set = true
	k.generated = true
	return k
}

// IsZero reports a Config that carries no vault key: production refuses to be one (see Load), so
// this is either development with a key it generated, or a Config nobody loaded.
func (k MasterKey) IsZero() bool { return !k.set }

// Generated reports a key this process made up for itself in development, which is gone when the
// process ends.
func (k MasterKey) Generated() bool { return k.generated }

// Bytes returns a copy of the key, or nil when there is none. It is the one way out of this type:
// everything else about a MasterKey is redacted.
//
// Its caller is vault.NewLocal, which reads it once at start-up and keeps the AES key schedule
// rather than the key.
//
// The copy is explicitness rather than protection, and the mutation run of item B13 says so: the
// receiver is a value, so k is already a copy of the caller's MasterKey and k.key[:] could not
// alias the one in a Config either way. It is written out because that argument depends on the
// receiver staying a value, and a pointer receiver added later would turn the short spelling into
// a slice into the configuration.
func (k MasterKey) Bytes() []byte {
	if !k.set {
		return nil
	}
	out := make([]byte, masterKeyLen)
	copy(out, k.key[:])
	return out
}

// state is how a key is described wherever one is printed: whether there is one and where it came
// from, never what it is.
func (k MasterKey) state() string {
	switch {
	case !k.set:
		return "none"
	case k.generated:
		return "generated for development, and gone when this process ends"
	default:
		return "configured"
	}
}

// String covers %v, %+v and %s.
func (k MasterKey) String() string { return "[" + k.state() + "]" }

// GoString covers %#v, which prints a struct's unexported fields and would otherwise print the
// key itself, byte by byte.
func (k MasterKey) GoString() string { return "config.MasterKey{" + k.state() + "}" }

// LogValue is what slog logs.
func (k MasterKey) LogValue() slog.Value { return slog.StringValue(k.state()) }
