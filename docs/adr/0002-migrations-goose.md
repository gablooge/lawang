# 2. goose migrations, embedded, run as the application role

Status: accepted, 2026-09-19 (backlog item B03)

## Decision

Migrations are plain SQL files in `migrations/`, embedded in the binary and applied by
`sluiceway migrate` using [`goose`](https://github.com/pressly/goose) as a library. A session-level
advisory lock serializes concurrent runs, so several replicas may migrate at once.

Migrations run as the **non-superuser application role**, the same login `serve` and `worker` use.
`store.Open` refuses a `SUPERUSER` or `BYPASSRLS` login outright, for migrations as for everything
else.

What that role cannot do for itself is a separate, one-time **bootstrap script**
(`migrations/bootstrap/roles.sql`, printed by `sluiceway migrate bootstrap`) that an administrator
applies once per database. It creates the three roles, grants the two helper roles `WITH INHERIT
FALSE, SET TRUE`, and creates the `sluiceway` schema owned by the application role. It is safe to
run again.

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
