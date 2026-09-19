# 1. Typed queries with sqlc, infrastructure SQL by hand

Status: accepted, 2026-09-19 (backlog item B03)

## Decision

Queries against Sluiceway's own tables are written as SQL files and compiled to Go with
[`sqlc`](https://sqlc.dev), targeting `pgx/v5`. The generated code is checked in, and CI fails if
it is stale (`sqlc diff`).

A small amount of SQL stays hand-written in Go, in `internal/store` and `internal/tenancy`: binding
the tenant, entering a helper role, and the start-up preflight. These are not queries over the
schema. They are the mechanism every other query runs inside, they take no part in any table's
shape, and two of them (`SET LOCAL ROLE ...`) cannot be parameterized at all.

sqlc arrives with the first real table queries, in B04 (the outbox).

## Why

Most data-layer bugs in the Python predecessor were query-shape mistakes: a column renamed in a
migration but not in a query string, a nullable column scanned into a non-nullable value, the wrong
number of arguments. None of those fail until the query runs. sqlc checks every query against the
migrations at build time, so they fail in CI.

It also keeps SQL as SQL. The claim query (`FOR UPDATE SKIP LOCKED`, head of each ordering key) and
the row-level security policies are the heart of this project, and they should be readable as
plain SQL by someone reviewing isolation, not assembled by a query builder.

## Cost

- One more tool in the build. It is only needed when a query changes, because the output is
  checked in; `go build` alone never needs it.
- Dynamic queries (optional filters in the operator API) do not fit sqlc well. Those few may be
  hand-written with pgx, each with a test that runs it.
