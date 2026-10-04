package clickup_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/clickup"
	"github.com/gablooge/lawang/internal/provider/providertest"
	"github.com/gablooge/lawang/internal/record"
)

var update = flag.Bool("update", false, "rewrite the files in testdata/golden")

// normalized runs one delivery through the whole provider: parse, hydrate against the fake API,
// normalize, and seal for a tenant. Sealing is part of it on purpose, because an unsealed record
// cannot be marshalled and the golden files are the bytes a sink receives.
func normalized(t *testing.T, p *clickup.Provider, body []byte) []record.Record {
	t.Helper()
	c, h := hydrate(t, p, body)
	recs, err := p.Normalize(h, c)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	sealed := make([]record.Record, 0, len(recs))
	for i, r := range recs {
		s, err := r.Seal(clickup.Key, testTenant)
		if err != nil {
			t.Fatalf("record %d does not seal: %v", i, err)
		}
		sealed = append(sealed, s)
	}
	return sealed
}

// The golden files pin the records a ClickUp delivery becomes: every field, the scope, the
// version, the id. Run with -update after a deliberate change and read the diff, because an id
// that moves re-keys everything already delivered.
//
// Every payload behind them is synthesized from ClickUp's documentation (testdata/README.md).
// B12 re-records them against a real workspace, and these tests then say whether anything the
// documentation implies is wrong.
func TestNormalizeGolden(t *testing.T) {
	for _, tc := range []struct{ name, webhook, task string }{
		{"task_updated", "webhook_task_updated.json", "api_task.json"},
		{"task_moved", "webhook_task_moved.json", "api_task_moved.json"},
		{"comment_posted", "webhook_comment_posted.json", "api_task.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, api := newHydrating(t)
			api.taskBody = fixture(t, tc.task)
			got, err := json.MarshalIndent(normalized(t, p, fixture(t, tc.webhook)), "", "  ")
			if err != nil {
				t.Fatalf("the records do not marshal: %v", err)
			}
			got = append(got, '\n')

			path := filepath.Join("testdata", "golden", tc.name+".json")
			if *update {
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from what the normalizer produces.\n--- got\n%s\n--- want\n%s", path, got, want)
			}
		})
	}
}

// A comment yields two records: the comment and its re-hydrated parent task, the parent first.
// This is the line the backlog item names.
func TestACommentYieldsTheCommentAndItsParentTask(t *testing.T) {
	p, _ := newHydrating(t)
	recs := normalized(t, p, fixture(t, "webhook_comment_posted.json"))
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	parent, comment := recs[0], recs[1]
	if parent.Kind != record.KindTask || parent.ExternalID != "clickup:task:86a1b2" {
		t.Errorf("the first record is %s %q, want the parent task", parent.Kind, parent.ExternalID)
	}
	if comment.Kind != record.KindMessage || comment.ExternalID != "clickup:comment:648893191" {
		t.Errorf("the second record is %s %q, want the comment", comment.Kind, comment.ExternalID)
	}
	if comment.Edges.ReplyParent != record.Ref(parent.ExternalID) {
		t.Errorf("the comment's reply parent is %q, want %q", comment.Edges.ReplyParent, parent.ExternalID)
	}
	// Access to a comment is decided on the task's list, which is the task's own scope. The
	// container is where it lives, which is the task, and the two are deliberately different.
	if comment.Visibility.Scope != parent.Visibility.Scope {
		t.Errorf("the comment is decided on %q and the task on %q, and they must be one scope",
			comment.Visibility.Scope, parent.Visibility.Scope)
	}
	if comment.Container.Kind != clickup.ContainerKindTask || comment.Container.ID != "86a1b2" {
		t.Errorf("the comment lives in %s %q, want the task", comment.Container.Kind, comment.Container.ID)
	}
	if parent.Container.Kind != clickup.ContainerKindList || parent.Container.ID != "901100" {
		t.Errorf("the task lives in %s %q, want the list", parent.Container.Kind, parent.Container.ID)
	}
	if comment.ID == parent.ID {
		t.Error("the comment and the task have one record id")
	}
}

