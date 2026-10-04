# 14. The records of a delivery are rows of their own, and that is where the per-record dead letter lives

Status: accepted, 2026-10-04. Built in #10 (B10).

## Context

Two things were owed here, and they turn out to be the same thing.

**The prepared records had nowhere to live.** Architecture 3.2 has said since before B04 that the
drain commits "the ledger rows and the prepared records together" in step 6, delivers in step 7,
and that a row which is already `prepared` is "delivered from what step 6 stored and is **not**
prepared again". Nothing stored them. `record_ledger` holds one row per record id that has been
**prepared** (ADR 12, decision 2), so a worker that prepared a delivery and then died could not
derive its records a second time: every one of them would come back from the ledger as already
prepared, the pipeline would skip them all, and the drain would mark the delivery delivered with
nothing sent. The acceptance line "a crash injected between prepare and deliver re-drains into
already delivered with exactly one record at the stub" cannot be met without storing them.

**`sink.DeliveryResult.Rejected` had no API behind it.** A sink answers in exactly two ways
(ADR 13, and the `Sink` interface): a `*Fault`, where nothing was delivered and the whole batch
goes again, or a `DeliveryResult` that covers every record of the batch, where the records it
names were refused and every other one was taken. The second one could not be recorded. Every
outbox transition takes a `Claimed`, which is one row and one delivery, while a delivery carries
several records and `dead_reason` is a column on the row:

- `MarkDelivered` on the row loses the dead letter, and the refused record with it, silently.
  That is exactly the loss the B09 redesign removed at the sink, arriving one layer down.
- `MarkDead` on the row kills the records that landed.

Architecture section 11 has promised "sink rejects one record: that record dead-letters; the rest
of the batch lands" since before B04. The cheap way out under schedule pressure was to collapse
every `Rejection` into a whole-row `MarkDead`, which is the one that loses records, and the
honest alternative was to write the promise down as not kept and define what a `Rejection` makes
the worker do instead.

## Decision

**One table, `outbox_record`, holding the records of a delivery, with a state and a dead letter
of its own. The promise is kept.**

```sql
CREATE TABLE outbox_record (
  outbox_id   text        NOT NULL REFERENCES outbox (id) ON DELETE CASCADE,
  record_id   text        NOT NULL,
  pos         integer     NOT NULL,   -- the order they must be delivered in
  document    bytea       NOT NULL,   -- exactly what record.Record.MarshalJSON wrote
  state       text        NOT NULL DEFAULT 'prepared',  -- prepared | delivered | dead
  dead_reason text        NOT NULL DEFAULT '',
  last_error  text        NOT NULL DEFAULT '',
  ...
);
```

It was going to be built for the first reason whatever happened to the second, and once it
exists the second costs one more column: a record that was refused is `dead` and carries its
`outbox.Cause`, the rest of the batch is `delivered`, and the row itself finishes as
`delivered`, because the delivery was made. `outbox.MarkDelivered` now takes the refused
records beside the claim and writes all three in one transaction.

Four things follow from it, each of which is the reason for a line of code elsewhere:

1. **A re-drain delivers what was stored, not what it can derive.** `record.Reopen` reads a
   document back and mints its id again from the fields it carries plus the provider and the
   tenant, refusing a document whose stored id is not the one that comes out. A decoded record
   knows no tenant (the tenant is in no field of the envelope), so without this every sink would
   refuse a re-drained record as another tenant's; with it, a document edited in the table, or
   read back for the wrong tenant or the wrong provider, is refused instead of delivered to
   somebody.
2. **The delivery offers what is left.** A claim is given the records that are neither delivered
   nor dead, so a crash after a partial delivery never offers a record twice and a dead letter
   waits for a replay.
3. **Replay has a second case.** A dead row replays as before. A **delivered** row is replayable
   only while one of its records is dead, and then the dead records come back with it. Without
   that, a per-record dead letter would be a dead letter nothing can replay, which is not a dead
   letter at all.
4. **The tenant is not a column here.** The table's row-level security policy asks the delivery's
   own row for the tenant. A second copy of the tenant is a second thing that can disagree, and a
   record row written under tenant A against tenant B's delivery would be invisible to B, whose
   drain would then deliver a batch with a record missing from it and mark the delivery
   delivered. That is the silent loss this table exists to remove, so it is made unwritable
   rather than merely wrong, with no composite foreign key and no second unique index on the hot
   `outbox` table to maintain one.

## Why not the other option

Writing the promise down as not kept means one of two things, and both are worse than a table.

Either a refused record kills the whole delivery, which destroys the records the receiver
accepted, and a sink that refuses one record of a batch of six would cost five records that
landed. Or a refused record is dropped with a log line, which is the silent loss that the whole
redesign of B09 exists to remove, now moved one layer down where the operator cannot even see it
in `last_error`.

The cost of the table is real and is accepted:

- **A migration and a second copy of the record.** A delivery's documents sit beside its
  `raw_body` until retention (B25) deletes the row, which cascades. For the ClickUp and Slack
  shapes of v0.1 a document is a few kilobytes against a webhook body of a few kilobytes, so the
  outbox roughly doubles in size per unfinished delivery. The alternative to storing it is not
  storing less, it is losing deliveries on a crash.
- **A package three other items depend on changes shape.** `MarkPrepared` is gone and
  `PrepareIn(ctx, tx, claimed, records)` takes its place: it takes the caller's transaction,
  because the records and the ledger rows have to commit together and a signature that opened
  its own transaction could not do that. `MarkDelivered` grew the refused records as a
  parameter, so that no caller can finish a delivery without saying what happened to each
  record. Both are compile-time changes and the only caller is the drain.
- **Two places an operator looks for a dead letter.** A dead row, and a dead record of a
  delivered row. B25's metrics and operator surface have to cover both, which is written on
  [issue #25](https://github.com/gablooge/lawang/issues/25#issuecomment-5975283608) together
  with the second half of it: there is no Go API for a per-record dead letter yet.
  `PreparedRecords` returns `state = 'prepared'` only, because it answers "what does this claim
  still have to deliver", so the `dead_reason` and `last_error` of a record are written by
  `MarkRecordDead` and read by nothing outside the tests. That is deliberate for now and it
  means an operator today has raw SQL and nothing else for one of the two places. B25 is where
  the reader belongs.

## Consequences

- `docs/architecture.md` 3.2 steps 6 and 7, its replay paragraph and section 11 say all of the
  above; this record holds the reasoning.
- The worker never re-prepares a prepared row, and the thing that makes that safe is in the
  database and not in the drain's control flow: the records are there or the row is not
  `prepared`, because one transaction writes both.
- A sink author's contract is unchanged. `Rejected` now has somewhere to go.
