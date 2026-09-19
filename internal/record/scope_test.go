package record

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestScopeID(t *testing.T) {
	tests := []struct {
		provider, kind, id string
		want               string
	}{
		{"slack", "channel", "C0GENERAL", "slack:channel:C0GENERAL"},
		{"slack", "dm", "D024BE91L", "slack:dm:D024BE91L"},
		{"clickup", "list", "901100", "clickup:list:901100"},
		{"hubspot", "portal", "62515", "hubspot:portal:62515"},
		{"outlook", "mailbox", "ben@example.com", "outlook:mailbox:ben%40example.com"},
		// A Microsoft Graph channel id: a colon and an at sign.
		{"teams", "channel", "19:abc123@thread.tacv2", "teams:channel:19%3Aabc123%40thread.tacv2"},
		// A Graph id in base64: slash, plus, equals. Hyphen and underscore stay.
		{"outlook", "mailbox", "AAMkAGI2/x+y-z_w==", "outlook:mailbox:AAMkAGI2%2Fx%2By-z_w%3D%3D"},
		// Every unreserved character stays, the percent sign and the space do not.
		{"p", "k", "Az09._~-", "p:k:Az09._~-"},
		{"p", "k", "100% sure", "p:k:100%25%20sure"},
		// Case is the provider's and is kept.
		{"slack", "channel", "c0general", "slack:channel:c0general"},
		// UTF-8 goes byte by byte.
		{"p", "k", "é☕", "p:k:%C3%A9%E2%98%95"},
		// Bytes that are not UTF-8 are bytes like any other: an id is opaque.
		{"p", "k", "\xff\x80", "p:k:%FF%80"},
		{"p2_x", "k_2", "1", "p2_x:k_2:1"},
	}
	for _, tt := range tests {
		got, err := ScopeID(tt.provider, tt.kind, tt.id)
		if err != nil {
			t.Errorf("ScopeID(%q, %q, %q): %v", tt.provider, tt.kind, tt.id, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ScopeID(%q, %q, %q) = %q, want %q", tt.provider, tt.kind, tt.id, got, tt.want)
		}
		if n := strings.Count(got, ":"); n != 2 {
			t.Errorf("%q has %d colons, want exactly 2", got, n)
		}
		back, err := ParseScopeID(got)
		if err != nil {
			t.Errorf("ParseScopeID(%q): %v", got, err)
			continue
		}
		if back != (Scope{Provider: tt.provider, ContainerKind: tt.kind, ContainerID: tt.id}) {
			t.Errorf("ParseScopeID(%q) = %+v", got, back)
		}
	}
}

func TestScopeIDRefusesBadParts(t *testing.T) {
	tests := []struct{ name, provider, kind, id string }{
		{"no provider", "", "channel", "C1"},
		{"a capital in the provider", "Slack", "channel", "C1"},
		{"a colon in the provider", "sla:ck", "channel", "C1"},
		{"a hyphen in the provider", "ms-teams", "channel", "C1"},
		{"a provider starting with a digit", "1up", "channel", "C1"},
		{"a provider of 33 characters", "p" + strings.Repeat("a", 32), "channel", "C1"},
		{"no kind", "slack", "", "C1"},
		{"a capital in the kind", "slack", "Channel", "C1"},
		{"a colon in the kind", "slack", "chan:nel", "C1"},
		{"a kind of 33 characters", "slack", "k" + strings.Repeat("a", 32), "C1"},
		{"no id", "slack", "channel", ""},
		{"a NUL in the id", "slack", "channel", "C1\x00"},
		{"a newline in the id", "slack", "channel", "C1\nslack:channel:C2"},
		{"the 0x1F separator in the id", "slack", "channel", "C\x1f1"},
		{"DEL in the id", "slack", "channel", "C1\x7f"},
		{"an id that is too long", "slack", "channel", strings.Repeat("a", longest+1)},
		{"an id that is too long once escaped", "slack", "channel", strings.Repeat("=", longest/3+1)},
	}
	for _, tt := range tests {
		got, err := ScopeID(tt.provider, tt.kind, tt.id)
		if !errors.Is(err, ErrBadScopeID) {
			t.Errorf("%s: err = %v, want ErrBadScopeID", tt.name, err)
		}
		if got != "" {
			t.Errorf("%s: returned %q alongside the error", tt.name, got)
		}
		if err != nil && tt.id != "" && strings.Contains(err.Error(), tt.id) {
			t.Errorf("%s: the error quotes the id", tt.name)
		}
	}

	// The longest ids that fit, plain and escaped.
	if _, err := ScopeID("slack", "channel", strings.Repeat("a", longest)); err != nil {
		t.Errorf("an id that makes exactly MaxScopeID bytes: %v", err)
	}
	if _, err := ScopeID("slack", "channel", strings.Repeat("=", longest/3)); err != nil {
		t.Errorf("an escaped id that fits: %v", err)
	}
}

type namedScope struct{ name, id string }

// badScopeIDs is every way a string can fail to be a scope id. The agreement test runs the same
// list through the schema, so the pattern there and the parser here cannot drift apart.
func badScopeIDs() []namedScope {
	return []namedScope{
		{"empty", ""},
		{"one segment", "slack"},
		{"two segments", "slack:channel"},
		{"no container id", "slack:channel:"},
		{"no container kind", "slack::C1"},
		{"no provider", ":channel:C1"},
		{"a capital in the provider", "Slack:channel:C1"},
		{"a capital in the kind", "slack:Channel:C1"},
		{"a hyphen in the provider", "ms-teams:channel:C1"},
		{"a provider of 33 characters", "p" + strings.Repeat("a", 32) + ":channel:C1"},
		{"a kind of 33 characters", "slack:k" + strings.Repeat("a", 32) + ":C1"},
		{"a third colon", "teams:channel:19:abc"},
		{"a trailing colon", "slack:channel:C1:"},
		{"a space", "slack:channel:C 1"},
		{"a slash", "slack:channel:a/b"},
		{"an at sign", "outlook:mailbox:ben@example.com"},
		{"a plus", "slack:channel:a+b"},
		{"an equals sign", "slack:channel:ab=="},
		{"raw UTF-8", "slack:channel:é"},
		{"a lowercase escape", "slack:channel:a%3ab"},
		{"a mixed case escape", "slack:channel:a%3aB%3A"},
		{"an escaped letter", "slack:channel:%41"},
		{"an escaped digit", "slack:channel:%30"},
		{"an escaped hyphen", "slack:channel:a%2Db"},
		{"an escaped dot", "slack:channel:a%2Eb"},
		{"an escaped underscore", "slack:channel:a%5Fb"},
		{"an escaped tilde", "slack:channel:a%7Eb"},
		{"an escaped NUL", "slack:channel:a%00"},
		{"an escaped newline", "slack:channel:a%0Ab"},
		{"an escaped 0x1F", "slack:channel:a%1Fb"},
		{"an escaped DEL", "slack:channel:a%7F"},
		{"a bare percent", "slack:channel:100%"},
		{"half an escape", "slack:channel:a%4"},
		{"an escape that is not hex", "slack:channel:a%ZZ"},
		{"a percent before another escape", "slack:channel:%%41"},
		{"a raw newline at the end", "slack:channel:C1\n"},
		{"a raw NUL", "slack:channel:C1\x00"},
		{"a raw tab", "slack:channel:C\t1"},
		{"one byte too long", scope3 + strings.Repeat("a", longest+1)},
		{"leading space", " slack:channel:C1"},
	}
}

func TestParseScopeIDRefusesWhatScopeIDNeverWrites(t *testing.T) {
	for _, tt := range badScopeIDs() {
		got, err := ParseScopeID(tt.id)
		if !errors.Is(err, ErrBadScopeID) {
			t.Errorf("%s: err = %v, want ErrBadScopeID", tt.name, err)
		}
		if got != (Scope{}) {
			t.Errorf("%s: returned %+v alongside the error", tt.name, got)
		}
	}
}

// One scope, one spelling: whatever parses is what ScopeID writes for the parts it parsed into,
// and whatever ScopeID writes parses back into its parts. Ids are arbitrary bytes.
func TestScopeIDRoundTripsAndIsCanonical(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 2026))
	refused := 0
	for range 20000 {
		id := make([]byte, 1+rng.IntN(40))
		for i := range id {
			id[i] = byte(rng.IntN(256))
		}
		s, err := ScopeID("teams", "chat", string(id))
		if err != nil {
			if !strings.ContainsFunc(string(id), func(r rune) bool { return r < 0x20 || r == 0x7F }) {
				t.Fatalf("ScopeID refused %q, which has no control character: %v", id, err)
			}
			refused++
			continue
		}
		back, err := ParseScopeID(s)
		if err != nil {
			t.Fatalf("ParseScopeID(%q): %v", s, err)
		}
		if back.ContainerID != string(id) || back.Provider != "teams" || back.ContainerKind != "chat" {
			t.Fatalf("%q came back as %+v", id, back)
		}
		again, err := ScopeID(back.Provider, back.ContainerKind, back.ContainerID)
		if err != nil || again != s {
			t.Fatalf("%q was rebuilt as %q (%v)", s, again, err)
		}
	}
	if refused == 0 || refused == 20000 {
		t.Fatalf("%d of 20000 random ids were refused, the generator is not exercising both paths", refused)
	}

	// Two different container ids never share a scope id, however they are spelled.
	pairs := [][2]string{{"a:b", "a%3Ab"}, {"A", "a"}, {"a b", "a%20b"}, {"a", "a "}}
	for _, p := range pairs {
		x, err1 := ScopeID("p", "k", p[0])
		y, err2 := ScopeID("p", "k", p[1])
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		if x == y {
			t.Errorf("%q and %q share the scope id %q", p[0], p[1], x)
		}
	}
}

func FuzzParseScopeID(f *testing.F) {
	for _, s := range badScopeIDs() {
		f.Add(s.id)
	}
	f.Add("teams:channel:19%3Aabc123%40thread.tacv2")
	f.Add("slack:channel:C0GENERAL")
	f.Fuzz(func(t *testing.T, s string) {
		scope, err := ParseScopeID(s)
		if err != nil {
			return
		}
		again, err := ScopeID(scope.Provider, scope.ContainerKind, scope.ContainerID)
		if err != nil || again != s {
			t.Fatalf("%q parsed into %+v, which builds %q (%v)", s, scope, again, err)
		}
	})
}
