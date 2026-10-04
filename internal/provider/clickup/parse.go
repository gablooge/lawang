package clickup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/record"
)

// ErrBadDelivery reports a body ClickUp would not have sent, or one this package will not build
// a record out of. internal/pipeline treats a Parse error as a dead letter, because the same
// stored bytes produce the same answer next time, so every one of these is a delivery an
// operator sees rather than something quietly turned into a record.
//
// It never quotes the delivery.
var ErrBadDelivery = errors.New("clickup: bad delivery")

// maxID bounds every ClickUp identifier this package reads off a delivery. A task id is a short
// base-36 string ("86a1b2"), a custom task id is a key and a number ("ABC-1234"), a comment id
// and a list id are decimal, and a webhook id is a UUID, so 64 is far above all of them and far
// below anything that would be a problem in a URL path or an index.
const maxID = 64

// maxDigits bounds a ClickUp time, which is epoch milliseconds as a decimal string. Nineteen
// digits is what an int64 holds, so a longer run is refused here rather than overflowing.
//
// The arm is not redundant with strconv.ParseInt below, and removing it changes answers.
// ParseInt refuses an over-long VALUE; this arm refuses an over-long STRING, which is the wider
// set, because leading zeros are legal to ParseInt. Delete the arm and
// "00000000000001791100000000" parses to 1791100000000, where today it is refused as a time
// that is not epoch milliseconds. Nothing between the wire and here bounds the length of that
// string (json.Unmarshal into a string, historyDate.UnmarshalJSON, digits), so a padded run of
// any width arrives straight from a webhook body or an API response.
//
// Two cases pin it, one per path: TestAHistoryDateThatCannotBeReadIsSkipped's "a zero-padded
// date of twenty-six digits" and TestNormalizeRefuses's "a task with a zero-padded date_updated
// of twenty-six digits".
const maxDigits = 19

// subject is what a delivery is about: which entity changed, and therefore what is hydrated.
type subject uint8

const (
	subjectTask subject = iota + 1
	subjectComment
)

// handled is every ClickUp event this package turns into a change, and nothing else is accepted.
// The set is closed on purpose and the refusal is a dead letter: an event nobody has looked at
// is a delivery whose meaning is unknown, and guessing "it is probably a task update" is how a
// record gets built out of something that did not happen.
//
// Every task event is one change, because they all say the same thing to this normalizer: the
// task changed, re-read it. ClickUp sends taskUpdated alongside most of the others anyway, so a
// workspace registered for several of them re-delivers the same task, which the ledger skips
// when nothing moved.
//
// What is deliberately absent:
//
//   - taskDeleted. v0.1 produces no tombstone (ADR 4, decision 5, and architecture section 6),
//     and a deleted task cannot be hydrated, so there is nothing honest to build. Registered
//     (see Events) is therefore the smaller set, and a workspace that registers this event by
//     hand gets a visible dead letter rather than a silent loss.
//   - every event of another object (a list, a space, a folder, a goal). They carry no task, and
//     v0.1 records tasks and comments.
var handled = map[string]subject{
	"taskCreated":             subjectTask,
	"taskUpdated":             subjectTask,
	"taskMoved":               subjectTask,
	"taskStatusUpdated":       subjectTask,
	"taskPriorityUpdated":     subjectTask,
	"taskAssigneeUpdated":     subjectTask,
	"taskDueDateUpdated":      subjectTask,
	"taskTagUpdated":          subjectTask,
	"taskTimeEstimateUpdated": subjectTask,
	"taskTimeTrackedUpdated":  subjectTask,
	"taskCommentPosted":       subjectComment,
	"taskCommentUpdated":      subjectComment,
}

// Events are the ClickUp events Lawang registers a webhook for (B14 and B19 create the
// registration; this package owns the list, because it is this package that knows what it can
// turn into a record).
//
// It is smaller than handled: taskUpdated accompanies most other task events, taskMoved included
// (https://developer.clickup.com/docs/webhooktaskpayloads), so registering the narrow set keeps
// one change from arriving as three deliveries. handled stays wider so that a workspace
// registered by hand, or by an older Lawang, still resolves.
func Events() []string {
	return []string{"taskCreated", "taskUpdated", "taskCommentPosted", "taskCommentUpdated"}
}

