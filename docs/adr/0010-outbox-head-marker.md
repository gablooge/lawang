# 10. The head of an ordering key is stored, not computed by the claim

Status: accepted, 2026-09-19 (backlog item B04, review round 3)

## Decision

Every outbox row has a boolean `is_head`. It is true for exactly one row of each (tenant, ordering
key) that has unfinished rows: the earliest one, which is the only row of that entity a worker may
take. The claim does not work out which rows are heads. It walks a partial index of heads in the
order they fall due and stops at its batch:

```sql
SELECT c.id FROM outbox c
 WHERE c.is_head AND c.due_at <= now()
 ORDER BY c.due_at, c.seq LIMIT $batch
   FOR UPDATE OF c SKIP LOCKED
```

`due_at` is a generated column, `GREATEST(next_attempt_at, lease_until)`: when the backoff is over
and the lease has run out. The index is `(due_at, seq) WHERE is_head`.

The marker is kept by the writers and guarded by the database:

- `Accept` and `Replay` set it to "this key has no unfinished row".
- `MarkDelivered` and `MarkDead` clear it on the row they finish and, in the same transaction, set
  it on the earliest unfinished row of the key, if there is one.
- All four first take the transaction-scoped advisory lock of the key, as a statement of their own.
  Before this decision only `Accept` and `Replay` took it.
- `UNIQUE (tenant_id, ordering_key) WHERE is_head` allows one head per key.
  `CHECK (NOT is_head OR state IN ('pending', 'prepared'))` says a head is unfinished.
- The worker role cannot write `is_head`.

## Why

The claim used to compute the head of every unfinished key on every poll (`DISTINCT ON` over all
unfinished rows, a join back to them, an anti-join per head, a sort) and then throw away all but
the batch. Its cost followed the backlog. The backlog is set by senders and by the health of a
sink someone else runs, and a worker polls once a second from several goroutines, so that is
unbounded work on a per-poll path.

Measured on Postgres 17.11 with default settings, the real roles and migrations, as
`sluiceway_worker`, 5,000,000 delivered rows present (heap 1.3 GB), 120 byte bodies, analyzed,
second of two runs, each rolled back. Time is execution time, buffers are shared pages hit or read.

| Unfinished rows | Batch | Before | After |
|---|---|---|---|
| 100,000 over 90,000 keys | 10 | 265 ms, 386,914 | 0.37 ms, 339 |
| same | 100 | 269 ms, 389,626 | 1.4 ms, 2,856 |
| 1,000,000 over 990,000 keys | 10 | 4,142 ms, 4,382,297 (two sequential scans of the heap) | 0.28 ms, 344 |
| same | 100 | 4,147 ms, 4,385,122 | 1.1 ms, 3,060 |
| 1,000,000, every row backing off (a sink outage) | 10 | 391 ms, 237,065, to return nothing | 0.02 ms, 3 |
| the same before any vacuum, after 990,000 heads moved in one statement | 10 | 391 ms, 237,065 | 5.9 ms, 3,844 (the first poll after the move: 116 ms, once) |
| 100,000: 20 hot keys of 4,500 versions, and 10,000 keys of one | 10 | 148 ms, 107,234 | 0.22 ms, 285 |
| same | 100 | 152 ms, 109,961 | 1.3 ms, 2,849 |
| same | 1000 | not measured | 11.6 ms, 28,818 |
| same, the hot keys' heads backing off | 10 | 122 ms, 49,268 | 0.19 ms, 303 |

What is left after the change is almost all the UPDATE, about 25 buffers per leased row. Finding
the rows costs about one buffer each.

The writers pay for it, a little. At 6,000,000 rows, `Accept` of a 1 KB body takes 0.17 ms and 24
buffers onto a new key and 0.78 ms and 104 buffers onto a key with 4,500 unfinished versions (the
new part is one index-only probe, 3 to 4 buffers). Finishing a row gained the lock (0.02 ms), and
the promotion (0.12 ms, 30 buffers), next to the 0.11 ms of the state change itself.

## Why it is correct

Call (tenant, ordering key) a key, `pending` and `prepared` rows unfinished, and `Accept`,
`Replay`, `MarkDelivered` and `MarkDead` the writers of a key. The claim, `MarkPrepared` and the
retry never change `is_head`, a `seq`, or which rows are unfinished.

**Writers of one key are serial.** Each takes the key's advisory lock before its first statement
that reads or writes the key's rows, and holds it until it ends. Postgres makes a transaction
visible before it releases its locks, and under READ COMMITTED every statement takes a new
snapshot. So each writer's statements see everything the writers before it committed, no writer of
the key is open beside it, and any snapshot anyone takes sees, for each key, the work of the first
so many writers and nothing of the rest. `store` opens transactions at the server's default
isolation level, which this relies on.

**The invariant.** On every snapshot: a key with unfinished rows has exactly one head, that head is
its unfinished row with the lowest `seq`, and no finished row is a head. By induction over the
writers of a key:

- `Accept` assigns a `seq` above every `seq` of the key (the sequence only grows, and every earlier
  writer has finished). If the key had no unfinished row the new row is the only one, and the head.
  If it had, its head stays the lowest, and the new row is not a head.
- `Replay` does the same for a dead row, which had no marker (the CHECK).
- A finishing writer holds a valid lease token, so its row was claimed, so it was a head, and it
  still is: only finishing takes the marker off an unfinished row. It gives the marker up and hands
  it to the lowest unfinished row that is left, if any. The two steps are separate statements in
  that order, because the unique index is checked row by row.
