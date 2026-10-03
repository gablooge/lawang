// Package sink delivers records to whatever receives them. It holds the Sink interface of
// docs/architecture.md section 7 and the three implementations v0.1 ships: HTTP (the format over
// an HTTP endpoint), Stub (the strict test double of principle 5) and JSONL (a file per tenant,
// for development).
//
// Three things are the same for all of them, and a fourth is the reason this package exists.
//
//   - The tenant is not in the envelope. It is handed to Deliver beside the records, and each
//     sink decides what it is on the wire: for HTTP the per-tenant credential the request
//     carries, for JSONL the file the records are written to, for Stub the key they are stored
//     under (architecture section 4).
//   - The wire name is sink configuration (principle 4). Names maps the internal provider key
//     that Seal put in Record.Source to the name this sink's receiver knows that source by, and
//     each sink applies it on the way out. Nothing else is rewritten.
//   - Every record goes out through record.Record.MarshalJSON, which validates it and checks its
//     seal, so what a sink writes is a document the schema accepts.
//   - A failure is reported as a *Fault: an Action for the worker and an outbox.Cause for the
//     outbox. Neither takes text from the other end. A Cause is a class, a status and a code
//     that outbox.Cause.WithCode filters, and a Fault's Detail is one of the phrases
//     faultDetails lists. The accident that shape is for is recording err.Error(): an HTTP
//     client's error is a *url.Error whose text quotes the request URL, and a sink URL is where
//     an API key often travels.
package sink

import (
	"context"
	"fmt"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// Sink receives records. It is idempotent on Record.ID: delivering a record a second time leaves
// the receiver holding what it already held, because one id is one version of one entity in one
// scope for one tenant, and the worker re-delivers after a crash between its two commits
// (architecture section 3.2, step 7).
//
// Deliver answers in one of exactly two ways, and the worker's two jobs follow from which:
//
//   - A DeliveryResult and no error. Every record of the batch has an outcome: the ones in
//     Rejected were refused and dead-letter one by one, and every other record was taken.
//   - A *Fault. The DeliveryResult is the zero value, nothing in the batch counts as delivered,
//     and the whole batch goes again: at once on ActionRetry, and after an operator has fixed
//     the credential on ActionHalt. The repeat costs nothing, because a sink is idempotent on
//     Record.ID.
//
// So a Fault is always about the delivery and never about a record, and a record's own fate is
// only ever reported as a Rejection. That is what makes a batch safe to send in several requests:
// a sink that cannot deliver all of a batch says so for the batch, and never kills a record it
// has not offered.
type Sink interface {
	Deliver(ctx context.Context, t tenancy.ID, recs []record.Record) (DeliveryResult, error)
}

// DeliveryResult is what one batch came to, and it covers every record in it: a record not named
// in Rejected was taken.
type DeliveryResult struct {
	// Rejected names the records that did not get through, one entry each. Every other record
	// of the batch landed, so each of these dead-letters on its own (architecture section 11).
	Rejected []Rejection
}

// Rejection is one record that did not get through, with what the outbox stores for it.
type Rejection struct {
	// ID is the Record.ID of the refused record.
	ID string
	// Cause is the classification and the facts that go into the dead letter.
	Cause outbox.Cause
	// Detail is one of this package's own phrases, the ones faultDetails lists, or empty. It is
	// set where Lawang itself refused to send the record, and it is empty where the receiver
	// refused it, because then the receiver's own error code is in the Cause. Like Fault.Detail
	// it is for a log line and is not stored in the outbox.
	Detail string
}

// Action is what the worker does about a Fault. There are two, because a Fault is about the
// delivery and never about a record: both leave the batch undelivered and send it again, and
// they differ in what has to happen first. The third sink row of architecture section 11, a
// record that dead-letters, is a Rejection and not a Fault, so that a sink never asks for a
// record to be killed without having offered it.
type Action uint8

const (
	// ActionUnset is the zero value, which no Fault built in this package carries.
	ActionUnset Action = iota
	// ActionRetry puts the row back on the backoff ladder, which dead-letters it at its end.
	ActionRetry
	// ActionHalt leaves the row prepared and marks nothing delivered: the credential was
	// refused, so sending it again changes nothing and an operator has to look.
	ActionHalt
)

// String names the action for an operator reading a log line.
func (a Action) String() string {
	switch a {
	case ActionRetry:
		return "retry"
	case ActionHalt:
		return "halt"
	case ActionUnset:
		return "unclassified"
	default:
		return fmt.Sprintf("unknown action %d", uint8(a))
	}
}

// Fault is a delivery that did not happen, in the only form a sink reports one. It is about the
// batch as a whole: a record the receiver refused is a Rejection in the DeliveryResult instead,
// so a partly delivered batch that then faults still names the records the receiver refused and
// still leaves the records it never saw alive.
//
// It carries an Action, an outbox.Cause and a Detail, and it wraps nothing: errors.Unwrap of a
// Fault is nil, and a transport error is read for its classification and then dropped. That is
// deliberate. The error of net/http is a *url.Error whose text quotes the request URL with its
// query string; Fault.Error() ends up in a worker's log line, and the Cause ends up in
// outbox.last_error, a plain column that every backup carries.
//
// Detail is one of this package's own phrases, the ones faultDetails lists. It is not stored in
// the outbox: it is there so a log line can tell a refused connection from a host name that does
// not resolve.
type Fault struct {
	Action Action
	Cause  outbox.Cause
	Detail string
}

// Error is the text of a fault: the action, the cause, and the detail when there is one.
func (f *Fault) Error() string {
	text := "sink: " + f.Action.String() + ": " + f.Cause.String()
	if f.Detail != "" {
		text += ": " + f.Detail
	}
	return text
}

// Names is sink configuration: what this sink's receiver calls each source (principle 4). A key
// is an internal provider key, which is what record.Seal puts in Record.Source, and its value is
// the wire name that replaces it on the way out. A record whose Source is not a key of the map
// keeps the provider key, and a nil Names renames nothing.
//
// Only Source is replaced. The first segment of Visibility.Scope stays the internal provider key,
// because the scope id is the join key between a record and its scope's membership (ADR 3), and
// so does the prefix of ExternalID, which is what makes an external id unique within a tenant
// (ADR 4). So Source and the first segment of the scope need not match, and a sink treats the
// scope as opaque.
type Names map[string]string

// Validate reports an entry of n that could not be used: a key that is not a provider key
// (record.ValidProviderKey, ADR 3), or a value that is not a source name (record.ValidSource). A
// sink calls it where it is built, rather than discovering a bad name one record at a time. With
// two bad entries it names one of them, and a map has no order, so which one is not fixed.
func (n Names) Validate() error {
	for key, wire := range n {
		if !record.ValidProviderKey(key) {
			return fmt.Errorf("sink: wire name: %q is not a provider key", key)
		}
		if !record.ValidSource(wire) {
			return fmt.Errorf("sink: wire name for %q: it is not a source name", key)
		}
	}
	return nil
}

// rename returns r with Source set to the wire name configured for it. Source is outside what
// Seal sealed, so the record still marshals.
func (n Names) rename(r record.Record) record.Record {
	if wire, ok := n[r.Source]; ok {
		r.Source = wire
	}
	return r
}

// checkTenant refuses a tenant id that tenancy.Parse refuses. Every Deliver starts here: ID is a
// string type, so a caller can build one without Parse, and a sink puts the tenant in a file name
// or looks a credential up by it.
func checkTenant(t tenancy.ID) error {
	if _, err := tenancy.Parse(t.String()); err != nil {
		return fmt.Errorf("sink: %w", err)
	}
	return nil
}
