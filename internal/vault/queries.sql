-- name: UpsertCredential :exec
-- Stores one tenant's sealed credential for one provider, replacing the one already stored.
-- Runs bound to the tenant, as the application role: row-level security is what keeps a bug in a
-- caller from writing into another tenant's row, and the envelope's authenticated data is what
-- keeps the row from being readable if one is written there anyway.
--
-- It returns nothing. There is one column worth returning and it is credential material, so the
-- caller would be handed back what it just passed in, and the value would exist in one more place
-- for no reason.
INSERT INTO vault_credentials (tenant_id, provider, ciphertext)
VALUES (@tenant_id, @provider, @ciphertext)
ON CONFLICT (tenant_id, provider) DO UPDATE
   SET ciphertext = EXCLUDED.ciphertext,
       updated_at = now();

-- name: FetchCredential :one
-- The sealed envelope of one tenant's credential for one provider, bound to the tenant.
--
-- It selects the envelope and nothing else: when the credential was stored is not a thing any
-- caller of the vault needs, and the operator API (B14) reports a connection from the
-- subscriptions table.
SELECT ciphertext FROM vault_credentials
 WHERE tenant_id = @tenant_id AND provider = @provider;

-- name: DeleteCredential :execrows
-- Removes one tenant's credential for one provider, and reports how many rows went, so that
-- revoking something that was never stored is a refusal the caller can see rather than a silent
-- success. A revoke that answers "done" for a credential that is still somewhere else is the
-- answer an operator acts on when a token has leaked.
DELETE FROM vault_credentials
 WHERE tenant_id = @tenant_id AND provider = @provider;
