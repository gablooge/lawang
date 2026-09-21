package record

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The acceptance criteria of B05, each against the SCHEMA. They run against both compilations:
// a sink whose validator treats "format" as an annotation must reject the same records.

func TestSchemaValidatesTheArchitectureExample(t *testing.T) {
	with, without := schemas(t)
	if err := schemaError(t, with, exampleBytes(t)); err != nil {
		t.Errorf("formats asserted: %v", err)
	}
	if err := schemaError(t, without, exampleBytes(t)); err != nil {
		t.Errorf("formats not asserted: %v", err)
	}
}

// The example in docs/architecture.md section 6 and the fixture the tests use are the same
// bytes. If either is edited alone, this fails and says which way they differ.
func TestTheExampleIsTheOneInTheArchitecture(t *testing.T) {
	arch, err := os.ReadFile("../../docs/architecture.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(arch), "\n## 6. ")
	if !ok {
		t.Fatal("docs/architecture.md has no section 6")
	}
	section, _, _ = strings.Cut(section, "\n## 7. ")
	_, block, ok := strings.Cut(section, "```json\n")
	if !ok {
		t.Fatal("section 6 has no json block")
	}
	block, _, ok = strings.Cut(block, "```")
	if !ok {
		t.Fatal("the json block of section 6 is not closed")
	}
	if want := string(exampleBytes(t)); block != want {
		t.Errorf("section 6 and %s differ.\n--- architecture\n%s\n--- fixture\n%s", examplePath, block, want)
	}
}

func TestSchemaRejectsARecordWithNoScope(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"no visibility.scope":    del("visibility.scope"),
		"no visibility":          del("visibility"),
		"visibility.scope empty": set("visibility.scope", ""),
		"visibility.scope null":  set("visibility.scope", nil),
	} {
		assertBothSchemasReject(t, name, edit)
	}
}

func TestSchemaRejectsAnUnknownKind(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"unknown kind":    set("kind", "email"),
		"kind in capital": set("kind", "Message"),
		"kind empty":      set("kind", ""),
		"kind a number":   set("kind", 1),
	} {
		assertBothSchemasReject(t, name, edit)
	}
}

// notRFC3339 is every occurred_at the format refuses for its shape. All of them fall to the
// pattern, so they are refused with and without format assertion.
var notRFC3339 = map[string]any{
	"a date with no time":         "2026-07-09",
	"a time with no zone":         "2026-07-09T12:30:45",
	"impossible month, day, time": "2026-13-40T99:99:99Z",
	"a Unix timestamp":            1752064245,
	"a Unix timestamp in text":    "1752064245",
	"an empty string":             "",
	"null":                        nil,
	"an offset, not Z":            "2026-07-09T14:30:45+02:00",
	"a zero offset, not Z":        "2026-07-09T12:30:45+00:00",
	"lowercase t and z":           "2026-07-09t12:30:45z",
	"a space for the T":           "2026-07-09 12:30:45Z",
	"a leap second":               "2026-06-30T23:59:60Z",
	"hour 24":                     "2026-07-09T24:00:00Z",
	"ten fractional digits":       "2026-07-09T12:30:45.1234567890Z",
	"a fraction with no digits":   "2026-07-09T12:30:45.Z",
	"a comma for the fraction":    "2026-07-09T12:30:45,5Z",
	"a letter in the fraction":    "2026-07-09T12:30:45.1x3Z",
	"slashes in the date":         "2026/07/09T12:30:45Z",
	"dots in the time":            "2026-07-09T12.30.45Z",
	"a letter in the year":        "2O26-07-09T12:30:45Z",
	"month 00":                    "2026-00-09T12:30:45Z",
	"day 00":                      "2026-07-00T12:30:45Z",
	"minute 60":                   "2026-07-09T12:60:45Z",
	"a trailing newline":          "2026-07-09T12:30:45Z\n",
	"a leading space":             " 2026-07-09T12:30:45Z",
	"Go's zero time":              "0001-01-01T00:00:00Z",
	"the year 999":                "0999-12-31T23:59:59Z",
	"a five digit year":           "12026-07-09T12:30:45Z",
	"words":                       "yesterday",
}

func TestSchemaRejectsAnOccurredAtThatIsNotRFC3339(t *testing.T) {
	for name, value := range notRFC3339 {
		assertBothSchemasReject(t, name, set("occurred_at", value))
	}
}

// The pattern cannot know how long a month is. That is what format assertion adds, and this
// pins that the tests really run with it: without assertion the same record passes.
func TestFormatAssertionIsOnAndCatchesWhatThePatternCannot(t *testing.T) {
	with, without := schemas(t)
	for _, day := range []string{"2026-02-30T12:00:00Z", "2026-04-31T12:00:00Z", "2025-02-29T12:00:00Z"} {
		doc := exampleDoc(t)
		set("occurred_at", day)(doc)
		data, _ := json.Marshal(doc)
		if err := schemaError(t, with, data); err == nil {
			t.Errorf("%s: accepted although formats are asserted", day)
		}
		if err := schemaError(t, without, data); err != nil {
			t.Errorf("%s: the pattern was expected to let this through: %v", day, err)
		}
	}
}

