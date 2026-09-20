package ids

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	idsImportPath = "github.com/gablooge/lawang/internal/ids"
	// recordPackage is the one package that may call RecordID: record.Seal hashes the scope the
	// record carries, and no other.
	recordPackage = "internal/record"
	// idsPackage is this package, where RecordID may be said once: where it is declared.
	idsPackage = "internal/ids"
)

// RecordID is exported because record.Seal is in another package, and it takes any string as the
// scope. A second caller could mint an id for a scope the record does not carry, and the promise
// made to sinks, that one id never appears with two scopes (ADR 4), would rest on nobody doing
// that. This fails when any file that is not a test refers to RecordID, outside internal/record
// and outside the declaration itself. Inside this package that includes a wrapper, which would
// hand the recipe to every package under another name, and anywhere it includes a go:linkname
// directive, which needs no reference at all.
//
// This scan is a tripwire, not the guard. It reads source text, and source text can always be
// arranged so that a scan does not see it (a build tag this run does not set, generated code,
// an assembly stub). The guard is the seal in record.MarshalJSON: whatever mints an id, a Record
// that was not sealed by record.Seal, for the fields it carries, cannot be marshalled. The scan
// exists so that an honest second caller is told at once, by name, why there must not be one.
func TestRecordIDHasNoCallerOutsideRecord(t *testing.T) {
	found, files := recordIDReferences(t, filepath.Join("..", ".."))
	for _, what := range found {
		t.Errorf("%s: a record id is minted by record.Seal and by nothing else", what)
	}
	if files < 10 {
		t.Errorf("only %d Go files were read, the walk is not seeing the repository", files)
	}
}

// The scan itself has to have teeth: it must see a plain call, an aliased import, a dot import
// and a function value, and must leave tests and the record package alone.
func TestTheRecordIDScanSeesEveryKindOfReference(t *testing.T) {
	tests := []struct {
		name, path, src string
		want            bool
	}{
		{"a call", "internal/worker/a.go", `package worker
import "` + idsImportPath + `"
var _, _ = ids.RecordID("p", "e", "v", "s", "t")`, true},
		{"an aliased import", "internal/worker/b.go", `package worker
import keys "` + idsImportPath + `"
var _, _ = keys.RecordID("p", "e", "v", "s", "t")`, true},
		{"a function value", "internal/worker/c.go", `package worker
import "` + idsImportPath + `"
var mint = ids.RecordID`, true},
		{"a dot import", "internal/worker/d.go", `package worker
import . "` + idsImportPath + `"
var _ = New()`, true},
		{"another function of the package", "internal/worker/e.go", `package worker
import "` + idsImportPath + `"
var _ = ids.New()`, false},
		{"a method of the same name on something else", "internal/worker/f.go", `package worker
type ledger struct{}
func (ledger) RecordID() string { return "" }
var ids ledger
var _ = ids.RecordID()`, false},
		{"a wrapper in this package, in a file of its own", "internal/ids/mint.go", `package ids
func MintAny(p, e, v, s, t string) (string, error) { return RecordID(p, e, v, s, t) }`, true},
		{"a function value in this package", "internal/ids/mint.go", `package ids
var Mint = RecordID`, true},
		{"a wrapper in the defining file", "internal/ids/ids.go", `package ids
func RecordID(p, e, v, s, t string) (string, error) { return "", nil }
func MintAny(p, e, v, s, t string) (string, error) { return RecordID(p, e, v, s, t) }`, true},
		{"the declaration alone, with its name in its own doc comment", "internal/ids/ids.go", `package ids
// RecordID is the recipe.
func RecordID(p, e, v, s, t string) (string, error) { return "", nil }`, false},
		{"another file of this package that leaves it alone", "internal/ids/other.go", `package ids
func Other() string { return New() }`, false},
		{"a linkname that pulls the recipe", "internal/worker/g.go", `package worker
import _ "unsafe"
//go:linkname mint ` + idsImportPath + `.RecordID
func mint(p, e, v, s, t string) (string, error)`, true},
		{"a linkname that pulls anything else out of this package", "internal/worker/h.go", `package worker
import _ "unsafe"
//go:linkname part ` + idsImportPath + `.checkPart
func part(name, value string) error`, true},
		{"a linkname in this package, which can push the recipe out", "internal/ids/push.go", `package ids
import _ "unsafe"
//go:linkname New github.com/gablooge/lawang/internal/worker.mint`, true},
		{"a linkname to something else", "internal/worker/i.go", `package worker
import _ "unsafe"
//go:linkname nanotime runtime.nanotime
func nanotime() int64`, false},
		{"the word in an ordinary comment", "internal/worker/j.go", `package worker
// go:linkname is how one would reach ` + idsImportPath + `.RecordID, and this is prose.
var _ = 1`, false},
	}
	for _, tt := range tests {
		got, err := refersToRecordID(tt.path, tt.path, tt.src)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if (got != "") != tt.want {
			t.Errorf("%s: found = %q, want a finding = %v", tt.name, got, tt.want)
		}
	}
	for path, want := range map[string]bool{
		"internal/worker/ledger.go":      true,
		"cmd/lawang/main.go":             true,
		"internal/worker/ledger_test.go": false,
		"internal/record/record.go":      false,
		"internal/ids/ids.go":            true,
		"internal/ids/mint.go":           true,
		"internal/ids/ids_test.go":       false,
		"internal/recordings/x.go":       true,
		"internal/outbox/bin/x.go":       true,
	} {
		if got := scanned(path); got != want {
			t.Errorf("scanned(%q) = %v, want %v", path, got, want)
		}
	}
	// Only what the Go tool itself does not build is skipped, and the repository's own bin/. A
	// package directory that happens to be called bin is a package like any other.
	for dir, want := range map[string]bool{
		"bin":                    true,
		"internal/outbox/bin":    false,
		"cmd/bin":                false,
		".git":                   true,
		".claude":                true,
		"internal/ids/testdata":  true,
		"internal/record":        false,
		"internal/ids":           false,
		"internal/outbox/binary": false,
	} {
		if got := skippedDir(dir); got != want {
			t.Errorf("skippedDir(%q) = %v, want %v", dir, got, want)
		}
	}
}

