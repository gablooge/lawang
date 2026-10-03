package sink_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/sink"
	"github.com/gablooge/lawang/internal/tenancy"
)

func newJSONL(t testing.TB, names sink.Names) (*sink.JSONL, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sink")
	s, err := sink.NewJSONL(sink.JSONLConfig{Dir: dir, Names: names})
	if err != nil {
		t.Fatalf("NewJSONL: %v", err)
	}
	return s, dir
}

// lines reads a tenant's file back, one document per line.
func lines(t testing.TB, dir string, tn tenancy.ID) [][]byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, tn.String()+".jsonl"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	trimmed := bytes.TrimSuffix(body, []byte("\n"))
	if len(trimmed) == 0 {
		return nil
	}
	return bytes.Split(trimmed, []byte("\n"))
}

// TestTheJSONLSinkWritesOneDocumentPerLine.
func TestTheJSONLSinkWritesOneDocumentPerLine(t *testing.T) {
	t.Parallel()
	s, dir := newJSONL(t, nil)
	recs := []record.Record{
		rec(t, tenantA, func(r *record.Record) { r.Version = "1" }),
		rec(t, tenantA, func(r *record.Record) {
			r.Version = "2"
			// Content that would end a line if it were written as it stands.
			r.Text = "first\nsecond\r\nthird"
		}),
	}
	if _, err := s.Deliver(context.Background(), tenantA, recs); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	got := lines(t, dir, tenantA)
	if len(got) != 2 {
		t.Fatalf("%d lines, want 2", len(got))
	}
	for i, line := range got {
		var back record.Record
		if err := json.Unmarshal(line, &back); err != nil {
			t.Fatalf("line %d does not decode: %v", i, err)
		}
		if back.ID != recs[i].ID {
			t.Errorf("line %d holds %s, want %s", i, back.ID, recs[i].ID)
		}
	}
}

// TestTheJSONLSinkAppends: a second batch goes after the first, and the file is a log.
func TestTheJSONLSinkAppends(t *testing.T) {
	t.Parallel()
	s, dir := newJSONL(t, nil)
	first := rec(t, tenantA, func(r *record.Record) { r.Version = "1" })
	second := rec(t, tenantA, func(r *record.Record) { r.Version = "2" })
	for _, r := range []record.Record{first, second, first} {
		if _, err := s.Deliver(context.Background(), tenantA, []record.Record{r}); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
	}
	got := lines(t, dir, tenantA)
	if len(got) != 3 {
		t.Fatalf("%d lines, want 3", len(got))
	}
	// Folding the file by id leaves what the receiver would have held had each record arrived
	// once, which is what idempotence on the id means for a file.
	folded := map[string][]byte{}
	for _, line := range got {
		var back record.Record
		if err := json.Unmarshal(line, &back); err != nil {
			t.Fatalf("line does not decode: %v", err)
		}
		folded[back.ID] = line
	}
	if len(folded) != 2 {
		t.Fatalf("the file folds to %d records, want 2", len(folded))
	}
	if !bytes.Equal(folded[first.ID], got[0]) {
		t.Errorf("the repeat of the first record is not the same bytes as the first write")
	}
}

// TestTheJSONLSinkKeepsTenantsApart: the tenant is the file.
func TestTheJSONLSinkKeepsTenantsApart(t *testing.T) {
	t.Parallel()
	s, dir := newJSONL(t, nil)
	if _, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)}); err != nil {
		t.Fatalf("Deliver A: %v", err)
	}
	if _, err := s.Deliver(context.Background(), tenantB, []record.Record{rec(t, tenantB)}); err != nil {
		t.Fatalf("Deliver B: %v", err)
	}
	if got := len(lines(t, dir, tenantA)); got != 1 {
		t.Errorf("tenant A's file has %d lines, want 1", got)
	}
	if got := len(lines(t, dir, tenantB)); got != 1 {
		t.Errorf("tenant B's file has %d lines, want 1", got)
	}
}

// TestTheJSONLSinkRefusesATenantItCannotParse: a tenant id reaches a file name, so it goes
// through tenancy.Parse first.
func TestTheJSONLSinkRefusesATenantItCannotParse(t *testing.T) {
	t.Parallel()
	s, dir := newJSONL(t, nil)
	recs := []record.Record{rec(t, tenantA)}
	for _, bad := range []tenancy.ID{"", ".", "..", "../outside", "a/b", "a\\b", "a b", tenancy.ID(strings.Repeat("t", 65))} {
		if _, err := s.Deliver(context.Background(), bad, recs); !errors.Is(err, tenancy.ErrInvalidID) {
			t.Errorf("Deliver(%q): %v, want ErrInvalidID", bad, err)
		}
		if _, err := s.Path(bad); !errors.Is(err, tenancy.ErrInvalidID) {
			t.Errorf("Path(%q): %v, want ErrInvalidID", bad, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the directory holds %v, want nothing", entries)
	}
}

// TestTheJSONLFilesAreTheOwnersAlone: a record holds a title, a text and an author.
func TestTheJSONLFilesAreTheOwnersAlone(t *testing.T) {
	t.Parallel()
	s, dir := newJSONL(t, nil)
	if _, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("the directory is %04o, want 0700", got)
	}
	path, err := s.Path(tenantA)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("the file is %04o, want 0600", got)
	}
}

