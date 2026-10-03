package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// What the Stub refuses beyond what the format refuses.
var (
	// ErrTooLarge reports a document over the stub's configured size. It is answered before the
	// document is parsed.
	ErrTooLarge = errors.New("sink: stub: the document is larger than this sink accepts")
	// ErrRepeatDiffers reports a record id that has arrived before with other content. One id is
	// one version of one entity in one scope for one tenant, so the two documents disagree about
	// something the id stands for.
	ErrRepeatDiffers = errors.New("sink: stub: this record id arrived before with different content")
	// ErrSupersedeCycle reports a supersedes chain that leads back to a record already on it.
	ErrSupersedeCycle = errors.New("sink: stub: the supersedes chain comes back to a record it has already passed")
)

// DefaultMaxDocumentBytes is the document size the stub accepts by default. It is the same
// number as DefaultMaxRequestBytes, whose comment has the arithmetic: at the format's limits a
// document encoding/json writes from a Record sums to a little over 6.3 MB.
const DefaultMaxDocumentBytes = 8 << 20

// StubConfig configures a Stub. Every field is optional.
type StubConfig struct {
	// Names is the wire name per provider key (principle 4).
	Names Names
	// MaxDocumentBytes is the largest document Accept will look at. Zero means
	// DefaultMaxDocumentBytes.
	MaxDocumentBytes int
}

// Stub is the strict sink: the test double of principle 5, which exists because a lenient one let
// a class of mis-routed records pass every end-to-end test in the predecessor.
//
// It decodes every document with internal/record's strict decoder. The agreement tests in
// internal/record hold that decoder and the JSON Schema to the same verdict, and
// TestTheStubRefusesWhateverTheSchemaRefuses runs a corpus of edited documents through both.
// Four refusals go beyond a schema. Three of them are rules of the format that no JSON Schema
// can state (ADR 4):
//
//   - the document's bytes are UTF-8 and no \u escape in it names half of a surrogate pair,
//     checked before parsing, because encoding/json puts U+FFFD in place of either and so turns
//     two different documents into one identity;
//   - no field name occurs twice in one object, because decoders disagree about which of the two
//     wins, so a validator and a consumer can read two different scopes out of one document;
//   - supersedes is not the record's own id.
//
// The fourth is the stub's own, and it is what a receiver has that a validator does not, which is
// memory of what it already holds: a record id that arrives again with different content
// (ErrRepeatDiffers). ADR 4 decision 7 promises a sink that one id never appears with two scopes,
// and this is the check that promise rests on.
//
// Two more things a sink author can read off it. The size limit comes before the decoder, because
// nothing in the format bounds a document once unknown fields are counted. And the walk over the
// supersedes chain carries a visited set, because a cycle across records is invisible to anything
// that validates one record at a time.
//
// A Stub is safe for concurrent use.
type Stub struct {
	names    Names
	maxBytes int

	mu      sync.Mutex
	tenants map[tenancy.ID]*stubTenant
}

// stubTenant is what one tenant's receiver holds. Tenants are kept apart here because a sink keys
// what it stores by tenant and the record's own fields together: the tenant is in no field of the
// envelope, so two tenants' records can otherwise meet under one key.
type stubTenant struct {
	order []string
	docs  map[string][]byte
	// content is each document's content, which is what a repeat is compared against. It is
	// kept beside the document so that a repeat canonicalizes one document and not two.
	content    map[string][]byte
	supersedes map[string]string
}

// NewStub builds a Stub, or reports what is wrong with the configuration.
func NewStub(cfg StubConfig) (*Stub, error) {
	if err := cfg.Names.Validate(); err != nil {
		return nil, err
	}
	maxBytes := cfg.MaxDocumentBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxDocumentBytes
	}
	if maxBytes < 1 {
		return nil, errors.New("sink: stub: the maximum document size is not a size")
	}
	return &Stub{names: cfg.Names, maxBytes: maxBytes, tenants: map[tenancy.ID]*stubTenant{}}, nil
}

// Deliver marshals each record under the configured wire name and hands the document to Accept,
// which is the same door a document off the wire comes through. A record Accept refuses is one
// Rejection and the rest of the batch still lands, which is the "sink rejects one record" row of
// architecture section 11.
func (s *Stub) Deliver(_ context.Context, t tenancy.ID, recs []record.Record) (DeliveryResult, error) {
	if err := checkTenant(t); err != nil {
		return DeliveryResult{}, err
	}
	var result DeliveryResult
	for _, r := range recs {
		doc, err := json.Marshal(s.names.rename(r))
		if err != nil {
			result.Rejected = append(result.Rejected, Rejection{
				ID:     r.ID,
				Cause:  outbox.NewCause(outbox.ClassInternal),
				Detail: detailInvalidRecord,
			})
			continue
		}
		if err := s.Accept(t, doc); err != nil {
			result.Rejected = append(result.Rejected, Rejection{
				ID:    r.ID,
				Cause: outbox.NewCause(outbox.ClassSinkRejected).WithCode(stubCode(err)),
			})
		}
	}
	return result, nil
}

