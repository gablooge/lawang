package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// File modes for the JSONL sink. A record holds a title, a text and an author, so the directory
// and the files are the owner's alone.
const (
	jsonlDirMode  os.FileMode = 0o700
	jsonlFileMode os.FileMode = 0o600
)

// JSONLConfig configures a JSONL sink.
type JSONLConfig struct {
	// Dir is the directory the per-tenant files live in. It is created if it is not there.
	Dir string
	// Names is the wire name per provider key (principle 4). It may be nil.
	Names Names
}

// JSONL appends records to one file per tenant, "<tenant>.jsonl" under the configured directory,
// one document per line. It is the development sink: somewhere to look at what the pipeline
// produced without standing a receiver up.
//
// The tenant is the file, which is how a sink whose records carry no tenant keeps two tenants
// apart. The name comes from tenancy.Parse, which admits A-Z, a-z, 0-9, underscore and hyphen
// and so no separator, no dot and nothing empty.
//
// # What idempotence on the record id means for a file
//
// The file is a log and the sink appends to it: a record delivered twice is written twice. A
// reader folds the file by id, where a later line for an id replaces an earlier one. Two lines
// that share an id are the same record, because the id covers the entity, the version, the
// scope and the tenant (ADR 4 decision 7), and what they may differ in is meta, which is not
// part of a record's content.
//
// A JSONL is safe for concurrent use within one process. Its mutex is what keeps two batches
// from interleaving inside a line, so one process owns the directory: a second process appending
// to the same file is outside what this guards.
type JSONL struct {
	dir   string
	names Names
	mu    sync.Mutex
}

// NewJSONL builds a JSONL sink and creates its directory.
func NewJSONL(cfg JSONLConfig) (*JSONL, error) {
	if err := cfg.Names.Validate(); err != nil {
		return nil, err
	}
	if cfg.Dir == "" {
		return nil, errors.New("sink: jsonl: no directory")
	}
	if err := os.MkdirAll(cfg.Dir, jsonlDirMode); err != nil {
		return nil, fmt.Errorf("sink: jsonl: %w", err)
	}
	return &JSONL{dir: cfg.Dir, names: cfg.Names}, nil
}

// Deliver appends the batch to the tenant's file. The whole batch is one write, so a reader
// never sees half of it.
func (j *JSONL) Deliver(ctx context.Context, t tenancy.ID, recs []record.Record) (DeliveryResult, error) {
	if err := checkTenant(t); err != nil {
		return DeliveryResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return DeliveryResult{}, transportFault(ctx)
	}
	var (
		result DeliveryResult
		lines  []byte
	)
	for _, r := range recs {
		doc, err := json.Marshal(j.names.rename(r))
		if err == nil {
			lines, err = appendLine(lines, doc)
		}
		if err != nil {
			result.Rejected = append(result.Rejected, Rejection{
				ID:     r.ID,
				Cause:  outbox.NewCause(outbox.ClassInternal),
				Detail: detailInvalidRecord,
			})
		}
	}
	if len(lines) == 0 {
		return result, nil
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := os.OpenFile(j.path(t), os.O_APPEND|os.O_CREATE|os.O_WRONLY, jsonlFileMode)
	if err != nil {
		return DeliveryResult{}, writeFault()
	}
	if _, err := f.Write(lines); err != nil {
		_ = f.Close()
		return DeliveryResult{}, writeFault()
	}
	if err := f.Close(); err != nil {
		return DeliveryResult{}, writeFault()
	}
	return result, nil
}

// Path is the file this sink writes a tenant's records to.
func (j *JSONL) Path(t tenancy.ID) (string, error) {
	if err := checkTenant(t); err != nil {
		return "", err
	}
	return j.path(t), nil
}

func (j *JSONL) path(t tenancy.ID) string {
	return filepath.Join(j.dir, t.String()+".jsonl")
}

// appendLine adds one document and a newline to lines.
//
// It reads the document for a newline first. encoding/json writes a line feed inside a string as
// \n, so a document it produced has none, and this looks at the bytes rather than resting on
// that: a document with one in it would be two lines of the file, and the second would be a
// record nothing ever wrote.
func appendLine(lines, doc []byte) ([]byte, error) {
	if bytes.IndexByte(doc, '\n') >= 0 {
		return lines, errors.New("sink: jsonl: the document holds a line feed")
	}
	lines = append(lines, doc...)
	return append(lines, '\n'), nil
}

// writeFault is the Fault for a file that could not be written. It names no path: the failure is
// Lawang's own, and the row goes back on the ladder for the operator who fixes the disk.
func writeFault() *Fault {
	return &Fault{
		Action: ActionRetry,
		Cause:  outbox.NewCause(outbox.ClassInternal),
		Detail: detailWriteFailed,
	}
}
