-- name: EntityLockKeys :many
-- The advisory lock key of each entity of one delivery, in one round trip. The caller sorts them
-- and takes them in that order (pipeline.lockOrder), which is what LockEntityKey below is for.
--
-- The key is what is sorted AND what is locked, which is the point of computing it here rather
-- than locking one entity at a time. The locks used to be taken in the order of the external
-- ids while the lock itself was over a 64 bit hash of them, so a hash collision broke the order
-- the sorting exists to create: with external ids A < B < C and hash(A) = hash(C), a delivery
-- carrying {A, B} took the two locks in the opposite order to one carrying {B, C}, which is a
-- deadlock and not a wait. Sorting the hashes makes that unwritable instead of improbable, and
-- two entities whose keys collide now really do only wait for each other.
--
-- The high bit is cleared, which puts every entity lock in the non-negative half of Postgres's
-- one advisory-lock namespace. internal/outbox sets it on its ordering keys, so the two halves
-- are disjoint by construction: see the order below.
SELECT DISTINCT
       (hashtextextended(@tenant_id::text || chr(31) || @provider::text || chr(31) || e, 0)
          & 9223372036854775807::bigint)::bigint AS lock_key
  FROM unnest(@external_ids::text[]) AS e;

-- name: LockEntityKey :exec
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
-- Two entities whose keys collide only wait for each other, which is harmless. That sentence is
-- true because the thing sorted and the thing locked are now the same value: see EntityLockKeys.
--
-- # The order between this lock and the outbox's
--
-- Postgres has ONE advisory lock namespace for the one-argument form this code uses, a 64 bit
-- key, and two modules take locks in it: internal/outbox over (tenant, ordering key), and this
-- one over (tenant, provider, external id). The rule for a transaction that ever holds both is
-- one sentence: **take every advisory lock ascending by key**.
--
-- (The two-argument pg_advisory_xact_lock(int4, int4) would give each module a classid of its
-- own. It was refused on cost and not on correctness: under the rule below a collision inside
-- a module is a wait either way, so 32 bit keys would not bring the deadlock back, they would
-- add false contention between unrelated entities, which starts to matter around 10,000 keys
-- per module. ADR 12 decision 1 has the arithmetic.)
--
-- That is a total order, and it needs no premise about the two modules' keys, because the
-- namespace is split by construction. The outbox sets the high bit of its key and this
-- statement clears it, so an ordering-key value is always negative, an entity-key value is
-- always non-negative, and the two sets are disjoint. Ascending order therefore puts every
-- ordering-key lock before every entity lock on its own, which is the order the rule used to
-- state as two clauses ("the ordering-key lock first, then the entity locks").
--
-- The split is the point. The rule used to be two clauses over one undivided namespace, and it
-- was a total order only while no ordering-key value ever equalled an entity-key value. Nothing
-- enforced that, and it is exactly the "at 64 bits it will not happen" reasoning the paragraph
-- above exists to delete: with K an ordering key equal to entity key b, a transaction holding
-- {K, a, b} takes K then a then b, while a Prepare holding {b, K} would take b then K, and the
-- two deadlock. (The two formulas cannot collide other than by hash: the outbox hashes
-- tenant \x1f provider:subscription and this hashes tenant \x1f provider \x1f external_id, and
-- a provider key admits neither ':' nor \x1f, so the strings always differ.) Each half still
-- holds 63 bits, and a collision WITHIN a half is a wait and not a deadlock, which is what the
-- paragraph above is about.
--
-- Nothing in the drain holds both today. The worker prepares a delivery in a transaction that
-- takes only entity locks, and finishes it in a later transaction that takes only the ordering
-- key's (internal/outbox, finishIn). The rule is written down because the next thing that works
-- an outbox row and a chain in one transaction (a reconciliation pass, a repair, a batch
-- finisher) would otherwise pick an order by accident. ADR 12, decision 1 states it beside this
-- paragraph.
SELECT pg_advisory_xact_lock(@lock_key::bigint);

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
--
-- is_head is the whole of it, and this row's own scope is deliberately NOT selected. Condition 3
-- compares the HEAD's scope, which EntityHead returns, and selecting this row's scope here invited
-- a reader to believe otherwise. The row's existence answers condition 1 and is_head answers
-- condition 2.
SELECT is_head
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