// TestAppendLineRefusesADocumentWithALineFeed: json.Marshal writes a line feed inside a string
// as \n, so this is about a document that came from somewhere else. One with a line feed in it
// would be two lines of the file, the second of them a record nothing ever wrote.
func TestAppendLineRefusesADocumentWithALineFeed(t *testing.T) {
	t.Parallel()
	out, err := sink.AppendLine(nil, []byte(`{"id":"a"}`))
	if err != nil {
		t.Fatalf("a document with no line feed: %v", err)
	}
	if string(out) != `{"id":"a"}`+"\n" {
		t.Errorf("wrote %q", out)
	}
	before := bytes.Clone(out)
	out, err = sink.AppendLine(out, []byte("{\"id\":\"a\"}\n{\"id\":\"b\"}"))
	if err == nil {
		t.Fatalf("a document with a line feed was written")
	}
	if !bytes.Equal(out, before) {
		t.Errorf("the buffer changed to %q", out)
	}
}

// TestTheJSONLSinkRefusesADirectoryItCannotMake.
func TestTheJSONLSinkRefusesADirectoryItCannotMake(t *testing.T) {
	t.Parallel()
	if _, err := sink.NewJSONL(sink.JSONLConfig{}); err == nil {
		t.Errorf("an empty directory was accepted")
	}
	// A path whose parent is a file, not a directory.
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := sink.NewJSONL(sink.JSONLConfig{Dir: filepath.Join(file, "sink")}); err == nil {
		t.Errorf("a directory under a file was accepted")
	}
	if _, err := sink.NewJSONL(sink.JSONLConfig{Dir: t.TempDir(), Names: sink.Names{"clickup": "Up"}}); err == nil {
		t.Errorf("a wire name that is not a source name was accepted")
	}
}

// TestADeliveryThatCannotBeWrittenIsRetried.
func TestADeliveryThatCannotBeWrittenIsRetried(t *testing.T) {
	t.Parallel()
	s, dir := newJSONL(t, nil)
	// A directory where the file should be: opening it for writing fails.
	if err := os.Mkdir(filepath.Join(dir, tenantA.String()+".jsonl"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := s.Deliver(context.Background(), tenantA, []record.Record{rec(t, tenantA)})
	f := fault(t, err)
	if f.Action != sink.ActionRetry {
		t.Errorf("action %v, want retry", f.Action)
	}
	if got, want := f.Cause.String(), "internal error"; got != want {
		t.Errorf("cause %q, want %q", got, want)
	}
	if f.Detail != "the file could not be written" {
		t.Errorf("detail %q", f.Detail)
	}
	if strings.Contains(f.Error(), dir) {
		t.Errorf("the fault names the path: %v", f)
	}
}

// TestACancelledDeliveryWritesNothing.
func TestACancelledDeliveryWritesNothing(t *testing.T) {
	t.Parallel()
	s, dir := newJSONL(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Deliver(ctx, tenantA, []record.Record{rec(t, tenantA)})
	f := fault(t, err)
	if f.Action != sink.ActionRetry {
		t.Errorf("action %v, want retry", f.Action)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the directory holds %v, want nothing", entries)
	}
}

// TestAnEmptyBatchWritesNoFile.
func TestAnEmptyBatchWritesNoFile(t *testing.T) {
	t.Parallel()
	s, dir := newJSONL(t, nil)
	if _, err := s.Deliver(context.Background(), tenantA, nil); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the directory holds %v, want nothing", entries)
	}
}

// TestTheJSONLSinkIsSafeForConcurrentUse: two batches never interleave inside a line.
func TestTheJSONLSinkIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	s, dir := newJSONL(t, nil)
	const batches = 8
	const perBatch = 4
	var wg sync.WaitGroup
	for b := range batches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recs := make([]record.Record, perBatch)
			for i := range recs {
				recs[i] = rec(t, tenantA, func(r *record.Record) {
					r.Version = string(rune('a'+b)) + string(rune('a'+i))
				})
			}
			if _, err := s.Deliver(context.Background(), tenantA, recs); err != nil {
				t.Errorf("Deliver: %v", err)
			}
		}()
	}
	wg.Wait()
	got := lines(t, dir, tenantA)
	if len(got) != batches*perBatch {
		t.Fatalf("%d lines, want %d", len(got), batches*perBatch)
	}
	for i, line := range got {
		var back record.Record
		if err := json.Unmarshal(line, &back); err != nil {
			t.Fatalf("line %d does not decode: %v", i, err)
		}
	}
}
