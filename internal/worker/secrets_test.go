package worker_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/provider"
	"github.com/gablooge/lawang/internal/provider/fake"
	"github.com/gablooge/lawang/internal/record"
	"github.com/gablooge/lawang/internal/sink"
	"github.com/gablooge/lawang/internal/tenancy"
	"github.com/gablooge/lawang/internal/worker"
)

// marker is the secret the drain must not repeat. It is planted in the text of every value
// that reaches the drain from outside this package, shaped like the thing that really travels
// there: an API key in the query string of a URL, which is where several receivers and several
// provider APIs take one.
const marker = "sUp3rS3cr3tMark3r"

func markerURL() string { return "https://api.example/v1/things?api_key=" + marker }

// codeMarker is the second marker, and it is planted somewhere the drain deliberately writes a
// value down rather than somewhere it must not: in the code of an outbox.Cause.
//
// outbox.Cause.WithCode is a shape filter and not a trust boundary (at most 64 bytes of
// [A-Za-z0-9_.-], which a bot token, a base64url run and a hex run all pass), and the Cause of
// a *sink.Fault or a sink.Rejection is chosen by the sink. A receiver's own error code reaching
// last_error is B04's design, written on Rejection.Detail and on sink.Fault, so this is not a
// leak to close here. It is a kept value, and a kept value has to be pinned on both sides or
// the next reader takes the surrounding claim literally: the cases below say exactly which log
// attribute and which column hold it, and the assertion fails if a seventh place starts to.
const codeMarker = "c0d3Mark3r"

// markerProvider is the fake provider with an API that fails the way a real client's does: a
// *url.Error whose text quotes the request URL with its query string. Degrade refuses as well,
// so the failure reaches pipeline.Normalize's caller instead of being degraded away.
//
// It stands in for a THIRD-PARTY provider, which is the point: provider.Provider is an
// interface with no constraint on its error values, and from B11 a provider holds an HTTP
// client of its own.
type markerProvider struct{ *fake.Provider }

func (markerProvider) Hydrate(context.Context, tenancy.ID, provider.Change) (provider.Hydrated, error) {
	return nil, &url.Error{Op: "Get", URL: markerURL(), Err: errors.New("connection refused")}
}

func (markerProvider) Degrade(provider.Change) ([]record.Record, error) {
	return nil, fmt.Errorf("%w: the degraded path went to %s too", provider.ErrCannotDegrade, markerURL())
}

