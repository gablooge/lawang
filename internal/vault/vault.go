// Package vault holds provider credentials, and is the seam a provider's API token is fetched
// through (docs/architecture.md section 7).
//
// Three implementations are built, and a deployment uses one:
//
//   - [Local], the dependency-free one: credentials sealed with AES-256-GCM under a master key
//     from the environment, in this deployment's own Postgres. It is what
//     [ADR 16](../../docs/adr/0016-vault-key-source.md) settles, and it refuses to work without a
//     key rather than storing anything an operator could read out of a backup.
//   - [Nango], for a deployment that already runs a self-hosted Nango: it holds nothing locally
//     and asks Nango over HTTP.
//   - [AzureApp], for Microsoft Graph: the credential is an app registration's client
//     credentials, which live in configuration, and what the vault hands out is an access token
//     it fetched with them.
//
// # What a credential is, and where it may go
//
// A [Credential] is a set of named secret fields: the API token, client secret or access token a
// provider authenticates with. It is secret material, so it never reaches a log line, an error, a
// plain table or a record (architecture section 7). This package keeps that promise in its types
// rather than in its callers' discipline:
//
//   - [Credential.String], [Credential.GoString] and [Credential.LogValue] print the field names
//     and never a value, so a credential in a %v, a %+v, a %#v or a slog argument is redacted.
//   - [Credential.MarshalJSON] returns an error. A credential cannot be serialized into an HTTP
//     body, a record or a JSON log by accident; the one encoder that writes one is this package's
//     own, and what it writes goes straight into AES-GCM.
//
// A Credential is also what a [github.com/gablooge/lawang/internal/provider.Credential] carries:
// that type is deliberately `any` (architecture section 7), and this is the concrete type the
// vault hands a Registrar. A provider package that needs one string out of it is better served by
// a narrow seam of its own, the way internal/provider/clickup asks for a Tokens.
package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// Vault holds provider credentials, keyed by tenant and provider. Raw tokens never reach a plain
// table (architecture section 7).
//
// Every method fails closed. A tenant that is not one, a provider key that is not one, a
// credential with no fields, a backend that cannot be reached and a stored value that cannot be
// decrypted are all refusals, and none of them is ever an empty credential with a nil error: a
// caller that is handed "" as a token sends an unauthenticated request to a provider and spends a
// retry ladder finding out (internal/provider/clickup's emptyToken is the double written for that
// mistake).
type Vault interface {
	// Store keeps one tenant's credential for one provider, replacing the one already stored.
	// A vault whose credentials live somewhere else refuses with [ErrUnsupported].
	Store(ctx context.Context, t tenancy.ID, provider string, c Credential) error
	// Fetch hands out one tenant's credential for one provider, or [ErrNotFound] when there is
	// none. It never returns a usable-looking credential together with an error.
	Fetch(ctx context.Context, t tenancy.ID, provider string) (Credential, error)
	// Revoke removes one tenant's credential for one provider. Removing one that is not there is
	// [ErrNotFound] and not a silent success: "done" for a credential that is still somewhere is
	// the answer an operator acts on when a token has leaked.
	Revoke(ctx context.Context, t tenancy.ID, provider string) error
}

// The refusals every implementation shares. They are sentinels because a caller acts on them
// differently: a tenant that is not connected is an operator's job, an unreachable backend is a
// retry (architecture section 11, "Vault unreachable: fail closed"), and a credential that cannot
// be decrypted is neither and must never be retried into a loop.
var (
	// ErrNotFound reports a tenant and provider nothing is stored for. It is a refusal and never
	// an empty credential.
	ErrNotFound = errors.New("vault: nothing is stored for this tenant and provider")

	// ErrInvalidCredential reports a credential that could never be used: no fields, a field name
	// that is not one, or a value that is empty or unstorable. Its message names the field and
	// never its value.
	ErrInvalidCredential = errors.New("vault: invalid credential")

	// ErrInvalidArgument reports a tenant or a provider key that is not one. It is separate from
	// ErrInvalidCredential because the fix is a different one: the caller addressed the wrong
	// thing rather than passed the wrong secret.
	ErrInvalidArgument = errors.New("vault: invalid argument")

	// ErrNoField reports a field the credential does not carry. The message names the field,
	// which is not secret: it is "token" or "client_secret", chosen by this program.
	ErrNoField = errors.New("vault: the credential has no such field")

	// ErrUnsupported reports an operation a backend cannot perform, such as storing a credential
	// in a vault whose credentials live in Nango or in an app registration. It is not a
	// not-implemented note: these backends own nothing to write to, and answering "stored" would
	// tell an operator a credential is somewhere it is not.
	ErrUnsupported = errors.New("vault: this vault cannot do that")

	// ErrUnavailable reports a backend that could not be reached or did not answer usefully. It
	// is the retryable one (architecture section 11): nothing is delivered unverified, the row
	// goes back on the ladder, and the credential is asked for again later.
	ErrUnavailable = errors.New("vault: the vault backend did not answer")
)

