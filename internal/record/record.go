// Package record is the record format: the envelope every sink receives, as Go types, and the
// JSON Schema that is its public contract (record.v1.schema.json, embedded as Schema).
//
// The format is settled in docs/adr/0004-record-format-v1.md and the scope id inside it in
// docs/adr/0003-scope-id-format.md. What a sink author needs to know is in the schema's
// descriptions and on the fields below. In short:
//
//   - One Record is one version of one entity, in one scope, for one tenant. Its ID is the
//     idempotency key. An edit, and a move to another scope, are each a new Record that names
//     the one it replaces in Supersedes.
//   - Access is decided on Visibility.Scope and on nothing else. A record carries its scope and
//     never the scope's members: membership travels through access sync, so a membership change
//     never re-delivers a record.
//   - The tenant is not in the envelope. It reaches the sink beside the records (Sink.Deliver
//     takes it), it is hashed into the ID, and a sink keys what it stores by tenant and scope
//     together.
//
// A Record that does not pass Validate cannot be marshalled, and a document the schema refuses
// cannot be unmarshalled into one, so nothing the format forbids passes through encoding/json
// in either direction. The Go side and the schema refuse the same things, and the tests run
// every case through both. The exceptions are the two rules no JSON Schema can express: a record
// that supersedes itself (Validate) and a field name used twice in one object (UnmarshalJSON).
package record

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gablooge/sluiceway/internal/ids"
	"github.com/gablooge/sluiceway/internal/tenancy"
)

// FormatV1 is the value of Record.Format. A sink learns which format it is reading from this
// field, because it is the only thing that is still there when a record sits in a file or a
// queue. Within v1 only fields a reader may ignore are added. Anything else is a new format with
// a new value: a field removed or renamed, a changed meaning, a new Op, Kind or Audience, any
// change inside Visibility.
const FormatV1 = "sluiceway.record/v1"

// Op says what the sink does with the record.
type Op string

const (
	// OpUpsert stores this version, replacing the one named by Supersedes if there is one.
	OpUpsert Op = "upsert"
	// OpDelete is a tombstone: the entity is gone at the source, and every stored version of
	// its ExternalID goes with it. Title and Text are empty. It is part of v1 so that shipping
	// deletions later does not change the format, and v0.1 never produces one. A sink that
	// cannot honour a delete refuses the record, it never ignores it.
	OpDelete Op = "delete"
)

// Kind says what the entity is. The set is closed: a new kind is a new format version.
type Kind string

// The kinds of v1.
const (
	KindTask     Kind = "task"
	KindMessage  Kind = "message"
	KindTicket   Kind = "ticket"
	KindDocument Kind = "document"
	KindPage     Kind = "page"
)

// Audience describes a scope. It is informational only: it grants and denies nothing, and a sink
// that reads it as "private" repeats the defect behind principle 9 of the architecture.
type Audience string

const (
	// AudienceDirect is a conversation or a mailbox of named participants.
	AudienceDirect Audience = "direct"
	// AudienceGroup is a shared space: a channel, a list, a portal.
	AudienceGroup Audience = "group"
)

// Record is the envelope. Every field that carries meaning is always present on the wire, with
// null or "" for "none", so a sink never has to tell an absent field from an empty one. Only
// Meta and what is inside it may be absent.
type Record struct {
	// Format is FormatV1. Seal sets it.
	Format string `json:"format"`
	// ID is "rec_" and 32 lowercase hex characters, minted by Seal from the provider key, the
	// ExternalID, the Version, the Visibility.Scope and the tenant. The format cannot check that
	// an id was minted that way, because the tenant is deliberately not in the envelope.
	ID string `json:"id"`
	Op Op     `json:"op"`
	// Source is the name the sink knows the source by. Seal sets it to the provider key, and a
	// sink configured with another wire name replaces it on the way out. It is in no id, and it
	// need not equal the first segment of Visibility.Scope, which is always the provider key.
	Source string `json:"source"`
	Kind   Kind   `json:"kind"`
	// ExternalID is the entity's identity at the source, the same for every version: 1 to
	// MaxExternalID characters, no control characters. Opaque.
	ExternalID string `json:"external_id"`
	// Version names this version of the entity: 1 to MaxVersion characters, no control
	// characters. The provider's normalizer promises that it changes whenever the entity
	// changes, and that for one ExternalID it never goes backwards. Nothing in the format can
	// check that promise. To a sink a version is opaque: equal or not equal, and the order of
	// versions is what Supersedes says.
	Version string `json:"version"`
	// Supersedes is the ID of the record this one replaces, or none (null on the wire). Links
	// point forward only, and a record never supersedes itself.
	Supersedes Ref `json:"supersedes"`
	// OccurredAt is when it happened at the source, never when it was ingested. It must be in
	// time.UTC (call UTC() on a parsed time) and in the years 1000 to 9999, which also refuses
	// the zero time.
	OccurredAt time.Time `json:"occurred_at"`
	// Title and Text may be empty, and are empty on a delete. Both are PII-masked before they
	// get here. At most MaxTitle and MaxText characters, and no NUL, which a Postgres text
	// column cannot hold. Cutting a longer text down is the normalizer's job.
	Title string `json:"title"`
	Text  string `json:"text"`
	// Author is informational. Access is never decided on it.
	Author    Author    `json:"author"`
	Container Container `json:"container"`
	// Visibility is everything access is decided on.
	Visibility Visibility `json:"visibility"`
	Origin     Origin     `json:"origin"`
	Edges      Edges      `json:"edges"`
	// Meta is diagnostics and not part of the record's content: the same ID may arrive again
	// with a different Meta.
	Meta Meta `json:"meta"`
}

