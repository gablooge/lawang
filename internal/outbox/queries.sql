-- name: Accept :one
-- Runs bound to the tenant that owns the verified subscription. A repeated delivery returns no row.
INSERT INTO outbox (id, tenant_id, provider, delivery_id, ordering_key, raw_body)
VALUES (@id, @tenant_id, @provider, @delivery_id, @ordering_key, @raw_body)
ON CONFLICT (tenant_id, delivery_id) DO NOTHING
RETURNING id;

-- name: Claim :many
-- Runs as the worker role, across tenants. Only the HEAD of each ordering key is ever eligible:
-- the earliest unfinished row. If the head is leased or waiting out a backoff, nothing behind it
-- moves, which is what keeps one entity's versions in arrival order.
--
-- The state and lease conditions are repeated on the locked row itself, so that a row another
-- claimer changed between this statement's snapshot and its lock is re-checked, not trusted.
UPDATE outbox o
   SET lease_until = now() + make_interval(secs => @lease_seconds::float8),
       lease_token = @lease_token::text,
       attempts    = o.attempts + 1
  FROM (
        SELECT c.id
          FROM outbox c
          JOIN (
                SELECT DISTINCT ON (tenant_id, ordering_key) id
                  FROM outbox
                 WHERE state IN ('pending', 'prepared')
                 ORDER BY tenant_id, ordering_key, seq
               ) head ON head.id = c.id
         WHERE c.state IN ('pending', 'prepared')
           AND c.next_attempt_at <= now()
           AND (c.lease_until IS NULL OR c.lease_until < now())
         ORDER BY c.seq
         LIMIT @batch_size
           FOR UPDATE OF c SKIP LOCKED
       ) claimed
 WHERE o.id = claimed.id
RETURNING o.id, o.tenant_id, o.attempts;

-- name: Get :one
SELECT id, seq, tenant_id, provider, delivery_id, ordering_key, raw_body, state, attempts,
       next_attempt_at, lease_until, last_error, dead_reason, accepted_at, finished_at
  FROM outbox
 WHERE id = @id;

-- The transitions below run bound to the row's tenant, and only for the holder of the current
-- lease. A worker whose lease ran out and was taken over changes nothing.

-- name: MarkPrepared :execrows
UPDATE outbox SET state = 'prepared'
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

-- name: Replay :execrows
-- Dead letters are a row state, so replay is an UPDATE. The row keeps its seq: if newer versions
-- of the entity were delivered meanwhile, the forward-only supersede chain handles the late arrival.
UPDATE outbox
   SET state = 'pending', attempts = 0, next_attempt_at = now(), dead_reason = '', finished_at = NULL
 WHERE id = @id AND state = 'dead';