// maxFields, maxFieldNameLen and maxValueLen bound a credential. A credential is one token, or a
// client id and a client secret, or a key and a certificate: sixteen fields is far more than any
// provider needs, and 8 KiB a value takes a PEM-encoded private key. The bounds exist because
// what is sealed here is written to a row, and a value with no bound is a row with no bound.
const (
	maxFields       = 16
	maxFieldNameLen = 32
	maxValueLen     = 8 << 10
)

// Credential is one tenant's credential for one provider: named secret fields, which is what the
// three backends have in common (a ClickUp API token, a Slack bot token, Microsoft Graph client
// credentials, whatever Nango holds for a connection).
//
// Build one with [NewCredential]. The zero Credential is not a credential: it carries nothing,
// [Credential.IsZero] reports it, and storing it is a refusal.
//
// It is a value, and copying one copies the credential. The map inside it is never shared with a
// caller: NewCredential clones what it is given, and [Credential.Names] returns a new slice.
type Credential struct {
	// fields is unexported, so a credential cannot be built by a struct literal somewhere that
	// skipped the validation, and so that the only ways to get a value out are Field, which
	// names the field it wants, and this package's own encoder.
	fields map[string]string
}

// NewCredential returns a credential carrying fields, or a refusal naming what is wrong with
// them. No message carries a value.
//
// A field name is lowercase ASCII: a letter, then letters, digits and underscores, at most 32
// characters. That is the shape of a provider key (ADR 3) on purpose, so that one spelling rule
// covers the identifiers this program writes down, but it is a rule of its own and not that
// function: a field name is not a provider key, and nothing joins the two.
//
// An empty value is refused rather than stored. A stored empty value comes back out as a token
// that authenticates nothing, which is a request to a provider that is answered 401 and a retry
// ladder spent on a credential that was never there. So is a value with whitespace at either end,
// which is the same mistake one character short of showing itself: see [validValue].
func NewCredential(fields map[string]string) (Credential, error) {
	if len(fields) == 0 {
		return Credential{}, fmt.Errorf("%w: a credential needs at least one field", ErrInvalidCredential)
	}
	if len(fields) > maxFields {
		return Credential{}, fmt.Errorf("%w: at most %d fields", ErrInvalidCredential, maxFields)
	}
	for name, value := range fields {
		if !validFieldName(name) {
			// The name is in the message and the value never is. A name is chosen by this
			// program, from the list in each backend's documentation, and an operator who
			// mistyped one has to be told which.
			return Credential{}, fmt.Errorf(
				"%w: %q is not a field name (lowercase letter, then lowercase letters, digits and underscores, at most %d)",
				ErrInvalidCredential, name, maxFieldNameLen)
		}
		if err := validValue(value); err != nil {
			return Credential{}, fmt.Errorf("%w: field %q: %w", ErrInvalidCredential, name, err)
		}
	}
	return Credential{fields: maps.Clone(fields)}, nil
}

// validFieldName is the field-name grammar: ^[a-z][a-z0-9_]{0,31}$, checked byte by byte so that
// no regular expression is compiled on a path a request can reach.
func validFieldName(name string) bool {
	if name == "" || len(name) > maxFieldNameLen {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && (c >= '0' && c <= '9' || c == '_'):
		default:
			return false
		}
	}
	return true
}

// validValue holds a field's value to what can be stored and handed to a provider. Its errors
// name no value, because the value is the secret.
func validValue(value string) error {
	switch {
	case strings.TrimSpace(value) == "":
		// Empty, and whitespace, in one rule: a secret manager entry that holds only a newline, a
		// copied line with spaces on it, and an issuer answering " " for a token all arrive as a
		// credential that authenticates nothing, which is a request answered 401 and a retry
		// ladder spent on a credential that was never there. They are one case and not two
		// because TrimSpace covers the empty string as well, and a separate case for it was a
		// branch no input could reach on its own (the mutation run of item B13 found it
		// surviving).
		return errors.New("the value is empty or only whitespace, and such a credential authenticates nothing")
	case value != strings.TrimSpace(value):
		// Whitespace around a real value, refused for the reason config.ParseMasterKey gives for
		// refusing it in the key: a value read out of a file keeps the newline at the end of it,
		// and a value copied out of a console keeps the spaces around it. Such a token is not the
		// token, and it fails late and in the wrong words. net/http refuses an invalid header
		// value, so a token with a newline on it comes back from a provider's client as a
		// transport failure, which this package's own vocabulary would render as ErrUnavailable,
		// "could not be reached": an operator sent to look at the network for a credential they
		// pasted with a newline on it. The round trip through the vault makes it worse, because
		// the value is sealed by then and nobody can look at it.
		//
		// No v0.1 field needs the whitespace kept. The documented names are token, client_secret
		// and access_token, nango produces access_token, api_key, username and password, and the
		// one certificate-shaped credential (an app registration's client secret) is
		// configuration and never a Credential. A field whose value really does end in a newline,
		// a PEM-encoded key being the obvious one, is a field this rule has to be revisited for,
		// and the item that adds one is where that is decided: this earlier read "only an
		// ALL-whitespace value is refused: a PEM-encoded key ends in a newline", which exempted a
		// byte every real credential refuses for a field no credential can hold.
		return errors.New("the value has whitespace at the start or the end (a value read from a file keeps the newline at the end of it)")
	case len(value) > maxValueLen:
		return fmt.Errorf("the value is longer than %d bytes", maxValueLen)
	case !utf8.ValidString(value):
		return errors.New("the value is not valid UTF-8")
	case strings.ContainsRune(value, 0):
		// A NUL cannot be stored in a Postgres text column, and a credential carrying one is a
		// mis-decoded value rather than a secret. It is refused here, where the message can say
		// so, rather than by the driver on the way into the row.
		return errors.New("the value contains a NUL byte")
	}
	return nil
}

