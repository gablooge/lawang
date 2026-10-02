package pipeline_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is a tripwire of the kind internal/ids already has for RecordID, and it is here for
// the same reason: a property that a behavioural test keeps failing to pin is pinned by making
// the thing it forbids unwritable instead.
//
// ADR 12 decision 5 says a masking placeholder's 80 random bits are not a function of ANY input a
// sink can see. Four schemes have now satisfied the behavioural tests while handing a sink a
// complete offline oracle, each narrower than the last, and the fourth one (keep the real clock,
// draw the entropy from sha256(kind, value, that millisecond)) survives because the clock is
// published in the first 10 characters of the token, so a function of (value, clock) differs
// between two mintings and is still computable by whoever holds the token. No assertion over two
// mintings can tell that apart from crypto/rand, because the two really do differ.
//
// What all four schemes need is the value. pipeline.token takes a kind and nothing else, so none
// of them is expressible without changing the program's shape: the reviewer's mutation had to
// widen the signature. This scan fails on every way of changing that shape, by name, so a scheme
// in this family is refused at the source rather than caught by a test that happens to notice it.
//
// Like the RecordID scan, this is a tripwire and not a guard. It reads source text, and source
// text can always be arranged so that a scan does not see it (a build tag this run does not set,
// generated code, an assembly stub). What it buys is that an honest change is told at once, by
// name, why the mint is shaped the way it is.

const (
	// idsPackage holds the one entropy source a placeholder may use.
	idsPackage = "internal/ids"
	// mintEntropy is that source. The mint is tied to it here, because nothing else was tying
	// them together: a mutation that stopped calling it and built a ULID inline from a digest
	// left the whole repository green.
	mintEntropy = "NewUnpredictable"
	// mintFunc is the one function that may produce a placeholder.
	mintFunc = "token"
	// kindType is the only thing the mint is allowed to see.
	kindType = "secretKind"
)

// idsImportPath is derived from go.mod rather than written out, because a literal is how the
// RecordID scan went blind once: the module was renamed, the literal kept the old path, and every
// reference the scan looked for resolved to nothing while the scan stayed green.
var idsImportPath = modulePath() + "/" + idsPackage

func modulePath() string {
	b, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		panic("mint_tripwire_test: cannot read go.mod: " + err.Error())
	}
	for line := range strings.Lines(string(b)) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	panic("mint_tripwire_test: go.mod has no module line")
}

// freeNamesAllowedInMint is every name the mint's body may use that it does not declare itself:
// the two packages it calls into, and the predeclared identifiers. A package-level variable, a
// helper of this package, or any other import shows up here as a name that is not on the list,
// which is the point. A hook variable is how a value would reach a closed function without
// passing through its parameters.
var freeNamesAllowedInMint = []string{"fmt", "ids", "nil", "string"}

// qualifiedCallsAllowedInMint is every qualified name the mint's body may name. It is what ties
// the mint to its entropy source, together with the package-wide rule at the end of mintFindings:
// one reference to internal/ids, inside this body, and no other function of that package.
//
// A name on the left of a dot is not checked for being a package, deliberately. A method call on
// a local is a way out of the function too, so "id.String" is reported here exactly as a second
// import would be.
var qualifiedCallsAllowedInMint = []string{"fmt.Errorf", "ids." + mintEntropy}

// forbiddenImport reports why a non-test file of this package may not import path, or "".
//
// These are the packages that turn a value into bytes. None of them is imported here today, and
// the mint does not need any of them, since its bytes come from internal/ids. A later item that
// genuinely needs one for something that is not a placeholder changes this list and says why
// here: that edit is the conversation this tripwire exists to force.
func forbiddenImport(path string) string {
	switch {
	case path == "crypto/rand":
		// A placeholder's entropy is drawn in internal/ids, which is where that recipe is read
		// and reviewed. A second crypto/rand caller here would be a second recipe.
		return "draws entropy outside internal/ids"
	case strings.HasPrefix(path, "crypto/"), path == "hash", strings.HasPrefix(path, "hash/"):
		return "computes a digest, which is how every derived placeholder was written"
	case path == "math/rand" || strings.HasPrefix(path, "math/rand/"):
		return "is the predictable source ids.New's own doc comment forbids for a placeholder"
	case strings.HasPrefix(path, "github.com/oklog/ulid"):
		return "mints a ULID without going through internal/ids"
	case strings.HasPrefix(path, "github.com/zeebo/blake3"):
		return "computes a digest, which is how every derived placeholder was written"
	}
	return ""
}

