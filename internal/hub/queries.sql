-- name: CandidatesByWorkspace :many
-- The candidate subscriptions a delivery's workspace id could belong to, under the resolver role,
-- across tenants. It reads and returns only the delivery-resolution columns, which are the only
-- ones that role is granted.
--
-- The delivery's keys narrow the lookup and decide nothing: every row this returns is a CANDIDATE,
-- and the tenant is whichever one's secret verifies the exact bytes (principle 2).
--
-- A subscription id on the delivery narrows further, but a row that recorded none still matches:
-- a provider that learns the id of its own registration only later would otherwise have every
-- delivery parked as unowned. The two conditions after the workspace are filters on the rows the
-- index prefix (provider, workspace_id) already found, not the way the rows are found.
--
-- This is HALF of the candidate set. The other half is CandidatesBySubscription, and internal/hub
-- runs both and unions them whenever a delivery carries both keys: a row registered with only a
-- registration id cannot satisfy the workspace equality here, however exactly the delivery names
-- it, and asking only this query would hide a tenant that has a claim on the delivery.
--
-- The limit is the hub's maximum plus one, so that "more candidates than the hub will verify" is
-- something the caller can see rather than a set silently cut short: a truncated candidate set
-- could hide the second tenant that verifies, and route a delivery that should have been parked.
SELECT id, tenant_id, secret
  FROM subscriptions
 WHERE provider = @provider
   AND workspace_id = @workspace::text
   AND (external_id = '' OR @subscription::text = '' OR external_id = @subscription::text)
 ORDER BY id
 LIMIT @max_candidates;

-- name: CandidatesBySubscription :many
-- The candidate subscriptions a delivery's registration id could belong to: the mirror image of
-- CandidatesByWorkspace, over the (provider, external_id) index, and everything that query says
-- applies here too.
--
-- It is the whole candidate set for a delivery that carries no workspace id at all, such as a
-- Microsoft Graph notification, and half of it for a delivery that carries both. The workspace
-- filter is the mirror of the subscription filter in the other query: a key that both the delivery
-- and the row carry must agree, and a key either of them left empty says nothing either way.
SELECT id, tenant_id, secret
  FROM subscriptions
 WHERE provider = @provider
   AND external_id = @subscription::text
   AND (workspace_id = '' OR @workspace::text = '' OR workspace_id = @workspace::text)
 ORDER BY id
 LIMIT @max_candidates;

-- name: UpsertSubscription :one
-- Registers a subscription, or updates the one already registered for the same resource in place
-- (architecture section 5: unique on tenant, provider, resource). Runs bound to the tenant, as the
-- application role: the resolver role may only read, and only on the accept path.
--
-- The id of an existing row is kept, because it is what the outbox orders that subscription's
-- deliveries by: giving a re-registered subscription a new id would put its next delivery in a new
-- queue, alongside the one still in flight in the old one.
--
-- It does NOT return the secret, on purpose. The two candidate queries above are the only readers
-- of that column in the program, which is what makes encrypting it (B13) a change to them and to
-- one function; a RETURNING list with the secret in it would be a third reader, and one whose
-- value travels on to the operator API (B14). The caller already holds what it just wrote.
INSERT INTO subscriptions (id, tenant_id, provider, resource, workspace_id, external_id, secret)
VALUES (@id, @tenant_id, @provider, @resource, @workspace_id, @external_id, @secret)
ON CONFLICT (tenant_id, provider, resource) DO UPDATE
   SET workspace_id = EXCLUDED.workspace_id,
       external_id  = EXCLUDED.external_id,
       secret       = EXCLUDED.secret
RETURNING id, tenant_id, provider, resource, workspace_id, external_id;
