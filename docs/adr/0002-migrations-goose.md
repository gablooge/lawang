# 2. goose migrations, embedded, run as the application role

Status: accepted, 2026-09-19 (backlog item B03)

## Decision

Migrations are plain SQL files in `migrations/`, embedded in the binary and applied by
`sluiceway migrate` using [`goose`](https://github.com/pressly/goose) as a library. A session-level
advisory lock serializes concurrent runs, so several replicas may migrate at once. A run that finds
the lock taken asks again every second, for up to five minutes. The goose default is every five
seconds, which made the k-th of N replicas started together wait about 5(k-1) seconds, usually for
a lock that had been released within milliseconds; one second is the shortest period goose accepts.

Every migration has a `Down` section. No command exposes it (rolling a production database back is
a restore, not a command), but a test runs every `Down` to zero and back up as the application
role, so the sections stay correct as migrations are added and remain usable by hand.

Migrations run as the **non-superuser application role**, the same login `serve` and `worker` use.
`store.Open` refuses a `SUPERUSER` or `BYPASSRLS` login outright, for migrations as for everything
else.

What that role cannot do for itself is a separate, one-time **bootstrap script**
(`migrations/bootstrap/roles.sql`, printed by `sluiceway migrate bootstrap`) that an administrator
applies once per database. It creates the three roles, grants the two helper roles `WITH INHERIT
FALSE, SET TRUE`, and creates the `sluiceway` schema owned by the application role.

The administrator is a superuser, or a non-superuser role with `CREATEROLE` and `CREATE` on the
database, which is all that most managed Postgres gives out. A `CREATEROLE` role holds the roles it
creates `WITH ADMIN OPTION` only, and creating a schema for another role requires `SET` on it, so
the script grants itself `SET` on `sluiceway` for that one step and gives it back. Creating the
schema first and handing it over with `ALTER SCHEMA ... OWNER TO` needs the same `SET` (checked on
Postgres 16 and 17), so there is no way around borrowing it. The script borrows only when the
schema does not exist yet and the administrator lacks `SET`. Giving it back is not a plain
`REVOKE`: Postgres keeps one membership row per grantor, so when the administrator already holds a
row from the same grantor without `SET` (`createrole_self_grant = 'inherit'` makes one), the grant
rewrites that row, and a revoke would delete it. The script records the administrator's rows
before the grant, and afterwards restores the options of whichever row changed, or revokes the
row that is new, so the administrator ends with exactly what it started with. A `CREATEROLE` role can only grant roles it created or administers:
if someone else created the roles, that administrator or a superuser runs the script.

The script is a single `DO` statement, so under any client (including `psql`, which carries on
after an error) it applies completely or not at all. It is safe to run again, also as a different
administrator. Postgres then keeps one membership row per grantor, which is harmless, because:

`store.Open` runs a **preflight** on every start and never counts membership rows. For each helper
role it asks `pg_has_role(helper, 'USAGE')`, which must be false, and `pg_has_role(helper, 'SET')`,
which must be true. `USAGE` is Postgres's own answer to "do this role's privileges and policies
apply without `SET ROLE`", over every grantor's row and every intermediate role, which is the only
question that matters: an inherited worker role would give a plain transaction, with no tenant
bound, the worker's cross-tenant policies. It also asks `pg_has_role(helper, 'MEMBER WITH ADMIN
OPTION')`, which must be false: a login that administers a helper role passes the other two checks
and can then grant itself that role `WITH INHERIT TRUE` while running. Postgres answers the `USAGE`
and `SET` spellings of that question identically (all three are `is_admin_of_role`, which follows
memberships whether or not they are inherited), and that reach is wanted, because `ADMIN OPTION`
held by a role the login can only `SET ROLE` to is just as usable. It also refuses a helper role
that is `SUPERUSER` or `BYPASSRLS`, a database without the schema (`ErrNotBootstrapped`), and a
schema the login role does not own (`ErrSchemaNotOwned`). The last has a sentinel of its own
because isolation is not at stake, so it is not `ErrUnsafeRole`, and running the bootstrap again
does not change the owner of a schema that exists, so `ErrNotBootstrapped` would send the operator
the wrong way. The bootstrap script raises a clear error in the same situation instead of
accepting the schema silently, which `CREATE SCHEMA IF NOT EXISTS` used to do.

The preflight runs once per process. A grant changed by an administrator afterwards is not seen
until the next start. That is accepted: it guards against misconfiguration, and an actor who can
change memberships can already read the tables.

Everything lives in the `sluiceway` schema, and every connection sets `search_path` to it
explicitly. Postgres 16 is the minimum, for `GRANT ... WITH INHERIT FALSE`.

## Why

- Plain SQL, no separate install, and the binary always carries the migrations that match it.
- Principle 12 in the architecture: a guard that only fired for a non-superuser passed every test
  run as superuser and blocked the first real deploy. Here there is no superuser path to test by
  accident. The integration tests connect as the application role, and the one test that connects
  as a superuser asserts that it is refused.
- Tables owned by the application role means ownership is the same everywhere, and `FORCE ROW LEVEL
  SECURITY` (owners are otherwise exempt) is exercised by every test.
- The alternative, giving the application role `CREATEROLE` so migrations could create the helper
  roles, would hand a network-facing service the power to mint database roles.
- The explicit `search_path`: the default `"$user", public` follows `SET ROLE`, so it would stop
  resolving Sluiceway's tables the moment a transaction entered a helper role.

## Cost

- Deploying needs one administrator step before the first migrate. `docker compose` will do it in
  the Postgres init hook (B27); managed Postgres users run the printed script once.
- A future migration that needs a privilege the application role lacks (an extension, for
  instance) has to go into the bootstrap script, and existing deployments must re-run it.
