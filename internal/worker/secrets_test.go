package worker_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
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
			searched := map[string]string{
				"the drain's log":    log.text(),
				"the default log":    fallback.text(),
				"outbox.last_error":  row.LastError,
				"outbox.dead_reason": row.DeadReason,
			}
			for what, text := range e.recordText(id) {
				searched["outbox_record."+what] = text
			}
			for what, text := range searched {
				if strings.Contains(text, marker) {
					t.Errorf("%s holds the secret: %q", what, text)
				}
			}
			if fallback.text() != "" {
				t.Errorf("the drain wrote to the default logger: %q", fallback.text())
			}
		})
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
