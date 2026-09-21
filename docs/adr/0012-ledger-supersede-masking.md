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

### 1. A version is ordered by three rules, the third of which is a refusal

ADR 4 calls `version` opaque to a sink and, in the same paragraph, makes "monotonic per
`external_id`" a promise the provider's normalizer makes to the pipeline, "which B08 uses to keep
the supersede chain forward only". It never said what monotonic means for a string.

This record first said it means **byte order**, on the grounds that it is the only order an opaque
string has, and left the rest as a promise the normalizer had to keep. The round 1 review of the
pull request showed that this fails in the direction the whole item exists to prevent, so the rule
is now three rules and one of them is a refusal. `compareVersions` in `internal/pipeline` is the
whole of it:

1. **Two runs of decimal digits are ordered by the number they spell**, whatever their length, with
   leading zeros counting for nothing. An unpadded counter is the commonest spelling a real
   provider uses, and reading it as bytes gets it wrong at the tenth change in both directions.
2. **Two versions of equal length are ordered by their bytes.** Every encoding whose byte order is
   its value order is fixed width for as long as it is in use: a ULID, an epoch in milliseconds, an
   RFC 3339 timestamp in UTC, a zero-padded counter. For two equal-length runs of digits this is
   the same answer as rule 1, so the two rules can never disagree.
3. **Anything else carries no order, and the delivery is refused** (`ErrVersionNotComparable`,
   wrapping `ErrDeadLetter`, counted on `Prepared.VersionUnordered`). The head does not move,
   nothing is delivered, the error names the entity and both versions, and the fix is on the
   normalizer.

Rule 3 is why the other two are safe. **What was wrong with byte order alone** is worth writing
down, because it is the exact failure this item exists to prevent and the first version of this
record claimed it could not happen: with versions `"9"` and `"10"`, `"9" > "10"` byte-wise, so an
older record arriving after a newer one (a replayed dead letter, a backfill beside the live feed,
both named below) was **not** stale. It superseded the newer record, became the head, and the sink's
live version and the scope access is decided on both went backwards, with `Stale` at zero and no
error anywhere.

A rule that inverts unpadded decimals is not viable, because unpadded decimals are what real
providers send. Ordering by length and then by bytes was considered and rejected: it gets unpadded
decimals right and gets a fixed-width base32 counter such as a ULID wrong, which is the encoding
this record recommends. Letting the provider declare the comparison was considered and rejected for
B08: an optional interface leaves the unsafe default in place for the provider that forgets, which
is exactly the case that hurts. Rules 1 and 2 cover every encoding a normalizer should be using,
and rule 3 refuses the rest instead of guessing.

**Two records of one entity that carry the same version are ordered by arrival.** That is the case
decision 7 is about, an entity that moved while the provider's version stood still, and the second
of the two supersedes the first.

**Arrival order is not a real order, and this is the one thing it decides.** The first version of
this record said the outbox makes it one, because "one entity's versions share an ordering key".
They do not: by [ADR 11](0011-hub-resolution.md) decision 3 the ordering key is
`{provider key}:{subscription id}`, so the outbox's FIFO
([ADR 10](0010-outbox-head-marker.md)) serializes one entity only while it arrives through one
subscription. One entity reached through two subscriptions of one tenant is two keys, and a
reconciliation pass beside the live feed is a third path; those drain concurrently. The residual is
in the Cost section.

The advisory lock is about the same paths and does not close that gap: it takes a transaction-scoped
lock on (tenant, provider, `external_id`) before the chain is read, which stops the chain being
**corrupted**, not the head being decided by whoever gets there second. Without it two transactions
can both read head X, and the second can demote what the first inserted, leaving two records
superseding X and one orphaned in the middle of the chain, with no constraint violated. The locks of
one delivery are taken in sorted order, so two deliveries carrying the same entities can queue but
cannot deadlock.

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

**The residual risk, stated plainly, in both directions.** The first version of this record stated
it in one direction only, which is how the unsafe one went unnoticed for a round of review. Under
the three rules of decision 1 the two directions are:

- **A newer record held back.** A normalizer whose versions are ordered by neither rule 1 nor rule
  2, but which happen to be the same length, has the wrong record called stale. The symptom is a
  `Stale` count that is not zero while the sink stays behind. Nothing wrong is delivered and no
  access is widened, which is the safe direction.
- **A delivery refused.** A normalizer whose versions cannot be ordered at all loses the delivery
  to `ErrVersionNotComparable` rather than having it guessed at. That is loud, it is per delivery,
  and it is also the safe direction: the head does not move.

**The unsafe direction, an older record taking the head and its scope, is closed by rule 3** and
not by a promise. That is the whole reason decision 1 has a refusal in it.

### 4. A, B, and back to A: dead-lettered and counted, as ADR 4 requires

ADR 4 decision 7 states the rule and marks it *decided by default, the maintainer may overrule*.
It is implemented here as written. An incoming record whose id is already in the ledger, that is
not its entity's head, and whose scope differs from the head's scope is **dead-lettered with a
reason of its own** (`ErrScopeReturned`, wrapping `ErrDeadLetter`) **and counted**
(`Prepared.ScopeReturned`), never skipped and never delivered. The error names the tenant, the
provider, the entity, the incoming scope and the head's scope, which is what an operator needs to
look the entity up at the source; ADR 4 says what they do then.

