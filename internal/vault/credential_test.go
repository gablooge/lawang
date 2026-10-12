package vault_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/gablooge/lawang/internal/vault"
)

// marker is the value every test in this package stores and then looks for in places it must
// never reach. It is a throwaway string of this file's own: no credential from
// ~/.config/lawang is ever read here, and nothing in this repository is a real token (CLAUDE.md).
const marker = "lawang-test-token-3f8a1c7d9b2e4a60"

// testCredential is one credential carrying the marker.
func testCredential(t *testing.T) vault.Credential {
	t.Helper()
	c, err := vault.NewCredential(map[string]string{"token": marker})
	if err != nil {
		t.Fatalf("NewCredential: %v", err)
	}
	return c
}

func TestNewCredentialRefusesWhatCouldNotBeACredential(t *testing.T) {
	t.Parallel()
	// Every refusal of NewCredential, and for each the mistake it stands for. The empty value is
	// the one that matters most: a vault that hands out "" as a token sends an unauthenticated
	// request to a provider and spends a retry ladder on it, which is why
	// internal/provider/clickup has a double called emptyToken.
	long := strings.Repeat("x", vault.MaxValueLen+1)
	many := make(map[string]string, vault.MaxFields+1)
	for i := range vault.MaxFields + 1 {
		many[fmt.Sprintf("f%d", i)] = "v"
	}
	for name, fields := range map[string]map[string]string{
		"nil":                          nil,
		"empty":                        {},
		"too many fields":              many,
		"an empty name":                {"": "v"},
		"an uppercase name":            {"Token": "v"},
		"a name starting with a digit": {"1token": "v"},
		"a name with a hyphen":         {"api-key": "v"},
		"a name with a space":          {"api key": "v"},
		"a name with a dot":            {"api.key": "v"},
		"a name that is too long":      {strings.Repeat("a", vault.MaxFieldNameLen+1): "v"},
		"an empty value":               {"token": ""},
		"a value that is too long":     {"token": long},
		"a value that is not UTF-8":    {"token": "\xff\xfe"},
		"a value with a NUL":           {"token": "abc\x00def"},
		"a value of one space":         {"token": " "},
		"a value of whitespace":        {"token": " \n\t "},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := vault.NewCredential(fields)
			if err == nil {
				t.Fatalf("NewCredential(%v) was accepted, and returned %v", fields, got)
			}
			if !errors.Is(err, vault.ErrInvalidCredential) {
				t.Errorf("err = %v, want ErrInvalidCredential", err)
			}
			if !got.IsZero() {
				t.Errorf("a refusal also returned a credential: %v", got)
			}
			// The field's value is never in the message. The name is, deliberately: it is this
			// program's word for what a credential holds, and an operator has to be told which
			// field is wrong.
			for _, value := range fields {
				// Only a value long enough to be a secret is worth the check: the short ones
				// above are a space, a letter and a few bytes of nonsense, and a word of a
				// message ("the value is whitespace") contains a space by construction.
				if len(value) >= 8 && strings.Contains(err.Error(), value) {
					t.Errorf("the refusal echoes the value: %v", err)
				}
			}
		})
	}
}

func TestNewCredentialAcceptsWhatACredentialHolds(t *testing.T) {
	t.Parallel()
	for name, fields := range map[string]map[string]string{
		"one token":                         {"token": marker},
		"client credentials":                {"client_id": "an-id", "client_secret": marker},
		"a name with digits":                {"token2": marker},
		"a value with newlines (a PEM key)": {"private_key": "-----BEGIN-----\nabc\n-----END-----\n"},
		"a value in another script":         {"token": "鍵" + marker},
		"the longest value":                 {"token": strings.Repeat("x", vault.MaxValueLen)},
		"the longest name":                  {strings.Repeat("a", vault.MaxFieldNameLen): marker},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, err := vault.NewCredential(fields)
			if err != nil {
				t.Fatalf("NewCredential: %v", err)
			}
			for want, value := range fields {
				got, err := c.Field(want)
				if err != nil {
					t.Fatalf("Field(%q): %v", want, err)
				}
				if got != value {
					t.Errorf("Field(%q) did not return what was stored", want)
				}
			}
		})
	}
}

func TestACredentialDoesNotShareItsFieldsWithACaller(t *testing.T) {
	t.Parallel()
	// NewCredential clones, so a caller that keeps the map it passed in, or edits the slice Names
	// returns, cannot change what the credential holds. A credential that aliased its caller's
	// map would be a credential whose value changes under a vault that already sealed it.
	fields := map[string]string{"token": marker}
	c, err := vault.NewCredential(fields)
	if err != nil {
		t.Fatalf("NewCredential: %v", err)
	}
	fields["token"] = "something-else"
	delete(fields, "token")
	fields["extra"] = "x"

	got, err := c.Field("token")
	if err != nil {
		t.Fatalf("Field: %v", err)
	}
	if got != marker {
		t.Errorf("editing the caller's map changed the credential: Field returned a different value")
	}
	if _, err := c.Field("extra"); !errors.Is(err, vault.ErrNoField) {
		t.Errorf("a field added to the caller's map reached the credential: %v", err)
	}

	names := c.Names()
	names[0] = "edited"
	if got := c.Names(); !slices.Equal(got, []string{"token"}) {
		t.Errorf("editing the result of Names changed the credential: Names = %v", got)
	}
}

