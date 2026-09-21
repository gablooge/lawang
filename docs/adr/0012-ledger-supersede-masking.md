# 12. The ledger, the supersede chain and the masker

Status: accepted, 2026-09-21 (backlog item B08)

## Decision

Step 5 of the drain path ([architecture 3.2](../architecture.md#32-drain-path-inside-worker)) is
`internal/pipeline`. [ADR 4](0004-record-format-v1.md) settled what a record is and left five
things to the stage that writes one; this record settles those five.

1. What "monotonic version" means, and what the chain is ordered by.
2. What the chain is keyed by, and what the ledger holds.
3. What happens to a record older than what an entity already has.
4. How the A, B, back to A case of ADR 4 decision 7 is detected, and what it costs.
5. What the masker recognizes, what a placeholder is, and where the map lives.

### 1. A version is ordered by its bytes, and equal versions by arrival

ADR 4 calls `version` opaque to a sink and, in the same paragraph, makes "monotonic per
`external_id`" a promise the provider's normalizer makes to the pipeline, "which B08 uses to keep
the supersede chain forward only". It never said what monotonic means for a string.

**It means byte order.** That is the only order an opaque string has, and it is what
`strings.Compare` gives. So a normalizer spells a version so that byte order is version order: an
epoch in milliseconds, an RFC 3339 timestamp in UTC, a fixed-width counter. **Never a bare decimal
counter**, where `"10"` sorts before `"9"`.

**Two records of one entity that carry the same version are ordered by arrival.** That is the case
decision 7 is about, an entity that moved while the provider's version stood still, and the second
of the two supersedes the first. Arrival order is a usable order because the outbox makes it one:
one entity's versions share an ordering key, only the head of a key is ever claimable
([ADR 10](0010-outbox-head-marker.md)), and a replayed dead letter goes to the back of the queue.
The ordering key of B07 is coarser than one entity ([ADR 11](0011-hub-resolution.md)), which only
serializes more.

The stage does not lean on that alone. It takes a transaction-scoped advisory lock on
(tenant, provider, `external_id`) before it reads the chain, because the outbox's FIFO does not
cover every path in: one entity reached through two subscriptions of one tenant, and a
reconciliation pass running beside the live feed. Without the lock two transactions can both read
head X, and the second can demote what the first inserted, leaving two records superseding X and
one orphaned in the middle of the chain, with no constraint violated. The locks of one delivery are
taken in sorted order, so two deliveries carrying the same entities can queue but cannot deadlock.

### 2. The chain is per entity, and the ledger holds what was prepared

`record_ledger` has one row per record id, keyed `(tenant_id, record_id)`, and a partial unique
index makes exactly one row per `(tenant_id, provider, external_id)` the **head**. The chain is
therefore per entity and **never per scope**: a record that moves to another scope supersedes what
it was in the old one, so the sink replaces it rather than keeping a stale copy that the old
scope's members go on reading.

The provider is in the key although the external id already begins with it ("clickup:task:86a1xyz",
enforced by `record.Seal`). It adds no identity; it is what an operator filters a dead letter by.

**A row means prepared, never merely seen.** A record the stage skipped or held back is not
written, because a later legitimate arrival of that same version would then be skipped as already
delivered and the sink would never get it.

The external id is bounded at **2,048 bytes** in the ledger, which the record format does not bound
(it allows 1,024 characters, and 1,024 characters can be 4,096 bytes, while a btree tuple cannot
exceed about 2,700). A longer one cannot be linked into a chain at all, so `internal/pipeline`
refuses it by name (`ErrExternalIDTooLong`) rather than leaving a caller with "index row size
exceeds maximum". No provider mints one; if that ever changes, the format's own limit should become
a byte limit in a v2.

### 3. An older version is held back, not delivered and not linked

A record that is new to the ledger and older than the entity's head is **skipped and counted**
(`Prepared.Stale`). It is not linked, because links point forward only, and it is not delivered
unlinked, because a sink would then hold two live versions of one entity with nothing saying which
is current.

The commonest cause is a replayed dead letter, which architecture 3.2 sends to the back of the
queue on purpose and calls "a late arrival of an old version". The alternative, letting it take the
head, would supersede a newer record at the sink and with it the scope that access is decided on,
which is the failure this whole item exists to prevent.

**The residual risk, stated plainly.** A normalizer whose versions are not in byte order has its
newer records held back, and the symptom is a `Stale` count that is not zero while the sink stays
behind. That is a visible failure and the safe direction: nothing wrong is delivered and no access
is widened. The opposite rule (trust arrival order alone) fails in the direction that widens
access, silently.

### 4. A, B, and back to A: dead-lettered and counted, as ADR 4 requires

ADR 4 decision 7 states the rule and marks it *decided by default, the maintainer may overrule*.
It is implemented here as written. An incoming record whose id is already in the ledger, that is
not its entity's head, and whose scope differs from the head's scope is **dead-lettered with a
reason of its own** (`ErrScopeReturned`, wrapping `ErrDeadLetter`), never skipped and never
delivered. The error names the tenant, the provider, the entity, the incoming scope and the head's
scope, which is what an operator needs to look the entity up at the source; ADR 4 says what they do
then.

Three things follow that are worth writing down because they are what the tests pin:

- **The rule also catches A, B, C, B**, since it compares against the head and not against the
  scope two steps back.
- **Retention turns it into an ordinary supersede.** B25 prunes ledger rows that are **not** their
  entity's head. Once the first record of the A, B, A story is pruned, the third record is simply
  one nobody has seen: it supersedes the head and becomes the head, which is right. So retention
  may prune non-heads freely and **may never prune a head**.
- **The second condition is equivalent to the third** and is written out anyway. When the incoming
  record is its entity's head, the head row is this row, so the scopes are equal and the third
  condition is false on its own. It stays so that the code can be read against this document line
  by line, and it is marked as equivalent at the line, because a mutation of it survives every test
  in the package and a reviewer should not have to rediscover why.

**A degraded record derives its scope through the same function the hydrated one uses.** That is a
rule on the provider, stated on `provider.Degrader`, not something the pipeline can check: it never
holds both paths' output for one record. Where the webhook body does not carry what the scope is
made of, `Degrade` returns `provider.ErrCannotDegrade` and the delivery waits for hydration or dies
on the ladder. **A guessed scope is never an option**: it would give one version of one entity two
ids, deliver it twice, and look like a move to this stage.

### 5. Masking: three patterns, a random placeholder, and a map that stays here

The masker is the conservative regex baseline of architecture section 7: an email address, a
telephone number and an IBAN, in `Title` and `Text` only.

- **Only title and text.** `author.display` is the provider's name for the author, which B23 will
  join a person to; masking it would empty the field for every provider that uses an address as a
  display name and would defeat its purpose.
- **Conservative means it refuses to guess.** An IBAN is masked only when its ISO 7064 mod-97 check
  digits are right, which is what keeps an uppercase token that merely looks like an account number
  out. A telephone number needs either a country code or separators between its groups, so a bare
  run of digits is never masked, whatever its length: an id, an epoch and an amount are bare runs
  of digits too. Overlaps are resolved by position, so the digit groups inside an IBAN are not also
  read as a telephone number.
- **The placeholder is a ULID, not a hash of the value.** A deterministic token would hand every
  sink an oracle: anyone with a guess at an address could compute its token and confirm that the
  address appears in that tenant's records, which is exactly the fact masking withholds. One value
  keeps one token within one tenant, so the same person reads as the same placeholder everywhere,
  and the same value in two tenants is two tokens.
- **The map stays here.** `redaction_map` is row-level secured per tenant, no helper role is
  granted anything on it, and nothing sends it anywhere. It holds personal data by construction and
  an operator prunes it by `last_seen_at` (B25).
- **Masking is the last stage**, after the ledger has decided, so a record that is skipped or held
  back costs no work and leaves no trace in the map.
- **A field that masking makes too long is a dead letter** (`ErrMaskedTooLong`). A placeholder is
  longer than a short address, so a title cut to exactly the format's limit can outgrow it. Cutting
  it down again would lose somebody's content silently and leaving it unmasked is the one thing
  this stage must never do, so the delivery dies and the fix is on the normalizer.

## Why

- The two halves of the stage are separate calls (`Normalize`, then `Prepare` inside the caller's
  transaction) because one of them talks to a provider's API and the other holds a transaction, and
  architecture section 10 forbids doing both at once. It also lets the worker commit the ledger
  rows together with whatever else it stores for the row it is draining, which is step 6.
- Everything the stage refuses is classified once, as `ErrDeadLetter` or not, so the caller has one
  question to ask. A provider's API that is down, or a database that is not answering, is not a
  dead letter. A record the format refuses, a tenant that does not match the seal, an entity that
  moved back, an external id that cannot be keyed: those are, because the same bytes produce the
  same answer on every attempt.
- `record.SealedFor` is asked here and nowhere else. The tenant is in no field of the envelope, so
  a record sealed for tenant A marshals identically under tenant B and no sink can tell. This is
  the one place that can, and a false there is a bug or an attack, never a retry.

## Cost

- **The version order is a promise no code can check.** A normalizer that spells versions in an
  order that is not byte order loses records to the `Stale` count, visibly but silently in the
  sense that nothing errors. Every provider from B11 on has to know it, and the normalizer contract
  now says so.
- **The A, B, A window stays open until an operator acts.** That is ADR 4's decision, not a new
  one: the dead letter and the counter are what make it visible, and the record stays at the sink
  in the scope the entity has left until the dead letter is dealt with.
- **The entity advisory lock is one statement per entity per delivery**, and it makes a second
  delivery of one entity wait rather than fail. It is cheap and it is not free.
- **The external id's byte bound is stricter than the format's**, so a record the format allows can
  be refused here. Nothing a provider mints comes near it, and the refusal is by name.
- **A degraded record's scope is a contract and not an enforcement.** The pipeline cannot compare a
  scope it only ever sees once. A provider that derives it twice, by two routes, breaks the rule
  and the symptom is a duplicate delivery that looks like a move. Every provider's tests have to
  hold the two paths to one id, as `internal/provider/fake` does.
- **The masker's patterns will both miss and over-reach.** It is a baseline, replaceable per
  architecture section 7, and its job is to be predictable rather than complete. What it misses
  reaches the sink; what it over-reaches costs a placeholder in somebody's text.
- **The redaction map is a table of personal data**, in the same database as everything else. It is
  the price of being able to resolve a placeholder at all, and the alternative (no map) would make
  masking irreversible even for the deployment that did it.