// srcFile is one file to scan: rel is how it is named in a finding, src is its text (nil reads it
// from rel).
type srcFile struct {
	rel string
	src any
}

// TestTheMaskingMintCannotSeeTheValue is the tripwire. It fails when the minting path grows a way
// to see a masked value, or stops going through ids.NewUnpredictable for its bits.
func TestTheMaskingMintCannotSeeTheValue(t *testing.T) {
	t.Parallel()
	files := packageFiles(t)
	if len(files) < 3 {
		t.Fatalf("only %d non-test files were read, the scan is not seeing internal/pipeline", len(files))
	}
	for _, what := range mintFindings(files) {
		t.Errorf("%s", what)
	}
}

// TestTheMintScanLooksAtAPackageThatExists is the other half of deriving the import path. If the
// derived path named no package this repository imports, every reference the scan looks for would
// resolve to nothing and the scan would report that by passing.
func TestTheMintScanLooksAtAPackageThatExists(t *testing.T) {
	t.Parallel()
	var importers int
	for _, f := range packageFiles(t) {
		parsed, err := parser.ParseFile(token.NewFileSet(), f.rel, f.src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", f.rel, err)
		}
		for _, spec := range parsed.Imports {
			if p, err := strconv.Unquote(spec.Path.Value); err == nil && p == idsImportPath {
				importers++
			}
		}
	}
	if importers != 1 {
		t.Fatalf("%d files of internal/pipeline import %q, want exactly the one that mints a "+
			"placeholder: with none, the scan looks for a package nothing uses and passes whatever "+
			"the mint does", importers, idsImportPath)
	}
}

// packageFiles is every Go file of this package that is not a test. The generated queries in
// pipelinedb are a package of their own and mint nothing; the mint and all its callers are here.
func packageFiles(t *testing.T) []srcFile {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	var files []srcFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, srcFile{rel: name})
	}
	return files
}

