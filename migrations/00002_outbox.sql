-- +goose Up

-- The only queue. Accepting a webhook is an INSERT here; retries and dead letters are row states,
-- so replaying a dead letter is an UPDATE.
CREATE TABLE outbox (
  id              text        PRIMARY KEY,                       -- ULID
  seq             bigint      NOT NULL GENERATED ALWAYS AS IDENTITY, -- arrival order
  tenant_id       tenant_id   NOT NULL,
  provider        text        NOT NULL CHECK (provider <> ''),
  -- blake3(provider, raw_body). Unique per tenant, not globally: two tenants may connect the same
  -- provider workspace, and reconciliation synthesizes byte-identical deliveries for both.
  delivery_id     text        NOT NULL CHECK (delivery_id <> ''),
  -- All rows that share a key deliver in arrival order. One key per source entity.
  ordering_key    text        NOT NULL CHECK (ordering_key <> ''),
  raw_body        bytea       NOT NULL,                          -- exactly as received
  state           text        NOT NULL DEFAULT 'pending'
                              CHECK (state IN ('pending', 'prepared', 'delivered', 'dead')),
  attempts        integer     NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  -- A claim is a lease, not a held lock, because the work spans two transactions and a sink call.
  -- A worker that dies simply lets its lease run out.
  lease_until     timestamptz,
  lease_token     text,
  last_error      text        NOT NULL DEFAULT '',
  dead_reason     text        NOT NULL DEFAULT '',
  accepted_at     timestamptz NOT NULL DEFAULT now(),
  finished_at     timestamptz,
  UNIQUE (tenant_id, delivery_id),
  CHECK ((lease_until IS NULL) = (lease_token IS NULL))
);

-- The claim reads the earliest unfinished row of every key.
CREATE INDEX outbox_unfinished ON outbox (tenant_id, ordering_key, seq)
  WHERE state IN ('pending', 'prepared');

ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON outbox
  USING (tenant_id = current_tenant())
  WITH CHECK (tenant_id = current_tenant());

-- The worker role claims across tenants, because it cannot know which tenants have work. It sees
-- only what a scheduler needs, and may change only the lease. It can never read a payload: the
-- work itself happens afterwards, as the application role bound to the claimed row's own tenant.
CREATE POLICY worker_claim_select ON outbox FOR SELECT TO sluiceway_worker USING (true);
CREATE POLICY worker_claim_update ON outbox FOR UPDATE TO sluiceway_worker USING (true) WITH CHECK (true);

GRANT SELECT (id, seq, tenant_id, ordering_key, state, attempts, next_attempt_at, lease_until, lease_token)
  ON outbox TO sluiceway_worker;
GRANT UPDATE (attempts, lease_until, lease_token) ON outbox TO sluiceway_worker;

-- +goose Down
DROP TABLE outbox;
