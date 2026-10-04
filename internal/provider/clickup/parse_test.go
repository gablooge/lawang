package clickup_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/clickup"
	"github.com/gablooge/lawang/internal/record"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newProvider is a provider with no API behind it, for the tests that never hydrate.
func newProvider(t *testing.T) *clickup.Provider {
	t.Helper()
	p, err := clickup.New(clickup.Options{Tokens: fixedToken("placeholder-token")})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A delivery ClickUp documents becomes exactly one change, named the way the record format names
// an entity.
func TestParseTheDocumentedDeliveries(t *testing.T) {
	p := newProvider(t)
	for _, tc := range []struct {
		file       string
		externalID string
	}{
		{"webhook_task_updated.json", "clickup:task:86a1b2"},
		{"webhook_task_moved.json", "clickup:task:86a1b2"},
		{"webhook_comment_posted.json", "clickup:comment:648893191"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			body := fixture(t, tc.file)
			changes, err := p.Parse(body)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(changes) != 1 {
				t.Fatalf("got %d changes, want 1", len(changes))
			}
			c := changes[0]
			if c.ExternalID != tc.externalID {
				t.Errorf("external id %q, want %q", c.ExternalID, tc.externalID)
			}
			if c.Op != record.OpUpsert {
				t.Errorf("op %q, want upsert", c.Op)
			}
			if !bytes.Equal(c.Payload, body) {
				t.Error("the payload is not the bytes the delivery arrived as")
			}
			// The payload must be the change's own copy: the accept path's buffer is reused.
			if len(c.Payload) > 0 && &c.Payload[0] == &body[0] {
				t.Error("the payload aliases the caller's slice")
			}
		})
	}
}