- A writer whose lease was lost changes nothing.

**Safety: never two rows of one key in flight.** This does not depend on the lock, or on any
writer being right. Suppose the claim leases r while r' of the same key is in flight. r' was leased
as a head, is unfinished, and so still has the marker in its latest committed version. The claim
leased r because r's latest committed version, read under r's row lock, has the marker too. Two
committed rows of one key with the marker: the unique index forbids it. (If the transaction
finishing r' is open and has already promoted r, the claim cannot see that promotion, and if it
meets r's row lock it skips the row.) A row that reaches the table without this package has no
marker by default, and is refused one while its key has a head.

**Order.** Only heads are leased and the head is the lowest unfinished `seq`, so the rows of a key
are leased in `seq` order. A replayed row has a new `seq`, at the back.

**Liveness: no row is left without a head in front of it.** That is the invariant, and the one way
to break it is the reason the finishing writers take the lock. Without it: `Accept` of version 2
sees version 1 unfinished and inserts version 2 without the marker. `MarkDelivered` of version 1
runs meanwhile, cannot see the uncommitted version 2, and promotes nothing. Version 2 is unfinished
with no head, forever. The same race exists between a finish and a replay. With the lock, whichever
comes second runs entirely after the first has committed.

**The claim's window.** The claim chooses rows on its snapshot and leases each on its latest
version. Postgres re-evaluates the conditions on that version under the row lock, and both
conditions are columns of the locked row, with no join whose other side could be stale. Between
the snapshot and the lock, a chosen row r may have:

| What happened to r | Its latest version | Outcome |
|---|---|---|
| finished (the old holder of an expired lease delivered it) | no marker | dropped |
| been leased by another claimer | `due_at` in the future | dropped |
| been given a backoff by its holder | `due_at` in the future | dropped |
| died, the next version went in flight, r was replayed | no marker, it is behind that version | dropped |
| died, everything else of the key finished, r was replayed | the marker, and due | leased, rightly: it is the head |
| nothing, but another transaction holds its row lock | | skipped, never waited for |

A row that was promoted after the snapshot is not a candidate in this poll, and is one in the next.

The claim has no condition on the state. The CHECK makes every head unfinished on every version of
every row, so the marker says it. Repeating it is harmful: the planner treats `is_head` and the
state as independent, expects a small fraction of the heads there are, concludes that a walk of
the index would take too long to fill a batch of 100, and ANDs in a bitmap of the index of all
unfinished rows. That was measured during this work (100,202 index entries read to return 100
rows), and the guard test checks the plan's shape because of it.

**Deadlocks.** A writer takes its advisory lock before any row lock. The claim takes row locks only
and never waits for one. `MarkPrepared` and the retry lock one row and nothing else. The lock by
row id reads the row without locking it, and `ordering_key` never changes. So no transaction waits
for an advisory lock while holding a row lock that an advisory lock holder could want. What
remains was there before: a transaction that accepts several deliveries holds several key locks
and must take them in a stable order or retry a deadlock.

## What was considered and rejected

- **Walk the unfinished rows in `seq` order, and ask per row whether an earlier unfinished row of
  its key exists.** The reviewer's probe: the same 10 heads in 0.10 ms where heads are due. But the
  walk visits every row that is not a head on its way. One hot key whose head is backing off or
  leased puts thousands of rows in front of every poll, and in a sink outage, where every head is
  backing off, the walk covers the whole backlog to return nothing. An index on `next_attempt_at`
  does not help: the rows behind a head are due from the moment they are accepted.
- **A recursive skip scan over the keys.** It costs the number of keys with work, which in the
  1,000,000 row shape is 990,000.
- **A marker without the lock in the finishing transitions.** The lost promotion above.
- **A marker kept by a trigger.** The same lock is needed, and the writes would be hidden from a
  reader of `queries.sql`.
- **Leaving the lease out of the index order** (`(next_attempt_at, seq)`, and a filter on the
  lease). Every poll would then revisit every row every worker has in flight: replicas times
  goroutines times batch. It is bounded, but it grows with replicas, and section 9 of the
  architecture promises that replicas add capacity.
- **Keeping the claim's defensive `NOT EXISTS`** (no other unfinished row of the key holds a live
  lease). It was evaluated on the snapshot, not under the lock, and needed the worker to read
  `ordering_key` and `lease_until`. The unique index is stronger and holds at every moment.

## Cost

- Every writer of a key waits for an open writer of the same key. Finishing a row therefore waits
  for an open `Accept` of the same entity: keep accepting transactions short.
- Finishing a row is three statements instead of one.
- A lease moves the row in the index of due heads, so a claim is never a HOT update, and every
  index gets an entry for the new row version. (The review measured 0 HOT updates of 10,000 before
  this change as well, at the default fillfactor.)
- Dead entries collect at the front of the index the claim walks: one for every lease, retry and
  finish. The first poll to pass one marks it, later polls skip it, and the pages that hold them
  (about 260 entries each) stay until a vacuum. A poll therefore costs the batch plus one page per
  260 heads that moved since the last vacuum: 5.9 ms after 990,000 moves with no vacuum at all,
  in the worst shape measured. A long-running transaction anywhere in the database stops both the
  marking and the vacuum, and then every poll walks everything that moved since it began. That is
  true of any queue in Postgres, and it is principle 6 of the architecture again.
- A row inserted behind this package's back, without the marker, into a key that has no head, is
  never delivered. It is also never delivered out of order, which is the failure that was chosen.
  The invariant is one query (`headsBroken` in the tests), should an operator ever need it.