// stubCode is the stub's own error code for a refusal, in the place a receiver's documented
// error code goes. The switch below is the whole list: the three sentinels of this file, the
// format's own ErrInvalid, and "refused" for an error that is none of them, so no text from a
// decoder travels into the outbox column.
func stubCode(err error) string {
	switch {
	case errors.Is(err, ErrTooLarge):
		return "too_large"
	case errors.Is(err, ErrRepeatDiffers):
		return "repeat_differs"
	case errors.Is(err, ErrSupersedeCycle):
		return "supersede_cycle"
	case errors.Is(err, record.ErrInvalid):
		return "invalid_record"
	default:
		return "refused"
	}
}

// Accept takes one record document for a tenant, exactly as a receiver off the wire would get it,
// and stores it. A repeat of an id it already holds whose content matches is a no-op, which is
// what idempotence on the record id means for a receiver.
//
// The error says which rule refused the document. It never quotes the document: a title, a text
// and an author are personal data.
func (s *Stub) Accept(t tenancy.ID, doc []byte) error {
	if err := checkTenant(t); err != nil {
		return err
	}
	// Before the decoder, and before anything else looks at the bytes. Nothing in the format
	// bounds a document, because unknown fields are allowed and unbounded, and the strict
	// decoder allocated about 35 times a document's size on the one that was measured (ADR 4, in
	// the notes carried onto issue #9). The sink's own limit is the bound there is.
	if len(doc) > s.maxBytes {
		return ErrTooLarge
	}
	var r record.Record
	if err := json.Unmarshal(doc, &r); err != nil {
		return fmt.Errorf("sink: stub: %w", err)
	}
	content, err := contentOf(doc)
	if err != nil {
		return fmt.Errorf("sink: stub: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	held := s.tenants[t]
	if held == nil {
		held = &stubTenant{
			docs:       map[string][]byte{},
			content:    map[string][]byte{},
			supersedes: map[string]string{},
		}
		s.tenants[t] = held
	}
	if before, ok := held.content[r.ID]; ok {
		if !bytes.Equal(before, content) {
			return ErrRepeatDiffers
		}
		return nil
	}
	if held.leadsBack(r.ID, string(r.Supersedes)) {
		return ErrSupersedeCycle
	}
	held.order = append(held.order, r.ID)
	held.docs[r.ID] = bytes.Clone(doc)
	held.content[r.ID] = content
	held.supersedes[r.ID] = string(r.Supersedes)
	return nil
}

// leadsBack walks the supersedes chain that would start at id and reports whether it comes back
// to a record already on it. A record may name one that has not arrived yet, which ends the walk.
//
// The visited set is what ends the walk: each step either names an id the walk has already
// passed, and answers true, or one more of the ids this tenant holds, of which there are
// finitely many. A cycle is spread over several records, and each record on it names a
// different one, so the one-record checks have nothing to compare: Validate's self-supersede
// rule is the case where the cycle is one record long (the note on issue #9).
func (st *stubTenant) leadsBack(id, supersedes string) bool {
	visited := map[string]bool{id: true}
	for next := supersedes; next != ""; next = st.supersedes[next] {
		if visited[next] {
			return true
		}
		visited[next] = true
	}
	return false
}

// Documents returns the documents the stub holds for a tenant, in the order they first arrived.
// The slices are copies.
func (s *Stub) Documents(t tenancy.ID) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	held := s.tenants[t]
	if held == nil {
		return nil
	}
	docs := make([][]byte, 0, len(held.order))
	for _, id := range held.order {
		docs = append(docs, bytes.Clone(held.docs[id]))
	}
	return docs
}

// contentOf is a record's content: the document decoded, meta taken out, and written again.
// meta is not part of the content (ADR 4, "Required, empty, null, absent"), because the same id
// may arrive again carrying a different meta.delivery and that is the same record.
//
// Writing it again is what makes the comparison about content and not about spelling: a decoded
// object is a map, and encoding/json writes a map's members in sorted order, so the order the
// members arrived in does not reach the comparison, and nor does the way the document escaped
// its strings, which decoding has already undone. Numbers keep the literal they were written
// with (json.Number), so "1" and "1.0" in an unknown field are two contents.
//
// It compares documents and never record.Record values. A sealed Record and the same record
// decoded from its own document are unequal under == and under reflect.DeepEqual, because only
// the sealed one knows its tenant, although the two marshal to identical bytes
// (record.Record.SealedFor says so). Comparing values would refuse a legitimate repeat.
func contentOf(doc []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	if fields, ok := tree.(map[string]any); ok {
		delete(fields, "meta")
	}
	return json.Marshal(tree)
}
