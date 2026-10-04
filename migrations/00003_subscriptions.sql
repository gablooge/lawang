-- +goose Up

-- One webhook registration Lawang owns: one tenant's, at one provider, covering one resource.
--
-- This is the table the accept path resolves a delivery's owner in, and it is the only place a
-- tenant can come from on that path (docs/architecture.md, principle 2). A delivery arrives with
-- the provider's own identifiers in it, which are text a stranger sent: they say which rows are
-- CANDIDATES, and nothing more. The tenant is the one whose row's secret verifies the exact bytes,
-- and when more than one row verifies the delivery is parked rather than routed.
CREATE TABLE subscriptions (
  id            text        PRIMARY KEY,                    -- ULID, minted by Lawang
  tenant_id     tenant_id   NOT NULL,
  provider      text        NOT NULL CHECK (provider <> ''),
  -- What the subscription covers, in the provider's own words: a ClickUp workspace, a mailbox, a
  -- channel. It is the identity of the registration for Lawang, so re-registering the same
  -- resource updates the row in place and never adds a second one (architecture section 5).
  resource      text        NOT NULL CHECK (resource <> ''),
  -- The two delivery keys (provider.DeliveryKeys): the provider's id of the workspace, team,
  -- portal or account, and the provider's id of the registration itself. A provider that sends
  -- neither leaves both empty, and such a row could never be a candidate for any delivery, so the
  -- CHECK below refuses it rather than let it sit in the table unreachable. Both are bounded,
  -- because both are compared against text from a delivery on every accept; internal/hub refuses a
  -- longer delivery key by the same number.
  workspace_id  text        NOT NULL CHECK (octet_length(workspace_id) <= 256),
  external_id   text        NOT NULL CHECK (octet_length(external_id) <= 256),
  -- The secret the provider signs its deliveries with, for this registration.
  --
  -- It is a column and not a vault entry ON PURPOSE, and docs/adr/0011-hub-resolution.md is the
  -- record of why: the accept path derives the tenant FROM the secret, so it needs every candidate
  -- tenant's secret before it knows whose delivery this is, and the Vault interface of
  -- architecture section 7 is keyed by (tenant, provider) and cannot answer that question. The
  -- API tokens that hydration and registration use are a different credential and do go to the
  -- vault (B13). An empty secret verifies nothing, so it is refused here rather than stored and
  -- silently answering 401 for every delivery.
  secret        bytea       NOT NULL CONSTRAINT subscriptions_secret_is_not_empty CHECK (octet_length(secret) > 0),
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, provider, resource),
  CONSTRAINT subscriptions_have_a_delivery_key CHECK (workspace_id <> '' OR external_id <> '')
);

-- The two candidate lookups of the accept path, one per delivery, each an equality probe on a
-- whole index prefix. They are not partial indexes on a non-empty key: whether the key a query
-- carries is empty is not something the planner can prove of a parameter, so a partial index
-- would be skipped under a generic plan and the lookup would read the table.
CREATE INDEX subscriptions_by_workspace ON subscriptions (provider, workspace_id);
CREATE INDEX subscriptions_by_external ON subscriptions (provider, external_id);

ALTER TABLE subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscriptions FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON subscriptions
  USING (tenant_id = current_tenant())
  WITH CHECK (tenant_id = current_tenant());

-- The resolver role reads across tenants, because deriving the tenant is its whole job and it
-- therefore cannot be filtered by one. It may only SELECT, and only the columns the two candidate
-- queries read: the three the WHERE narrows by, and the three the hub gets back. It can never see
-- "resource" or "created_at", and it can never write anything.
CREATE POLICY resolver_read ON subscriptions FOR SELECT TO lawang_resolver USING (true);

GRANT SELECT (id, tenant_id, provider, workspace_id, external_id, secret)
  ON subscriptions TO lawang_resolver;

-- The sentinel tenant, which owns every delivery nobody can be shown to own (internal/tenancy,
-- Sentinel). Parked rows are stored under it so that they are somewhere auditable, re-resolvable
-- (B25) and subject to retention, and under nobody's real tenant.
--
-- Nothing may ever exist as a tenant under that id. A tenants row with it would hand whoever holds
-- that tenant's operator credential every stranger's parked delivery, from every workspace that
-- ever pointed at this deployment. internal/outbox is the only writer of a parked row and takes no
-- tenant from its caller, so the id can never be chosen by a request; this CHECK closes the other
-- half, an operator or a later item creating it as a tenant by hand.
ALTER TABLE tenants ADD CONSTRAINT tenants_are_not_the_sentinel CHECK (id <> '_parked');

-- +goose Down
ALTER TABLE tenants DROP CONSTRAINT tenants_are_not_the_sentinel;
DROP TABLE subscriptions;
