package outbox_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gablooge/lawang/internal/outbox"
	"github.com/gablooge/lawang/internal/tenancy"
)

// TestStressOrderingUnderRandomLoad runs every writer of the outbox at once, on a few hot keys:
// acceptors, workers that deliver or dead-letter what they claim, and an operator replaying the
// dead letters. It asserts what the package promises, while it runs and at the end:
//
//   - never two rows of one key in flight together;
//   - each key's rows are claimed in queue order (a replayed row has a fresh seq, at the back);
//   - the head marker's invariant holds on every snapshot, not only once things are quiet;
//   - every row ends up delivered: no row is left behind a marker that was never handed on.
//
// The choices (which key, deliver or dead-letter) come from a seed, logged so that a failure can be
// run again with OUTBOX_STRESS_SEED. The scheduling of goroutines is not reproducible, the mix is.
// A run that stops making progress fails within seconds instead of hanging.
//
// What it is NOT a test of: the claim's re-checks on the row it has locked. The window between a
// claim's snapshot and its row lock is too short for random load to hit. With the is_head re-check
// taken out, this test passed 10 runs of 10 in review, and a missing lock in the finishers gets
// past it about every second run. Those properties belong to the TestClaimRechecks... tests and
// the three lost-promotion tests in interleaving_test.go, which hold a transaction at the exact
// point. This one is for what nobody thought of.
func TestStressOrderingUnderRandomLoad(t *testing.T) {
	const (
		acceptors    = 6
		perAcceptor  = 40
		workers      = 8
		keys         = 4
		deadPercent  = 30
		stallTimeout = 20 * time.Second
	)
	seed := uint64(time.Now().UnixNano()) //nolint:gosec // any 64 bits will do
	if s := os.Getenv("OUTBOX_STRESS_SEED"); s != "" {
		parsed, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("OUTBOX_STRESS_SEED: %v", err)
		}
		seed = parsed
	}
	t.Logf("seed %d (run again with OUTBOX_STRESS_SEED=%d)", seed, seed)

	e := setup(t)
	ctx, stop := context.WithCancel(e.ctx)
	defer stop()
	var failed atomic.Bool
	fail := func(format string, args ...any) {
		t.Helper()
		if failed.CompareAndSwap(false, true) {
			t.Errorf(format, args...)
		}
		stop() // fail fast: everyone else winds down
	}
	running := func() bool { return ctx.Err() == nil }

	type dead struct {
		tenant tenancy.ID
		id     string
	}
	var (
		mu        sync.Mutex
		inFlight  = map[string]string{} // tenant/key -> row id
		lastSeq   = map[string]int64{}
		diedOnce  = map[string]bool{}
		delivered atomic.Int64
		replays   atomic.Int64
		toReplay  = make(chan dead, acceptors*perAcceptor)
		total     = int64(acceptors * perAcceptor)
		all       sync.WaitGroup
	)

	for a := range acceptors {
		all.Go(func() {
			rng := rand.New(rand.NewPCG(seed, uint64(a))) //nolint:gosec // a reproducible mix, not a secret
			for i := 0; i < perAcceptor && running(); i++ {
				// Paced to about the rate the workers drain at, so that the queues stay nearly
				// empty. That is where the marker is at risk: a promotion can only be lost when
				// the row that finishes is the last unfinished row of its key, and an Accept of
				// that key is open at that very moment. Deep queues would hide it.
				time.Sleep(time.Duration(rng.IntN(4000)) * time.Microsecond)
				tenant := tenantA
				if rng.IntN(2) == 1 {
					tenant = tenantB
				}
				_, fresh, err := e.ob.Accept(ctx, tenant, outbox.Delivery{
					Provider:    "fake",
					OrderingKey: fmt.Sprintf("hot:%d", rng.IntN(keys)),
					RawBody:     fmt.Appendf(nil, `{"acceptor":%d,"n":%d}`, a, i),
				})
				if running() && (err != nil || !fresh) {
					fail("Accept: fresh %v, err %v", fresh, err)
				}
			}
		})
	}

	for w := range workers {
		all.Go(func() {
			rng := rand.New(rand.NewPCG(seed, uint64(1000+w))) //nolint:gosec // as above
			for running() && delivered.Load() < total {
				got, err := e.ob.Claim(ctx, 1+rng.IntN(3), lease)
				if err != nil {
					if running() {
						fail("Claim: %v", err)
					}
					return
				}
				if len(got) == 0 {
					time.Sleep(2 * time.Millisecond)
					continue
				}
				// Everything in the batch is in flight from the moment it is claimed.
				keysOf := make([]string, len(got))
				for i, c := range got {
					row, err := e.ob.Get(ctx, c.Tenant(), c.ID())
					if err != nil {
						if running() {
							fail("Get: %v", err)
						}
						return
					}
					key := row.TenantID + "/" + row.OrderingKey
					keysOf[i] = key
					mu.Lock()
					if other, busy := inFlight[key]; busy {
						fail("%s: row %s claimed while %s is still in flight", key, c.ID(), other)
					}
					if row.Seq <= lastSeq[key] {
						fail("%s: seq %d claimed after seq %d", key, row.Seq, lastSeq[key])
					}
					if !row.IsHead {
						fail("%s: row %s was claimed and is not the head of its key", key, c.ID())
					}
					inFlight[key] = c.ID()
					lastSeq[key] = row.Seq
					mu.Unlock()
				}
				for i, c := range got {
					time.Sleep(time.Duration(rng.IntN(1500)) * time.Microsecond) // the work
					mu.Lock()
					dies := !diedOnce[c.ID()] && rng.IntN(100) < deadPercent
					if dies {
						diedOnce[c.ID()] = true
					}
					// No longer in flight from just before the finishing call: the next row of the
					// key is claimable from the moment that call commits, which is before it returns.
					delete(inFlight, keysOf[i])
					mu.Unlock()

					if dies {
						err = e.ob.MarkDead(ctx, c, badShape)
						toReplay <- dead{c.Tenant(), c.ID()}
					} else {
						err = e.ob.MarkDelivered(ctx, c)
						delivered.Add(1)
					}
					if err != nil && running() {
						fail("finishing %s: %v", c.ID(), err)
					}
				}
			}
		})
	}

	// The operator.
	all.Go(func() {
		for running() && delivered.Load() < total {
			select {
			case d := <-toReplay:
				if err := e.ob.Replay(ctx, d.tenant, d.id); err != nil && running() {
					fail("Replay(%s): %v", d.id, err)
				}
				replays.Add(1)
			case <-time.After(5 * time.Millisecond):
			}
		}
	})

	// The invariant, on a snapshot of its own, again and again while everything above runs. And
	// the watchdog: a marker that was not handed on shows as a run that stops delivering.
	all.Go(func() {
		admin := e.adminConn()
		last, lastProgress := int64(-1), time.Now()
		for running() && delivered.Load() < total {
			var broken string
			if err := admin.QueryRow(ctx, headsBroken).Scan(&broken); err != nil {
				if running() {
					fail("head invariant: %v", err)
				}
				return
			}
			if broken != "" {
				fail("head invariant broken while running: %s", broken)
				return
			}
			if now := delivered.Load(); now != last {
				last, lastProgress = now, time.Now()
			} else if time.Since(lastProgress) > stallTimeout {
				fail("no delivery for %v, at %d of %d: some row is unfinished and nothing in front of it is claimable",
					stallTimeout, now, total)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	all.Wait()
	if failed.Load() {
		return
	}

	admin := e.adminConn()
	e.checkHeads(admin)
	var unfinished, isDelivered int64
	err := admin.QueryRow(e.ctx, `
		SELECT count(*) FILTER (WHERE state <> 'delivered'), count(*) FILTER (WHERE state = 'delivered')
		  FROM lawang.outbox`).Scan(&unfinished, &isDelivered)
	if err != nil {
		t.Fatal(err)
	}
	if unfinished != 0 || isDelivered != total {
		t.Errorf("%d rows delivered and %d not, want all %d delivered", isDelivered, unfinished, total)
	}
	t.Logf("%d deliveries, %d dead letters replayed", delivered.Load(), replays.Load())
}
