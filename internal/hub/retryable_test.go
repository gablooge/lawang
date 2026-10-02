package hub_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gablooge/lawang/internal/hub"
)

// TestAnErrorFromAnExpiredAcceptIsRetryable.
//
// The edge answers 503 with a Retry-After for errors.Is(err, context.DeadlineExceeded), and 500
// for everything else. Both error shapes pgx produces for a saturated connection pool match, but
// a statement the server cancelled comes back as a *pgconn.PgError with SQLSTATE 57014 and no
// context error anywhere in its chain: left alone it is a 500, where "try again in a moment" is
// the truth and 503 is what makes a provider retry rather than count an outage.
//
// It is a unit test because the shape it is about cannot be produced on demand from a test: it
// needs another session to cancel this one's statement at exactly the right moment.
func TestAnErrorFromAnExpiredAcceptIsRetryable(t *testing.T) {
	t.Parallel()
	// SQLSTATE 57014, query_canceled: what pg_cancel_backend leaves behind.
	cancelled := &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"}
	if errors.Is(cancelled, context.DeadlineExceeded) {
		t.Fatal("the premise is gone: a cancelled statement now carries a context error of its own")
	}

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	got := hub.Retryable(expired, cancelled)
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("an error from an accept that ran out of time is %v, which the edge answers 500 to", got)
	}
	if !errors.Is(got, cancelled) {
		t.Errorf("the original error was dropped, so the log says nothing about what failed: %v", got)
	}

	// A context that is still good leaves the error exactly as it was: a failure that is not about
	// time must not be dressed up as one, or every bug becomes "try again".
	live, cancelLive := context.WithCancel(context.Background())
	defer cancelLive()
	if got := hub.Retryable(live, cancelled); !errors.Is(got, cancelled) || errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("Retryable(a live context, err) = %v, want the error unchanged", got)
	}
	if got := hub.Retryable(expired, nil); got != nil {
		t.Errorf("Retryable(ctx, nil) = %v, want nil", got)
	}
	// A context that was cancelled rather than timed out stays cancelled: the edge tells that from
	// a deadline, because a sender that hung up is not something to answer at all.
	cancelledCtx, stop := context.WithCancel(context.Background())
	stop()
	got = hub.Retryable(cancelledCtx, cancelled)
	if !errors.Is(got, context.Canceled) || errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("Retryable(a cancelled context, err) = %v, want the cancellation", got)
	}
}
