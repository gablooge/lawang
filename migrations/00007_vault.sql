-- +goose Up

-- The local vault: one tenant's credential for one provider, as a sealed envelope and never as
-- the credential itself (docs/architecture.md section 7, "raw tokens never reach a plain table",
-- and docs/adr/0016-vault-key-source.md).
--
-- There is one column of credential material and it is bytea, because what goes in it is the
-- output of AES-256-GCM: a format byte, the id of the key it was sealed under, and the sealed
-- bytes. The plaintext of that envelope is a JSON object of named secret fields, and nothing in
-- this database can read it: the key is in the environment of the process (LAWANG_VAULT_KEY),
-- which a backup of this table does not carry.
--
-- The tenant and the provider are NOT only the primary key. They are also the additional
-- authenticated data the envelope is sealed with, so a row copied from one tenant to another, or
-- from one provider to another, no longer decrypts. The key alone is therefore not enough to move
-- a credential between tenants by editing this table (internal/vault, aad).
CREATE TABLE vault_credentials (
  tenant_id  tenant_id   NOT NULL,
  -- The provider key as this program spells it (ADR 3, record.ValidProviderKey). internal/vault
  -- refuses anything that grammar refuses before it gets here; the bound is repeated because this
  -- column is part of the sealed envelope's authenticated data and a column with no bound is a
  -- column a mistake can grow without limit.
  provider   text        NOT NULL CHECK (provider <> '' AND octet_length(provider) <= 32),
  -- The sealed envelope. The CHECK is what makes "this table never holds a plaintext credential"
  -- the database's own rule rather than the writer's discipline: 37 bytes is the shortest envelope
  -- that can exist (1 format byte, 8 key id bytes, and AES-GCM's own 28 bytes of random nonce and
  -- authentication tag over an empty plaintext), and byte 0 is the format, which is 1. A JSON
  -- credential written here by hand, by a restore, or by a future version that forgot to encrypt
  -- starts with "{" (0x7B) and is refused by the server.
  --
  -- internal/vault checks the same two things again when it opens an envelope, and on purpose:
  -- this constraint guards what is written, and that check guards what is read, which is what a
  -- row that predates the constraint or arrived through a restore goes through.
  ciphertext bytea       NOT NULL
    CONSTRAINT vault_credentials_ciphertext_is_sealed
    CHECK (octet_length(ciphertext) >= 37 AND get_byte(ciphertext, 0) = 1),
  created_at timestamptz NOT NULL DEFAULT now(),
  -- When the credential was last stored. Connecting a provider again replaces the row in place,
  -- the way re-registering a subscription updates one, so a tenant has one credential per
  -- provider and never two.
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, provider)
);

-- FORCE matters as much as ENABLE: the application role owns the table, and an owner is exempt
-- from row-level security unless it is forced.
ALTER TABLE vault_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE vault_credentials FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON vault_credentials
  USING (tenant_id = current_tenant())
  WITH CHECK (tenant_id = current_tenant());

-- No grant to lawang_resolver or lawang_worker, and none is coming. The accept path resolves a
-- delivery's owner from the subscription's verification secret and never from a vault entry
-- (ADR 11), and the claim path reads no payload at all: the work on a claimed row runs as the
-- application role bound to that row's tenant, which is the only reader this table has.

-- +goose Down
DROP TABLE vault_credentials;