// TestNoThirdPartyTextReachesAnythingTheDrainWrites is B09's marker acceptance (issue #9,
// internal/sink's TestASinkURLSecretNeverLeaves) extended to the drain, which is the layer that
// was left out of it.
//
// internal/sink took the secret out of everything it hands back, and then this package grew log
// attributes that print values it did not build. Every one of them crosses an interface
// somebody else implements, so no argument about what the sinks in THIS repository return
// covers them, and the type system permits any value:
//
//  1. pipeline.Normalize, whose one outside call is the provider's API;
//  2. Sinks.Sink, which from B13 is a vault lookup;
//  3. sink.Sink.Deliver, both when it breaks its contract with a plain error and when it keeps
//     the contract with a *sink.Fault whose Detail is not one of internal/sink's own phrases;
//  4. a Rejection, whose Detail and whose id are the sink's own text.
//
// What is searched is everything the drain writes: every line of its own logger, the default
// logger (which it must not touch at all), and the text columns of outbox and outbox_record,
// which are plain text that every backup carries.
//
// Each case asserts first that it drove the drain to the line it is about. A case that stopped
// producing that line would otherwise pass by searching nothing, which is how the same test one
// layer down went wrong in its first round.
func TestNoThirdPartyTextReachesAnythingTheDrainWrites(t *testing.T) {
	// Not parallel: each case replaces the default logger while the drain runs.
	type attempt struct {
		// brokenProvider installs the provider whose API fails with the marker in it.
		brokenProvider bool
		// setUp breaks the sink side after the delivery has been accepted.
		setUp func(e *env)
		// wantLog is a message the drain must write, so that the case searches the line it is
		// here to search.
		wantLog string
		// codeAttrs and codeCols are for a case that plants codeMarker: the log attributes and
		// the stored columns that hold it, exactly. Both are checked in both directions, so a
		// kept value that spreads to a seventh place and a kept value that quietly stops being
		// written both fail here. A case that plants nothing leaves them nil, and then the
		// code must appear nowhere at all.
		codeAttrs []string
		codeCols  []string
		// wantWhy, when set, is the whole of the why attribute. It is what outside decided,
		// so a case that sets it says exactly what the drain is willing to write about an
		// error it did not build, with no room for a wider value that happens not to hold a
		// marker on this run.
		wantWhy string
	}
	for name, a := range map[string]attempt{
		"the provider's API": {
			brokenProvider: true,
			setUp:          func(*env) {},
			wantLog:        "the delivery failed and will be tried again",
		},
		"the vault behind Sinks": {
			setUp: func(e *env) {
				e.sinks.mu.Lock()
				defer e.sinks.mu.Unlock()
				e.sinks.err = fmt.Errorf("vault: reading this tenant's sink from %s", markerURL())
			},
			wantLog: "the delivery is halted until something is changed",
		},
		"a sink that breaks its contract": {
			setUp: func(e *env) {
				e.sinks.mu.Lock()
				defer e.sinks.mu.Unlock()
				e.sinks.plainErr = &url.Error{Op: "Post", URL: markerURL(), Err: errors.New("i/o timeout")}
			},
			wantLog: "the delivery is halted until something is changed",
		},
		"a Fault whose Detail is not internal/sink's": {
			setUp: func(e *env) {
				e.sinks.breakWith(&sink.Fault{
					Action: sink.ActionRetry,
					Cause:  outbox.NewCause(outbox.ClassSinkUnavailable).WithStatus(503),
					Detail: "POST " + markerURL() + " failed",
				})
			},
			wantLog: "the delivery failed and will be tried again",
		},
		"a Rejection naming a record that was never offered": {
			setUp: func(e *env) {
				e.sinks.mu.Lock()
				defer e.sinks.mu.Unlock()
				e.sinks.extra = []string{"rec_" + marker}
			},
			wantLog: "the delivery failed and will be tried again",
		},
		"a Rejection's own Detail": {
			setUp: func(e *env) {
				e.sinks.mu.Lock()
				defer e.sinks.mu.Unlock()
				e.sinks.rejectAll = true
				e.sinks.rejectDetail = "see " + markerURL()
			},
			wantLog: "the sink refused a record",
		},
		// The two cases below are the other half of the sentence. A *sink.Fault is
		// {Action, Cause, Detail} and a Rejection is {ID, Cause, Detail}: the Detail is
		// withheld unless internal/sink vouches for it, and the Cause is kept. These plant
		// codeMarker in the Cause and say where it comes out, so the asymmetry is on the
		// record instead of being implied away by six cases that never test it.
		"a Fault's own Cause, which is kept": {
			setUp: func(e *env) {
				e.sinks.breakWith(&sink.Fault{
					Action: sink.ActionRetry,
					// No Detail at all, which is the shape KnownDetail passes without
					// looking at a list: the Cause is then the whole of what the sink chose.
					Cause: outbox.NewCause(outbox.ClassSinkUnavailable).WithCode(codeMarker),
				})
			},
			wantLog: "the delivery failed and will be tried again",
			// cause, and not why: the Cause is logged once, in the attribute that names it
			// and holds the same text as the column. outside hands back nothing that
			// reprints it.
			codeAttrs: []string{"cause"},
			codeCols:  []string{"outbox.last_error"},
			// The type and nothing else. There is no Detail to keep, so the Fault takes the
			// same route as any other error the drain did not build, and the Cause it
			// carries is not reprinted here.
			wantWhy: "*sink.Fault",
		},
		"a Rejection's own Cause, which is kept": {
			setUp: func(e *env) {
				e.sinks.mu.Lock()
				defer e.sinks.mu.Unlock()
				e.sinks.rejectAll = true
				e.sinks.rejectCode = codeMarker
			},
			wantLog:   "the sink refused a record",
			codeAttrs: []string{"cause"},
			// The per-record dead letter, and not the delivery's own last_error: the delivery
			// was made. dead_reason beside it is this program's own constant.
			codeCols: []string{"outbox_record.last_error"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var log, fallback lines
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&fallback, nil)))
			defer slog.SetDefault(old)

			opts := worker.Options{Logger: slog.New(slog.NewTextHandler(&log, nil))}
			var e *env
			if a.brokenProvider {
				e = setup(t, opts, markerProvider{Provider: fake.New(fake.DefaultKey)})
			} else {
				e = setup(t, opts)
			}
			id := e.accept(tenantA, "fake:S1", ev(entity, "1", listA))
			a.setUp(e)
			e.drainOnce()

			if got := log.text(); !strings.Contains(got, a.wantLog) {
				t.Fatalf("the drain did not write %q, so this case searched nothing it is here to search:\n%s",
					a.wantLog, got)
			}
			row := e.row(tenantA, id)
			stored := map[string]string{
				"outbox.last_error":  row.LastError,
				"outbox.dead_reason": row.DeadReason,
			}
			for what, text := range e.recordText(id) {
				stored["outbox_record."+what] = text
			}
			searched := map[string]string{
				"the drain's log": log.text(),
				"the default log": fallback.text(),
			}
			for what, text := range stored {
				searched[what] = text
			}
			for what, text := range searched {
				if strings.Contains(text, marker) {
					t.Errorf("%s holds the secret: %q", what, text)
				}
			}
			if fallback.text() != "" {
				t.Errorf("the drain wrote to the default logger: %q", fallback.text())
			}

			// And the value the drain keeps on purpose, held to exactly the places that may
			// hold it. A case that plants nothing wants it nowhere, which is why this runs
			// for every case and not only for the two that plant it.
			if got := attrsHolding(log.text(), codeMarker); !slices.Equal(got, sorted(a.codeAttrs)) {
				t.Errorf("the log attributes holding the sink's code are %v, want %v:\n%s",
					got, sorted(a.codeAttrs), log.text())
			}
			if got := columnsHolding(stored, codeMarker); !slices.Equal(got, sorted(a.codeCols)) {
				t.Errorf("the columns holding the sink's code are %v, want %v (stored: %v)",
					got, sorted(a.codeCols), stored)
			}
			if strings.Contains(fallback.text(), codeMarker) {
				t.Errorf("the default log holds the sink's code: %q", fallback.text())
			}
			if a.wantWhy != "" {
				if why, ok := attrValue(log.text(), "why"); !ok || why != a.wantWhy {
					t.Errorf("why = %q (found %v), want %q", why, ok, a.wantWhy)
				}
			}
		})
	}
}

