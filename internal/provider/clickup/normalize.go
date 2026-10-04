package clickup

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/tenancy"
)

// ErrBadObject reports a hydrated object that did not come from this provider's Hydrate, or one
// whose content ClickUp's API would not have returned. Like ErrBadDelivery it quotes nothing.
var ErrBadObject = errors.New("clickup: bad object")

// task is the part of ClickUp's task object this package reads
// (https://developer.clickup.com/reference/gettask). Everything else in the answer is ignored.
type task struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// TextContent is the description as plain text, and Description is the same text with
	// ClickUp's own markup. The plain one is what a record carries, and the other is the
	// fallback for an answer that omits it.
	TextContent string `json:"text_content"`
	Description string `json:"description"`
	// DateUpdated is epoch milliseconds as a decimal string, like every other ClickUp time.
	DateUpdated string `json:"date_updated"`
	Creator     *user  `json:"creator"`
	List        struct {
		ID string `json:"id"`
	} `json:"list"`
}

// comment is the part of a ClickUp comment this package reads
// (https://developer.clickup.com/reference/gettaskcomments).
type comment struct {
	ID          string `json:"id"`
	CommentText string `json:"comment_text"`
	Date        string `json:"date"`
	User        *user  `json:"user"`
}

// user is who ClickUp says did something. The id is a number in the API's JSON, and the username
// is a name somebody chose for themselves, which is why it is the one field here that is cleaned.
type user struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// Object is what Hydrate fetched: the task, the comment when the delivery was about one, and the
// delivery it was all fetched for. Its fields are unexported because only Normalize reads them,
// and because an Object a caller assembled by hand would be a second route to a record, with
// nothing holding the two together.
type Object struct {
	task    task
	comment *comment
	// d and at are what the delivery said, carried from Hydrate so that Normalize does not parse
	// the body a second time and cannot reach a different answer about it.
	d  delivery
	at int64
}

// Hydrate fetches the full objects a Change is about: always the task, and for a comment event
// the comment too, so that the normalizer can emit the comment and its re-hydrated parent.
//
// It is the only place this package talks to ClickUp. It runs in the worker, never on the accept
// path, and it honours ctx.
func (p *Provider) Hydrate(ctx context.Context, t tenancy.ID, c provider.Change) (provider.Hydrated, error) {
	if _, err := tenancy.Parse(t.String()); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadObject, err) // fail closed: no tenant, no token
	}
	d, at, err := parseDelivery(c.Payload)
	if err != nil {
		return nil, err
	}
	externalID, err := d.externalID()
	if err != nil {
		return nil, err
	}
	if externalID != c.ExternalID {
		// The stored payload is not this change's. Nothing good comes of hydrating one entity
		// and labelling it as another.
		return nil, fmt.Errorf("%w: the payload is not this change's", ErrBadObject)
	}
	token, err := p.api.tokens.Token(ctx, t)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoToken, err)
	}
	if token == "" {
		return nil, ErrNoToken
	}
	obj := Object{d: d, at: at}
	if obj.task, err = p.api.task(ctx, token, d.TaskID); err != nil {
		return nil, err
	}
	if handled[d.Event] == subjectComment {
		cm, err := p.api.comment(ctx, token, d.TaskID, d.commentID())
		if err != nil {
			return nil, err
		}
		obj.comment = &cm
	}
	return obj, nil
}

