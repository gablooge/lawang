# Decision records

One short file per settled decision, numbered to match the open-decisions table in
[roadmap.md](../roadmap.md). A record states the decision, why, and what it costs. If a decision is
reversed, add a new record that supersedes the old one instead of editing history.

| # | Decision | Status |
|---|---|---|
| [1](0001-queries-sqlc.md) | Typed queries with `sqlc`, infrastructure SQL by hand | accepted |
| [2](0002-migrations-goose.md) | `goose` migrations embedded in the binary, run as the application role | accepted |
| [3](0003-scope-id-format.md) | The scope id is `{provider}:{container_kind}:{container_id}`: internal provider key, percent-escaped container id, one canonical spelling, no tenant | accepted |
| [4](0004-record-format-v1.md) | Record format v1: a `format` version field, what is required, where unknown fields are allowed, `delete` defined, and the scope hashed into the record id | accepted |
| [10](0010-outbox-head-marker.md) | The head of an ordering key is a stored marker, so a claim costs its batch and not the backlog | accepted |