// logAttr is one key=value of a slog.TextHandler line. A value with a space in it is quoted,
// and nothing the drain logs puts an '=' in a message, so this reads the whole line.
var logAttr = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)=("(?:[^"\\]|\\.)*"|[^ \n]*)`)

// attrsHolding is every log attribute key whose value holds needle, sorted and without
// repeats. It is the key and not the line, because "the Cause is written in the cause
// attribute and nowhere else" is the claim, and a search of the whole line cannot tell the
// cause attribute from a why attribute that reprints it.
func attrsHolding(text, needle string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range logAttr.FindAllStringSubmatch(text, -1) {
		value := m[2]
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		if strings.Contains(value, needle) && !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

// columnsHolding is every stored column that holds needle, by column and not by row: the
// record id a per-record column belongs to is the test's own value and varies per run.
func columnsHolding(stored map[string]string, needle string) []string {
	var out []string
	seen := map[string]bool{}
	for what, text := range stored {
		column, _, _ := strings.Cut(what, " of ")
		if strings.Contains(text, needle) && !seen[column] {
			seen[column] = true
			out = append(out, column)
		}
	}
	slices.Sort(out)
	return out
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// attrValue is the last value a log attribute took, with the quoting slog.TextHandler adds
// taken off again.
func attrValue(text, key string) (string, bool) {
	var value string
	var found bool
	for _, m := range logAttr.FindAllStringSubmatch(text, -1) {
		if m[1] != key {
			continue
		}
		v := m[2]
		if unquoted, err := strconv.Unquote(v); err == nil {
			v = unquoted
		}
		value, found = v, true
	}
	return value, found
}

// TestAnErrorChainDeeperThanTheBoundIsCutOffAndSaysSo.
//
// outside unwraps at most maxWhyDepth links, because the chain is built by whoever returned the
// error and its length is theirs to choose: a sink that wraps its failure twenty deep would
// otherwise put twenty type names into one attribute, on every attempt of every row it fails,
// which is a log line a sink can make as long as it likes.
//
// A bound is only worth having if the line says when it was reached, or an operator cannot tell
// a short chain from a cut one. Nothing drove a chain past the bound before this, so the arm
// that says it, and the bound the comment exists to justify, were both unexercised.
func TestAnErrorChainDeeperThanTheBoundIsCutOffAndSaysSo(t *testing.T) {
	t.Parallel()
	var log lines
	e := setup(t, worker.Options{Logger: slog.New(slog.NewTextHandler(&log, nil))})
	e.accept(tenantA, "fake:S1", ev(entity, "1", listA))

	// Twice the bound, and every layer carries text, so this also shows the cut-off arm
	// withholds what the chain was about and not only how long it was.
	deep := errors.New("the bottom of the chain, which no log line may quote")
	for i := 0; i < 2*worker.MaxWhyDepth; i++ {
		deep = fmt.Errorf("a layer a sink wrapped: %w", deep)
	}
	e.sinks.mu.Lock()
	e.sinks.plainErr = deep // not a *sink.Fault, so the drain halts and reduces the error
	e.sinks.mu.Unlock()
	e.drainOnce()

	why, ok := attrValue(log.text(), "why")
	if !ok {
		t.Fatalf("the drain logged no why attribute, so this test searched nothing:\n%s", log.text())
	}
	if !strings.HasSuffix(why, " wrapping more") {
		t.Errorf("why = %q, which does not say the chain was cut off at %d links", why, worker.MaxWhyDepth)
	}
	if got := strings.Count(why, "*fmt.wrapError"); got != worker.MaxWhyDepth {
		t.Errorf("why names %d links, want exactly maxWhyDepth (%d): %q", got, worker.MaxWhyDepth, why)
	}
	if strings.Contains(log.text(), "the bottom of the chain") ||
		strings.Contains(log.text(), "a layer a sink wrapped") {
		t.Errorf("the drain wrote the sink's own text:\n%s", log.text())
	}
}

// recordText is every text column of every record of a delivery, by column and record id, read
// as the superuser so that nothing about row-level security can hide one from the search.
func (e *env) recordText(outboxID string) map[string]string {
	e.t.Helper()
	conn, err := pgx.Connect(e.ctx, e.tdb.AdminURL)
	if err != nil {
		e.t.Fatalf("connect as the superuser: %v", err)
	}
	defer func() { _ = conn.Close(e.ctx) }()
	rows, err := conn.Query(e.ctx,
		`SELECT record_id, dead_reason, last_error FROM lawang.outbox_record WHERE outbox_id = $1`, outboxID)
	if err != nil {
		e.t.Fatalf("query: %v", err)
	}
	out := map[string]string{}
	for rows.Next() {
		var recordID, deadReason, lastError string
		if err := rows.Scan(&recordID, &deadReason, &lastError); err != nil {
			e.t.Fatalf("scan: %v", err)
		}
		out["record_id of "+recordID] = recordID
		out["dead_reason of "+recordID] = deadReason
		out["last_error of "+recordID] = lastError
	}
	if err := rows.Err(); err != nil {
		e.t.Fatalf("read the rows: %v", err)
	}
	return out
}
