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
| [11](0011-hub-resolution.md) | The webhook verification secret is a subscription column (the vault cannot serve a path that has no tenant yet), an unattributable delivery is parked under the sentinel tenant `_parked`, and an accepted delivery is ordered by its subscription | accepted |
| [12](0012-ledger-supersede-masking.md) | A version is ordered by its number when it is a decimal counter and by its bytes when it is fixed width, two that are neither are refused rather than guessed at, equal versions are ordered by arrival, the supersede chain is keyed per entity and holds only what was prepared, an entity that moved back is dead-lettered as ADR 4 requires, and a masked value's placeholder is random with the map kept locally | accepted |
| [13](0013-sink-wire-protocol.md) | The `http` sink's wire protocol v1: the request carries `{"v":1,...}`, a non-empty 2xx answer must carry a member the sink knows or it is unreadable, the `rejected` member must be a list (`null` is unreadable), and an unrecognised 4xx halts instead of dead-lettering the records, leaving 422 as the whole per-record refusal band | accepted |
| [14](0014-prepared-records.md) | The records of a delivery are rows of their own, which is both what the drain's first commit stores and where a per-record dead letter lives, so a sink that refuses one record of a batch kills that record and keeps the rest | accepted |
| [15](0015-clickup-provider.md) | The ClickUp provider: a delivery identifies the webhook and never the workspace, a subscription is one row per workspace keyed by its resource, with a per-tenant unique index on the registration id behind it, the version is the later of the entity's own timestamp and the delivery's event time so that a move always moves it, and ClickUp cannot degrade | accepted |