// The parent task re-hydrated beside a comment is not the subject of that change, so a task
// nobody edited keeps its version and mints the id it already has. Without that, every comment
// would re-deliver its task under a new id.
func TestASecondCommentDoesNotReDeliverTheTask(t *testing.T) {
	p, api := newHydrating(t)
	first := normalized(t, p, fixture(t, "webhook_comment_posted.json"))
	api.commentsBody = commentsAnswer(t, "648893192", "1791500000000", "placeholder-commenter")
	second := normalized(t, p, commentDelivery("648893192", "1791500000000"))
	if first[0].ID != second[0].ID {
		t.Errorf("the task has two ids across two comments (%s then %s), so the ledger will not skip it",
			first[0].ID, second[0].ID)
	}
	if first[0].Version != second[0].Version {
		t.Errorf("the task's version moved from %q to %q although the task did not change",
			first[0].Version, second[0].Version)
	}
	// The premise: the two deliveries really were different comments, so this is not two
	// identical runs agreeing with each other.
	if first[1].ID == second[1].ID {
		t.Error("the two comments have one record id")
	}
}

// A version moves on every change ClickUp reports, a move between lists included, and a task
// that moves away and back does not reproduce an id it has already used.
//
// That last one is ADR 4 decision 7's remaining hole, which the ledger can only dead-letter:
// the way out is on the normalizer, and this is it. The three deliveries below carry one
// unchanged date_updated on purpose, which is the case the hole needs.
func TestAMoveAndAMoveBackMoveTheVersion(t *testing.T) {
	p, api := newHydrating(t)

	api.taskBody = fixture(t, "api_task.json") // list 901100
	inOne := normalized(t, p, fixture(t, "webhook_task_updated.json"))[0]

	api.taskBody = fixture(t, "api_task_moved.json") // list 901200
	inTwo := normalized(t, p, fixture(t, "webhook_task_moved.json"))[0]

	api.taskBody = fixture(t, "api_task.json") // back in list 901100
	backInOne := normalized(t, p, movedBackDelivery())[0]

	// The premise this test rests on: ClickUp's own timestamp never moved, so nothing here is
	// saved by date_updated.
	for _, body := range [][]byte{fixture(t, "api_task.json"), fixture(t, "api_task_moved.json")} {
		if !bytes.Contains(body, []byte(`"date_updated": "1791100000000"`)) {
			t.Fatal("the fixtures no longer share one date_updated, so this test no longer tests what it says")
		}
	}
	if inOne.Visibility.Scope != backInOne.Visibility.Scope {
		t.Fatalf("the task did not come back to its first scope (%q then %q)",
			inOne.Visibility.Scope, backInOne.Visibility.Scope)
	}
	if inTwo.Visibility.Scope == inOne.Visibility.Scope {
		t.Fatal("the move did not change the scope, so this test no longer tests what it says")
	}

	versions := []string{inOne.Version, inTwo.Version, backInOne.Version}
	ids := []string{inOne.ID, inTwo.ID, backInOne.ID}
	for i := range versions {
		for j := i + 1; j < len(versions); j++ {
			if versions[i] == versions[j] {
				t.Errorf("deliveries %d and %d carry one version %q", i, j, versions[i])
			}
			if ids[i] == ids[j] {
				t.Errorf("deliveries %d and %d carry one record id %q", i, j, ids[i])
			}
		}
	}
	// The versions are decimal and they grow, which is what provider.VersionOrderDecimal
	// promises the pipeline.
	for i := 1; i < len(versions); i++ {
		if len(versions[i]) < len(versions[i-1]) || versions[i] <= versions[i-1] {
			t.Errorf("version %q does not come after %q", versions[i], versions[i-1])
		}
	}
}