// mintFindings parses the files and returns everything about them that would let a placeholder be
// a function of the value it stands for, each as one sentence. An empty result is the invariant
// holding.
func mintFindings(files []srcFile) []string {
	fset := token.NewFileSet()
	var findings []string
	var decl *ast.FuncDecl
	var declIn string
	var kindUnderlying ast.Expr
	var kindFound bool
	var idsRefs []string
	var inMint int

	for _, f := range files {
		parsed, err := parser.ParseFile(fset, f.rel, f.src, parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			findings = append(findings, f.rel+" does not parse: "+err.Error())
			continue
		}
		findings = append(findings, directiveFindings(f.rel, parsed)...)

		// Which local name, if any, this file reaches internal/ids by.
		idsName := ""
		for _, imp := range parsed.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if why := forbiddenImport(path); why != "" {
				findings = append(findings, f.rel+" imports "+path+", which "+why+
					": a masking placeholder's bits come from ids."+mintEntropy+" and from nothing else")
			}
			if imp.Name != nil && imp.Name.Name == "." {
				findings = append(findings, f.rel+" imports "+path+
					" with a dot, which hides which package a name in this file came from")
			}
			if path != idsImportPath {
				continue
			}
			switch {
			case imp.Name == nil:
				idsName = "ids"
			case imp.Name.Name != "_" && imp.Name.Name != ".":
				idsName = imp.Name.Name
			}
		}

		for _, d := range parsed.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if ok && fn.Recv == nil && fn.Name.Name == mintFunc {
				decl, declIn = fn, f.rel
			}
			gen, ok := d.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == kindType && ts.Assign == 0 {
					kindUnderlying, kindFound = ts.Type, true
				}
			}
		}

		// Every reference to internal/ids in this package, and every call of the mint.
		ast.Inspect(parsed, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := e.X.(*ast.Ident); ok && idsName != "" && id.Name == idsName {
					idsRefs = append(idsRefs, f.rel+" refers to ids."+e.Sel.Name)
					if decl != nil && declIn == f.rel && decl.Body != nil &&
						e.Pos() > decl.Body.Pos() && e.End() < decl.Body.End() {
						inMint++
					}
				}
			case *ast.CallExpr:
				id, ok := e.Fun.(*ast.Ident)
				if !ok || id.Name != mintFunc {
					return true
				}
				findings = append(findings, mintCallFindings(f.rel, e)...)
			}
			return true
		})
	}

	if decl == nil {
		return append(findings, "internal/pipeline declares no func "+mintFunc+
			", so this scan is guarding a function that has been renamed or inlined, and a "+
			"placeholder is now minted somewhere nothing checks")
	}
	findings = append(findings, signatureFindings(declIn, decl)...)
	findings = append(findings, bodyFindings(declIn, decl)...)
	if !kindFound {
		findings = append(findings, "internal/pipeline declares no type "+kindType+
			", so the mint's one parameter is of a type this scan cannot see the shape of")
	} else if id, ok := kindUnderlying.(*ast.Ident); !ok || id.Name != "string" {
		findings = append(findings, declIn+": "+kindType+" is no longer a plain string, so the "+
			"mint's one parameter can now carry a value alongside its kind")
	}

	// One rule and one sentence, because every way of breaking it is the same mistake: the bits a
	// placeholder is made of stop coming from the one reviewed recipe. Naming it twice would be a
	// second recipe, naming it outside the mint would be a second mint, naming something else of
	// that package would be another source, and naming it nowhere is the mutation that built a
	// ULID inline from a digest and left the repository green.
	if len(idsRefs) != 1 || inMint != 1 || !strings.HasSuffix(idsRefs[0], "."+mintEntropy) {
		where := "nowhere"
		if len(idsRefs) > 0 {
			where = strings.Join(idsRefs, "; ")
		}
		findings = append(findings, "internal/pipeline may name "+idsImportPath+" exactly once, as "+
			"ids."+mintEntropy+" inside "+mintFunc+", which is where a placeholder's 80 bits come "+
			"from. It names it at: "+where)
	}
	return findings
}

// directiveFindings reports a go:linkname in either direction. From outside it pulls a function
// out of internal/ids with no reference for the scan above to see; from inside this package it
// can push one in under another name.
func directiveFindings(rel string, file *ast.File) []string {
	var findings []string
	for _, group := range file.Comments {
		for _, c := range group.List {
			// A directive is a line comment with no space after the slashes.
			if strings.HasPrefix(c.Text, "//go:linkname ") {
				findings = append(findings, rel+" has a go:linkname directive, which reaches a "+
					"function with no reference this scan can see")
			}
		}
	}
	return findings
}

