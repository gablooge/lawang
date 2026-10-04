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
// acceptors, workers that take each row through one of five outcomes, and an operator replaying
// what can be replayed. It asserts what the package promises, while it runs and at the end:
//
//   - never two rows of one key in flight together;
//   - each key's rows are claimed in queue order (a replayed row has a fresh seq, at the back),
//     and a row that was given back is the only one that may be claimed again at its own seq;
//   - the head marker's invariant holds on every snapshot, not only once things are quiet;
//   - every row ends up delivered and no record is left dead: nothing is left behind a marker
//     that was never handed on, and nothing is left waiting for a replay nobody can make.
//
// The five outcomes are every transition a worker can write, because a transition that is never
// generated is a transition this test does not cover:
//
//   - delivered, the ordinary one;
//   - dead, the whole row, replayed later from the back of its key;
//   - **delivered with one record dead**, which finishes the row and still leaves it replayable.
//     That is the one way a FINISHED row goes backwards into the queue, and it reassigns seq and
//     recomputes is_head exactly as the dead-row replay does;
//   - **halted**, which leaves the row's state and its head marker alone and gives the attempt
//     back, so the same row is claimed again at the same seq;
//   - **released**, the same from the shutdown path.
//
// Each of the four non-ordinary outcomes happens at most once per row, so the run terminates:
// every row ends delivered with nothing dead, and the watchdog below fails a run that stops
// making progress instead of letting it hang.
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
		acceptors   = 6
		perAcceptor = 40
		workers     = 8
		keys        = 4
		// The outcome of one claim, as cumulative percentages of a 100-sided roll. Everything
		// above recordDeadUpTo is an ordinary delivery, and each of the four below is taken at
		// most once per row (see wantsOnce).
		haltUpTo       = 10
		releaseUpTo    = 20
		deadUpTo       = 45
		recordDeadUpTo = 65
		stallTimeout   = 20 * time.Second
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
		mu       sync.Mutex
		inFlight = map[string]string{} // tenant/key -> row id
		lastSeq  = map[string]int64{}
		lastRow  = map[string]string{} // tenant/key -> the row id that seq belongs to
		// done[what][row id] is true once that row has had that outcome. Each of the four
		// non-ordinary outcomes is taken at most once per row, so every row reaches delivered.
		done      = map[string]map[string]bool{}
		delivered atomic.Int64
		replays   atomic.Int64
		// Room for every row to be replayed twice: once from dead and once from delivered
		// with a dead record in it.
		toReplay = make(chan dead, 2*acceptors*perAcceptor)
		total    = int64(acceptors * perAcceptor)
		all      sync.WaitGroup
	)
	for _, what := range []string{"halt", "release", "dead", "record-dead"} {
		done[what] = map[string]bool{}
	}
	// wantsOnce reports whether this row may have this outcome now, and records that it did.
	// Caller holds mu.
	wantsOnce := func(what, id string) bool {
		if done[what][id] {
			return false
		}
		done[what][id] = true
		return true
	}

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
				statesOf := make([]string, len(got))
				for i, c := range got {
					row, err := e.ob.Get(ctx, c.Tenant(), c.ID())
					if err != nil {
						if running() {
							fail("Get: %v", err)
						}
						return
					}
					key := row.TenantID + "/" + row.OrderingKey
					keysOf[i], statesOf[i] = key, row.State
					mu.Lock()
					if other, busy := inFlight[key]; busy {
						fail("%s: row %s claimed while %s is still in flight", key, c.ID(), other)
					}
					// Strictly forward, with one exception: a row that was halted or given
					// back keeps its seq and its place, so the very same row may be claimed
					// again at the seq it already had. Any other repeat is the queue going
					// backwards.
					//
					// Called what it is: this is a RELAXATION of the plain "seq must not go
					// backwards" it replaced, and not a strengthening of it. Halt and Release
					// make the plain form false, so the smallest change that admits them is
					// to allow equality and tie-break on the row id, which is what these two
					// arms are. What it gives up is one case the plain form caught: the same
					// row claimed again at its own seq after it was finished. The head
					// invariant two lines down is what rules that out instead, and it is
					// pinned by a mutation (Release reassigning seq, caught 3 runs of 3 by
					// the head invariant and not by this check).
					switch {
					case row.Seq < lastSeq[key]:
						fail("%s: seq %d claimed after seq %d", key, row.Seq, lastSeq[key])
					case row.Seq == lastSeq[key] && c.ID() != lastRow[key]:
						fail("%s: row %s claimed at seq %d, which belongs to row %s",
							key, c.ID(), row.Seq, lastRow[key])
					}
					if !row.IsHead {
						fail("%s: row %s was claimed and is not the head of its key", key, c.ID())
					}
					inFlight[key] = c.ID()
					lastSeq[key], lastRow[key] = row.Seq, c.ID()
					mu.Unlock()
				}
				for i, c := range got {
					time.Sleep(time.Duration(rng.IntN(1500)) * time.Microsecond) // the work
					mu.Lock()
					// A row that has already been prepared (it came back from a replay of a
					// delivery with one dead record in it) is delivered and never prepared
					// again, which is what the drain does with one.
					outcome := "deliver"
					if roll := rng.IntN(100); statesOf[i] == outbox.StatePending {
						switch {
						case roll < haltUpTo && wantsOnce("halt", c.ID()):
							outcome = "halt"
						case roll < releaseUpTo && wantsOnce("release", c.ID()):
							outcome = "release"
						case roll < deadUpTo && wantsOnce("dead", c.ID()):
							outcome = "dead"
						case roll < recordDeadUpTo && wantsOnce("record-dead", c.ID()):
							outcome = "record-dead"
						}
					}
					// No longer in flight from just before the finishing call: the next row of the
					// key is claimable from the moment that call commits, which is before it returns.
					delete(inFlight, keysOf[i])
					mu.Unlock()

					switch outcome {
					case "halt":
						// The shortest pause the package takes, so the row comes straight
						// back. It keeps its state, its seq and its head marker, and gives
						// the attempt back.
						err = e.ob.Halt(ctx, c, time.Millisecond, badShape)
					case "release":
						err = e.ob.Release(ctx, c)
					case "dead":
						err = e.ob.MarkDead(ctx, c, badShape)
						toReplay <- dead{c.Tenant(), c.ID()}
					case "record-dead":
						// Prepared, then finished as delivered with its one record refused.
						// The row is finished and still replayable, which is the one way a
						// finished row goes back into the queue.
						recordID := "rec_" + c.ID()
						err = e.prepare(ctx, c, doc(recordID))
						if err == nil {
							err = e.ob.MarkDelivered(ctx, c, []outbox.DeadRecord{
								{RecordID: recordID, Cause: refused},
							})
							if err == nil {
								toReplay <- dead{c.Tenant(), c.ID()}
							}
						}
					default:
						err = e.ob.MarkDelivered(ctx, c, nil)
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
	// A delivered row with a dead record in it is replayable, so "every row is delivered" is
	// not the whole promise: a record left dead is a record nobody replayed.
	var deadRecords, allRecords int64
	err = admin.QueryRow(e.ctx, `
		SELECT count(*) FILTER (WHERE state = 'dead'), count(*) FROM lawang.outbox_record`).
		Scan(&deadRecords, &allRecords)
	if err != nil {
		t.Fatal(err)
	}
	if deadRecords != 0 {
		t.Errorf("%d records of %d are still dead, want none: a per-record dead letter was never replayed",
			deadRecords, allRecords)
	}
	if allRecords == 0 {
		t.Error("no record was ever stored, so the per-record outcomes were never generated")
	}
	t.Logf("%d deliveries, %d replays, %d prepared records", delivered.Load(), replays.Load(), allRecords)
}
