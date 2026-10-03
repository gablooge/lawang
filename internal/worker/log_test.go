package worker_test

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/worker"
)

// lines is a log a test can read back, safe for the drain's goroutines.
type lines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lines) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// TestThePreparedCountersAreLoggedOnBothPaths.
//
// pipeline.Prepare returns the counters it had reached beside every error, because a delivery
// that dies halfway still dropped automation noise, skipped what the ledger held and refused
// what it refused. Two of those counters exist nowhere else until B25 reads them into a metric,
// and ADR 4 decision 7 and ADR 12 decision 1 both ask an operator to watch them: ScopeReturned
// and VersionUnordered are only ever non-zero on an error path, so a drain that logged the
// counters only on success would never print either of them.
func TestThePreparedCountersAreLoggedOnBothPaths(t *testing.T) {
	t.Parallel()
	var log lines
	e := setup(t, worker.Options{Logger: slog.New(slog.NewTextHandler(&log, nil))})

	// A plain delivery, which is the success path.
	e.accept(tenantA, "fake:S1", ev(entity, "1", listA))
	e.drainOnce()
	if got := log.text(); !strings.Contains(got, `msg=prepared`) || !strings.Contains(got, `records=1`) {
		t.Errorf("the success path did not log the counters:\n%s", got)
	}

	// The entity moves to another list, and then the first version arrives again from a stale
	// body. Its record id is already prepared, it is not the head any more, and the head is in
	// another scope, which is the one shape ADR 4 decision 7 refuses to guess about: a dead
	// letter, and a count.
	e.accept(tenantA, "fake:S1", ev(entity, "2", "L2"))
	e.drainOnce()
	stale := ev(entity, "1", listA)
	stale.Title = "the same record from an older body" // new bytes, same record id
	id := e.accept(tenantA, "fake:S1", stale)

	log.mu.Lock()
	log.buf.Reset()
	log.mu.Unlock()
	e.drainOnce()

	e.wantState(id, outbox.StateDead, "not retryable")
	got := log.text()
	if !strings.Contains(got, `msg=prepared`) {
		t.Errorf("the error path logged no counters at all:\n%s", got)
	}
	if !strings.Contains(got, `scope_returned=1`) {
		t.Errorf("scope_returned was not logged, so the count ADR 4 asks for exists nowhere:\n%s", got)
	}
	if !strings.Contains(got, `records=0`) {
		t.Errorf("the error path logged records other than none, although Prepare returns none with an error:\n%s", got)
	}
}