// Normalize turns what Hydrate fetched into the records of one change: one record for a task
// event, and two for a comment event, the parent task first and then the comment.
//
// The parent task comes first because a sink that follows edges.reply_parent meets the task
// before the comment that names it. Both are ordinary records: the task is the version ClickUp
// holds now, so a repeat of it (a second comment on a task nobody edited in between) carries the
// same version and the same scope, mints the same id and is skipped by the ledger.
//
// It does no I/O, and it cleans every field the record format refuses things in before it builds
// anything (record.CleanDisplay and record.CleanTitle). It cleans no identifier, ever.
func (p *Provider) Normalize(h provider.Hydrated, c provider.Change) ([]record.Record, error) {
	obj, ok := h.(Object)
	if !ok {
		return nil, fmt.Errorf("%w: not a clickup.Object", ErrBadObject)
	}
	externalID, err := obj.d.externalID()
	if err != nil {
		// The zero Object lands here: it carries no event, so it names no entity. It is a bad
		// object and not a bad delivery, because nothing read a delivery to produce it.
		return nil, fmt.Errorf("%w: the object carries no event this provider handles", ErrBadObject)
	}
	if externalID != c.ExternalID {
		return nil, fmt.Errorf("%w: the object is not this change's", ErrBadObject)
	}
	// The API's answer is remote text too, and these three become an external id, a container id
	// and a scope id, which are compared and never cleaned.
	if !validID(obj.task.ID) {
		return nil, fmt.Errorf("%w: the task the API returned has no usable id", ErrBadObject)
	}
	if !validID(obj.task.List.ID) {
		return nil, fmt.Errorf("%w: the task the API returned is in no list this provider can name", ErrBadObject)
	}
	updated, err := epochMillis(obj.task.DateUpdated)
	if err != nil {
		return nil, fmt.Errorf("%w: the task's date_updated: %w", ErrBadObject, err)
	}

	switch handled[obj.d.Event] {
	case subjectTask:
		// The subject of the change: its version moves with the change, whatever date_updated
		// did. See version below.
		r, err := obj.taskRecord(max(updated, obj.at))
		if err != nil {
			return nil, err
		}
		return []record.Record{r}, nil
	case subjectComment:
		if obj.comment == nil {
			return nil, fmt.Errorf("%w: a comment event with no comment", ErrBadObject)
		}
		// The parent task is context and not the subject, so its version is its own
		// date_updated and not the comment's time. A task nobody touched therefore keeps its
		// version, mints the id it already has, and is skipped by the ledger instead of being
		// re-delivered once per comment.
		parent, err := obj.taskRecord(updated)
		if err != nil {
			return nil, err
		}
		cm, err := obj.commentRecord()
		if err != nil {
			return nil, err
		}
		return []record.Record{parent, cm}, nil
	default:
		return nil, fmt.Errorf("%w: the event has no subject", ErrBadObject)
	}
}

// taskRecord is the record of the task, at the version it is given.
//
// The scope is the task's list: that is what access to a task is decided on in ClickUp, and it is
// built through record.ScopeID, which is the one function both sides of the join use (ADR 3).
func (o Object) taskRecord(versionMS int64) (record.Record, error) {
	scope, err := scopeFor(o.task.List.ID)
	if err != nil {
		return record.Record{}, err
	}
	at, err := occurredAt(versionMS)
	if err != nil {
		return record.Record{}, err
	}
	text := o.task.TextContent
	if text == "" {
		text = o.task.Description
	}
	return record.Record{
		Op:         record.OpUpsert,
		Kind:       record.KindTask,
		ExternalID: Key + ":task:" + o.d.TaskID,
		Version:    version(versionMS),
		OccurredAt: at,
		Title:      record.CleanTitle(o.task.Name),
		Text:       record.CleanText(text),
		Author:     author(o.task.Creator),
		Container:  record.Container{Kind: ContainerKindList, ID: o.task.List.ID},
		Visibility: record.Visibility{Scope: scope, Audience: record.AudienceGroup},
		// origin.automation is false because ClickUp gives no positive signal that an author is
		// a bot or an integration. A history item's "source" says the change came through the
		// API, which a person with a token does as readily as an integration, and false means NO
		// SIGNAL and never "a person wrote it" (ADR 4, decision 10).
		Origin: record.Origin{},
	}, nil
}

// commentRecord is the record of the comment: a message, living in the task, decided on the
// task's list, and naming the task as its reply parent.
//
// Its scope is deliberately the same string as the parent task's. A comment is visible to
// whoever can see the task, so deciding it on the task would need a scope per task and a
// membership push per task, for the same set of people.
func (o Object) commentRecord() (record.Record, error) {
	scope, err := scopeFor(o.task.List.ID)
	if err != nil {
		return record.Record{}, err
	}
	posted, err := epochMillis(o.comment.Date)
	if err != nil {
		return record.Record{}, fmt.Errorf("%w: the comment's date: %w", ErrBadObject, err)
	}
	// The subject of the change, so its version moves with the change: an edit to a comment does
	// not move the comment's own date, and the event's time does.
	versionMS := max(posted, o.at)
	at, err := occurredAt(versionMS)
	if err != nil {
		return record.Record{}, err
	}
	if !validID(o.comment.ID) {
		return record.Record{}, fmt.Errorf("%w: the comment the API returned has no usable id", ErrBadObject)
	}
	return record.Record{
		Op:         record.OpUpsert,
		Kind:       record.KindMessage,
		ExternalID: Key + ":comment:" + o.comment.ID,
		Version:    version(versionMS),
		OccurredAt: at,
		// A ClickUp comment has no title of its own, and inventing one out of its first line
		// would be content in a field a sink shows as a heading.
		Title:      "",
		Text:       record.CleanText(o.comment.CommentText),
		Author:     author(o.comment.User),
		Container:  record.Container{Kind: ContainerKindTask, ID: o.d.TaskID},
		Visibility: record.Visibility{Scope: scope, Audience: record.AudienceGroup},
		Origin:     record.Origin{},
		Edges:      record.Edges{ReplyParent: record.Ref(Key + ":task:" + o.d.TaskID)},
	}, nil
}