func TestFieldNamesAFieldAndNeverReturnsAnEmptyValue(t *testing.T) {
	t.Parallel()
	c := testCredential(t)
	// A missing field is an error and not ("", false) that a caller can ignore. The name is in
	// the message, because a name is not secret and a caller debugging a wiring mistake needs it.
	got, err := c.Field("access_token")
	if !errors.Is(err, vault.ErrNoField) {
		t.Fatalf("Field of a missing field: err = %v, want ErrNoField", err)
	}
	if got != "" {
		t.Errorf("Field of a missing field returned %q", got)
	}
	if !strings.Contains(err.Error(), "access_token") {
		t.Errorf("the refusal does not name the field: %v", err)
	}
	var zero vault.Credential
	if _, err := zero.Field("token"); !errors.Is(err, vault.ErrNoField) {
		t.Errorf("the zero credential answered something for a field: %v", err)
	}
}

// TestACredentialNeverPrintsAValue is the promise architecture section 7 makes about a credential:
// it never reaches a log line, an error or a record. Every way Go prints a value is covered,
// including %#v, which prints unexported fields and is how the map would otherwise come out, and
// a credential inside something else, which is how one reaches a log in practice.
func TestACredentialNeverPrintsAValue(t *testing.T) {
	t.Parallel()
	c := testCredential(t)
	type holder struct {
		Where string
		Cred  vault.Credential
	}
	printed := []string{
		fmt.Sprintf("%v", c),
		fmt.Sprintf("%+v", c),
		fmt.Sprintf("%s", c), //nolint:staticcheck // S1025: what %s does with this type is the test
		fmt.Sprintf("%#v", c),
		fmt.Sprint(c),
		fmt.Sprintf("%v", holder{Where: "a struct", Cred: c}),
		fmt.Sprintf("%+v", holder{Where: "a struct", Cred: c}),
		fmt.Sprintf("%#v", holder{Where: "a struct", Cred: c}),
		fmt.Sprintf("%v", []vault.Credential{c}),
		fmt.Sprintf("%v", map[string]vault.Credential{"clickup": c}),
		fmt.Sprintf("%v", &c),
		fmt.Errorf("a wrapped credential: %v", c).Error(),
	}
	for _, out := range printed {
		if strings.Contains(out, marker) {
			t.Errorf("a credential printed its value: %s", out)
		}
		if !strings.Contains(out, "token") {
			t.Errorf("a credential did not print the field name, which is what makes it readable: %s", out)
		}
	}

	for name, newHandler := range map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	} {
		var buf bytes.Buffer
		slog.New(newHandler(&buf)).Info("vault", "credential", c, "pointer", &c,
			"in a group", slog.GroupValue(slog.Any("c", c)))
		if strings.Contains(buf.String(), marker) {
			t.Errorf("the %s log holds the value: %s", name, buf.String())
		}
		if !strings.Contains(buf.String(), "token") {
			t.Errorf("the %s log does not name the field: %s", name, buf.String())
		}
	}
}

// TestACredentialCannotBeSerialized pins the other half of the same promise. A credential that
// could be marshalled would reach an HTTP body, a record or a JSON log the first time one was
// put in a struct that is marshalled, and nothing in the type would have stopped it.
func TestACredentialCannotBeSerialized(t *testing.T) {
	t.Parallel()
	c := testCredential(t)
	type holder struct {
		Cred vault.Credential `json:"cred"`
	}
	for name, value := range map[string]any{
		"the credential":   c,
		"a pointer to one": &c,
		"inside a struct":  holder{Cred: c},
		"in a slice":       []vault.Credential{c},
		"in a map":         map[string]vault.Credential{"clickup": c},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := json.Marshal(value)
			if err == nil {
				t.Fatalf("json.Marshal(%s) was accepted: %s", name, out)
			}
			if strings.Contains(string(out), marker) || strings.Contains(err.Error(), marker) {
				t.Fatalf("the refusal carries the value: %s / %v", out, err)
			}
		})
	}
	// The zero credential is refused too. It carries nothing, so marshalling it would be
	// harmless, and allowing it would make the guard depend on what a credential holds: a struct
	// with an empty credential in it would marshal in a test and fail in production.
	if out, err := json.Marshal(vault.Credential{}); err == nil {
		t.Errorf("json.Marshal of the zero credential was accepted: %s", out)
	}
}
