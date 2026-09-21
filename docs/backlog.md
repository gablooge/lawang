# Backlog: 30 days to v0.1.0

[roadmap.md](roadmap.md) says what order things land in and what "done" means. This file is the
working plan underneath it: each milestone cut into items small enough to finish in one sitting,
with target dates for the 30 days from **2026-09-18 to 2026-10-17**.

The dates live here and not in the roadmap on purpose. When they slip, edit this file; the roadmap
stays true.

## How to work this file

Every item is also a GitHub issue with the same number: **B03 is
[#3](https://github.com/gablooge/lawang/issues/3)**, B17 is #17, and so on. Each milestone is a
[GitHub milestone](https://github.com/gablooge/lawang/milestones) carrying the target date from
the table below. GitHub holds the status (open, closed, comments); this file holds the order, the
dates and the log.

1. Take the first unchecked item. Items are ordered so each one only depends on items above it.
2. Do it on a branch named after the item (`b03-store-rls`), with tests, until its **Done when**
   line is true and `make check` is green.
3. Tick the box, fill in the date in the log at the bottom, and commit the tick with the work.
   End the commit or pull request message with `Closes #NN` so the issue closes when it reaches
   `main`, and tick the checklist in the issue.
4. If an item turns out to be two days of work, split it here and open a new issue under the same
   milestone before continuing, rather than letting it sprawl.
5. When a date moves, change it in the table below and on the GitHub milestone together.

Items marked **(needs you)** cannot be finished by code alone: they need a real provider account,
a secret, or a decision. The code and the fake-provider tests still land; the real-tenant check is
ticked separately.

## Schedule at a glance

| Milestone | Items | Target window | Days |
|---|---|---|---|
| M0 Foundations | B01 to B04 | Sep 18 to Sep 21 | 4 |
| M1 ClickUp end to end | B05 to B12 | Sep 22 to Sep 29 | 8 |
| M2 Credentials and connecting | B13 to B14 | Sep 30 to Oct 1 | 2 |
| M3 The other four providers | B15 to B20 | Oct 2 to Oct 7 | 6 |
| M4 Reconciliation and access sync | B21 to B24 | Oct 8 to Oct 11 | 4 |
| M5 Hardening and operations | B25 to B27 | Oct 12 to Oct 14 | 3 |
| M6 v0.1.0, public | B28 to B29 | Oct 15 to Oct 16 | 2 |
| Buffer | | Oct 17 | 1 |

This is tight: the roadmap's own sizing adds up to more than 30 days of solo work, and one buffer
day is not much. If the plan slips, cut in this order, and move what was cut to "After v0.1" in the
roadmap:

1. HubSpot signed webhooks (it is reconcile-only by default anyway)
2. the `nango` vault (keep `local` and `azureapp`)
3. Teams (it shares the Graph client with Outlook, so it is the cheapest provider to add back)
4. access sync for providers other than Slack and ClickUp

Never cut: the isolation suite, the strict stub sink, the crash and re-send tests. Those are the
project's whole claim.

---

## M0 · Foundations

- [x] **B01 Skeleton, config, CI.** `cmd/lawang` with `serve`, `worker`, `migrate`, `version`
  (stubs where the role does not exist yet); `internal/config` from the environment with
  fail-closed defaults; `Makefile` with `check`; `.golangci.yml`; GitHub Actions running vet, lint
  and `go test -race`.
  **Done when:** `make check` is green locally and in CI; config tests prove an unset environment
  means production, and production refuses to start without a database URL.
- [x] **B02 `internal/ids`.** ULIDs; the blake3 recipes for `delivery_id` and record `id` with the
  `0x1F` separator; golden test vectors checked in as a file so another implementation can verify
  against them.
  **Done when:** golden tests pass; a test proves `("ab","c")` and `("a","bc")` differ; a test
  proves the same change under two tenants yields two ids.
- [x] **B03 Store, migrations, roles, RLS.** pgx pool and transaction helpers; goose migrations
  embedded; the three roles with `INHERIT FALSE`; `internal/tenancy` binding the transaction-local
  setting; integration tests on testcontainers Postgres. Settles open decisions 1 and 2 as ADRs.
  **Done when:** a query with no tenant bound returns zero rows; tenant A cannot read tenant B's
  row; migrations run and re-run as the non-superuser application role.
- [x] **B04 Outbox.** The table, the accept insert with `ON CONFLICT DO NOTHING`, the FIFO-head
  claim with `SKIP LOCKED`, row states, and the retry ladder as pure functions.
  **Done when:** two concurrent claimers never get the same row (run with `-race`, many
  iterations); version 2 of an entity is not claimable while version 1 is in flight; a repeated
  `delivery_id` inserts nothing.

## M1 · One provider end to end: ClickUp

- [x] **B05 Record format.** Go types for the envelope; the JSON Schema; ADRs for open decisions
  3 (scope id format) and 4 (field names), since this becomes a public contract.
  **Done when:** the schema validates the example in architecture section 6 and rejects a record
  with no `visibility.scope`, an unknown `kind`, or an `occurred_at` that is not RFC 3339.
- [x] **B06 Provider interfaces and the ingress edge.** `internal/provider` with the interfaces
  from architecture section 7 and the registry; `/ingress/{provider}` with raw-body capture, a
  body-size cap and the handshake hook; a fake provider for tests.
  **Done when:** the handler hands the verifier byte-identical input (tested with a body whose
  JSON re-serialization differs); an oversize body is refused without being read into memory.
- [x] **B07 The hub.** Delivery keys, candidate subscriptions under the resolver role,
  per-candidate constant-time verification, owner resolution, accept, sentinel parking.
  **Done when:** forged signature gets 401 and stores nothing; unknown workspace gets 200 and a
  parked row under the sentinel tenant; two tenants whose secrets both verify get a parked row,
  never a routed one; an identical re-send is a no-op.
- [x] **B08 Pipeline stages.** Normalize hand-off, the automation-noise gate, the ledger,
  forward-only supersede, regex masking with the redaction map kept locally.
  **Done when:** an old version arriving after a newer one never gets a `supersedes` pointing at
  the newer; masked text contains no email, phone or IBAN from the fixture set; a ledgered id is
  skipped.
- [ ] **B09 Sinks.** `http`, the strict `stub`, `jsonl`; wire name as sink configuration; every
  record validated against the schema in tests.
  **Done when:** the stub rejects everything the schema rejects plus a repeated id with different
  content; the http sink classifies 401/403 as halt, 5xx and timeouts as retryable, 4xx per-record
  rejections as dead-letter.
- [ ] **B10 Worker drain.** Drain goroutine pool, each claimed row worked in a second transaction
  bound to its tenant (never a bind inside the worker role, architecture section 4), the two-transaction
  commit, degrade-to-minimal on hydration failure, dead letters and replay, graceful shutdown.
  **Done when:** a crash injected between prepare and deliver re-drains into "already delivered"
  with exactly one record at the stub; a failing sink walks the ladder and parks; replaying the
  dead letter delivers it.
- [ ] **B11 ClickUp provider.** Signature verification, `Parse`, the direct API client with a rate
  limiter that refuses requests above its capacity, hydration, the normalizer (a comment yields
  the comment and its re-hydrated parent task), golden files.
  **Done when:** signature-negative, parse and normalizer golden tests pass against recorded
  payloads with secrets scrubbed.
- [ ] **B12 M1 acceptance. (needs you)** An end-to-end test with a fake ClickUp API server, then
  the same run against a real workspace through a tunnel.
  **Done when:** every line of the roadmap's M1 "Done when" is demonstrated, the fake-server
  version in CI and the real-workspace version once by hand.

## M2 · Credentials and connecting

- [ ] **B13 Vaults.** The `Vault` interface; `local` (AES-GCM, refuses plaintext, refuses to start
  in production without a key); `nango` (self-hosted, HTTP only); `azureapp` (client credentials).
  Settles open decision 7 as an ADR.
  **Done when:** a test scans every table after a store and finds no token material; tampered
  ciphertext fails to decrypt; a missing key in production is a startup error.
- [ ] **B14 Operator API and `connect`.** `/v1` providers, connection lifecycle, capabilities,
  health; operator credential auth; `lawang connect <provider>` reading secrets from the
  environment only; the ClickUp registrar (update in place).
  **Done when:** a connection can be created, completed and deleted through `/v1`; deleting it
  calls the provider-side deregister (asserted against the fake server); re-registering does not
  duplicate the subscription.

## M3 · The other four providers

- [ ] **B15 Slack.** Events API, the URL challenge, the replay window, channels and DMs as scopes,
  mrkdwn links unwrapped before masking, hydration, normalizer goldens.
  **Done when:** signature-negative, stale-timestamp, challenge and normalizer tests pass; a DM's
  scope is the conversation, never a flag.
- [ ] **B16 Graph client and Outlook.** A shared Microsoft Graph client; per-mailbox subscriptions,
  the `validationToken` plain-text echo, `clientState` checks, mixed batches split per owner.
  **Done when:** a batch carrying two tenants' notifications produces two outbox rows with the
  right owners; a wrong `clientState` is refused.
- [ ] **B17 Teams.** Channel-message subscriptions, reply threading into `edges.reply_parent`,
  system events skipped.
  **Done when:** handshake, negative and normalizer golden tests pass.
- [ ] **B18 HubSpot.** Reconcile-only by default; signed webhooks (v3 signature) as an option.
  **Done when:** signature tests pass, and with webhooks off the provider registers no
  `WebhookSource` and the ingress route returns 404.
- [ ] **B19 Registrars and the renewal sweep.** Registrars for Slack, Outlook, Teams, HubSpot; the
  sweep framework (own ticker, advisory-lock election); renewal with recovery when the provider
  has deleted the subscription; an injectable clock.
  **Done when:** under a fast-forwarded clock an expiring subscription renews before it lapses; a
  404 on renew re-registers; two workers never run the same sweep at once.
- [ ] **B20 Subscription health and real tenants. (needs you)** `/v1/subscriptions/health`; one
  real event per provider landed at the sink.
  **Done when:** the health endpoint reports expiring and failed subscriptions; the real-tenant
  checklist below is ticked for each provider you have access to.

## M4 · Reconciliation and access sync

- [ ] **B21 Reconcile framework and ClickUp.** Cursors, chunked passes that commit per chunk and
  pace between chunks, replay through the accept path as a trusted synthesized delivery,
  `lawang reconcile <tenant> <provider>`.
  **Done when:** a test asserts no pause ever happens inside an open transaction; a gap left by a
  stopped webhook is filled and the overlap dedupes on record id.
- [ ] **B22 Slack and HubSpot reconcilers.**
  **Done when:** the drain keeps delivering live events while a large reconcile pass runs
  (asserted on delivery latency in an integration test).
- [ ] **B23 Member sources and identity.** `MemberSource` for all five providers; the email-join
  resolver, normalized and domain-restricted.
  **Done when:** an address outside the allowed domains resolves to nobody; a provider read error
  is an error, never an empty list.
- [ ] **B24 Membership diff and `AccessSink`.** The shadow record of what was sent, the diff,
  grants and revocations pushed to sinks that accept them.
  **Done when:** removing someone from a channel yields exactly one revocation; a provider outage
  yields none; a re-run with no change sends nothing.

## M5 · Hardening and operations

- [ ] **B25 Dead-letter re-resolution, retention, metrics.** Parked rows re-resolved on a sweep and
  deleted after retention; Prometheus metrics (outbox depth by stage, oldest undelivered age,
  errors by provider, sweep durations); `/readyz` beside `/healthz`.
  **Done when:** a row parked before its subscription existed delivers after the subscription is
  created; `/readyz` fails when Postgres is unreachable.
- [ ] **B26 Isolation suite.** Two tenants on one provider workspace, forged tenant attempts,
  cross-tenant claim checks, and a test that greps captured logs for token material.
  **Done when:** the suite is green in CI as its own job.
- [ ] **B27 Compose stack.** `docker compose up` with Postgres, serve, worker, the stub sink and
  the local vault; a multi-stage Dockerfile producing a static binary.
  **Done when:** the M1 acceptance test runs against the compose stack with only Docker installed.

## M6 · v0.1.0, public

- [ ] **B28 Docs.** A quickstart from a fresh clone, a setup guide per provider,
  `CONTRIBUTING.md`, the provider-authoring guide, `SECURITY.md`.
  **Done when:** the quickstart has been followed on a clean machine or container, start to finish.
- [ ] **B29 Release. (needs you)** Release workflow, image on GHCR, changelog, tag `v0.1.0`, flip
  the repository to public.
  **Done when:** `docker run ghcr.io/gablooge/lawang:v0.1.0 version` prints the tag.

---

## What needs you, and by when

| By | What | For |
|---|---|---|
| Sep 29 | A ClickUp workspace, an API token, and a tunnel (cloudflared or ngrok) | B12 |
| Oct 2 | A Slack app with a signing secret in a test workspace | B15, B20 |
| Oct 3 | An Azure app registration with Graph application permissions, and a test mailbox and team | B16, B17, B20 |
| Oct 5 | A HubSpot developer test portal | B18, B20 |
| Oct 15 | A decision to go public, and a final read of the README | B29 |

Real-tenant checklist (tick when one real event has reached the sink):

- [ ] ClickUp
- [ ] Slack
- [ ] Outlook
- [ ] Teams
- [ ] HubSpot

---

## Log

One line per finished item: date, item, anything worth remembering.

| Date | Item | Notes |
|---|---|---|
| 2026-09-19 | B01 | Code written 2026-09-18, first CI run green on PR #30 the next day. The build-info package is `internal/appversion`, because revive rejects package names that shadow the standard library (`version`, `buildinfo`). |
| 2026-09-18 | B02 | All 14 golden vectors cross-checked against the Python `blake3` package, so the recipes are reproducible outside Go. `RecordID` and `DeliveryID` return an error for an empty part or a part containing `0x1F`; architecture section 5 updated to say so. |
| 2026-09-19 | B03 | Roles and schema come from a one-time admin bootstrap script (`lawang migrate bootstrap`); migrations run as the application role, and `store.Open` refuses a superuser or BYPASSRLS login everywhere. Postgres 16 is the minimum. Found on the way: a transaction-local setting reads back as `''` on a pooled connection, so policies go through `current_tenant()`. Mutation-checked: dropping FORCE RLS fails six tests. ADRs 1 and 2 written; sqlc itself arrives with B04. |
| 2026-09-19 | B04 | Completes M0. A claim is a lease with a token, not a held lock. `delivery_id` is unique per tenant, not globally (design change, architecture section 5). The worker role sees only scheduling columns and can never read a payload. sqlc runs from its pinned Docker image (`make sqlc`, `make sqlc-check` in CI). Mutation-checked: the first version of the concurrent FIFO test missed a claim query with no head-of-key rule, so it was rewritten with adjacent versions and now catches it. Review round 1 (2026-09-19): `seq` is assigned at INSERT, not at COMMIT, so Accept and Replay now take a per-key `pg_advisory_xact_lock` before assigning one, and a replayed dead letter takes a fresh `seq` and goes to the BACK of its entity's queue (it could otherwise be leased alongside a newer version in flight). `prepared_at` lets a replay restore `prepared`. Round 2: the claim's snapshot-to-row-lock window is tested deterministically by pausing the claim in a test-only RESTRICTIVE select policy whose function waits on an advisory lock when it is shown a gate row, run under five planner settings. (The first attempt, a BEFORE UPDATE trigger, only paused under the default plan: do not use it.) The lock-before-INSERT order in `Accept` is pinned by stopping an INSERT after it has its `seq`. Round 3: the claim cost O(backlog) per poll (4.1 s at a million waiting rows), so the head of a key is now a stored marker, `is_head`, kept under the key's lock by every writer, the finishing transitions included, and guarded by a unique index and a CHECK ([ADR 10](adr/0010-outbox-head-marker.md)): 0.3 ms at the same size. Worth remembering: a condition that repeats what a CHECK already says (`state` next to `is_head`) made the planner read every waiting row at a batch of 100, so the claim has none, and a guard test bounds the claim's buffers and checks its plan. Also round 3: `Fail` and `MarkDead` take an `outbox.Cause` and no text (an HTTP client's error quotes the URL, API key included), `Claimed` is read-only, ordering keys are at most 512 bytes (`ErrBadOrderingKey`), a claim leases at most `MaxBatch` rows. After the review of the redesign: the head marker made the isolation level load-bearing (under a `default_transaction_isolation` of REPEATABLE READ a writer that waited for the key's lock decides on a snapshot from before the wait and strands a row, silently), so `store` begins every transaction READ COMMITTED by name. An ordering key with a NUL or invalid UTF-8 is `ErrBadOrderingKey` too, not the database's 22021. `Outbox.StrandedKeys` finds a key with work and no head, and ADR 10 has the repair; the sweep and the metric are B25. |
| 2026-09-19 | B05 | Format v1 lives in `internal/record`, and the contract is `record.v1.schema.json` there, embedded. ADRs 3 and 4. What differs from the proposal: a `format` version field, `meta.raw_ref` dropped, `delete` defined now because a closed `op` cannot gain a value later, `visibility` closed while unknown fields are allowed everywhere else, lowercase-only field names. **The record id recipe of B02 changed: the scope is hashed too**, so a record that moves to another scope is a new record even when the provider's version did not change (otherwise the ledger skips it and the sink keeps the old scope, silently); golden vectors regenerated and re-verified with Python `blake3`. The scope id uses the internal provider key, never the wire name, and percent-escapes the container id into one canonical spelling, because it is the join key between a record and its scope's membership. Worth remembering: `format: date-time` is only an annotation in many validators, so `occurred_at` has a pattern too and the tests compile the schema both ways; Python's `$` matches before a trailing newline, so every anchored pattern has a companion that does not lean on the anchor; Go's `encoding/json` matches field names case-insensitively and reads a missing `origin` as "trusted", so `Record` has a strict `UnmarshalJSON`. Left for later items: the kinds freeze at v0.1.0 (B18 is the last chance to add one), the strict stub must add the two checks no schema can make (self-supersede, a field name used twice) and compare content without `meta` (B09), normalizers must move `version` on every change including a move (B11), and the membership message and person identifier are B23 and B24 (issue #36). The schema `$id` needs the maintainer's confirmation before v0.1.0. Review round 1 (2026-09-19), all of it things that freeze with v0.1.0: the compatibility promise is stated from the reader's side (every record Lawang produces validates against every earlier v1 schema), so a limit or a pattern moves in neither direction within v1. A record document is UTF-8 with no unpaired surrogate escape, checked on the bytes before parsing, which makes three checks no schema can make (B09's strict stub needs all three): `encoding/json` rewrites both to U+FFFD and turns two documents into one identity. Character rules field by field: identifiers refuse controls (C0, DEL, C1), U+2028, U+2029, bidirectional formatting and the common zero-width characters, `author.display` the same but keeps U+200C and U+200D, `title` is one line, `text` refuses only NUL; normalizers must clean names and titles before `Seal` (B11). `external_id` is unique per tenant and begins with the provider key and a colon, `Seal` enforces it, and a sink keys by tenant and `external_id`, never by `source`. `origin.untrusted: false` means no signal, never safe, and v0.1 sets it for no provider. `Seal` is enforced as the only way to an id: a test fails on any caller of `ids.RecordID` outside `internal/record`, and `Marshal` refuses a record changed after `Seal`. In a URL a scope id is percent-encoded once more. **Decided by default, for the maintainer to confirm:** the A, B, A move with an unchanged version is not fixed in the format, and B08's ledger must dead-letter and count it, never skip it (ADR 4, decision 7). Worth remembering: regular expression dialects share no escape for a character above U+00FF, so the schema carries those as JSON escapes; and the editing tools turned `\u` escapes in source files into literal characters again (a NUL, a U+2028), so every touched file was checked byte by byte. Review round 2 (2026-09-19, approved, a should-fix pass): the three character classes of the schema did not compile in Ruby, whose engine refuses a hex escape of 0x80 or above in a UTF-8 pattern, so a pattern now escapes ASCII only and carries every character from U+0080 up as itself, written as a JSON escape in the file. Old and new spelling were compared over all 1,112,064 code points in Go, Python, ECMAScript (both modes) and Ruby, a test refuses the old spelling, and ADR 4 says that a respelling with such a proof is wording, not a change to the format. The character test now runs every code point of Unicode through the schema's patterns, the Go rule and the ADR's ranges, and what is deliberately NOT refused (tag characters, variation selectors, Hangul fillers, U+3000) is pinned as named accepted cases. The seal also holds `Op` and `Kind`, and keeps the tenant: `Record.SealedFor(tenant)` is for B08 to call where it delivers, and is false for a decoded record. The seal guards against accidents, not against a type conversion that sheds the methods. Identifiers are never cleaned (only `author.display` and `title` are), and an identifier a sender controls (a mail `Message-ID`) is escaped or hashed injectively, never cleaned and never raw (B11, B16). |
| 2026-09-20 | B06 | `internal/provider` has `Provider` and `WebhookSource` plus the registry, `internal/ingress` has the edge, and `internal/provider/fake` is a strict test double that tests of later items import. **The registry calls `record.ValidProviderKey`**, the one function that owns ADR 3's grammar (`[a-z][a-z0-9_]{0,31}`, no hyphen), and a test holds the two to the same verdict on every byte in the first and in a later position, so the registry and the scope id cannot drift. **The path segment never travels further than the lookup**: `net/http` decodes `%00` in it to a NUL byte, so the edge resolves the segment to a registered provider before a byte of the body is read and hands everything downstream the registry's own key, which is what makes `outbox.Delivery.Provider` a constant of the program (a test compares the string data pointers). An unknown provider, and a registered provider that is not a `WebhookSource`, both answer 404 with nothing read and nothing stored: the one deliberate exception to "2xx for everything else", safe because a provider only posts to the URL Lawang gave it. **The body is captured once** under an `http.MaxBytesReader` in front of everything that touches it, hashing included, and those exact bytes go on; the acceptance test signs a body whose JSON re-serializes differently and the control case (the same signature over the re-serialized bytes) answers 401. A body with no end is cut at the cap. The full response contract is in architecture 3.1 now, including 413 for an oversize body, 400 for a `Content-Length` that lies and **503 with a `Retry-After` for an accept that runs out of time**, which is how a saturated pool answers instead of hanging (`pool_max_conns` is documented in the usage text and in architecture 4). **What differs from the design:** `Registrar`, `Reconciler` and `MemberSource` are deliberately not written yet, because `Subscription`, `Credential`, `Cursor` and `ScopeMembers` are decided by B07, B13, B14, B19 and B23; architecture 7 says so. Left for B07: the `ingress.Hub` interface is the whole seam (`Accept(ctx, provider.Entry, provider.Request) (Verdict, error)`), and `serve` does not mount the edge until a hub exists, because an edge with no hub would answer a provider without storing anything (the README says so now, because a mux 404 and the edge's own 404 are indistinguishable). Worth remembering: a fixture that happens to re-serialize to itself proves nothing (the fake's event fixture is spelled out of field order on purpose), and the fast path on an honest `Content-Length` masked an off-by-one in the cap until a test sent a body of unknown length. Review round 1 (2026-09-20): **`Verify` now takes a `provider.Request`** (method, public URL, headers, body) rather than three arguments, because HubSpot's v3 signature (B18) covers the method and the full request URI and widening the interface after B07, B11 and B15 implement it would cost four packages. **The public URL is configuration, `LAWANG_PUBLIC_BASE_URL`, never `Host` or `X-Forwarded-Host`**: a sender that picks part of its own signed input is not being checked. Unset is a refusal and not a guess: the edge still serves, `Request.URL` is empty, and a scheme that signs the URL returns false (architecture 3.1 and 4). `config.NormalizePublicBaseURL` owns the spelling and `ingress.New` calls it, because the two strings meet inside an HMAC. Also: a registered provider that is not a `WebhookSource` still answers the same 404 but is now logged at warn, since only our own wiring can reach it and every real delivery is being dropped; a handshake may no longer answer **401 or 403** (401 is reserved for a signature failure) and its content type is an allowlist of `text/plain` and `application/json` with an optional `charset=utf-8`, capped at 64 bytes, because `text/html` with an echoed challenge is reflected script and `nosniff` does not help. **The lesson of the round is that a check whose test is satisfied by an earlier layer is untested**: `utf8.Valid` in the fake survived deletion because the only fixture naming UTF-8 was not valid JSON either, and two more (the external id length bound, the UTC normalization of `occurred_at`) turned out the same way. Coverage was 100% throughout and showed none of it. Review round 2 (2026-09-20): **`config.NormalizePublicBaseURL` was not idempotent**, and the composition B07 will write is what exposes it: `https://x//` came back as `https://x/`, so `Load` stored one spelling and `ingress.New` normalized the stored value to another, and two spellings that meet inside an HMAC are a 401 on every delivery. Every rule is now idempotent in itself (all trailing slashes removed rather than one, every percent-escape refused rather than some decoded) and the property is asserted over a corpus and a fuzz target rather than over table rows; the fuzzer found two more shapes in seconds, an escaped `!` the rebuilt answer re-escaped and a host of `%25` that url.Parse decodes to `%`. **A path the mux would clean is now refused, not redirected**: over a real socket `//ingress/fake` and `/ingress/fake/../fake` answered 307, which a provider follows by re-POSTing to a path it did not sign, so every delivery would be a 401 that reads as a forgery. `Mount` returns the handler the server serves (the mux with the guard in front), because the mux redirects before any handler runs. `ingress.New` warns when `LAWANG_PUBLIC_BASE_URL` is unset, the one signal an operator otherwise never gets. `provider.Request.Header` is a read-only `provider.Header` rather than an `http.Header`, since the hub hands one `Request` to `Verify` once per candidate and a map would let one implementation change what the next candidate reads. **Worth remembering: a property and a fuzz target found three bugs that eight table rows and two reviews did not.** Review round 3 (2026-09-20): the path guard read the **decoded** path while `net/http` cleans the **escaped** one, so it refused `%2f%2f` and `%2e%2e` that the mux would have routed, and it missed the mux's other redirect entirely (`POST /sub` still answered 307 to `/sub/` through the handler `Mount` returned). The guard now reads `r.URL.EscapedPath()`, and the second redirect is taken away from the mux rather than guarded against: `ingress.New` builds the routing table, takes every other route as an `ingress.Route`, and registers the slash-less path itself for each of the three pattern spellings that can match a path ending in a slash. **`Mount` is gone**: its return value could be dropped, and the reviewer proved that `h.Mount(mux); srv.Handler = mux` compiles, vets and lints clean while bringing back three 307s, so `New` now returns the only servable thing this package hands out and the edge no longer implements `http.Handler`. `NormalizePublicBaseURL` accepted `http://:8080`, which names a port and no machine (`u.Host` carries the port, so the empty-host check missed it), and it accepted an IDN host while refusing an IDN path prefix; both are refused now, and punycode is the spelling a dashboard holds. `provider.Request.Body` is documented as a rule rather than a guarantee, because it aliases the edge's buffer and only B07's per-candidate loop can copy it (#7 carries the ask, with the measured 0.9 us per 8 KiB copy). **Worth remembering: a guard in front of a mux can only see the request, so a redirect that depends on the routing table has to be fixed where the table is built.** Review round 4 (2026-09-20): **the rule that decided when to register the slash-less route compared path strings**, and whether the mux redirects is a property of the whole table, so it buried a caller's route (`POST /v1/{resource}` already answers `/v1/tenants` exactly, so the 404 registered to prevent a redirect that was never going to fire beat the wildcard for that one path, silently) and refused a table `net/http` accepts (`GET /v1/{id}/` plus `GET /v1/{name}`, with a message quoting a pattern the caller never wrote). `New` now builds a second mux carrying the same patterns and handlers that do nothing, and asks it, which cannot run a caller's handler. (Round 5 corrected the claim made here that this is "exact by construction for every pattern spelling": it is exact for the one representative path it asks about, and that generalises to the pattern's family only under the rule round 5 added.) That also removed the enumeration of "three spellings": it was incomplete, because `net/http` stores a last segment written `%2F` as the same segment as `{$}`, so `POST /a` answered 307 to `/a/` for a route the enumeration said had no root. **A route under `/ingress/` is refused at start**: a literal such as `POST /ingress/fake` is more specific than the webhook pattern and used to answer every delivery for that provider in the edge's place, with no error and nothing logged. **Worth remembering: a rule about what a library will do is worth less than asking the library, when the table is in hand before the answer is needed.** Review round 5 (2026-09-20): **a percent-escape in a `Route.Path` is refused**, which closes one defect reached from two sides. `net/http`'s pattern parser decodes a literal segment before it stores it, so every check in the package, and the redirect probe, read a different string from the one the mux matches with: `/%69ngress/fake` and `/ingres%73/fake` are stored as the pattern `/ingress/fake` and walked past the `/ingress/` refusal to answer deliveries in the edge's place, and `/a/%7Bx%7D` is stored as the literal segment `{x}`, which is the text a wildcard pattern is probed with, so `POST /a/{x}/` plus that route read "no redirect" while `POST /a/b` still answered 307 over a socket. Nothing this program wires needs an escape, so refusing it is what makes the written path and the parsed pattern the same string. That also made the probe's `RawPath` handling dead, and it is gone. Three mutants that survived at 100% statement coverage now have tests: the `< 400` bound on what counts as a redirect (a 405 read as a redirect turns the mux's own `Allow` into this package's 404), the trailing slash on `edgePrefix` (without it `/ingress`, `/ingressive` and `/ingressX/y` are refused, and nothing asserted what stays accepted), and a conflict in the second guard route rather than the first. **The `guarding` mutant the review called live is equivalent** and is marked at the line: the name is reassigned immediately before the only calls that can panic while it is read. **`New` can refuse a table `net/http` accepts**, when a method-less subtree meets a method-specific sibling at the same depth, and that is now documented on `New`, on `Route` and in architecture 3.1 with the way out (name the method on the subtree route); B08 is the caller that will meet it. **Worth remembering: a check that reads a string a library will reparse is checking the wrong string, and 100% statement coverage hid three of those.** |
| 2026-09-21 | B07 | The tenant now comes from an owned row and nothing else. `internal/hub` resolves a delivery: delivery keys (bounded, storable, and a delivery with none is parked rather than looked up, since finding its owner would mean verifying every subscription there is), candidates under `lawang_resolver` in a transaction that commits before anything is verified, per-candidate verification over the exact bytes, then the accept under `TenantTx`. **Two transactions, not one**, because a helper-role transaction is cross-tenant for its whole life and `store.RoleTx` refuses a bind. **Nothing short-circuits on the first candidate that verifies**: "exactly one" cannot be told from "the first of two" without asking them all, and more candidates than the hub will verify (32) is parked as ambiguous rather than truncated, because a set cut short could hide the second tenant. Decisions, all in [ADR 11](adr/0011-hub-resolution.md): the **verification secret is a subscription column**, because the accept path derives the tenant FROM the secret and `Vault.Fetch(tenant, provider)` cannot answer a question that has no tenant yet (the vault keeps the API tokens, B13); the **sentinel tenant is `_parked`**, which the `tenants` table refuses with a CHECK so no operator credential is ever issued for it, and `outbox.Park` takes no tenant from its caller at all; and an accepted delivery is **ordered by the subscription it arrived on**, since the hub does not parse a delivery and so cannot name an entity (coarser is safe, and it costs parallelism, which B08 and B11 can narrow). `serve` now opens the database, builds the registry and the hub and serves what `ingress.New` returns, `newMux` is gone and `/healthz` is an `ingress.Route`; the hub **refuses to start** when a registered provider implements `provider.URLSigner` and `LAWANG_PUBLIC_BASE_URL` is unset, since every one of its deliveries would be a 401. The per-candidate loop copies `Request.Body` (0.9 us at 8 KiB), recovers a `Verify` that panics and gives up on one that does not return, and parks in both cases: a candidate that did not answer cannot be ruled out as the owner. `internal/provider` gains `Subscription` (which redacts its secret in `%v` and in a log line), `Registrar`, `URLSigner` and `Registry.Entries`; `Credential` is declared opaque, as `Hydrated` is, because B13 decides it. Worth remembering: **22 mutations, and two survived at first**. The EXPLAIN test accepted any "Index Scan", and without the workspace index the planner walks the primary key for `ORDER BY id LIMIT` and reads the whole table, which is an Index Scan too; it now names the index and bounds the buffers. And refusing a subscription row that could not have come from Lawang was indistinguishable from letting `tenancy.Bind` refuse it later, until the test added a second, good candidate that would otherwise have been routed to. Review round 1 found the one way a delivery could become the wrong tenant's: the candidate lookup was an either/or, so a subscription registered with only a registration id was never asked when the delivery also carried a workspace, and two tenants sharing a secret were routed instead of parked. The lookup is now the union of one index probe per key the delivery carries (ADR 11, decision 4). The plan test now EXPLAINs the statement pgx prepared, found in pg_prepared_statements, because the version that EXPLAINed a copy of the SQL survived rewriting the checked-in query into a sequential scan. An unreadable delivery is parked as a note and not as a stranger's megabyte. Review round 2 (approved, close-out): **a type name is not a compile-time constant**, and the reviewer proved it by panicking with a `reflect.StructOf` type carrying the secret in a struct tag, which the hub logged at error level; a panic value's type name is now logged only when it is short and made of the characters a plainly written type name uses, and a placeholder otherwise. The ambiguous-owner park names the colliding subscription ids and tenant ids instead of a count, which also gives the candidate sort the observable effect its comment claimed (a `slices.Reverse` mutation had survived). **Two subscriptions of ONE tenant are parked too** and the message says so: the ordering key names the subscription, so choosing one of two rows is choosing a queue on no evidence (ADR 11, decision 5, and the shape B11 must not register). Review round 3 (delta, approved, should-fix pass): a **second** mutation of the candidate sort survived, deleting it outright, because both ambiguity tests collide two rows that the by-workspace probe returns together and that query is already `ORDER BY id`; the union of two DIFFERENT probes is now observed by a test where each row is found by a different probe and probe order is the opposite of id order. **Ambiguity is about subscriptions, not tenants**, and the sentence saying otherwise was in four more places than the two already fixed (`outbox.ParkAmbiguousOwner`, `ingress.Verdict.Parked`, the README isolation bullet and architecture principle 2), which matters because B25 is written from that constant's doc. The type-name filter survived 13 run-time constructions in the review and holds for TEXT; the comment now names the residual it does not close (an array length is digits, so `*[126664548954996]uint8` carries about six bytes of a secret per panic, which needs a provider package that encodes on purpose: recorded on #28, the provider-authoring guide). ADR 11 decision 5's third bullet is marked as supporting rather than load-bearing, since routing to both rows is a race only across transactions. |
| 2026-09-21 | B08 | `internal/pipeline` is step 5 of the drain path, in two calls on purpose: `Normalize` parses, hydrates, normalizes and gates outside any transaction, and `Prepare` does the ledger, the supersede chain and the masker inside the transaction the worker will commit, so a provider's API is never called with a transaction open. Decisions in [ADR 12](adr/0012-ledger-supersede-masking.md). **A version is ordered by three rules** (ADR 12 decision 1, rewritten in review round 1): two runs of decimal digits by the number they spell, two versions of equal length by their bytes, and anything else not at all, which is a dead letter by name rather than a guess. Two records with the SAME version are ordered by arrival, and that is the only thing arrival decides. A record older than its entity's head is **held back and counted, never linked and never delivered**: linking it backwards would replace a newer record at the sink and take the scope with it. The chain is keyed per `(tenant, provider, external_id)` and never per scope, and a ledger row means PREPARED, never merely seen, so a record that was skipped or held back is not written (writing it would make a later legitimate arrival of that version look already delivered). **A, B and back to A is dead-lettered with its own error and counted** (`Prepared.ScopeReturned`, which survives the error), as ADR 4 decision 7 requires, with the entity and both scopes named; the rule also catches A, B, C, B, and once retention (B25) has pruned the first record it becomes an ordinary supersede, which is why retention may prune non-heads freely and **never a head**. `record.SealedFor` is asked here and nowhere else, against the tenant of the outbox row, and a false is a dead letter. `provider.Degrader` is new: a degraded record derives its scope through the same function the hydrated one uses, and a body that does not carry what the scope is made of returns `ErrCannotDegrade` and waits for hydration rather than guess. Masking is last, after the ledger, so a skipped record costs nothing; the placeholder is a **ULID and not a hash of the value**, because a deterministic token would let any sink confirm a guessed address, and the map lives in `redaction_map`, row-level secured, never sent anywhere. What differs from the design: the ledger bounds `external_id` at 2,048 **bytes** where the format bounds it at 1,024 characters (a btree tuple cannot hold 4,096), refused by name as `ErrExternalIDTooLong`; and a field that masking makes longer than the format allows is a dead letter rather than silently cut. Worth remembering: **three mutants survived at first**. Masking moved before the ledger was invisible, because one value is one row however often the masker runs, until the test compared `last_seen_at` as well as the row count. The phone digit count had no evidence outside its own unit test, because every "must not match" fixture was already refused by the pattern a step earlier; three fixtures that the pattern accepts and the count rejects fixed it. And nothing pinned the order the entity locks are taken in, which is the whole defence against a deadlock, so the ordering is a function of its own now with a test on it. One mutant is **equivalent and marked at the line**: condition 2 of ADR 4 decision 7 ("not the head") cannot change an answer, because when the incoming record is the head, condition 3 compares a scope with itself; it stays so the code can be read against the ADR line by line. Review round 1 (2026-09-21): **byte order fails in the unsafe direction**, which is the one thing ADR 12 promised it could not do. Prepare version "10", then drain version "9" (a replayed dead letter or a backfill, both paths ADR 12 names): byte-wise `"9" > "10"`, so the older record was not stale, it superseded the newer one, became the head, and the sink's live version and the scope access is decided on both went backwards, with `Stale` at zero and no error. The pinned test only covered 9-then-10, which is the safe direction. The rule is now the three of ADR 12 decision 1 and both directions are tested, together with the replayed dead letter and the backfill. Length-then-bytes was rejected (it inverts a ULID, the encoding the ADR recommends) and so was an optional provider comparison (it leaves the unsafe default for whoever forgets). **The masker replaced most IPv4 addresses with a telephone token** while the code comment and a fixture said dotted quads were excluded: every address whose octets are all three digits matched (`172.217.169.110`), and so did a version, an invoice total and an order reference. The national form now needs exactly three groups, one of four digits, and a first group that is not a year, and the "must not match" fixtures carry the reviewer's whole list plus an order id, a version, a hash and two timestamps. **The A, B, A count was claimed in three documents and existed in none**: `Prepared` had no field and `Prepare` returned `Prepared{}` beside every error, so a delivery that died halfway lost everything it had decided. There is a counter now and every error returns the counters it had reached, with `Records` nil. **The test that existed to prove the placeholder is not derived from its value could not tell a digest from a random token**: `sha256(tenant, kind, value)` passed the whole package, and a tenant id is not a secret, so the offline oracle ADR 12 decision 5 exists to prevent was fully back. The property is pinned as what it is now: the map is emptied and the same value minted again, and the two tokens must differ. Also: the arrival-order justification was false for the two paths the advisory lock exists for (the ordering key is per subscription, ADR 11 decision 3), and the residual is in ADR 12's Cost section; a masking failure the database caused is now proved not to be a dead letter (R19 had survived); one delivery may map at most 1,024 distinct values, because the text is sender-controlled and a measured 500 KB of addresses wrote 20,445 permanent rows in one statement; `mapSecrets` keeps the SQLSTATE and the constraint name, which can hold no value, and drops only the message and the detail; the masker's pure half is unexported and reachable from `export_test.go` alone, because `Find(text).Secrets()` was a public one-liner returning every address in the clear; and `internal/store` now pins the whole privilege surface of both helper roles, since RLS says nothing about grants and a grant on `redaction_map` would be a straight leak. **Worth remembering: the one direction a test does not run is the one that fails, and a test that asserts a token does not quote its value cannot see a hash of it.** |
