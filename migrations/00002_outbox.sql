-- +goose Up

-- The only queue. Accepting a webhook is an INSERT here; retries and dead letters are row states,
-- so replaying a dead letter is an UPDATE.
CREATE TABLE outbox (
  id              text        PRIMARY KEY,                       -- ULID
  -- The place in the queue: arrival order, except that a replayed dead letter takes a fresh value
  -- and so goes to the back. Every statement that assigns one first takes the advisory lock of the
  -- row's ordering key (LockOrderingKey in internal/outbox/queries.sql), so that within one key a
  -- lower seq always commits first.
  seq             bigint      NOT NULL GENERATED ALWAYS AS IDENTITY,
  tenant_id       tenant_id   NOT NULL,
  provider        text        NOT NULL CHECK (provider <> ''),
  -- blake3(provider, raw_body). Unique per tenant, not globally: two tenants may connect the same
  -- provider workspace, and reconciliation synthesizes byte-identical deliveries for both.
  delivery_id     text        NOT NULL CHECK (delivery_id <> ''),
  -- All rows that share a key deliver in arrival order. One key per source entity. Bounded, because
  -- it is a column of two indexes and a btree tuple cannot exceed about 2700 bytes: a longer key
  -- would fail the INSERT, or not, depending on how well it compresses. internal/outbox refuses one
  -- by name (ErrBadOrderingKey, MaxOrderingKeyLen) before it gets here.
  ordering_key    text        NOT NULL CHECK (ordering_key <> '' AND octet_length(ordering_key) <= 512),
  raw_body        bytea       NOT NULL,                          -- exactly as received
  state           text        NOT NULL DEFAULT 'pending'
                              CHECK (state IN ('pending', 'prepared', 'delivered', 'dead')),
  -- The head marker: true for exactly the earliest unfinished row of its (tenant, ordering key),
  -- and only a head is ever claimable. It is stored, not computed by the claim, so that a claim
  -- costs what it returns and not what is waiting (docs/adr/0010-outbox-head-marker.md). Writers
  -- keep it under the ordering key's advisory lock: Accept and Replay set it when the key has no
  -- unfinished row, and finishing the head hands it to the next row in the same transaction. The
  -- database refuses the two states that would break ordering: a finished row that is still a head
  -- (the CHECK below) and two heads for one key (outbox_one_head). A row that reaches the table
  -- any other way is not a head, and waits until the head of its key finishes. The claim relies on
  -- that CHECK: it asks for is_head and does not look at the state.
  is_head         boolean     NOT NULL DEFAULT false,
  attempts        integer     NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  -- A claim is a lease, not a held lock, because the work spans two transactions and a sink call.
  -- A worker that dies simply lets its lease run out.
  lease_until     timestamptz,
  lease_token     text,
  -- When the row may next be claimed: once its backoff is over AND its lease has run out. One
  -- column, so that one index range holds exactly the claimable rows, and a poll visits neither
  -- the rows waiting out a backoff nor the rows other workers hold. (GREATEST ignores a NULL, so
  -- a row with no lease is due at next_attempt_at.)
  due_at          timestamptz NOT NULL GENERATED ALWAYS AS (GREATEST(next_attempt_at, lease_until)) STORED,
  -- last_error and dead_reason are plain text that operators read and every backup carries. They
  -- must never hold token material, a URL, or text written by a remote system. internal/outbox
  -- writes last_error from an outbox.Cause (one of its own texts, an HTTP status, and a remote
  -- error code only if it looks like one) and dead_reason from a fixed text, and offers no way to
  -- pass an error string in: the error of an HTTP client quotes the request URL, query string and
  -- API key included.
  last_error      text        NOT NULL DEFAULT '',
  dead_reason     text        NOT NULL DEFAULT '',
  accepted_at     timestamptz NOT NULL DEFAULT now(),
  -- Set once the ledger rows and prepared records are committed. It outlives the state, so that a
  -- replayed dead letter knows whether it goes back to pending or to prepared.
  prepared_at     timestamptz,
  finished_at     timestamptz,
  UNIQUE (tenant_id, delivery_id),
  CHECK ((lease_until IS NULL) = (lease_token IS NULL)),
  CONSTRAINT outbox_head_is_unfinished CHECK (NOT is_head OR state IN ('pending', 'prepared'))
);

-- The unfinished rows of one key, in queue order. Accept and Replay ask it whether the key has
-- any, and finishing a head asks it for the next one. Each is a probe of one key.
CREATE INDEX outbox_unfinished ON outbox (tenant_id, ordering_key, seq)
  WHERE state IN ('pending', 'prepared');

-- At most one head per key. This is what makes "never two versions of one entity in flight" a
-- property of the database and not of the callers' discipline.
CREATE UNIQUE INDEX outbox_one_head ON outbox (tenant_id, ordering_key) WHERE is_head;

-- What the claim walks: heads only, in the order they fall due. Its size is the number of keys
-- with work, never the number of rows behind them, and the claim reads only its due front.
CREATE INDEX outbox_due_heads ON outbox (due_at, seq) WHERE is_head;

ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON outbox
  USING (tenant_id = current_tenant())
  WITH CHECK (tenant_id = current_tenant());

-- The worker role claims across tenants, because it cannot know which tenants have work. It sees
-- only what a scheduler needs, and may change only the lease. It can never read a payload: the
-- work itself happens afterwards, as the application role bound to the claimed row's own tenant.
-- It writes lease_token but cannot read it back: the token guards every transition. It cannot
-- write is_head, so it can never make a row claimable.
CREATE POLICY worker_claim_select ON outbox FOR SELECT TO sluiceway_worker USING (true);
CREATE POLICY worker_claim_update ON outbox FOR UPDATE TO sluiceway_worker USING (true) WITH CHECK (true);

GRANT SELECT (id, seq, tenant_id, is_head, attempts, due_at) ON outbox TO sluiceway_worker;
GRANT UPDATE (attempts, lease_until, lease_token) ON outbox TO sluiceway_worker;

-- +goose Down
DROP TABLE outbox;
