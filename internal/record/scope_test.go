package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
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
		{"a provider that starts with a digit", "1up:channel:C1"},
		{"a kind that starts with a digit", "slack:1list:C1"},
		{"a provider that starts with an underscore", "_up:channel:C1"},
		{"an escaped lowercase letter", "slack:channel:%61"},
		{"lowercase hex in an escaped UTF-8 byte", "slack:channel:%c3%a9"},
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

// keptAsIs is the unreserved set of ADR 3, spelled out here and not taken from scope.go, so that
// the tests below hold the rule and not the code's copy of it.
const keptAsIs = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._~-"

// mustBeEscaped is the escaping rule of ADR 3, stated once: a byte is carried as %XX exactly when
// it is neither kept as it is nor a control byte (which is never carried at all).
func mustBeEscaped(b byte) bool {
	return strings.IndexByte(keptAsIs, b) < 0 && b >= 0x20 && b != 0x7F
}

// scopeVerdicts runs one candidate scope id through everything that judges one: the Go parser,
// the schema, and the strict decoder. They must all say want.
func scopeVerdicts(t *testing.T, what, candidate string, want bool) {
	t.Helper()
	with, without := schemas(t)
	_, parseErr := ParseScopeID(candidate)
	if (parseErr == nil) != want {
		t.Errorf("%s: ParseScopeID accepted = %v, want %v", what, parseErr == nil, want)
	}
	doc := exampleDoc(t)
	set("visibility.scope", candidate)(doc)
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	for name, sch := range map[string]*jsonschema.Schema{"formats asserted": with, "formats not asserted": without} {
		if err := schemaError(t, sch, data); (err == nil) != want {
			t.Errorf("%s: the schema (%s) accepted = %v, want %v", what, name, err == nil, want)
		}
	}
	var r Record
	if err := json.Unmarshal(data, &r); (err == nil) != want {
		t.Errorf("%s: the decoder accepted = %v, want %v", what, err == nil, want)
	}
}

// One scope, one spelling, exhaustively. The escape space is 256 bytes in four spellings (both
// hex digits uppercase, both lowercase, and the two mixtures), so all of it is run, through the
// Go parser and through the schema. Exactly the bytes that must be escaped are accepted, and
// only in uppercase. A table of examples cannot hold this: the Go side and the schema can drift
// together on a byte no example names (%61, an escaped 'a', was such a byte), and then an
// agreement test sees two halves that agree.
func TestEveryEscapeHasOneVerdictInGoAndInTheSchema(t *testing.T) {
	const upper, lower = "0123456789ABCDEF", "0123456789abcdef"
	accepted := 0
	for b := range 256 {
		hi, lo := b>>4, b&0x0F
		spellings := map[string]bool{ // spelling: is it uppercase throughout
			string([]byte{'%', upper[hi], upper[lo]}): true,
			string([]byte{'%', lower[hi], lower[lo]}): lower[hi] == upper[hi] && lower[lo] == upper[lo],
			string([]byte{'%', upper[hi], lower[lo]}): lower[lo] == upper[lo],
			string([]byte{'%', lower[hi], upper[lo]}): lower[hi] == upper[hi],
		}
		for esc, isUpper := range spellings {
			want := isUpper && mustBeEscaped(byte(b))
			candidate := "slack:channel:a" + esc + "b"
			scopeVerdicts(t, "the escape "+esc, candidate, want)
			if !want {
				continue
			}
			accepted++
			scope, err := ParseScopeID(candidate)
			if err != nil {
				continue // reported above
			}
			if wantID := "a" + string([]byte{byte(b)}) + "b"; scope.ContainerID != wantID {
				t.Errorf("the escape %s decodes to %q, want %q", esc, scope.ContainerID, wantID)
			}
		}
	}
	// 256 bytes, less 66 kept as they are, less 33 control bytes.
	if accepted != 157 {
		t.Errorf("%d escapes were accepted, want 157", accepted)
	}
}

// Every character, as the first and as a later character of the provider and of the container
// kind: a lowercase letter first, then a-z, 0-9 and underscore. The run goes past ASCII far enough
// to include the long s (U+017F) and adds the Kelvin sign (U+212A), which fold onto 's' and 'k'.
func TestEveryCharacterOfTheProviderAndKindSegments(t *testing.T) {
	runes := []rune{0x212A}
	for r := rune(0); r <= 0x17F; r++ {
		runes = append(runes, r)
	}
	for _, r := range runes {
		first := r >= 'a' && r <= 'z'
		later := first || r >= '0' && r <= '9' || r == '_'
		c := string(r)
		scopeVerdicts(t, fmt.Sprintf("U+%04X first in the provider", r), c+"x:channel:C1", first)
		scopeVerdicts(t, fmt.Sprintf("U+%04X later in the provider", r), "x"+c+":channel:C1", later)
		scopeVerdicts(t, fmt.Sprintf("U+%04X first in the kind", r), "slack:"+c+"x:C1", first)
		scopeVerdicts(t, fmt.Sprintf("U+%04X later in the kind", r), "slack:x"+c+":C1", later)
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

// A scope id in a URL (ADR 3). It holds percent signs, and an HTTP server decodes a path and a
// query once. So a scope id spliced into a URL as it stands arrives as a different string, which
// equals no stored scope, and an escaped slash in it becomes path segments. Encoded once more,
// as one opaque value, it arrives byte for byte.
func TestAScopeIDInAURLIsEncodedOnceMore(t *testing.T) {
	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /scopes/{scope}/members", func(_ http.ResponseWriter, r *http.Request) {
		got = r.PathValue("scope")
	})
	mux.HandleFunc("GET /members", func(_ http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("scope")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(rawURL string) string {
		t.Helper()
		got = "(the handler was not reached)"
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return got
	}

	for _, id := range []string{"19:abc@thread.tacv2", "../../admin", "100% sure", "a?b#c&d=e+f"} {
		scope, err := ScopeID("teams", "channel", id)
		if err != nil {
			t.Fatal(err)
		}

		// Right: the scope id is one opaque value, percent-encoded again like any other string.
		if got := get(srv.URL + "/scopes/" + url.PathEscape(scope) + "/members"); got != scope {
			t.Errorf("path, encoded once more: the handler saw %q, want %q", got, scope)
		}
		if got := get(srv.URL + "/members?" + url.Values{"scope": {scope}}.Encode()); got != scope {
			t.Errorf("query, encoded once more: the handler saw %q, want %q", got, scope)
		}

		// Wrong: spliced in as it stands. The server decodes it once, so what arrives is not the
		// scope id, and a lookup by it finds nothing (or, for an escaped slash, another route).
		if got := get(srv.URL + "/scopes/" + scope + "/members"); got == scope {
			t.Errorf("path, as it stands: %q arrived unchanged, so the warning in ADR 3 is out of date", scope)
		}
		if got := get(srv.URL + "/members?scope=" + scope); got == scope {
			t.Errorf("query, as it stands: %q arrived unchanged, so the warning in ADR 3 is out of date", scope)
		}
	}

	// What a proxy or a router that decodes the path makes of an escaped slash.
	u, err := url.Parse("http://sink.example/scopes/teams:channel:..%2F..%2Fadmin/members")
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/scopes/teams:channel:../../admin/members" {
		t.Errorf("the decoded path is %q", u.Path)
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