// Everything Parse refuses, and the reason it refuses it. A refusal here is a dead letter
// (internal/pipeline wraps it), so each of these is a delivery an operator will see rather than
// a record quietly built out of something else.
func TestParseRefuses(t *testing.T) {
	p := newProvider(t)
	for _, tc := range []struct{ name, body string }{
		{"empty", ""},
		{"not json", "not json"},
		{"a json array", `[{"event":"taskUpdated"}]`},
		{"trailing data", `{"event":"taskUpdated","task_id":"a","webhook_id":"w"} {}`},
		{"no event", `{"task_id":"a","webhook_id":"w"}`},
		{"an event Lawang does not handle", `{"event":"taskDeleted","task_id":"a","webhook_id":"w"}`},
		{"an event of another object", `{"event":"listUpdated","list_id":"1","webhook_id":"w"}`},
		{"an unknown event", `{"event":"somethingNew","task_id":"a","webhook_id":"w"}`},
		{"no webhook id", `{"event":"taskUpdated","task_id":"a"}`},
		{"no task id", `{"event":"taskUpdated","webhook_id":"w"}`},
		{"an empty task id", `{"event":"taskUpdated","task_id":"","webhook_id":"w"}`},
		{"a task id with a slash", `{"event":"taskUpdated","task_id":"a/../b","webhook_id":"w"}`},
		{"a task id with a space", `{"event":"taskUpdated","task_id":"a b","webhook_id":"w"}`},
		{"a task id with a NUL", "{\"event\":\"taskUpdated\",\"task_id\":\"a\u0000b\",\"webhook_id\":\"w\"}"},
		{"a task id that is too long", `{"event":"taskUpdated","task_id":"` + strings.Repeat("a", 65) + `","webhook_id":"w"}`},
		{"a comment event with no history", `{"event":"taskCommentPosted","task_id":"a","webhook_id":"w","history_items":[]}`},
		{"a comment event with no comment id", `{"event":"taskCommentPosted","task_id":"a","webhook_id":"w","history_items":[{"id":"1","date":"1","field":"comment"}]}`},
		{"a comment event with an empty comment id", `{"event":"taskCommentPosted","task_id":"a","webhook_id":"w","history_items":[{"id":"1","date":"1","field":"comment","comment":{"id":""}}]}`},
		{"a comment event with a bad comment id", `{"event":"taskCommentPosted","task_id":"a","webhook_id":"w","history_items":[{"id":"1","date":"1","field":"comment","comment":{"id":"a:b"}}]}`},
		{"a history date that is not a number", `{"event":"taskUpdated","task_id":"a","webhook_id":"w","history_items":[{"id":"1","date":"yesterday"}]}`},
		{"a history date that is negative", `{"event":"taskUpdated","task_id":"a","webhook_id":"w","history_items":[{"id":"1","date":"-1"}]}`},
		// Nineteen digits fits the length bound and not an int64, which is the one way past the
		// digit check and into strconv's own refusal.
		{"a history date larger than an int64", `{"event":"taskUpdated","task_id":"a","webhook_id":"w","history_items":[{"id":"1","date":"9999999999999999999"}]}`},
		{"a history date of twenty digits", `{"event":"taskUpdated","task_id":"a","webhook_id":"w","history_items":[{"id":"1","date":"12345678901234567890"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changes, err := p.Parse([]byte(tc.body))
			if err == nil {
				t.Fatalf("accepted it, and returned %d changes", len(changes))
			}
			if !errors.Is(err, clickup.ErrBadDelivery) {
				t.Errorf("error is not an ErrBadDelivery: %v", err)
			}
			if changes != nil {
				t.Error("returned changes beside the error")
			}
		})
	}
}

// A body that is not UTF-8 is refused before anything reads it as text. encoding/json would
// rewrite the bad bytes to U+FFFD, which turns two different deliveries into one.
func TestParseRefusesBytesThatAreNotUTF8(t *testing.T) {
	body := []byte(`{"event":"taskUpdated","task_id":"86a1b2","webhook_id":"w1","x":"` + "\xff" + `"}`)
	if _, err := newProvider(t).Parse(body); !errors.Is(err, clickup.ErrBadDelivery) {
		t.Fatalf("Parse: %v, want an ErrBadDelivery", err)
	}
}

// An error from Parse never quotes the delivery. The bytes came from the public internet, and
// the error reaches a log line and the outbox's last_error column.
func TestParseErrorsNeverQuoteTheDelivery(t *testing.T) {
	const marker = "sensitive-marker-value"
	body := []byte(`{"event":"somethingNew","task_id":"` + marker + `","webhook_id":"` + marker + `"}`)
	_, err := newProvider(t).Parse(body)
	if err == nil {
		t.Fatal("accepted an unknown event")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("the error quotes the delivery: %v", err)
	}
}

// ClickUp puts one identifier on a delivery, the webhook's own id, and no workspace id at all
// (https://developer.clickup.com/docs/webhooktaskpayloads). The hub narrows candidate
// subscriptions by the keys it is given, so a Workspace invented here would select the wrong
// rows, and a missing Subscription would mean no row could ever be selected.
func TestDeliveryKeysAreTheWebhookIDAlone(t *testing.T) {
	p := newProvider(t)
	keys, err := p.DeliveryKeys(fixture(t, "webhook_task_updated.json"), provider.Header{})
	if err != nil {
		t.Fatalf("DeliveryKeys: %v", err)
	}
	if keys.Subscription != "7fa3ec74-0000-4000-8000-000000000001" {
		t.Errorf("subscription key %q", keys.Subscription)
	}
	if keys.Workspace != "" {
		t.Errorf("workspace key %q, want none: a ClickUp delivery carries none", keys.Workspace)
	}
}

// A delivery whose keys cannot be read is parked as "unreadable delivery", which is what the hub
// does with the error. It must never come back as an empty key, which would be looked up.
func TestDeliveryKeysRefuses(t *testing.T) {
	p := newProvider(t)
	for _, tc := range []struct{ name, body string }{
		{"empty", ""},
		{"not json", "nope"},
		{"no webhook id", `{"event":"taskUpdated","task_id":"a"}`},
		{"an empty webhook id", `{"event":"taskUpdated","task_id":"a","webhook_id":""}`},
		{"a webhook id with a control character", "{\"webhook_id\":\"w\u0001\"}"},
		{"a webhook id that is too long", `{"webhook_id":"` + strings.Repeat("w", 129) + `"}`},
		{"not utf8", "{\"webhook_id\":\"\xff\"}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := p.DeliveryKeys([]byte(tc.body), provider.Header{})
			if err == nil {
				t.Fatalf("accepted it and returned %+v", keys)
			}
			if keys != (provider.DeliveryKeys{}) {
				t.Errorf("returned keys beside the error: %+v", keys)
			}
		})
	}
}

// The delivery keys and the parse read one body, and they must agree about the webhook id and
// the event: a delivery whose keys select a subscription but which Parse then refuses is a row
// accepted into a tenant's queue only to die there.
func TestDeliveryKeysAndParseAgree(t *testing.T) {
	p := newProvider(t)
	for _, name := range []string{"webhook_task_updated.json", "webhook_task_moved.json", "webhook_comment_posted.json"} {
		body := fixture(t, name)
		if _, err := p.DeliveryKeys(body, provider.Header{}); err != nil {
			t.Errorf("%s: DeliveryKeys: %v", name, err)
		}
		if _, err := p.Parse(body); err != nil {
			t.Errorf("%s: Parse: %v", name, err)
		}
	}
}

// The key is the one ADR 3 freezes, and the registry validates it with record.ValidProviderKey.
// It is hashed into every record id, so changing it re-keys everything ClickUp ever delivered.
func TestTheKeyIsAProviderKey(t *testing.T) {
	p := newProvider(t)
	if p.Key() != clickup.Key {
		t.Errorf("Key() is %q but the constant is %q", p.Key(), clickup.Key)
	}
	if !record.ValidProviderKey(p.Key()) {
		t.Errorf("%q is not a provider key", p.Key())
	}
	if _, err := provider.NewRegistry(p); err != nil {
		t.Errorf("the registry refuses this provider: %v", err)
	}
}

// Every event Lawang registers a webhook for is an event Parse accepts. They are two lists in
// one package, and the registrar reads one while the drain reads the other: an event registered
// that Parse refuses would park every delivery of it, for ever, and the registration would look
// correct from both ends.
func TestEveryRegisteredEventIsOneParseAccepts(t *testing.T) {
	p := newProvider(t)
	events := clickup.Events()
	if len(events) == 0 {
		t.Fatal("no events are registered, so no webhook would ever deliver anything")
	}
	for _, event := range events {
		// One body that satisfies both subjects: a comment event needs the comment id, and a
		// task event ignores it.
		body := `{"event":"` + event + `","task_id":"86a1b2","webhook_id":"w1","history_items":[` +
			`{"id":"1","date":"1791100000000","field":"comment","comment":{"id":"648893191"}}]}`
		if _, err := p.Parse([]byte(body)); err != nil {
			t.Errorf("%s is registered and Parse refuses it: %v", event, err)
		}
	}
}

// ClickUp spells every time as a decimal string of epoch milliseconds (date_updated on a task,
// date on a comment and on a history item), so the versions this normalizer mints are decimal
// runs ordered by their number. Declaring lexical instead would be wrong the first time an
// entity's version crossed a digit-count boundary, which for epoch milliseconds is rare and not
// impossible, and declaring base64 would refuse every version.
func TestVersionOrderIsDecimal(t *testing.T) {
	if got := newProvider(t).VersionOrder(); got != provider.VersionOrderDecimal {
		t.Errorf("VersionOrder is %v, want decimal", got)
	}
}