// signatureFindings reports anything about the mint's signature that would let a value in. The
// value can only arrive as an argument, so one parameter, of the kind type, is the whole rule.
func signatureFindings(rel string, fn *ast.FuncDecl) []string {
	var findings []string
	params := fn.Type.Params.List
	n := 0
	for _, p := range params {
		n += max(len(p.Names), 1)
	}
	if n != 1 {
		return append(findings, rel+": "+mintFunc+" takes "+strconv.Itoa(n)+" parameters, and it "+
			"may take exactly one, the kind. The value a placeholder stands for reaches a mint as "+
			"an argument and in no other way, so a second parameter is how every derived "+
			"placeholder scheme in this pull request's history was written (ADR 12 decision 5)")
	}
	if _, variadic := params[0].Type.(*ast.Ellipsis); variadic {
		findings = append(findings, rel+": "+mintFunc+"'s parameter is variadic, so it takes any "+
			"number of values and not one kind")
	}
	if id, ok := params[0].Type.(*ast.Ident); !ok || id.Name != kindType {
		findings = append(findings, rel+": "+mintFunc+"'s parameter is not a "+kindType+
			", so the mint can now see something other than which class of value it is masking")
	}
	var results []string
	if fn.Type.Results != nil {
		for _, r := range fn.Type.Results.List {
			id, ok := r.Type.(*ast.Ident)
			name := "?"
			if ok {
				name = id.Name
			}
			for range max(len(r.Names), 1) {
				results = append(results, name)
			}
		}
	}
	if !slices.Equal(results, []string{"string", "error"}) {
		findings = append(findings, rel+": "+mintFunc+" returns ("+strings.Join(results, ", ")+
			"), and it returns (string, error): the entropy source can fail and that failure is "+
			"the caller's to roll back")
	}
	return findings
}

// bodyFindings reports every name the mint's body uses that it did not declare and that is not on
// the short list. A closed body is what makes the signature rule mean anything: a parameter list
// of one proves nothing if a package-level hook, a helper of this package or a second import can
// hand the value over behind it.
func bodyFindings(rel string, fn *ast.FuncDecl) []string {
	if fn.Body == nil {
		return []string{rel + ": " + mintFunc + " has no body, so it is implemented somewhere this scan cannot read"}
	}
	declared := map[string]bool{}
	for _, p := range fn.Type.Params.List {
		for _, name := range p.Names {
			declared[name.Name] = true
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.AssignStmt:
			if d.Tok == token.DEFINE {
				for _, lhs := range d.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						declared[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for _, name := range d.Names {
				declared[name.Name] = true
			}
		}
		return true
	})

	free := map[string]bool{}
	qualified := map[string]bool{}
	skip := map[ast.Node]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.SelectorExpr:
			// The selected name belongs to the package or the value on the left, not to this
			// file's scope, so it is judged as a whole below and not as a bare name.
			skip[e.Sel] = true
			if id, ok := e.X.(*ast.Ident); ok {
				qualified[id.Name+"."+e.Sel.Name] = true
			}
		case *ast.Ident:
			if !skip[e] && !declared[e.Name] {
				free[e.Name] = true
			}
		}
		return true
	})

	var findings []string
	for _, name := range sortedKeys(free) {
		if !slices.Contains(freeNamesAllowedInMint, name) {
			findings = append(findings, rel+": "+mintFunc+" uses the name "+name+
				", which it does not declare and which is not on the short list in "+
				"mint_tripwire_test.go. A mint that reaches outside itself can be handed the value "+
				"without taking it as a parameter")
		}
	}
	for _, name := range sortedKeys(qualified) {
		if !slices.Contains(qualifiedCallsAllowedInMint, name) {
			findings = append(findings, rel+": "+mintFunc+" names "+name+
				", and it may name only "+strings.Join(qualifiedCallsAllowedInMint, " and "))
		}
	}
	// That the body DOES call ids.NewUnpredictable is not checked here. The package-wide rule at
	// the end of mintFindings requires exactly one reference to internal/ids, inside this body,
	// and the allow-list above admits no other function of that package, so the two together say
	// it. A second check here would be a rule no case can fail on its own.
	return findings
}

