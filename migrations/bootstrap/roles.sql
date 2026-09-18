-- Sluiceway database bootstrap.
--
-- Run ONCE per database, connected to that database, as an administrator. Everything after this
-- (migrations, serve, worker) runs as the non-superuser role "sluiceway". Requires Postgres 16 or
-- newer.
--
-- The administrator is either a superuser, or, as on most managed Postgres, a non-superuser role
-- that has both of:
--   CREATEROLE                 to create the three roles and grant the memberships
--   CREATE on this database    to create the schema (GRANT CREATE ON DATABASE ... TO ...)
-- A CREATEROLE administrator can only grant roles it created itself, or holds WITH ADMIN OPTION.
-- So if another administrator already created the roles, run the script as that one, or as a
-- superuser.
--
-- The script is a single statement, so it applies completely or not at all, whatever client
-- sends it. Keep it that way. It is safe to run again, also as a different administrator.
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
-- SET LOCAL ROLE, and leaves it again when the transaction ends. Never grant a helper role to the
-- application role in any other way, directly or through another role: the service checks on
-- every start that it inherits neither, and refuses to start if it does.

DO $$
DECLARE
  borrow boolean;
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

  GRANT sluiceway_resolver TO sluiceway WITH INHERIT FALSE, SET TRUE;
  GRANT sluiceway_worker TO sluiceway WITH INHERIT FALSE, SET TRUE;

  -- Sluiceway keeps everything in its own schema, owned by the application role, so migrations
  -- need no rights on "public".
  --
  -- Creating a schema for another role requires being able to SET ROLE to it. A superuser always
  -- can. A CREATEROLE administrator holds the roles it creates WITH ADMIN OPTION only, so it
  -- takes SET for this one step and gives it straight back. It does so only if it lacks SET, so
  -- an administrator that already had it keeps it. REVOKE removes only the grant made here
  -- (memberships are per grantor), and a failure in between rolls the grant back with the rest.
  borrow := NOT pg_has_role(current_user, 'sluiceway', 'SET');
  IF borrow THEN
    GRANT sluiceway TO CURRENT_USER WITH INHERIT FALSE, SET TRUE;
  END IF;
  CREATE SCHEMA IF NOT EXISTS sluiceway AUTHORIZATION sluiceway;
  IF borrow THEN
    REVOKE sluiceway FROM CURRENT_USER;
  END IF;
END
$$;