// Author is who wrote the entity, in the source's own terms. Both fields may be empty when the
// source does not say (a record degraded to the webhook body, a system event). At most MaxAuthor
// characters each, no control characters.
type Author struct {
	ID      string `json:"id"`
	Display string `json:"display"`
}

// Container is where the entity lives at the source: the channel of a message, the task of a
// comment. It is often the container the scope is made of, and need not be (a comment lives in
// a task and is decided on the task's list).
type Container struct {
	// Kind follows the container kind grammar of a scope id: a lowercase letter, then up to 31
	// of a-z 0-9 _.
	Kind string `json:"kind"`
	// ID is the source's id exactly as the source spells it, not escaped: 1 to MaxContainerID
	// characters, no control characters.
	ID string `json:"id"`
}

// Visibility is closed. Unmarshalling a Record refuses a field it does not know in here, because
// a field added here could only be one a sink must not ignore.
type Visibility struct {
	// Scope is a scope id, built with ScopeID: a person may see the record if they are a member
	// of this scope, for the record's tenant.
	Scope    string   `json:"scope"`
	Audience Audience `json:"audience"`
}

// Origin says where the content came from, as far as it bears on trusting it.
type Origin struct {
	// Automation: written by a bot or an integration.
	Automation bool `json:"automation"`
	// Untrusted: written by somebody outside the tenant (inbound mail, an external guest).
	// Such text may be written to steer an AI agent that reads it.
	Untrusted bool `json:"untrusted"`
}

// Edges are relations to other entities, taken from fields of the source, never from the text.
type Edges struct {
	// ReplyParent is the ExternalID of the entity this one replies to, or none.
	ReplyParent Ref `json:"reply_parent"`
}

// Meta is diagnostics.
type Meta struct {
	// Delivery is Sluiceway's id of the accepted delivery the record was made from. Optional,
	// at most MaxDelivery characters.
	Delivery string `json:"delivery,omitempty"`
}

// Ref is a reference that may be missing. The zero value is "none" and is null on the wire. The
// empty string is not a reference, so unmarshalling refuses "".
type Ref string

// MarshalJSON writes null for no reference.
func (r Ref) MarshalJSON() ([]byte, error) {
	if r == "" {
		return []byte("null"), nil
	}
	return json.Marshal(string(r))
}

// UnmarshalJSON reads null as no reference and refuses an empty string.
func (r *Ref) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*r = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("%w: a reference is a string or null", ErrInvalid)
	}
	if s == "" {
		return fmt.Errorf("%w: a reference is null, never an empty string", ErrInvalid)
	}
	*r = Ref(s)
	return nil
}

// wire has Record's fields and none of its methods, so marshalling it does not come back here.
type wire Record

// MarshalJSON refuses a record that does not pass Validate. The error says which field, never
// what was in it.
func (r Record) MarshalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(wire(r))
}

// Seal finishes a record a normalizer built: it sets Format, sets Source to the provider key,
// mints the ID, and validates the result. It is the only way to an ID, so an id always hashes
// the scope the record carries, and a record with no tenant or no scope never gets one.
//
// provider is the internal provider key (Provider.Key), never a wire name. Seal refuses a scope
// that is not in that provider's namespace, so one provider cannot stamp a record into the scope
// of another. It refuses a Source that was already set to anything else, and a tenant that
// tenancy.Parse would refuse.
//
// Supersedes is usually set after sealing, once the ledger has been asked which record the new
// ID replaces. Nothing is lost by that order: marshalling validates again.
func (r Record) Seal(provider string, tenant tenancy.ID) (Record, error) {
	if !validName(provider, maxKind, false) {
		return Record{}, invalid("provider", "is not a provider key")
	}
	if r.Source != "" && r.Source != provider {
		return Record{}, invalid("source", "is set by Seal, a wire name is the sink's to set")
	}
	scope := r.Visibility.Scope
	if err := checkScopeID(scope); err != nil {
		return Record{}, invalid("visibility.scope", "is not a scope id")
	}
	if len(scope) <= len(provider) || scope[:len(provider)] != provider || scope[len(provider)] != ':' {
		return Record{}, invalid("visibility.scope", "belongs to another provider")
	}
	if _, err := tenancy.Parse(tenant.String()); err != nil {
		return Record{}, invalid("tenant", "is not a tenant id")
	}
	id, err := ids.RecordID(provider, r.ExternalID, r.Version, scope, tenant.String())
	if err != nil {
		return Record{}, invalid("id", "cannot be minted: "+err.Error())
	}
	r.Format = FormatV1
	r.Source = provider
	r.ID = id
	if err := r.Validate(); err != nil {
		return Record{}, err
	}
	return r, nil
}
