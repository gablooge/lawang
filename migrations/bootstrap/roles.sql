-- Sluiceway database bootstrap.
--
-- Run ONCE per database, connected to that database, as a superuser or a role with CREATEROLE.
-- Everything after this (migrations, serve, worker) runs as the non-superuser role "sluiceway".
-- The script is safe to run again. Requires Postgres 16 or newer.
--
--   sluiceway migrate bootstrap | psql "$ADMIN_DATABASE_URL"
--   psql "$ADMIN_DATABASE_URL" -c "ALTER ROLE sluiceway PASSWORD '...'"
--
-- Three roles (docs/architecture.md, section 4):
--   sluiceway           the application role. NOSUPERUSER NOBYPASSRLS, so row-level security
--                       actually applies to it.
--   sluiceway_resolver  reads the delivery-resolution columns of subscriptions across tenants.
--   sluiceway_worker    claims outbox rows across tenants, then re-binds to each row's tenant.
--
-- The two helper roles cannot log in. The application role is a member of both WITH INHERIT
-- FALSE, so it never picks up their wider policies silently: it has to enter one explicitly with
-- SET LOCAL ROLE, and leaves it again when the transaction ends.

DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'sluiceway') THEN
    CREATE ROLE sluiceway LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'sluiceway_resolver') THEN
    CREATE ROLE sluiceway_resolver NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'sluiceway_worker') THEN
    CREATE ROLE sluiceway_worker NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
  END IF;
END
$$;

GRANT sluiceway_resolver TO sluiceway WITH INHERIT FALSE, SET TRUE;
GRANT sluiceway_worker TO sluiceway WITH INHERIT FALSE, SET TRUE;

-- Sluiceway keeps everything in its own schema, owned by the application role, so migrations need
-- no rights on "public".
CREATE SCHEMA IF NOT EXISTS sluiceway AUTHORIZATION sluiceway;
