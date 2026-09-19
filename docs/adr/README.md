# Decision records

One short file per settled decision, numbered to match the open-decisions table in
[roadmap.md](../roadmap.md). A record states the decision, why, and what it costs. If a decision is
reversed, add a new record that supersedes the old one instead of editing history.

| # | Decision | Status |
|---|---|---|
| [1](0001-queries-sqlc.md) | Typed queries with `sqlc`, infrastructure SQL by hand | accepted |
| [2](0002-migrations-goose.md) | `goose` migrations embedded in the binary, run as the application role | accepted |
| [10](0010-outbox-head-marker.md) | The head of an ordering key is a stored marker, so a claim costs its batch and not the backlog | accepted |