// Field returns one field's value, or [ErrNoField] naming the field.
//
// It does not return a bool. A caller that ignored one would hand an empty string to a provider
// as a token, which is the one failure mode a vault must not have: see the Vault doc comment.
func (c Credential) Field(name string) (string, error) {
	if v, ok := c.fields[name]; ok {
		return v, nil
	}
	return "", fmt.Errorf("%w: %q", ErrNoField, name)
}

// Names returns the field names, sorted, in a slice of the caller's own. Names are not secret:
// they are this program's words for what a credential holds, and they are what an error, a log
// line and the operator API may say.
func (c Credential) Names() []string { return slices.Sorted(maps.Keys(c.fields)) }

// IsZero reports a credential that carries nothing: the zero value, or one built from an empty
// map, which NewCredential refuses.
func (c Credential) IsZero() bool { return len(c.fields) == 0 }

// redacted is what a credential prints instead of a value, wherever it is printed.
const redacted = "[redacted]"

// String covers %v, %+v and %s. It prints the field names and no value, so a credential in a log
// line, an error or a test failure says what it holds and never what it is.
func (c Credential) String() string {
	if c.IsZero() {
		return "vault.Credential{}"
	}
	var b strings.Builder
	b.WriteString("vault.Credential{")
	for i, name := range c.Names() {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(name)
		b.WriteString(":")
		b.WriteString(redacted)
	}
	b.WriteString("}")
	return b.String()
}

// GoString covers %#v, which prints a struct's unexported fields and would otherwise print the
// map.
func (c Credential) GoString() string { return c.String() }

// LogValue is what slog logs: the field names, and the count, with no value anywhere.
func (c Credential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("fields", len(c.fields)),
		slog.String("names", strings.Join(c.Names(), ",")),
	)
}

// MarshalJSON always fails, on purpose, and that is the whole point of it.
//
// A credential must not be serializable by a caller: json.Marshal of a struct that happens to
// carry one, of a slice of them, or of a map with one in it would otherwise put token material
// into an HTTP body, a record or a JSON log line, and nothing in the type would have stopped it.
// Returning an error makes every one of those a failure at the point of the mistake.
//
// This package's own encoder does not go through here (see encode): what it writes is sealed
// before it leaves the function.
func (c Credential) MarshalJSON() ([]byte, error) {
	return nil, errors.New("vault: a credential is secret material and is not serializable; ask for the field you need with Credential.Field")
}

// Compile-time proof that a credential cannot be marshalled into anything by accident.
var _ json.Marshaler = Credential{}

// checkArgs holds a tenant and a provider key to what a vault entry can be addressed by. Every
// implementation calls it first, so "which tenant" and "which provider" are refused in one place
// and by one rule.
func checkArgs(t tenancy.ID, providerKey string) error {
	if _, err := tenancy.Parse(string(t)); err != nil {
		return fmt.Errorf("%w: tenant: %w", ErrInvalidArgument, err)
	}
	if t == tenancy.Sentinel {
		// The sentinel is a tenant id and not a tenant: no tenants row may carry it and no
		// operator credential is ever issued for it (tenancy.Sentinel), so a credential stored
		// under it could only come from a bug and would sit in the table unreachable. Parking a
		// delivery is the one write that belongs to it, and that is internal/outbox's.
		return fmt.Errorf("%w: tenant: the sentinel tenant %q owns parked deliveries and never a credential",
			ErrInvalidArgument, tenancy.Sentinel)
	}
	if !record.ValidProviderKey(providerKey) {
		// ADR 3's grammar, from the function that owns it and never from a copy of the pattern
		// (CLAUDE.md). The key is this program's own constant and not a value from a request, so
		// naming it in the message is safe, which is what provider.ErrBadKey does too.
		return fmt.Errorf("%w: %q is not a provider key", ErrInvalidArgument, providerKey)
	}
	return nil
}