// scanned reports whether a file, named by its slash-separated path from the repository root, is
// one the rule applies to: every Go file that is not a test and not in internal/record. This
// package is scanned too, by a stricter rule (see refersToRecordID).
func scanned(path string) bool {
	if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
		return false
	}
	return filepath.ToSlash(filepath.Dir(path)) != recordPackage
}

// skippedDir reports whether the walk leaves a directory alone, named by its slash-separated
// path from the repository root: the build output in bin/ at the top and nowhere else, and what
// the Go tool does not build either (testdata, and a name that begins with a dot).
func skippedDir(rel string) bool {
	name := rel[strings.LastIndex(rel, "/")+1:]
	return rel == "bin" || name == "testdata" || strings.HasPrefix(name, ".")
}

func recordIDReferences(t *testing.T, root string) (found []string, files int) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && skippedDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") {
			return nil
		}
		files++
		if !scanned(rel) {
			return nil
		}
		what, err := refersToRecordID(rel, path, nil)
		if err != nil {
			return err
		}
		if what != "" {
			found = append(found, what)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found, files
}

// refersToRecordID parses one file (rel is its slash-separated path from the repository root,
// filename where to read it, and src may be nil to read it from there) and says what in it
// reaches the recipe, or "" for nothing:
//
//   - outside this package: a selection of RecordID from this package under any import name, a
//     dot import of this package, which would hide such a reference from a scan of selectors,
//     and a go:linkname directive that names this package, which reaches it with no reference;
//   - inside this package: the identifier RecordID anywhere but in its own declaration, so a
//     wrapper or a function value, in the file that declares it or in a new one, and any go:linkname
//     directive, because from inside it can push a function out under another name.
func refersToRecordID(rel, filename string, src any) (string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filename, src, parser.SkipObjectResolution|parser.ParseComments)
	if err != nil {
		return "", err
	}
	inside := filepath.ToSlash(filepath.Dir(rel)) == idsPackage
	for _, group := range file.Comments {
		for _, c := range group.List {
			// A directive is a line comment with no space after the slashes.
			if strings.HasPrefix(c.Text, "//go:linkname ") && (inside || strings.Contains(c.Text, idsImportPath+".")) {
				return rel + " has a go:linkname directive that reaches internal/ids", nil
			}
		}
	}
	if inside {
		return insideReference(rel, file), nil
	}
	names := map[string]bool{}
	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil || importPath != idsImportPath {
			continue
		}
		switch {
		case imp.Name == nil:
			names["ids"] = true
		case imp.Name.Name == ".":
			return rel + " imports internal/ids with a dot, which hides a reference to RecordID", nil
		case imp.Name.Name != "_":
			names[imp.Name.Name] = true
		}
	}
	what := ""
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && names[pkg.Name] && sel.Sel.Name == "RecordID" {
			what = rel + " refers to ids.RecordID"
		}
		return true
	})
	return what, nil
}

// insideReference is the rule for a file of this package: the identifier RecordID may appear
// once, as the name of the function where it is declared (and a package declares it once, or
// does not compile).
func insideReference(rel string, file *ast.File) string {
	var declared *ast.Ident
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "RecordID" {
			declared = fn.Name
		}
	}
	what := ""
	ast.Inspect(file, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "RecordID" && id != declared {
			what = rel + " refers to RecordID inside internal/ids, outside its declaration: a wrapper hands the recipe to every package"
		}
		return true
	})
	return what
}