func assertBothSchemasReject(t *testing.T, name string, edit func(map[string]any)) {
	t.Helper()
	with, without := schemas(t)
	doc := exampleDoc(t)
	before, _ := json.Marshal(doc)
	edit(doc)
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, data) {
		t.Fatalf("%s: the edit changed nothing", name)
	}
	if err := schemaError(t, with, data); err == nil {
		t.Errorf("%s: accepted (formats asserted)", name)
	}
	if err := schemaError(t, without, data); err == nil {
		t.Errorf("%s: accepted (formats not asserted)", name)
	}
}

// In some regular expression dialects '$' also matches before a trailing newline, so a schema
// that leaned on the anchor alone would accept "rec_...\n" in a validator built on one. Go's
// dialect is not one of them, so no Go test can show the difference by validating. This checks
// the schema's structure: every subschema with an anchored pattern also has a fixed length, or
// a "not" with an unanchored character class, which does not depend on the anchor.
func TestEveryAnchoredPatternHasACompanion(t *testing.T) {
	var schema any
	if err := json.Unmarshal(Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	patterns := 0
	var visit func(path string, node any)
	visit = func(path string, node any) {
		switch n := node.(type) {
		case []any:
			for _, v := range n {
				visit(path+"[]", v)
			}
		case map[string]any:
			for k, v := range n {
				visit(path+"/"+k, v)
			}
			pattern, ok := n["pattern"].(string)
			if !ok || !strings.HasSuffix(pattern, "$") {
				return
			}
			patterns++
			minLen, hasMin := n["minLength"]
			maxLen, hasMax := n["maxLength"]
			fixed := hasMin && hasMax && reflect.DeepEqual(minLen, maxLen)
			not, _ := n["not"].(map[string]any)
			class, _ := not["pattern"].(string)
			unanchored := strings.HasPrefix(class, "[^") && strings.HasSuffix(class, "]")
			if !fixed && !unanchored {
				t.Errorf("%s: the anchored pattern %q has no fixed length and no unanchored companion", path, pattern)
			}
		}
	}
	visit("", schema)
	if patterns < 6 {
		t.Errorf("found %d anchored patterns, the walk is not seeing the schema", patterns)
	}
}

// The schema is a contract for validators in any language, and each hands "pattern" to its own
// regular expression engine. The escapes those engines share stop at ASCII: Ruby's engine refuses
// a two-digit hex escape of 0x80 or above in a UTF-8 pattern ("invalid multibyte escape"), so a
// schema that spelled U+009F that way could not be loaded there at all, and the brace form of a
// hex escape, the four-digit form and a property class are each unknown to one of Go, Python and
// ECMAScript. So a pattern escapes ASCII only and holds every other character as itself, which
// the FILE spells with a JSON escape (ADR 4, "Portability of the patterns"). This pins the
// spelling, so that the unportable one cannot come back. It reads the patterns as they are after
// JSON decoding, which is what an engine is handed.
func TestNoPatternUsesAnEscapeThatOnlySomeEnginesRead(t *testing.T) {
	file, err := os.ReadFile("record.v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range file {
		if b >= 0x80 {
			t.Fatalf("byte %d of the schema file is not ASCII: a character above U+007F is written as a JSON escape", i)
		}
	}
	var schema any
	if err := json.Unmarshal(Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	// A backslash, then: x and a hex byte of 0x80 or above, or x and a brace, or one of u, p, P.
	unportable := regexp.MustCompile(`[\\](?:x[89A-Fa-f][0-9A-Fa-f]|x[{]|[upP])`)
	patterns, literal := 0, 0
	var visit func(path string, node any)
	visit = func(path string, node any) {
		switch n := node.(type) {
		case []any:
			for _, v := range n {
				visit(path+"[]", v)
			}
		case map[string]any:
			for k, v := range n {
				visit(path+"/"+k, v)
			}
			pattern, ok := n["pattern"].(string)
			if !ok {
				return
			}
			patterns++
			if found := unportable.FindString(pattern); found != "" {
				t.Errorf("%s: the pattern uses the escape %s, which not every engine reads: write the character itself, as a JSON escape in the file", path, found)
			}
			if strings.ContainsRune(pattern, 0x0080) && strings.ContainsRune(pattern, 0x009F) {
				literal++
			}
		}
	}
	visit("", schema)
	if patterns < 16 {
		t.Errorf("found %d patterns, the walk is not seeing the schema", patterns)
	}
	// identifier, displayName and oneLine name the C1 controls, and they do it with the characters.
	if literal != 3 {
		t.Errorf("%d patterns hold U+0080 and U+009F as themselves, want 3", literal)
	}
}

func TestSchemaIsTheEmbeddedFileAndNamesItself(t *testing.T) {
	file, err := os.ReadFile("record.v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(file, Schema()) {
		t.Error("Schema() is not record.v1.schema.json")
	}
	var head struct {
		Schema     string `json:"$schema"`
		ID         string `json:"$id"`
		Properties struct {
			Format struct {
				Const string `json:"const"`
			} `json:"format"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(file, &head); err != nil {
		t.Fatal(err)
	}
	if head.Schema != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("$schema = %q, want draft 2020-12", head.Schema)
	}
	if head.ID != SchemaID {
		t.Errorf("$id = %q, SchemaID = %q", head.ID, SchemaID)
	}
	if head.Properties.Format.Const != FormatV1 {
		t.Errorf("format const = %q, FormatV1 = %q", head.Properties.Format.Const, FormatV1)
	}

	// A caller that scribbles on the result does not reach the embedded bytes.
	got := Schema()
	got[0] = 'X'
	if Schema()[0] == 'X' {
		t.Error("Schema() hands out the embedded slice itself")
	}
}
