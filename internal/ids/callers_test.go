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
	idsImportPath = "github.com/gablooge/sluiceway/internal/ids"
	// recordPackage is the one package that may call RecordID: record.Seal hashes the scope the
	// record carries, and no other.
	recordPackage = "internal/record"
)

// RecordID is exported because record.Seal is in another package, and it takes any string as the
// scope. A second caller could mint an id for a scope the record does not carry, and the promise
// made to sinks, that one id never appears with two scopes (ADR 4), would rest on nobody doing
// that. This fails when any file that is not a test, outside internal/record and this package,
// refers to RecordID.
func TestRecordIDHasNoCallerOutsideRecord(t *testing.T) {
	found, files := recordIDReferences(t, filepath.Join("..", ".."))
	for _, where := range found {
		t.Errorf("%s refers to ids.RecordID: a record id is minted by record.Seal and by nothing else", where)
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
	}
	for _, tt := range tests {
		got, err := refersToRecordID(tt.path, tt.src)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("%s: found = %v, want %v", tt.name, got, tt.want)
		}
	}
	for path, want := range map[string]bool{
		"internal/worker/ledger.go":      true,
		"cmd/sluiceway/main.go":          true,
		"internal/worker/ledger_test.go": false,
		"internal/record/record.go":      false,
		"internal/ids/ids.go":            false,
		"internal/recordings/x.go":       true,
	} {
		if got := scanned(path); got != want {
			t.Errorf("scanned(%q) = %v, want %v", path, got, want)
		}
	}
}

// scanned reports whether a file, named by its slash-separated path from the repository root, is
// one the rule applies to.
func scanned(path string) bool {
	if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
		return false
	}
	dir := filepath.ToSlash(filepath.Dir(path))
	return dir != recordPackage && dir != "internal/ids"
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
			if name := d.Name(); rel != "." && (strings.HasPrefix(name, ".") || name == "testdata" || name == "bin") {
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
		refers, err := refersToRecordID(path, nil)
		if err != nil {
			return err
		}
		if refers {
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found, files
}

// refersToRecordID parses one file (src may be nil to read it from path) and reports whether it
// selects RecordID from this package under any import name, or imports this package with a dot,
// which would hide such a reference from a scan of selectors.
func refersToRecordID(path string, src any) (bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil {
		return false, err
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
			return true, nil
		case imp.Name.Name != "_":
			names[imp.Name.Name] = true
		}
	}
	refers := false
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && names[pkg.Name] && sel.Sel.Name == "RecordID" {
			refers = true
		}
		return true
	})
	return refers, nil
}