// Every record this provider mints survives what the sink contract does to it: it marshals, and
// it comes back out of the store as the same record, under the same tenant, with the op and the
// kind the row says (B09 and B10, and record.Reopen).
func TestTheRecordsSurviveTheStoreAndTheSink(t *testing.T) {
	p, _ := newHydrating(t)
	for _, name := range []string{"webhook_task_updated.json", "webhook_comment_posted.json"} {
		for i, r := range normalized(t, p, fixture(t, name)) {
			doc, err := json.Marshal(r)
			if err != nil {
				t.Fatalf("%s record %d does not marshal: %v", name, i, err)
			}
			stored := record.Stored{ID: r.ID, Op: r.Op, Kind: r.Kind, Document: doc}
			back, err := record.Reopen(stored, clickup.Key, testTenant)
			if err != nil {
				t.Fatalf("%s record %d does not reopen: %v", name, i, err)
			}
			if !back.SealedFor(testTenant) {
				t.Errorf("%s record %d is not sealed for the tenant it was stored for", name, i)
			}
			// The op and the kind are the two the id does not hash, which is why B10 gave them
			// columns of their own. A record whose columns disagree with its document is
			// refused, and these do agree.
			if _, err := record.Reopen(record.Stored{ID: r.ID, Op: record.OpDelete, Kind: r.Kind, Document: doc},
				clickup.Key, testTenant); !errors.Is(err, record.ErrNotTheRecordStored) {
				t.Errorf("%s record %d: a disagreeing op was not refused: %v", name, i, err)
			}
		}
	}
}

// Forgetting to clean must be hard. The harness plants what a source really sends in the fields
// the record format really refuses things in, and every one of them has to come back as a
// record that seals.
func TestConformance(t *testing.T) {
	providertest.CheckCleaning(t, clickup.Key, func(t *testing.T, c providertest.Case) []record.Record {
		t.Helper()
		p, api := newHydrating(t)
		// The hostile title is the task's name, and the hostile display name is the author of
		// both the task and the comment, so one delivery covers every field that is cleaned.
		api.taskBody = taskAnswer(t, "901100", "1791100000000", c.Title, c.Display)
		api.commentsBody = commentsAnswer(t, "648893191", "1791100600000", c.Display)
		change, h := hydrate(t, p, fixture(t, "webhook_comment_posted.json"))
		recs, err := p.Normalize(h, change)
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		return recs
	})
}

// A task's text is content, and the format refuses two things in it: a NUL, which a Postgres
// text column cannot hold, and more than a megabyte of characters. Cutting a text down is the
// normalizer's job (the format refuses and never truncates), so a task with both still becomes
// a record.
//
// The conformance harness does not cover this one: it plants a display name and a title, which
// are the fields the reviews of #43 named, and a text is the third field class.
func TestALongTextWithANULStillBecomesARecord(t *testing.T) {
	p, api := newHydrating(t)
	long := strings.Repeat("a", record.MaxText+100) + "\x00tail"
	api.taskBody = taskAnswer(t, "901100", "1791100000000", "a title", "an author", long)
	recs := normalized(t, p, fixture(t, "webhook_task_updated.json"))
	if len(recs) != 1 {
		t.Fatalf("got %d records", len(recs))
	}
	got := recs[0].Text
	if n := len([]rune(got)); n != record.MaxText {
		t.Errorf("the text is %d characters, want it cut to %d", n, record.MaxText)
	}
	if strings.ContainsRune(got, 0) {
		t.Error("the text still holds a NUL")
	}
}

