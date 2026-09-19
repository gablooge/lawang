package outboxdb_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gablooge/sluiceway/internal/outbox/outboxdb"
	"github.com/gablooge/sluiceway/internal/store"
	"github.com/gablooge/sluiceway/internal/testdb"
)

// The backlog the guard builds. It is large enough to tell a claim that costs what it returns from
// one that costs what is waiting, and it has the proportions that matter: a long delivered
// history, few heads compared to it, and most of the waiting rows behind a handful of hot keys.
const (
	deliveredRows = 200_000 // history, which a claim must never look at
	distinctKeys  = 5_000   // one unfinished row each: 5,000 heads
	hotKeys       = 10      // and these have 2,000 versions each: 10 heads, 19,990 rows behind them
	hotVersions   = 2_000
)

// What a claim may touch, in shared buffers (pages hit or read). Two numbers per case: what it
// costs to FIND the rows (everything under the plan's Limit node), and the whole statement. The
// whole statement is mostly the UPDATE itself, about 25 buffers a row, because a lease moves the
// row in the index of due heads and so every index of the table gets an entry for the new
// version. That is a cost per row returned, and at a batch of 100 it would hide a search that had
// gone wrong, which is why the search is bounded on its own.
//
// Measured on Postgres 17 with this data, the search and the whole statement:
//
//	a batch of 10 that returns 10           16      245
//	a batch of 100 that returns 100        110    2,363
//	a batch of 1000 that returns 1000    1,040   23,384
//	nothing due, 5,010 heads just moved     21       21   (the pages of dead entries at the front of
//	                                                       the index: about one page for every 260
//	                                                       heads that moved since the last vacuum)
//	nothing due, after a vacuum              2        2
//
// The bounds are about an order of magnitude above that. Two claims are known to fail this test,
// and both were run against it as mutations on 2026-09-19:
//
//   - The claim this one replaced, which worked out the head of every key on every poll: 16,694
//     to 20,948 buffers to find its rows where something was due, and 1,288 to 1,632 where
//     nothing was, vacuumed or not. The bound on the whole statement would have let it through at
//     a batch of 100 (22,103), the bound on the search does not.
//   - This claim with a condition on the state added back (see the comment on Claim in
//     queries.sql). Its buffers stay inside the bounds on a backlog as small as this one, which
//     is why the shape of the plan is checked as well: at a batch of 1000 it reads
//     outbox_unfinished, every waiting row, next to the index of due heads.
const (
	maxFindBatch10    = 150
	maxTotalBatch10   = 2_500
	maxFindBatch100   = 1_000
	maxTotalBatch100  = 25_000
	maxFindBatch1000  = 10_000
	maxTotalBatch1000 = 250_000
	maxNoneDue        = 250
	maxVacuumed       = 30
)

// errRollback ends a measuring transaction without keeping its leases.
var errRollback = errors.New("roll back")

type planNode struct {
	NodeType         string     `json:"Node Type"`
	IndexName        string     `json:"Index Name"`
	ActualRows       float64    `json:"Actual Rows"`
	SharedHitBlocks  int64      `json:"Shared Hit Blocks"`
	SharedReadBlocks int64      `json:"Shared Read Blocks"`
	Plans            []planNode `json:"Plans"`
}

func (n planNode) buffers() int64 { return n.SharedHitBlocks + n.SharedReadBlocks }

// scans lists the leaves of the plan below n: how the rows are actually read.
func (n planNode) scans() []string {
	if len(n.Plans) == 0 {
		return []string{strings.TrimSpace(n.NodeType + " " + n.IndexName)}
	}
	var out []string
	for _, child := range n.Plans {
		out = append(out, child.scans()...)
	}
	return out
}

// find returns the first node of the given type, searching depth first.
func (n planNode) find(nodeType string) (planNode, bool) {
	if n.NodeType == nodeType {
		return n, true
	}
	for _, child := range n.Plans {
		if found, ok := child.find(nodeType); ok {
			return found, true
		}
	}
	return planNode{}, false
}