// delivery is the part of a ClickUp webhook body this package reads. Unknown fields are ignored
// on purpose, which is the one place this package is not strict: ClickUp adds fields to its
// payloads, and a decoder that refused them would park every delivery of a workspace the day it
// happened. Everything this package does read is checked before it is used.
type delivery struct {
	Event     string        `json:"event"`
	WebhookID string        `json:"webhook_id"`
	TaskID    string        `json:"task_id"`
	History   []historyItem `json:"history_items"`
}

// historyItem is one entry of history_items: when the change happened and, for a comment event,
// the comment it is about. Only the comment's id is read, because the rest of what a webhook
// says about a comment is also in the API's answer, and the API's shape is the documented one.
type historyItem struct {
	Date    historyDate `json:"date"`
	Comment *struct {
		ID string `json:"id"`
	} `json:"comment"`
}

// historyDate is a history item's date, read leniently: one that cannot be read is skipped and
// never refuses the delivery.
//
// This is the same argument the delivery struct makes above, applied to a value rather than to a
// field name. The dates contribute the event time, which moves a version and is an optimisation
// (ADR 15, decision 4); they are not identity, and a delivery carrying NO history at all is
// already accepted, falling back to the task's own date_updated. So refusing the whole delivery
// over one unreadable date would buy nothing the lenient path does not already give, and would
// cost every delivery of a workspace, permanently, the day ClickUp ships a history item type
// whose date is absent, null or a JSON number. Identity (event, webhook_id, task_id, a comment
// event's comment id) stays strictly refused.
type historyDate struct {
	ms int64
	ok bool
}

// UnmarshalJSON reads a date that is epoch milliseconds as a decimal string, and records "no
// readable date" for anything else, including null, a number and an object. It returns no error
// on purpose: the decoder's error would be the whole delivery's, which is the failure this type
// exists to prevent.
func (h *historyDate) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if ms, err := epochMillis(s); err == nil {
			h.ms, h.ok = ms, true
		}
	}
	return nil
}

// keys is the smallest part of a body the hub needs: the webhook's own id. It is decoded on its
// own, and not through delivery, so that a body whose task id or event this package would refuse
// is still attributable: it reaches its owner's queue and dies there as a dead letter, instead of
// being parked under the sentinel tenant as unreadable.
type keys struct {
	WebhookID string `json:"webhook_id"`
}

// DeliveryKeys reads the webhook's own id out of a delivery. ClickUp puts no workspace or team id
// on a webhook body (https://developer.clickup.com/docs/webhooktaskpayloads), so the registration
// id is the only key, and the hub selects candidate subscriptions by it alone.
//
// Everything here is unauthenticated: the id says which rows are candidates and never which
// tenant this is.
func (p *Provider) DeliveryKeys(body []byte, _ provider.Header) (provider.DeliveryKeys, error) {
	var k keys
	if err := decode(body, &k); err != nil {
		return provider.DeliveryKeys{}, err
	}
	if !validID(k.WebhookID) {
		return provider.DeliveryKeys{}, badDelivery("the webhook id is missing or is not an identifier")
	}
	return provider.DeliveryKeys{Subscription: k.WebhookID}, nil
}

// Parse turns a verified delivery into its changes: exactly one, because a ClickUp delivery
// reports one event about one entity. A comment event becomes one change about the comment, and
// Normalize turns that into two records, the comment and its re-hydrated parent task.
//
// It runs in the worker, on the stored bytes.
func (p *Provider) Parse(body []byte) ([]provider.Change, error) {
	d, _, err := parseDelivery(body)
	if err != nil {
		return nil, err
	}
	externalID, err := d.externalID()
	if err != nil {
		return nil, err
	}
	return []provider.Change{{
		ExternalID: externalID,
		// Every change this package makes is an upsert. v0.1 sends no tombstone, and the one
		// event that would be one (taskDeleted) is not handled.
		Op: record.OpUpsert,
		// The delivery's own bytes, cloned: the accept path's buffer is not this slice's to
		// keep, and Hydrate reads the payload back.
		Payload: bytes.Clone(body),
	}}, nil
}

