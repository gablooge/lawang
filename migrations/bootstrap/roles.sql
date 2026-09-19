-- Lawang database bootstrap.
--
-- Run ONCE per database, connected to that database, as an administrator. Everything after this
-- (migrations, serve, worker) runs as the non-superuser role "lawang". Requires Postgres 16 or
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
-- sends it. Keep it that way. It is safe to run again, also as a different administrator, and it
-- leaves the administrator's own role memberships exactly as it found them. It refuses a database
-- that already has a schema "lawang" owned by any other role.
--
--   lawang migrate bootstrap | psql "$ADMIN_DATABASE_URL"
--   psql "$ADMIN_DATABASE_URL" -c "ALTER ROLE lawang PASSWORD '...'"
--
-- Three roles (docs/architecture.md, section 4):
--   lawang           the application role. NOSUPERUSER NOBYPASSRLS, so row-level security
--                       actually applies to it.
--   lawang_resolver  reads the delivery-resolution columns of subscriptions across tenants.
--   lawang_worker    claims outbox rows across tenants, then re-binds to each row's tenant.
--
-- The two helper roles cannot log in. The application role is a member of both WITH INHERIT
-- FALSE, so it never picks up their wider policies silently: it has to enter one explicitly with
-- SET LOCAL ROLE, and leaves it again when the transaction ends. Never grant a helper role to the
-- application role in any other way, directly or through another role, and never WITH ADMIN
-- OPTION, which would let it grant itself the inheriting kind: the service checks on every start
-- that it inherits neither and administers neither, and refuses to start otherwise.

DO $$
DECLARE
  me oid := (SELECT oid FROM pg_roles WHERE rolname = current_user);
  schema_owner name;
  borrow boolean := false;
  before jsonb;
  m record;
BEGIN
  -- A schema of this name that belongs to anyone else is not ours to adopt. Say so now, and change
  -- nothing, instead of reporting success and leaving the service to fail later.
  SELECT r.rolname INTO schema_owner
    FROM pg_namespace n JOIN pg_roles r ON r.oid = n.nspowner
   WHERE n.nspname = 'lawang';
  IF FOUND AND schema_owner <> 'lawang' THEN
    RAISE EXCEPTION 'schema "lawang" already exists and is owned by "%", not by the role "lawang"', schema_owner
      USING ERRCODE = 'object_not_in_prerequisite_state',
            HINT = 'If it is meant for Lawang, run ALTER SCHEMA lawang OWNER TO lawang as a superuser and apply this script again. Otherwise use another database.';
  END IF;

  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'lawang') THEN
    CREATE ROLE lawang LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'lawang_resolver') THEN
    CREATE ROLE lawang_resolver NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'lawang_worker') THEN
    CREATE ROLE lawang_worker NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION;
  END IF;

  GRANT lawang_resolver TO lawang WITH INHERIT FALSE, SET TRUE;
  GRANT lawang_worker TO lawang WITH INHERIT FALSE, SET TRUE;

  -- Lawang keeps everything in its own schema, owned by the application role, so migrations
  -- need no rights on "public". If the schema is already there, the rest has nothing to do.
  IF schema_owner IS NOT NULL THEN
    RETURN;
  END IF;

  -- Creating a schema for another role requires being able to SET ROLE to it, and so does
  -- creating it first and handing it over with ALTER SCHEMA ... OWNER TO. A superuser always can.
  -- A CREATEROLE administrator holds the roles it creates WITH ADMIN OPTION only, so it takes SET
  -- for this one step and gives it straight back. A failure in between rolls it all back.
  --
  -- Giving it back is not simply REVOKE. Postgres keeps one membership row per grantor, so if the
  -- administrator already has a row from the same grantor without SET (createrole_self_grant =
  -- 'inherit' makes one), the GRANT below rewrites that row instead of adding one, and a REVOKE
  -- would then delete what the administrator had before. So remember the rows as they were, and
  -- afterwards put back whichever one changed, or remove the one that is new.
  borrow := NOT pg_has_role(current_user, 'lawang', 'SET');
  IF borrow THEN
    SELECT coalesce(jsonb_object_agg(a.grantor::text, jsonb_build_object('inherit', a.inherit_option, 'set', a.set_option)), '{}')
      INTO before
      FROM pg_auth_members a
     WHERE a.roleid = 'lawang'::regrole AND a.member = me;
    GRANT lawang TO CURRENT_USER WITH INHERIT FALSE, SET TRUE;
  END IF;

  CREATE SCHEMA lawang AUTHORIZATION lawang;

  IF borrow THEN
    FOR m IN
      SELECT g.rolname AS grantor, a.grantor::text AS key, a.inherit_option, a.set_option
        FROM pg_auth_members a JOIN pg_roles g ON g.oid = a.grantor
       WHERE a.roleid = 'lawang'::regrole AND a.member = me
    LOOP
      IF NOT before ? m.key THEN
        EXECUTE format('REVOKE lawang FROM CURRENT_USER GRANTED BY %I', m.grantor);
      ELSIF (before -> m.key ->> 'inherit')::boolean IS DISTINCT FROM m.inherit_option
         OR (before -> m.key ->> 'set')::boolean IS DISTINCT FROM m.set_option THEN
        EXECUTE format('GRANT lawang TO CURRENT_USER WITH INHERIT %s, SET %s GRANTED BY %I',
                       upper(before -> m.key ->> 'inherit'), upper(before -> m.key ->> 'set'), m.grantor);
      END IF;
    END LOOP;
  END IF;
END
$$;
