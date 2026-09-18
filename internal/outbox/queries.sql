-- name: LockOrderingKey :exec
-- Serializes every writer that gives a row of one ordering key its place in the queue (Accept and
-- Replay). seq is assigned when the statement runs, not when it commits: without this lock, v1
-- could be inserted first and commit last, and a claim in between would lease v2 and then v1 while
-- v2 is still in flight. The lock is held to the end of the transaction, so within one key, seq
-- order is commit order, and a row is never visible before a row of its key with a lower seq.
--
-- It must be its own statement, before the one that assigns seq. Two keys that hash alike only
-- wait for each other, which is harmless.
SELECT pg_advisory_xact_lock(hashtextextended(@tenant_id::text || chr(31) || @ordering_key::text, 0));

-- name: Accept :one
-- Runs bound to the tenant that owns the verified subscription, after LockOrderingKey. A repeated
-- delivery returns no row.
INSERT INTO outbox (id, tenant_id, provider, delivery_id, ordering_key, raw_body)
VALUES (@id, @tenant_id, @provider, @delivery_id, @ordering_key, @raw_body)
ON CONFLICT (tenant_id, delivery_id) DO NOTHING
RETURNING id;

-- name: Claim :many
-- Runs as the worker role, across tenants. Only the HEAD of each ordering key is ever eligible:
-- the unfinished row with the lowest seq. If the head is leased or waiting out a backoff, nothing
-- behind it moves, which is what keeps one entity's versions in order, one at a time.
--
-- The head is found on this statement's snapshot, but a row is leased on its LATEST version, which
-- another transaction may have changed between the snapshot and the row lock. On that re-check
-- Postgres re-evaluates only the conditions on the locked row itself (c), against the head row it
-- joined before. So every condition that must hold at lock time is on c:
--   state     the old holder of an expired lease may have finished the row meanwhile;
--   lease     another claimer may have leased it meanwhile;
--   seq       the row may have died and been replayed to the back of its queue meanwhile, so it is
--             not the head it was on the snapshot, and a row behind it may be in flight by now.
-- With LockOrderingKey, these make the leased row the true head of its key at lock time, and a key
-- with a leased head has nothing else claimable. The NOT EXISTS is a second line of defense on
-- the snapshot, for a row that reached the table without LockOrderingKey: a key is not eligible
-- while any other unfinished row of it holds a live lease.
UPDATE outbox o
   SET lease_until = now() + make_interval(secs => @lease_seconds::float8),
       lease_token = @lease_token::text,
       attempts    = o.attempts + 1
  FROM (
        SELECT c.id
          FROM outbox c
          JOIN (
                SELECT DISTINCT ON (tenant_id, ordering_key) id, seq
                  FROM outbox
                 WHERE state IN ('pending', 'prepared')
                 ORDER BY tenant_id, ordering_key, seq
               ) head ON head.id = c.id
         WHERE c.state IN ('pending', 'prepared')
           AND c.seq = head.seq
           AND c.next_attempt_at <= now()
           AND (c.lease_until IS NULL OR c.lease_until < now())
           AND NOT EXISTS (
                SELECT 1
                  FROM outbox l
                 WHERE l.tenant_id = c.tenant_id
                   AND l.ordering_key = c.ordering_key
                   AND l.state IN ('pending', 'prepared')
                   AND l.id <> c.id
                   AND l.lease_until >= now()
               )
         ORDER BY c.seq
         LIMIT @batch_size
           FOR UPDATE OF c SKIP LOCKED
       ) claimed
 WHERE o.id = claimed.id
RETURNING o.id, o.tenant_id, o.attempts;

-- name: Get :one
SELECT id, seq, tenant_id, provider, delivery_id, ordering_key, raw_body, state, attempts,
       next_attempt_at, lease_until, last_error, dead_reason, accepted_at, prepared_at, finished_at
  FROM outbox
 WHERE id = @id;

-- The transitions below run bound to the row's tenant, and only for the holder of the current
-- lease. A worker whose lease ran out and was taken over changes nothing.

-- name: MarkPrepared :execrows
UPDATE outbox SET state = 'prepared', prepared_at = now()
 WHERE id = @id AND lease_token = @lease_token::text AND state = 'pending';

-- name: MarkDelivered :execrows
UPDATE outbox
   SET state = 'delivered', finished_at = now(), lease_until = NULL, lease_token = NULL, last_error = ''
 WHERE id = @id AND lease_token = @lease_token::text AND state IN ('pending', 'prepared');

-- name: Retry :execrows
-- The state is kept: a prepared row retries its delivery, it does not prepare again.
UPDATE outbox
   SET next_attempt_at = now() + make_interval(secs => @delay_seconds::float8),
       last_error = @last_error, lease_until = NULL, lease_token = NULL
 WHERE id = @id AND lease_token = @lease_token::text AND state IN ('pending', 'prepared');

-- name: MarkDead :execrows
UPDATE outbox
   SET state = 'dead', dead_reason = @dead_reason, last_error = @last_error, finished_at = now(),
       lease_until = NULL, lease_token = NULL
 WHERE id = @id AND lease_token = @lease_token::text AND state IN ('pending', 'prepared');

-- name: LockOrderingKeyOf :exec
-- LockOrderingKey for a row known only by its id. Runs bound to the row's tenant, before Replay.
SELECT pg_advisory_xact_lock(hashtextextended(tenant_id::text || chr(31) || ordering_key, 0))
  FROM outbox
 WHERE id = @id;

-- name: Replay :execrows
-- Dead letters are a row state, so replay is an UPDATE. Runs after LockOrderingKeyOf.
--
-- The row goes to the BACK of its key's queue: it takes a fresh seq, exactly as if it had just
-- been accepted. A dead row does not hold back newer versions, so by the time it is replayed a
-- newer version may be delivered, or in flight right now. If the row kept its old seq it would
-- become the head again and be leased alongside the version in flight. The replayed version is
-- therefore delivered after every version accepted before the replay, and the forward-only
-- supersede chain handles it like any other late arrival of an old version.
--
-- A row that died after it was prepared goes back to prepared, not pending: its ledger rows and
-- prepared records are committed, and a prepared row is never prepared again.
UPDATE outbox
   SET seq = DEFAULT,
       state = CASE WHEN prepared_at IS NULL THEN 'pending' ELSE 'prepared' END,
       attempts = 0, next_attempt_at = now(), dead_reason = '', finished_at = NULL
 WHERE id = @id AND state = 'dead';