// TestClaimCostFollowsTheBatchNotTheBacklog runs the claim the application runs, as the worker
// role, through EXPLAIN (ANALYZE, BUFFERS), and bounds what it touches. It guards the property
// that lets a worker poll once a second through a sink outage or a backfill: finding work costs
// what is found, whatever is waiting.
func TestClaimCostFollowsTheBatchNotTheBacklog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb := testdb.New(t)
	db, err := store.Open(ctx, tdb.URL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Migrate(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	admin, err := pgx.Connect(ctx, tdb.AdminURL)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	sql := func(stmt string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, stmt, args...); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	// Bulk inserts as the superuser: 225,000 rows in a few seconds. The hot keys go in version by
	// version, so that version 1 of each has the lowest seq and is the head.
	sql(`INSERT INTO sluiceway.outbox (id, tenant_id, provider, delivery_id, ordering_key, raw_body, state, finished_at)
	     SELECT 'd' || g, 'tenant_' || g % 5, 'fake', 'd' || g, 'done:' || g, '\x7b7d', 'delivered', now()
	       FROM generate_series(1, $1::int) g`, deliveredRows)
	sql(`INSERT INTO sluiceway.outbox (id, tenant_id, provider, delivery_id, ordering_key, raw_body, is_head)
	     SELECT 'h' || k || 'v' || v, 'tenant_' || k % 5, 'fake', 'h' || k || 'v' || v, 'hot:' || k, '\x7b7d', v = 1
	       FROM generate_series(1, $2::int) v, generate_series(1, $1::int) k
	      ORDER BY v, k`, hotKeys, hotVersions)
	sql(`INSERT INTO sluiceway.outbox (id, tenant_id, provider, delivery_id, ordering_key, raw_body, is_head)
	     SELECT 'u' || g, 'tenant_' || g % 5, 'fake', 'u' || g, 'one:' || g, '\x7b7d', true
	       FROM generate_series(1, $1::int) g`, distinctKeys)
	sql("VACUUM sluiceway.outbox")

	var (
		raw   []byte   // the last plan, for the failure message
		scans []string // and how it read the rows it was looking for
	)
	claim := func(batch int) (rows int, find, total int64) {
		t.Helper()
		sql("ANALYZE sluiceway.outbox")
		err := db.RoleTx(ctx, store.RoleWorker, func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+outboxdb.ClaimSQL, 60.0, "guard", int32(batch)).Scan(&raw) //nolint:gosec // batch is 10 or 100
			if err != nil {
				return err
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatalf("EXPLAIN of the claim as the worker role: %v", err)
		}
		var out []struct {
			Plan planNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || len(out) != 1 {
			t.Fatalf("EXPLAIN output: %v (%d plans)", err, len(out))
		}
		limit, ok := out[0].Plan.find("Limit")
		if !ok {
			t.Fatalf("the claim's plan has no Limit node, so its search cannot be told from its UPDATE\nplan: %s", raw)
		}
		scans = limit.scans()
		return int(out[0].Plan.ActualRows), limit.buffers(), out[0].Plan.buffers()
	}
	check := func(what string, batch, wantRows int, maxFind, maxTotal int64) {
		t.Helper()
		rows, find, total := claim(batch)
		t.Logf("%s: batch %d returned %d rows; finding them touched %d buffers (bound %d), the statement %d (bound %d)",
			what, batch, rows, find, maxFind, total, maxTotal)
		if rows != wantRows {
			t.Errorf("%s: batch %d returned %d rows, want %d", what, batch, rows, wantRows)
		}
		// The search is one walk of the index of due heads, and nothing else. A plan that also
		// reads another index (a bitmap AND with outbox_unfinished is the one that has happened)
		// or the table is reading what is waiting. On a backlog of this size that costs little
		// enough to slip under the bounds below, and on a real one it does not.
		if want := []string{"Index Scan outbox_due_heads"}; !slices.Equal(scans, want) {
			t.Errorf("%s: batch %d finds its rows with %q, want %q\nplan: %s", what, batch, scans, want, raw)
		}
		if find > maxFind || total > maxTotal {
			t.Errorf("%s: batch %d touched %d buffers to find its rows (at most %d) and %d in all (at most %d): the claim's cost is following the backlog, not the batch\nplan: %s",
				what, batch, find, maxFind, total, maxTotal, raw)
		}
	}
	// moveHeads changes every head in one statement, which no code path of the application does:
	// they move one at a time, as workers claim and fail them. Each move leaves the row's old entry
	// in the index of due heads dead, at the front, where the claim walks. The first poll to pass a
	// dead entry marks it, and later polls skip it, so in production that cost is paid once per
	// move, by whichever poll comes next. Here it is 5,000 moves at once, so one poll goes
	// unmeasured to pay for them, and the measured poll after it shows what every later one costs
	// until a vacuum.
	moveHeads := func(set string) {
		t.Helper()
		sql("UPDATE sluiceway.outbox SET " + set + " WHERE is_head")
		_, find, _ := claim(10)
		t.Logf("the first poll after moving every head touched %d buffers, once", find)
	}

	check("everything due", 10, 10, maxFindBatch10, maxTotalBatch10)
	check("everything due", 100, 100, maxFindBatch100, maxTotalBatch100)
	check("everything due", 1000, 1000, maxFindBatch1000, maxTotalBatch1000)

	// The heads of the hot keys are backing off, and they are the oldest rows in the queue: a claim
	// that walked the queue in seq order would wade through the 19,990 rows behind them.
	sql(`UPDATE sluiceway.outbox SET next_attempt_at = now() + interval '1 hour' WHERE is_head AND ordering_key LIKE 'hot:%'`)
	check("hot keys backing off", 10, 10, maxFindBatch10, maxTotalBatch10)
	check("hot keys backing off", 100, 100, maxFindBatch100, maxTotalBatch100)

	// A sink outage: every head is backing off, and a poll has to find that out for next to nothing.
	moveHeads(`next_attempt_at = now() + interval '1 hour'`)
	check("every head backing off", 10, 0, maxNoneDue, maxNoneDue)

	// Every head is held by some worker: what others have in flight is not visited either.
	moveHeads(`next_attempt_at = now() - interval '1 hour', lease_until = now() + interval '1 hour', lease_token = 'someone'`)
	check("every head leased", 10, 0, maxNoneDue, maxNoneDue)

	// What is left of the cost above is the pages of dead index entries in front of the index,
	// which a vacuum removes.
	sql("VACUUM sluiceway.outbox")
	check("every head leased, after a vacuum", 10, 0, maxVacuumed, maxVacuumed)
}
