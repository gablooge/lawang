-- name: LockEntity :exec
-- Serializes the readers and writers of one entity's supersede chain. The pipeline reads the head,
-- demotes it and inserts a new head, and two transactions doing that for one entity at once can
-- corrupt the chain without violating a single constraint: both read head X, the first demotes X
-- and makes A the head, and the second then demotes A and makes B the head with supersedes = X. X
-- would be superseded twice and A would be orphaned in the middle of the chain.
--
-- The unique index catches the common interleaving, where the second transaction demotes before
-- the first has committed and then finds a head already there. It cannot catch that one: by the
-- time the second transaction demotes, the head it takes the marker from is a row it never read,
-- so nothing is violated and the chain is quietly wrong. So the entity is locked before it is read,
-- for the whole transaction, exactly as internal/outbox locks an ordering key before it assigns a
-- seq.
--
-- It must be its own statement, before the read: under READ COMMITTED the next statement takes a
-- new snapshot, so the waiter sees what the holder committed. store begins every transaction READ
-- COMMITTED by name for that reason.
--
-- In the ordinary case there is nothing to wait for. The outbox already delivers one entity's
-- versions one at a time, in arrival order, because they share an ordering key and only the head
-- of a key is ever claimable (ADR 10, ADR 11): that FIFO is what makes arrival order a usable
-- order for two records that carry the same provider version. This lock is the second line of
-- defense, for the shapes the ordering key does not cover: one entity reached through two
-- subscriptions of one tenant, and a reconciliation pass running beside the live feed.
--
-- Two entities whose lock strings hash alike only wait for each other, which is harmless.
SELECT pg_advisory_xact_lock(
  hashtextextended(@tenant_id::text || chr(31) || @provider::text || chr(31) || @external_id::text, 0));

-- name: EntityHead :one
-- The newest record prepared for one entity: what a new record supersedes, and the scope the move
-- detection of ADR 4 decision 7 compares against. No row means the entity has nothing prepared.
--
-- It is an equality probe on the whole of record_ledger_one_head, which holds only the heads.
SELECT record_id, version, scope
  FROM record_ledger
 WHERE tenant_id = @tenant_id
   AND provider = @provider
   AND external_id = @external_id
   AND is_head;

-- name: LedgerEntry :one
-- Whether this record id has already been prepared, and whether it is still its entity's head.
-- Those are conditions 1 and 2 of ADR 4 decision 7. A primary key lookup.
SELECT record_id, is_head, scope
  FROM record_ledger
 WHERE tenant_id = @tenant_id
   AND record_id = @record_id;

-- name: DemoteEntityHead :exec
-- Takes the head marker off the entity's current head, so the new record can take it. Runs under
-- LockEntity, in the same statement order every time.
UPDATE record_ledger
   SET is_head = false
 WHERE tenant_id = @tenant_id
   AND provider = @provider
   AND external_id = @external_id
   AND is_head;

-- name: InsertLedgerEntry :exec
-- Records a record as prepared and as its entity's head. Runs under LockEntity, after
-- DemoteEntityHead, so record_ledger_one_head is satisfied within the transaction.
--
-- A repeat cannot reach this statement: the caller has already asked LedgerEntry, and a record id
-- it knows is skipped or dead-lettered rather than inserted. If one ever did, the primary key
-- refuses it rather than storing a second copy.
INSERT INTO record_ledger
  (tenant_id, record_id, provider, external_id, version, scope, supersedes, is_head)
VALUES
  (@tenant_id, @record_id, @provider, @external_id, @version, @scope, sqlc.narg('supersedes'), true);

-- name: UpsertRedactions :many
-- Gives every (kind, value) the masker found the token that stands for it: the one it already had,
-- or the one this statement stores for it now.
--
-- One statement for a whole delivery, not one per value: the masker collects everything it found
-- first, so nothing here runs in a loop however many addresses a text holds.
--
-- ON CONFLICT DO UPDATE and not DO NOTHING, because DO NOTHING returns no row for a value that is
-- already mapped and the caller needs that value's existing token. The update writes last_seen_at,
-- which is the column an operator prunes the map by.
INSERT INTO redaction_map (tenant_id, token, kind, value)
SELECT @tenant_id::text, tok.token, knd.kind, val.value
  FROM unnest(@tokens::text[]) WITH ORDINALITY AS tok(token, n)
  JOIN unnest(@kinds::text[])  WITH ORDINALITY AS knd(kind, n)  USING (n)
  JOIN unnest(@values::text[]) WITH ORDINALITY AS val(value, n) USING (n)
ON CONFLICT ON CONSTRAINT redaction_map_one_token_per_value
DO UPDATE SET last_seen_at = now()
RETURNING token, kind, value;
