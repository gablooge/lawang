-- +goose Up

-- Every tenant-scoped column uses this domain, so an empty or malformed tenant id cannot be stored
-- anywhere. The empty string matters: a transaction-local setting reads back as '' (not NULL) on a
-- pooled connection once its transaction has ended, and no row may ever match that.
CREATE DOMAIN tenant_id AS text
  CHECK (VALUE ~ '^[A-Za-z0-9_-]{1,64}$');

-- The tenant bound to the current transaction, or NULL when none is bound. Every row-level
-- security policy compares against this, and a comparison with NULL matches no rows: a query with
-- no tenant bound fails closed.
-- +goose StatementBegin
CREATE FUNCTION current_tenant() RETURNS text
  LANGUAGE sql STABLE PARALLEL SAFE
AS $$ SELECT NULLIF(current_setting('lawang.tenant', true), '') $$;
-- +goose StatementEnd

CREATE TABLE tenants (
  id         tenant_id   PRIMARY KEY,
  name       text        NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now()
);

-- FORCE matters as much as ENABLE: the application role owns the table, and an owner is exempt
-- from row-level security unless it is forced.
ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON tenants
  USING (id = current_tenant())
  WITH CHECK (id = current_tenant());

-- The helper roles can reach the schema, and nothing in it until a later migration grants it.
GRANT USAGE ON SCHEMA lawang TO lawang_resolver, lawang_worker;

-- +goose Down
REVOKE USAGE ON SCHEMA lawang FROM lawang_resolver, lawang_worker;
DROP TABLE tenants;
DROP FUNCTION current_tenant();
DROP DOMAIN tenant_id;