// A task or a comment whose author ClickUp does not name becomes a record with an empty author,
// which the format allows, rather than a refusal. "The source did not say" is not a failure.
func TestAnEntityWithNoAuthorStillBecomesARecord(t *testing.T) {
	p, api := newHydrating(t)
	api.taskBody = []byte(`{"id":"86a1b2","name":"a title","date_updated":"1791100000000",` +
		`"creator":null,"list":{"id":"901100"}}`)
	api.commentsBody = []byte(`{"comments":[{"id":"648893191","comment_text":"a comment",` +
		`"date":"1791100600000","user":null}]}`)
	recs := normalized(t, p, fixture(t, "webhook_comment_posted.json"))
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	for i, r := range recs {
		if r.Author != (record.Author{}) {
			t.Errorf("record %d has author %+v, want none", i, r.Author)
		}
	}
}

// A comment whose own fields the API returns in a shape this package will not use is a refusal,
// not a record with a guessed time or a guessed id.
func TestACommentTheAPIReturnsBadly(t *testing.T) {
	for _, tc := range []struct{ name, comments string }{
		{"no date", `{"comments":[{"id":"648893191","comment_text":"c"}]}`},
		{"a date that is not a number", `{"comments":[{"id":"648893191","comment_text":"c","date":"today"}]}`},
		{"a date in the year 70000", `{"comments":[{"id":"648893191","comment_text":"c","date":"2000000000000000"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, api := newHydrating(t)
			api.commentsBody = []byte(tc.comments)
			c, h := hydrate(t, p, fixture(t, "webhook_comment_posted.json"))
			if _, err := p.Normalize(h, c); !errors.Is(err, clickup.ErrBadObject) {
				t.Fatalf("%v, want ErrBadObject", err)
			}
		})
	}
}

// What Normalize refuses. Each of these is a dead letter rather than a record built out of
// something the API did not say.
func TestNormalizeRefuses(t *testing.T) {
	p, _ := newHydrating(t)
	change, good := hydrate(t, p, fixture(t, "webhook_task_updated.json"))

	t.Run("an object from somewhere else", func(t *testing.T) {
		for _, h := range []provider.Hydrated{nil, struct{ A int }{1}, "a string", clickup.Object{}} {
			if _, err := p.Normalize(h, change); !errors.Is(err, clickup.ErrBadObject) {
				t.Errorf("%T: %v, want ErrBadObject", h, err)
			}
		}
	})
	t.Run("another change's object", func(t *testing.T) {
		other := provider.Change{ExternalID: "clickup:task:somethingelse"}
		if _, err := p.Normalize(good, other); !errors.Is(err, clickup.ErrBadObject) {
			t.Errorf("%v, want ErrBadObject", err)
		}
	})
	for _, tc := range []struct{ name, task string }{
		{"a task with no list", `{"id":"86a1b2","name":"n","date_updated":"1791100000000"}`},
		{"a task with a list id that is not an identifier", `{"id":"86a1b2","name":"n","date_updated":"1791100000000","list":{"id":"a/b"}}`},
		{"a task with no id", `{"name":"n","date_updated":"1791100000000","list":{"id":"901100"}}`},
		{"a task with no date_updated", `{"id":"86a1b2","name":"n","list":{"id":"901100"}}`},
		{"a task with a date_updated that is not a number", `{"id":"86a1b2","name":"n","date_updated":"yesterday","list":{"id":"901100"}}`},
		{"a task updated in the year 70000", `{"id":"86a1b2","name":"n","date_updated":"2000000000000000","list":{"id":"901100"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, api := newHydrating(t)
			api.taskBody = []byte(tc.task)
			c, h := hydrate(t, p, fixture(t, "webhook_task_updated.json"))
			recs, err := p.Normalize(h, c)
			if !errors.Is(err, clickup.ErrBadObject) {
				t.Fatalf("%v, want ErrBadObject", err)
			}
			if recs != nil {
				t.Error("returned records beside the error")
			}
		})
	}
}

// The scope is built by record.ScopeID and by nothing else, which is what keeps it equal to the
// one access sync will push for the same list (ADR 3).
func TestTheScopeIsTheListThroughScopeID(t *testing.T) {
	p, _ := newHydrating(t)
	want, err := record.ScopeID(clickup.Key, clickup.ContainerKindList, "901100")
	if err != nil {
		t.Fatal(err)
	}
	got := normalized(t, p, fixture(t, "webhook_task_updated.json"))[0]
	if got.Visibility.Scope != want {
		t.Errorf("scope %q, want %q", got.Visibility.Scope, want)
	}
	if got.Visibility.Audience != record.AudienceGroup {
		t.Errorf("audience %q, want group: a ClickUp list is a shared space", got.Visibility.Audience)
	}
}

// A record carries no email address. ClickUp returns one for every user it names, and the
// record format has nowhere it belongs: author.id is the numeric user id, and an address in a
// record would travel to every sink.
func TestNoRecordCarriesAnEmailAddress(t *testing.T) {
	p, _ := newHydrating(t)
	for _, name := range []string{"webhook_task_updated.json", "webhook_comment_posted.json"} {
		for i, r := range normalized(t, p, fixture(t, name)) {
			doc, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			// The fixtures put an address in the task's text on purpose, which the masker
			// removes later (internal/pipeline), so only the fields this provider fills are
			// searched.
			for field, value := range map[string]string{
				"author.display": r.Author.Display,
				"author.id":      r.Author.ID,
				"title":          r.Title,
			} {
				if strings.Contains(value, "@example.invalid") {
					t.Errorf("%s record %d: %s carries an address: %q", name, i, field, value)
				}
			}
			if !bytes.Contains(doc, []byte(`"id": "183"`)) && !bytes.Contains(doc, []byte(`"id":"183"`)) &&
				!bytes.Contains(doc, []byte(`"id":"184"`)) && !bytes.Contains(doc, []byte(`"id": "184"`)) {
				t.Errorf("%s record %d carries no numeric author id, so this test proves nothing: %s", name, i, doc)
			}
		}
	}
}

// commentDelivery is a taskCommentPosted body for another comment, built here rather than kept
// as a fixture because what varies is two values.
func commentDelivery(commentID, at string) []byte {
	return fmt.Appendf(nil, `{"event":"taskCommentPosted","task_id":"86a1b2",`+
		`"webhook_id":"7fa3ec74-0000-4000-8000-000000000001","history_items":[`+
		`{"id":"1","type":1,"date":%q,"field":"comment","parent_id":"86a1b2",`+
		`"comment":{"id":%q,"date":%q}}]}`, at, commentID, at)
}

// movedBackDelivery is the third delivery of the move test: the same task, moved back to the
// list it started in, at a later moment.
func movedBackDelivery() []byte {
	return []byte(`{"event":"taskUpdated","task_id":"86a1b2",` +
		`"webhook_id":"7fa3ec74-0000-4000-8000-000000000001","history_items":[` +
		`{"id":"3","type":1,"date":"1791300000000","field":"section_moved","parent_id":"901100",` +
		`"before":{"id":"901200"},"after":{"id":"901100"}}]}`)
}

// taskAnswer and commentsAnswer are the API's answers with the values a test wants in them. They
// are marshalled rather than spliced, so that a hostile name cannot break the JSON and the test
// goes on testing the normalizer rather than the decoder.
func taskAnswer(t *testing.T, listID, updated, name, author string, text ...string) []byte {
	t.Helper()
	content := "a placeholder description"
	if len(text) == 1 {
		content = text[0]
	}
	b, err := json.Marshal(map[string]any{
		"id":           "86a1b2",
		"name":         name,
		"text_content": content,
		"date_updated": updated,
		"creator":      map[string]any{"id": 183, "username": author},
		"list":         map[string]any{"id": listID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func commentsAnswer(t *testing.T, id, date, author string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"comments": []any{map[string]any{
		"id":           id,
		"comment_text": "a placeholder comment",
		"date":         date,
		"user":         map[string]any{"id": 184, "username": author},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