// parseDelivery decodes a body and checks everything this package will read out of it. It is the
// one place that does, so Parse and Hydrate cannot disagree about what a delivery says.
//
// The second result is the event time: the newest history item's date, in epoch milliseconds, or
// zero when the delivery carries no history and none it can read (see historyDate). It is what
// makes a version move on a change the task's own date_updated does not move on, a move between
// lists above all (ADR 4, decision 7, and ADR 15).
func parseDelivery(body []byte) (delivery, int64, error) {
	var d delivery
	if err := decode(body, &d); err != nil {
		return delivery{}, 0, err
	}
	if _, ok := handled[d.Event]; !ok {
		// The event name is a stranger's text, so it is not quoted. An operator reading the dead
		// letter has the stored body.
		return delivery{}, 0, badDelivery("the event is missing, or is not one this provider handles")
	}
	if !validID(d.WebhookID) {
		return delivery{}, 0, badDelivery("the webhook id is missing or is not an identifier")
	}
	if !validID(d.TaskID) {
		return delivery{}, 0, badDelivery("the task id is missing or is not an identifier")
	}
	var at int64
	for _, h := range d.History {
		if h.Date.ok {
			at = max(at, h.Date.ms)
		}
	}
	if handled[d.Event] == subjectComment {
		if id := d.commentID(); !validID(id) {
			return delivery{}, 0, badDelivery("a comment event carries no comment id")
		}
	}
	return d, at, nil
}

// commentID is the id of the comment a comment event is about: the first history item that has
// one. The documented path is history_items[].comment.id
// (https://developer.clickup.com/docs/webhooktaskpayloads), and it is the only field of that
// object this package reads.
func (d delivery) commentID() string {
	for _, h := range d.History {
		if h.Comment != nil && h.Comment.ID != "" {
			return h.Comment.ID
		}
	}
	return ""
}

// externalID is the entity's identity in the record format's namespaced form. It is the same
// string for every version of the entity, which is what the outbox and the ledger key on, so it
// is built from the provider's own ids and nothing else: never from a time, never from the
// event, and never from anything that was cleaned.
func (d delivery) externalID() (string, error) {
	switch handled[d.Event] {
	case subjectTask:
		return Key + ":task:" + d.TaskID, nil
	case subjectComment:
		return Key + ":comment:" + d.commentID(), nil
	default:
		// Unreachable: parseDelivery refuses an event that is not handled. It is here because a
		// thirteenth event added to handled with no subject must fail closed and not fall
		// through to "it is probably a task".
		return "", badDelivery("the event has no subject")
	}
}

// decode reads one JSON object out of body and nothing else.
//
// Three refusals matter. Bytes that are not UTF-8 are refused before the decoder sees them,
// because encoding/json rewrites them to U+FFFD and would make two different deliveries one.
// Anything after the object is refused, because a body with a second document in it is not a
// delivery. And the decoder's own message is dropped, because it quotes the input.
func decode(body []byte, into any) error {
	if !utf8.Valid(body) {
		return badDelivery("the body is not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(into); err != nil {
		return badDelivery("the body is not the JSON object a delivery is")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return badDelivery("the body carries more than one document")
	}
	return nil
}

// epochMillis reads one of ClickUp's times: epoch milliseconds as a decimal string. A time that
// is not a digit run is refused rather than guessed at, which also means a version this package
// mints is always one the pipeline can order under VersionOrderDecimal.
//
// It is the only place this package turns provider text into a time, which is why a leap second
// cannot reach a record from here: epoch milliseconds count no leap second, so 23:59:60 has no
// spelling in this format. A provider whose times are RFC 3339 has to clamp second 60 to 59
// before parsing; this one has nothing to clamp, and a string that looks like a timestamp is
// refused by the digit rule below rather than half read.
func epochMillis(s string) (int64, error) {
	if s == "" || len(s) > maxDigits || !digits(s) {
		return 0, badDelivery("a time is not epoch milliseconds")
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, badDelivery("a time is not epoch milliseconds")
	}
	return ms, nil
}

func digits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// validID reports whether s is one of ClickUp's own identifiers as this package will use it: 1 to
// maxID of A-Z a-z 0-9 underscore and hyphen.
//
// It is narrow on purpose, and it is never relaxed into cleaning. These strings become part of an
// external id and of a scope id, which the record format compares and never reads, so removing a
// character from one would give one entity two identities. They also become a path segment of a
// request to ClickUp, so a slash, a dot or a percent sign in one is a request to somewhere else
// (the path is escaped as well, which is the second of the two defences and not the first).
// An id this refuses is a refusal, not something to encode around.
func validID(s string) bool {
	if s == "" || len(s) > maxID {
		return false
	}
	for i := range len(s) {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}
