# Security policy

Lawang's whole claim is access control: what gets through, and who may see it. A flaw in that is
worth reporting, and worth reporting privately.

## Supported versions

Lawang is pre-alpha. There is no released version yet, and no tag is supported. Only the default
branch (`main`) is supported, and a fix lands there.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting on this repository: the **Security** tab, then
**Report a vulnerability**. That opens a private advisory that only the maintainer can read.

Please do not open a public issue for a vulnerability, and please do not describe one in a pull
request, a discussion or a comment. An ordinary bug that has no security consequence is welcome as
a normal issue.

Include what you need to make the problem reproducible: the commit, the configuration, and the
smallest sequence of steps that shows it. Never include a real token, a real credential or a real
database URL in a report, yours or anyone else's. Describe the credential, do not paste it.

## What to expect

One maintainer, working on this in spare time, on a best-effort basis. There is no service level
agreement, no guaranteed response time and no bounty. You will get an acknowledgement when the
report is read, and an answer when there is something to say. If a report goes unanswered for a
long time, that is capacity, not disregard: say so in the advisory thread and it will be picked up
again.

## Scope

In scope:

- A record reaching someone who is not a member of its scope.
- One tenant reading, writing or inferring another tenant's data, including through the
  helper roles, the outbox, or an error message.
- A credential, a token, a signing secret or a database URL reaching a log, an error, a metric, a
  plain database column, a test fixture or a golden file.
- An unauthenticated caller reaching the operator API, or a caller reaching an operation that
  belongs to another tenant.
- Signature verification on the webhook edge that can be bypassed, replayed or confused about
  which tenant a delivery belongs to.

Out of scope for now, because nothing is released and nothing is hosted:

- Denial of service against a developer's own machine or a local test database.
- Anything that needs an attacker who is already a Postgres superuser, or who already holds
  `BYPASSRLS`. Lawang assumes the database administrator is trusted.

## Background

- The trust model, including what is trusted and what is not, is
  [architecture section 4](docs/architecture.md#4-trust-model).
- The visibility rule a record carries, which is the property most of the above protects, is
  [architecture section 6](docs/architecture.md#6-the-record-format-proposed).
