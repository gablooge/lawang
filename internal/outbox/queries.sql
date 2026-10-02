-- name: LockOrderingKey :exec
-- Serializes every writer that changes which rows of one ordering key are unfinished, or which of
-- them is the head: Accept, Replay, and the two transitions that finish a row (MarkDelivered,
-- MarkDead). It does two jobs.
--
-- Queue order. seq is assigned when the statement runs, not when it commits: without this lock, v1
-- could be inserted first and commit last. The lock is held to the end of the transaction, so
-- within one key, seq order is commit order.
--
-- The head marker. Accept decides whether its row is the head by asking whether the key has an
-- unfinished row, and a finishing transition hands the marker to the next unfinished row. Without
-- the lock the two can miss each other: Accept sees v1 unfinished and inserts v2 as a non-head,
-- while the MarkDelivered of v1 does not see the uncommitted v2 and promotes nothing. v2 would
-- never be claimable. With the lock, one of them runs entirely after the other has committed.
--
-- It must be its own statement, before the one that reads or writes the key's rows: under READ
-- COMMITTED the next statement's snapshot is then taken after the previous holder committed. Under
-- any other level it is not (the snapshot is the transaction's, from before the wait), and the two
-- miss each other just the same. store begins every transaction READ COMMITTED by name for that
-- reason, whatever default_transaction_isolation says. Two keys that hash alike only wait for each
-- other, which is harmless. The claim never takes it.
SELECT pg_advisory_xact_lock(hashtextextended(@tenant_id::text || chr(31) || @ordering_key::text, 0));

-- name: Accept :one
-- Runs bound to the tenant that owns the verified subscription, after LockOrderingKey. A repeated
-- delivery returns no row. The row is the head of its key exactly when the key has no unfinished
-- row: a probe of one key in outbox_unfinished, whatever the size of the table.
--
-- The tenant and the key are used twice, so they are cast at every use: left to itself, Postgres
-- deduces the tenant_id domain for one use and text for the other, and refuses the statement.
INSERT INTO outbox (id, tenant_id, provider, delivery_id, ordering_key, raw_body, is_head)
VALUES (@id, @tenant_id::text, @provider, @delivery_id, @ordering_key::text, @raw_body,
        NOT EXISTS (
          SELECT 1
            FROM outbox u
           WHERE u.tenant_id = @tenant_id::text
             AND u.ordering_key = @ordering_key::text
             AND u.state IN ('pending', 'prepared')
        ))
ON CONFLICT (tenant_id, delivery_id) DO NOTHING
RETURNING id;

-- name: Park :one
-- Stores a delivery nobody can be shown to own, under the sentinel tenant, already finished.
--
-- It takes no advisory lock, and that is not an omission. The lock exists to keep queue order and
-- the head marker within one ordering key, and a parked row has neither: it is inserted 'dead' and
-- is_head false, so nothing ever claims it, nothing waits behind it, and the "is the key
-- unfinished?" test that Accept and Replay make (which counts only pending and prepared rows)
-- cannot see it. Its ordering key is its own delivery id, so no two parked rows share a key and a
-- flood of unattributable deliveries queues on nothing at all.
--
-- A repeated delivery returns no row, exactly as Accept does: the unique key is
-- (tenant_id, delivery_id), and the sentinel is one tenant, so re-sending an unowned delivery
-- parks it once.
INSERT INTO outbox (id, tenant_id, provider, delivery_id, ordering_key, raw_body,
                    state, is_head, dead_reason, finished_at)
VALUES (@id, @tenant_id::text, @provider, @delivery_id::text, @delivery_id::text, @raw_body,
        'dead', false, @dead_reason, now())
ON CONFLICT (tenant_id, delivery_id) DO NOTHING
RETURNING id;

-- name: Claim :many
-- Runs as the worker role, across tenants. Only the HEAD of an ordering key is ever eligible, and
-- being the head is a stored column (is_head), so this statement walks outbox_due_heads from its
-- front and stops at the batch. It never visits a row behind a head, a head waiting out a backoff,
-- or a head another worker holds (due_at covers both the backoff and the lease): the cost follows
-- the batch, not the backlog. If the head is leased or backing off, nothing behind it moves,
-- because nothing behind it is a head.
--
-- The candidates are found on this statement's snapshot, but a row is leased on its LATEST
-- version, which another transaction may have changed between the snapshot and the row lock.
-- Postgres then re-evaluates the conditions below against that latest version, and both are
-- conditions on the locked row itself, with no join whose other side could be stale:
--   is_head   the row may have finished and handed the marker on, or died and been replayed to
--             the back of its queue behind a version that is in flight by now;
--   due_at    another claimer may have leased it meanwhile, or it may have been given a backoff.
-- A leased row is therefore a head at lock time. outbox_one_head allows one head per key, and an
-- unfinished row keeps the marker until it finishes, so no other row of the key can be in flight.
--
-- There is deliberately no condition on the state. The table's CHECK makes every head unfinished,
-- on every version of every row, so is_head says it all. Saying it again is not free: the planner
-- takes is_head and the state for independent, expects a fraction of the heads there are, decides
-- that walking the index to fill a batch of 100 would take too long, and ANDs in a bitmap of
-- outbox_unfinished instead, which is every waiting row (measured: 100,000 index entries read to
-- return 100 rows). Do not add one.
UPDATE outbox o
   SET lease_until = now() + make_interval(secs => @lease_seconds::float8),
       lease_token = @lease_token::text,
       attempts    = o.attempts + 1
  FROM (
        SELECT c.id
          FROM outbox c
         WHERE c.is_head
           AND c.due_at <= now()
         ORDER BY c.due_at, c.seq
         LIMIT @batch_size
           FOR UPDATE OF c SKIP LOCKED
       ) claimed
 WHERE o.id = claimed.id
RETURNING o.id, o.tenant_id, o.attempts;

-- name: Get :one
SELECT id, seq, tenant_id, provider, delivery_id, ordering_key, raw_body, state, is_head, attempts,
       next_attempt_at, lease_until, last_error, dead_reason, accepted_at, prepared_at, finished_at
  FROM outbox
 WHERE id = @id;

-- The transitions below run bound to the row's tenant, and only for the holder of the current
-- lease. A worker whose lease ran out and was taken over changes nothing.

-- name: MarkPrepared :execrows
UPDATE outbox SET state = 'prepared', prepared_at = now()
 WHERE id = @id AND lease_token = @lease_token::text AND state = 'pending';

-- name: Retry :execrows
-- The state is kept: a prepared row retries its delivery, it does not prepare again. The row stays
-- the head of its key, so nothing behind it moves while it waits.
UPDATE outbox
   SET next_attempt_at = now() + make_interval(secs => @delay_seconds::float8),
       last_error = @last_error, lease_until = NULL, lease_token = NULL
 WHERE id = @id AND lease_token = @lease_token::text AND state IN ('pending', 'prepared');

-- The two transitions that FINISH a row run after LockOrderingKeyOf and are followed, in the same
-- transaction, by PromoteNextHead. They return the key to promote in. No row means the lease was
-- lost, and then nothing is promoted.

-- name: MarkDelivered :one
UPDATE outbox
   SET state = 'delivered', is_head = false, finished_at = now(),
       lease_until = NULL, lease_token = NULL, last_error = ''
 WHERE id = @id AND lease_token = @lease_token::text AND state IN ('pending', 'prepared')
RETURNING ordering_key;

-- name: MarkDead :one
UPDATE outbox
   SET state = 'dead', is_head = false, dead_reason = @dead_reason, last_error = @last_error,
       finished_at = now(), lease_until = NULL, lease_token = NULL
 WHERE id = @id AND lease_token = @lease_token::text AND state IN ('pending', 'prepared')
RETURNING ordering_key;

-- name: PromoteNextHead :exec
-- Hands the head marker to the earliest unfinished row of the key, if there is one. A statement of
-- its own, after the one that finished the old head: the unique index on heads is checked row by
-- row, so the old head has to be gone first, and this statement has to see that it is. Under the
-- key's lock no Accept or Replay of the key is open, so the row chosen here really is the earliest.
UPDATE outbox
   SET is_head = true
 WHERE id = (
        SELECT n.id
          FROM outbox n
         WHERE n.tenant_id = @tenant_id::text
           AND n.ordering_key = @ordering_key::text
           AND n.state IN ('pending', 'prepared')
         ORDER BY n.seq
         LIMIT 1
       );

-- name: LockOrderingKeyOf :exec
-- LockOrderingKey for a row known only by its id. Runs bound to the row's tenant, before Replay
-- and before the finishing transitions. It reads the row without locking it (ordering_key never
-- changes), so the advisory lock always comes before any row lock.
SELECT pg_advisory_xact_lock(hashtextextended(tenant_id::text || chr(31) || ordering_key, 0))
  FROM outbox
 WHERE id = @id;

-- name: StrandedKeys :many
-- Detection, not part of any delivery path. The ordering keys of one tenant that have unfinished
-- rows and NO head: the one broken state that is silent, because nothing of such a key is ever
-- claimed and nothing fails. (The other ways to break the marker are refused by the table: two
-- heads, and a finished head.) This package never produces it. A writer that does not come through
-- this package can, and so could a bug in a later change. The repair is in ADR 10.
--
-- Runs bound to the tenant, as the application role. It cannot run as the worker role, which
-- deliberately reads neither the state nor the ordering key of a row.
--
-- It reads the tenant's unfinished rows from outbox_unfinished, in key order, and for each that is
-- not a head asks outbox_one_head whether the key has one. So it costs the tenant's backlog, not
-- the table, and never touches the delivered history. Measured on 4,000,000 rows: 36 ms and 67,631
-- buffers for a tenant with 99,100 unfinished rows, 90,000 of them behind 20 heads, and 2 ms for a
-- tenant with 9,100. One statement is one snapshot, and every writer of this package leaves the
-- invariant intact when it commits, so a key reported here is broken, not merely in the middle of
-- being written.
SELECT u.ordering_key
  FROM outbox u
 WHERE u.tenant_id = @tenant_id::text
   AND u.state IN ('pending', 'prepared')
   AND NOT u.is_head
   AND NOT EXISTS (
         SELECT 1
           FROM outbox h
          WHERE h.tenant_id = u.tenant_id
            AND h.ordering_key = u.ordering_key
            AND h.is_head
       )
 GROUP BY u.ordering_key
 ORDER BY u.ordering_key
 LIMIT @max_keys;

-- name: Replay :execrows
-- Dead letters are a row state, so replay is an UPDATE. Runs after LockOrderingKeyOf.
--
-- The row goes to the BACK of its key's queue: it takes a fresh seq, exactly as if it had just
-- been accepted, and like an accepted row it is the head only if its key has no unfinished row. A
-- dead row does not hold back newer versions, so by the time it is replayed a newer version may be
-- delivered, or in flight right now, and then the replayed row waits behind it. The replayed
-- version is therefore delivered after every version accepted before the replay, and the
-- forward-only supersede chain handles it like any other late arrival of an old version.
--
-- A row that died after it was prepared goes back to prepared, not pending: its ledger rows and
-- prepared records are committed, and a prepared row is never prepared again.
UPDATE outbox r
   SET seq = DEFAULT,
       state = CASE WHEN r.prepared_at IS NULL THEN 'pending' ELSE 'prepared' END,
       is_head = NOT EXISTS (
         SELECT 1
           FROM outbox u
          WHERE u.tenant_id = r.tenant_id
            AND u.ordering_key = r.ordering_key
            AND u.state IN ('pending', 'prepared')
       ),
       attempts = 0, next_attempt_at = now(), dead_reason = '', finished_at = NULL
 WHERE r.id = @id AND r.state = 'dead';
