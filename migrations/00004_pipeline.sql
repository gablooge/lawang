-- +goose Up

-- The ledger: one row per record id that has been PREPARED for delivery, and the supersede chain
-- that links an entity's versions.
--
-- It answers the three questions step 5 of architecture 3.2 asks of it:
--
--   1. "Have I prepared this record id already?" (the primary key)
--   2. "What is the newest record of this entity, so the new one can supersede it?" (the head)
--   3. "Is this an entity that moved back into a scope it already had?" (the head's scope)
--
-- A row means prepared, never merely seen. A record the pipeline skipped or held back is NOT
-- written here: a later, legitimate arrival of that same version would then be skipped as already
-- delivered and the sink would never get it.
CREATE TABLE record_ledger (
  tenant_id   tenant_id   NOT NULL,
  -- The record id, "rec_" and 32 hex characters (record.Seal).
  record_id   text        NOT NULL CHECK (record_id <> ''),
  -- The entity this record is a version of. The chain is per (tenant, provider, external_id) and
  -- NEVER per scope: an entity that moves to another scope must supersede what it was in the old
  -- one, or the sink keeps a stale copy under the old scope's members.
  --
  -- The provider is already the prefix of the external id (ADR 4, and record.Seal enforces it), so
  -- it adds no identity here. It is a column of its own because it is what an operator filters a
  -- dead letter by, and it keeps the chain key readable at a glance.
  provider    text        NOT NULL CHECK (provider <> ''),
  -- Bounded in BYTES, which the record format is not: it allows 1,024 characters, and 1,024
  -- characters can be 4,096 bytes, while a btree tuple cannot exceed about 2,700. Without the
  -- bound, whether an entity can be ledgered would depend on which characters its id is spelled
  -- with. internal/pipeline refuses a longer one by name (ErrExternalIDTooLong) before it gets
  -- here, the way internal/outbox refuses a long ordering key, rather than leaving the caller with
  -- "index row size exceeds maximum".
  external_id text        NOT NULL CHECK (external_id <> '' AND octet_length(external_id) <= 2048),
  version     text        NOT NULL CHECK (version <> ''),
  -- The record's visibility.scope. It is here for the move detection of ADR 4, decision 7: an
  -- incoming record whose id is already known, that is not the head, and whose scope differs from
  -- the head's, is an entity that moved back (or a stale record from the old scope), and the
  -- pipeline dead-letters it rather than skipping it silently.
  scope       text        NOT NULL CHECK (scope <> ''),
  -- The record this one replaced, or NULL for the first record of an entity. Links point forward
  -- only: a record supersedes the head at the moment it is prepared, and the head only ever moves
  -- to a record that is not older than it.
  supersedes  text,
  -- True for exactly the newest prepared record of its entity.
  is_head     boolean     NOT NULL,
  prepared_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, record_id),
  CONSTRAINT record_ledger_never_supersedes_itself
    CHECK (supersedes IS NULL OR supersedes <> record_id)
);

-- At most one head per entity, so "the newest prepared record of this entity" is a property of the
-- database and not of the writers' discipline. It is also the index the head lookup and the demote
-- both probe, and it holds only the heads, so it is the size of the entity count and not of the
-- ledger.
CREATE UNIQUE INDEX record_ledger_one_head
  ON record_ledger (tenant_id, provider, external_id) WHERE is_head;

ALTER TABLE record_ledger ENABLE ROW LEVEL SECURITY;
ALTER TABLE record_ledger FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON record_ledger
  USING (tenant_id = current_tenant())
  WITH CHECK (tenant_id = current_tenant());

-- The redaction map: what the masker took out of a title or a text, and the placeholder it put
-- there instead.
--
-- It stays HERE. A token travels to the sink inside the record; the value it stands for never
-- leaves this deployment, and no helper role is granted anything on this table. That is the whole
-- point of masking: a sink is a third party, and what it is not told it cannot lose.
--
-- The token is a ULID and not a hash of the value, deliberately. A deterministic token would hand
-- every sink an oracle: anyone holding a guess at an address could compute its token and confirm
-- that the address appears in a tenant's records, which is exactly the fact masking is there to
-- withhold. A random token stands for the value and says nothing about it, and its 80 random bits
-- come from crypto/rand (ids.NewUnpredictable), so one observed token does not narrow the next.
--
-- This table holds personal data by construction, so it is row-level secured like everything else
-- and an operator prunes it by last_seen_at (retention is B25).
CREATE TABLE redaction_map (
  tenant_id     tenant_id   NOT NULL,
  token         text        NOT NULL CHECK (token <> ''),
  kind          text        NOT NULL CHECK (kind IN ('email', 'phone', 'iban')),
  -- What was masked, exactly as it stood in the text. Bounded because it is a column of a unique
  -- index and because the masker's own patterns are bounded well below this.
  value         text        NOT NULL CHECK (value <> '' AND octet_length(value) <= 512),
  -- Written every time the value is seen again. The upsert has to write something on conflict in
  -- order to RETURN the token the value already has, and this is the column worth writing: it is
  -- what tells an operator that a mapping is still in use before they prune it.
  --
  -- There is deliberately no first_seen_at beside it. Nothing would read one: the token is a ULID,
  -- so its own first 48 bits are the millisecond the mapping was minted, and a column that only a
  -- migration comment knows about is a column an operator is told about and cannot check.
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, token),
  -- One token per value, so the same address reads as the same placeholder everywhere in a
  -- tenant's records and the map does not grow with every repeat.
  CONSTRAINT redaction_map_one_token_per_value UNIQUE (tenant_id, kind, value)
);

ALTER TABLE redaction_map ENABLE ROW LEVEL SECURITY;
ALTER TABLE redaction_map FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON redaction_map
  USING (tenant_id = current_tenant())
  WITH CHECK (tenant_id = current_tenant());

-- +goose Down
DROP TABLE redaction_map;
DROP TABLE record_ledger;