// scopeFor is the one function that turns a ClickUp list id into a scope id. Everything that
// mints a ClickUp scope goes through it: the record id hashes the scope, so two routes to a
// scope are two ids for one version of one entity (ADR 4, decision 7). Access sync (B23) will
// call record.ScopeID with the same three parts for the same list.
//
// Its error cannot happen today, and it is kept rather than dropped. Normalize holds the list id
// to validID first, which is narrower than the scope id grammar in every direction (64 bytes of
// A-Z a-z 0-9 _ -, where ScopeID allows 512 bytes of anything it can escape), so no list id that
// reaches here can be refused there. That is a check standing in front of another one, which is
// how the second silently stops being exercised: no test covers this branch, and none can
// without reaching around validID. ScopeID owns the grammar (ADR 3), so the call stays as it is
// and the result stays checked.
func scopeFor(listID string) (string, error) {
	scope, err := record.ScopeID(Key, ContainerKindList, listID)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBadObject, err)
	}
	return scope, nil
}

// version is how this provider spells a version: the decimal epoch milliseconds of the newest
// moment ClickUp reports for the entity, which is a digit run and therefore exactly what
// provider.VersionOrderDecimal is.
//
// **It moves on every change ClickUp reports, a move between lists included**, which is what ADR
// 4 decision 7 requires of a normalizer and what the record id alone cannot give. A task's own
// date_updated is not enough: whether ClickUp moves it when a task changes list is not something
// the documentation says, and a version that did not move on a move leaves the A to B and back
// to A case (one id reused in two scopes) for the ledger to dead-letter. So the version of the
// entity a change is ABOUT is the later of the entity's own timestamp and the delivery's own
// event time, which a move always moves. The parent task re-hydrated beside a comment is not the
// subject of that change and keeps its own timestamp, so a task nobody edited is not re-delivered
// once per comment.
//
// Monotonic per external id, which is the promise ADR 4 makes to the pipeline: both numbers are
// epoch milliseconds of events that have happened, and a later change carries a later one.
func version(ms int64) string { return strconv.FormatInt(ms, 10) }

// occurredAt is when it happened at the source, in UTC, from epoch milliseconds.
//
// The record format wants a year between 1000 and 9999 and refuses anything else, so a time
// outside that is refused here, by name, rather than as a sealing failure three stages later.
// Epoch milliseconds count no leap second, so a second 60 cannot arrive through this function.
func occurredAt(ms int64) (time.Time, error) {
	t := time.UnixMilli(ms).UTC()
	if y := t.Year(); y < 1000 || y > 9999 {
		return time.Time{}, fmt.Errorf("%w: a time is outside the years the record format holds", ErrBadObject)
	}
	return t, nil
}

// author is what the record says about who wrote the entity. It is informational: access is
// never decided on it.
//
// The id is ClickUp's own numeric user id and is NEVER cleaned, because it is an identifier that
// something downstream compares. The display name is a name somebody chose for themselves, which
// is where a zero-width space, a right-to-left override or a line break would be planted, so it
// is the field the cleaner is for. The email ClickUp also returns is deliberately not carried:
// nothing in a record needs it, and it is the most personal thing in the answer.
func author(u *user) record.Author {
	if u == nil {
		return record.Author{} // the source did not say, which the format allows
	}
	return record.Author{
		ID:      strconv.FormatInt(u.ID, 10),
		Display: record.CleanDisplay(u.Username),
	}
}
