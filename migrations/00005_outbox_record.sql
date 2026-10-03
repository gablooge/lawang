-- +goose Up

-- The records one delivery was prepared into. It is two things that turn out to be one table,
-- and docs/adr/0014-prepared-records.md is the record of that decision.
--
-- 1. What step 6 of the drain path commits beside the ledger rows, and what step 7 delivers.
--    record_ledger holds one row per record id that has been PREPARED, so a worker that prepared
--    a delivery and then died cannot derive its records again: every one of them would be skipped
--    as already prepared and the row would be marked delivered with nothing sent. The architecture
--    has said since before B04 that a prepared row is "delivered from what step 6 stored and is
--    not prepared again". This is what step 6 stores.
--
-- 2. The per-record dead letter of architecture section 11 ("sink rejects one record: that record
--    dead-letters; the rest of the batch lands"). Every outbox transition takes one outbox row,
--    while one delivery carries several records and outbox.dead_reason is a column on the row, so
--    before this table the two moves a worker had for a batch with one refusal in it were to mark
--    the row delivered (losing the dead letter and the refused record with it) or to kill the row
--    (killing the records that landed).
CREATE TABLE outbox_record (
  outbox_id   text        NOT NULL REFERENCES outbox (id) ON DELETE CASCADE,
  -- The record id (record.Seal), which is what a sink is idempotent on.
  record_id   text        NOT NULL CHECK (record_id <> ''),
  -- Where the record sits in the delivery. The pipeline returns the records of one delivery in
  -- the order they must be delivered, and that order is this column.
  pos         integer     NOT NULL CHECK (pos >= 0),
  -- The document exactly as record.Record.MarshalJSON wrote it, which is a record that passed
  -- Validate and whose seal was intact at the moment it was stored. The wire name is NOT applied
  -- (principle 4: it is the sink's, and two sinks may call one source two things), so the stored
  -- document carries the internal provider key in "source" and internal/record can mint the id
  -- again from it and refuse a document that does not hash to the id beside it (record.Reopen).
  --
  -- bytea, like outbox.raw_body, and not jsonb: jsonb reorders members, drops a repeated name and
  -- rewrites numbers, and this column holds bytes that were already checked rather than a document
  -- to query.
  document    bytea       NOT NULL,
  state       text        NOT NULL DEFAULT 'prepared'
                          CHECK (state IN ('prepared', 'delivered', 'dead')),
  -- As on outbox, and under the same rule: plain text an operator reads and every backup carries,
  -- written from a fixed text and from an outbox.Cause, never from anything a remote system sent.
  dead_reason text        NOT NULL DEFAULT '',
  last_error  text        NOT NULL DEFAULT '',
  prepared_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  PRIMARY KEY (outbox_id, record_id),
  -- One position per delivery, so "the order they must be delivered in" is a total order and not
  -- whatever the planner returns for a tie.
  UNIQUE (outbox_id, pos),
  CONSTRAINT outbox_record_finished CHECK ((state = 'prepared') = (finished_at IS NULL)),
  CONSTRAINT outbox_record_dead_has_a_reason CHECK ((dead_reason <> '') = (state = 'dead'))
);

ALTER TABLE outbox_record ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_record FORCE ROW LEVEL SECURITY;

-- The tenant is deliberately NOT a column here, and the policy asks the delivery's own row for it.
--
-- A second copy of the tenant is a second thing that can disagree: a row written under tenant A
-- against tenant B's delivery would be invisible to B, whose drain would then deliver a batch
-- with a record missing from it and mark the delivery delivered, which is the silent loss this
-- whole table exists to remove. Keeping the tenant in one place makes that unwritable rather than
-- merely wrong, with no composite foreign key and no second unique index on the hot outbox table
-- to maintain it. The cost is a primary-key probe of outbox per row, which the planner runs as a
-- semi-join.
--
-- The helper roles are granted nothing on this table, as on no table holding a payload: the drain
-- claims across tenants as lawang_worker and reads what it prepared as the application role,
-- bound to the claimed row's own tenant (architecture section 4).
CREATE POLICY tenant_isolation ON outbox_record
  USING (EXISTS (
          SELECT 1 FROM outbox o
           WHERE o.id = outbox_record.outbox_id
             AND o.tenant_id = current_tenant()))
  WITH CHECK (EXISTS (
          SELECT 1 FROM outbox o
           WHERE o.id = outbox_record.outbox_id
             AND o.tenant_id = current_tenant()));

-- +goose Down
DROP TABLE outbox_record;