**The count is on `Prepared` and it survives the error.** ADR 4 asks for both because they answer
different questions: the dead letter tells an operator about one delivery, and the count tells them
whether this is happening at all. So `Prepare` returns the counters it had reached beside every
error it returns, with `Records` set to nil (the caller rolls the transaction back, so nothing it
decided may reach a sink). Returning a zero `Prepared` beside an error, which is what the first
implementation did, threw away not only this count but everything the delivery had already decided
about its other records. B25 adds `ScopeReturned` to a Prometheus counter; until B25 exists the
caller logs `Prepared`, which is one struct for the whole delivery.

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
  out. Overlaps are resolved by position, so the digit groups inside an IBAN are not also read as a
  telephone number.
- **The telephone rule is asymmetric on purpose, because its two failures cost different things.**
  A miss lets a number reach the sink, which is recoverable. An over-reach replaces content with a
  placeholder, and that is not: the sink never sees the original and the value is written into
  `redaction_map`. A number with a country code needs nothing but E.164's bounds, because the `+`
  is what says it is a number. Without one, the rule is narrow: nine to fifteen digits, **exactly
  three groups** (four is an IPv4 address or a build number), **at least one group of four digits**
  (three groups of three is an address, an invoice total or a version), and a **first group that
  does not read as a year** (a date, a release, an order reference). What that gives up is a real
  number whose first group is 19xx or 20xx. The first implementation had only the digit count, and
  turned most IPv4 addresses (`192.168.100.200`, `172.217.169.110`) into telephone placeholders,
  which for a connector whose first providers are a task tracker and a chat tool is ordinary
  content destroyed.
- **One delivery may map at most `MaxSecretsPerDelivery` (1,024) distinct values.** Everything the
  masker finds becomes a permanent row, in one statement, and the text it scans comes from a
  sender: `ingress.DefaultMaxBody` and `record.MaxText` are both 1 MiB, which is tens of thousands
  of distinct addresses if somebody wants it to be (a measured 500 KB of addresses produced 20,445
  rows in one statement). Without a bound, one delivery decides how large that statement is and how
  many rows of personal data this deployment keeps forever. Crossing it is a refusal by name
  (`ErrTooManySecrets`, wrapping `ErrDeadLetter`), the way an over-long external id is, and the fix
  is on the normalizer, which decides how much of a payload becomes one record. 1,024 is far above
  content and far below abuse; retention of what is under it stays B25's.
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
  34 characters and replaces something between 6 and 21, so this is not only "a title cut to
  exactly the format's limit": any field that is more than roughly 62% short secrets by length
  outgrows the limit whatever its size, and the whole class dies here. Cutting it down again would
  lose somebody's content silently and leaving it unmasked is the one thing this stage must never
  do, so the delivery dies and the fix is on the normalizer. Realistic content is nowhere near
  that ratio.
- **A masking failure the database caused is NOT a dead letter.** `ErrMaskedTooLong` and
  `ErrTooManySecrets` are about the record, and the same bytes give the same answer on every
  attempt. A cancelled context, a lost connection or a refused write inside the redaction upsert is
  about this deployment, and classifying one of those as a dead letter would destroy a delivery the
  retry ladder would have carried. `Prepare` tests for the two by name and lets everything else
  walk the ladder.

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

- **The version order covers two spellings and refuses the rest.** A normalizer whose versions are
  neither decimal counters nor fixed width loses every second delivery of an entity to
  `ErrVersionNotComparable`, which is loud and costs a dead letter per change. That is the price of
  not guessing, and it is deliberate: the alternative is the failure the round 1 review found. A
  normalizer whose versions are the same length but not in byte order still loses records to the
  `Stale` count, visibly in the sense that a counter moves and silently in the sense that nothing
  errors. Every provider from B11 on has to know both, and the normalizer contract says so.
- **Two subscriptions of one tenant onto one entity have no order at all.** The ordering key is
  `{provider key}:{subscription id}` (ADR 11 decision 3), so the outbox's FIFO does not serialize
  one entity across two subscriptions, and a reconciliation pass beside the live feed is a third
  path. With DIFFERENT versions the rules of decision 1 still decide, so this costs nothing. With
  EQUAL versions there is nothing left but arrival, and whichever transaction takes the advisory
  lock second becomes the head and its scope is what access is decided on, whichever one is
  actually current at the source. The lock prevents a corrupted chain; it does not create an
  order. `TestTwoWritersOfOneEntityLeaveOneChain` races six equal-version moves and asserts that
  the result is one chain, deliberately not which head. Closing it needs a tiebreaker the pipeline
  can check: `occurred_at` is on every record and is already validated, and it belongs to the item
  that ships deletes, where the same question comes back.
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
  reaches the sink; what it over-reaches costs a placeholder in somebody's text, and the original
  is gone from the sink for good. That asymmetry is why the telephone rule above gives up a real
  number whose first group reads as a year, and why the "must not match" fixtures carry an IPv4
  address, an order id, a version string, a hash and two timestamps: the over-reach is the failure
  worth testing for.
- **A delivery above the secret bound dies rather than being trimmed.** 1,024 distinct values is
  a number chosen against measurements, not derived from anything, and a provider that legitimately
  batches very large payloads into one record will meet it. The refusal is by name and the fix is
  on the normalizer, which is the same answer `ErrMaskedTooLong` gives.
- **The redaction map is a table of personal data**, in the same database as everything else. It is
  the price of being able to resolve a placeholder at all, and the alternative (no map) would make
  masking irreversible even for the deployment that did it.