// mintCallFindings reports a call of the mint whose argument is computed rather than carried. The
// parameter is a kind, but secretKind is a string, so a conversion at the call site would hand
// the mint the value under the kind's type.
func mintCallFindings(rel string, call *ast.CallExpr) []string {
	if len(call.Args) != 1 {
		return []string{rel + ": a call of " + mintFunc + " passes " + strconv.Itoa(len(call.Args)) +
			" arguments, and it passes one, the kind"}
	}
	switch call.Args[0].(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return nil
	}
	return []string{rel + ": a call of " + mintFunc + " computes its argument instead of passing a " +
		kindType + " it already holds. " + kindType + " is a string, so a conversion here is how a " +
		"value would reach the mint without the signature changing"}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestTheMintScanSeesEveryWayTheValueCouldGetIn gives the scan teeth. Each case is a way the
// minting path could be opened, including every one of the four schemes this pull request has
// already seen, and each must be reported. The cases that must NOT be reported are there so that
// the scan is not simply saying no to everything, which would be a scan nobody can work with.
func TestTheMintScanSeesEveryWayTheValueCouldGetIn(t *testing.T) {
	t.Parallel()
	// kindDecl and a correct mint, so that each case can supply only what it is about. A case
	// that leaves out the mint entirely is written out in full instead.
	const kindDecl = `package pipeline
type secretKind string
`
	const goodMint = `package pipeline
import (
	"fmt"

	"MODULE/internal/ids"
)
func token(k secretKind) (string, error) {
	id, err := ids.NewUnpredictable()
	if err != nil {
		return "", fmt.Errorf("pipeline: mint a %s placeholder: %w", k, err)
	}
	return "[" + string(k) + ":" + id + "]", nil
}
`
	fill := func(s string) string { return strings.ReplaceAll(s, "MODULE", modulePath()) }

	tests := []struct {
		name  string
		mint  string
		also  string
		kinds string
		want  bool
	}{
		{name: "the mint as it stands", mint: goodMint},
		{
			name: "the reviewer's M1, which has to widen the signature to reach the value",
			mint: `package pipeline
import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"MODULE/internal/ids"
	"github.com/oklog/ulid/v2"
)
func token(k secretKind, value string) (string, error) {
	sum := sha256.Sum256([]byte(string(k) + value + fmt.Sprint(ulid.Now())))
	var e [10]byte
	copy(e[:], sum[:])
	id, err := ulid.New(ulid.Now(), bytes.NewReader(e[:]))
	if err != nil {
		return "", fmt.Errorf("pipeline: mint a %s placeholder: %w", k, err)
	}
	_ = ids.NewUnpredictable
	return "[" + string(k) + ":" + id.String() + "]", nil
}
`,
			want: true,
		},
		{
			name: "a digest of the value alone, the first scheme",
			mint: `package pipeline
import (
	"crypto/sha256"
	"encoding/hex"
)
func token(k secretKind, value string) (string, error) {
	sum := sha256.Sum256([]byte(value))
	return "[" + string(k) + ":" + hex.EncodeToString(sum[:]) + "]", nil
}
`,
			want: true,
		},
		{
			name: "the value handed over by a package-level hook, with the signature untouched",
			mint: `package pipeline
import (
	"fmt"

	"MODULE/internal/ids"
)
var mintingValue string
func token(k secretKind) (string, error) {
	_ = mintingValue
	id, err := ids.NewUnpredictable()
	if err != nil {
		return "", fmt.Errorf("pipeline: mint a %s placeholder: %w", k, err)
	}
	return "[" + string(k) + ":" + id + "]", nil
}
`,
			want: true,
		},
		{
			name: "the value handed over by a helper of this package",
			mint: `package pipeline
import (
	"fmt"

	"MODULE/internal/ids"
)
func token(k secretKind) (string, error) {
	id, err := ids.NewUnpredictable()
	if err != nil {
		return "", fmt.Errorf("pipeline: mint a %s placeholder: %w", k, err)
	}
	return "[" + string(k) + ":" + salt(id) + "]", nil
}
`,
			want: true,
		},
		{
			name: "a variadic kind, which takes any number of strings",
			mint: `package pipeline
import (
	"fmt"

	"MODULE/internal/ids"
)
func token(k ...secretKind) (string, error) {
	id, err := ids.NewUnpredictable()
	if err != nil {
		return "", fmt.Errorf("pipeline: mint a placeholder: %w", err)
	}
	return "[" + string(k[0]) + ":" + id + "]", nil
}
`,
			want: true,
		},
		{
			name: "a kind that is no longer a plain string, so it can carry the value",
			mint: goodMint,
			kinds: `package pipeline
type secretKind struct {
	Name  string
	Value string
}
`,
			want: true,
		},
		{
			name: "a ULID built inline, so nothing goes through internal/ids",
			mint: `package pipeline
import "github.com/oklog/ulid/v2"
func token(k secretKind) (string, error) {
	return "[" + string(k) + ":" + ulid.Make().String() + "]", nil
}
`,
			want: true,
		},
		{
			name: "the predictable source internal/ids warns about",
			mint: `package pipeline
import (
	"fmt"

	"MODULE/internal/ids"
)
func token(k secretKind) (string, error) {
	id := ids.New()
	_ = fmt.Sprint(id)
	return "[" + string(k) + ":" + id + "]", nil
}
`,
			want: true,
		},
		{
			name: "a digest imported into the package, away from the mint itself",
			mint: goodMint,
			also: `package pipeline
import "crypto/sha256"
func fingerprint(s foundSecret) [32]byte { return sha256.Sum256([]byte(s.Value)) }
`,
			want: true,
		},
		{
			name: "a mint that draws its bits from a constant while another file names the source",
			mint: `package pipeline
func token(k secretKind) (string, error) {
	return "[" + string(k) + ":" + "0000000000000000000000000" + "]", nil
}
`,
			also: `package pipeline
import "MODULE/internal/ids"
var _ = ids.NewUnpredictable
`,
			want: true,
		},
		{
			name: "a second caller of the entropy source, which is a second mint",
			mint: goodMint,
			also: `package pipeline
import "MODULE/internal/ids"
func other() (string, error) { return ids.NewUnpredictable() }
`,
			want: true,
		},
		{
			name: "a linkname that reaches past the package boundary",
			mint: goodMint,
			also: `package pipeline
import _ "unsafe"

//go:linkname mint MODULE/internal/ids.NewUnpredictable
func mint() (string, error)
`,
			want: true,
		},
		{
			name: "a dot import, which hides where a name came from",
			mint: goodMint,
			also: `package pipeline
import . "strings"
var _ = Compare
`,
			want: true,
		},
		{
			name: "a call site that converts the value to a kind",
			mint: goodMint,
			also: `package pipeline
func mask(s foundSecret) (string, error) { return token(secretKind(s.Value)) }
`,
			want: true,
		},
		{
			name: "an ordinary call site, which is what the mint is for",
			mint: goodMint,
			also: `package pipeline
func mask(s foundSecret) (string, error) { return token(s.Kind) }
`,
		},
		{
			name: "the mint renamed away, so this scan would otherwise guard nothing",
			mint: `package pipeline
import "MODULE/internal/ids"
func placeholder(k secretKind) (string, error) {
	id, err := ids.NewUnpredictable()
	return "[" + string(k) + ":" + id + "]", err
}
`,
			want: true,
		},
		{
			name: "an unrelated helper of this package, which the scan must leave alone",
			mint: goodMint,
			also: `package pipeline
import "strings"
func isDecimalish(s string) bool { return !strings.ContainsAny(s, "abc") }
`,
		},
		{
			name: "an unrelated import of this package, which is not every import",
			mint: goodMint,
			also: `package pipeline
import (
	"encoding/base64"
	"strings"
)
var _ = base64.RawStdEncoding
var _ = strings.Compare
`,
		},
	}

	for _, tt := range tests {
		kinds := kindDecl
		if tt.kinds != "" {
			kinds = tt.kinds
		}
		files := []srcFile{
			{rel: "kinds.go", src: fill(kinds)},
			{rel: "redact.go", src: fill(tt.mint)},
		}
		if tt.also != "" {
			files = append(files, srcFile{rel: "extra.go", src: fill(tt.also)})
		}
		got := mintFindings(files)
		if (len(got) > 0) != tt.want {
			t.Errorf("%s: findings = %v, want a finding = %v", tt.name, got, tt.want)
		}
	}
}
