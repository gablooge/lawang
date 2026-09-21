# 11. Resolving a delivery's owner: where the verification secret lives, and what an unowned delivery becomes

Status: accepted, 2026-09-21 (backlog item B07)

## Context

The accept path has to turn bytes a stranger sent into one tenant, or into a refusal. Principle 2
says the tenant comes from a row Lawang owns and never from the payload, and that a delivery more
than one tenant could claim is refused rather than routed. Three questions had to be answered
before that could be built, and none of them was settled anywhere.

## Decision

### 1. The webhook verification secret is a column of the subscription row, not a vault entry

Architecture section 7 says raw tokens never reach a plain table, and the `Vault` interface is how
a provider credential is fetched: `Fetch(ctx, tenant, provider)`.

**That interface cannot serve the accept path, by construction.** It is keyed by tenant, and the
accept path's whole job is to work out which tenant this is. Using it would mean fetching every
tenant's credential for the provider and trying each, which is the same cross-tenant read with more
moving parts, more network calls per delivery, and a vault that has to be reachable before a
delivery can even be identified.

So the subscriptions table holds a `secret` column, read by `lawang_resolver` on the accept path
and by nothing else. Two things keep that from being a hole in "raw tokens never reach a plain
table":

- **It is a different credential.** The verification secret proves that a delivery came from the
  provider. It authenticates nothing to the provider, it cannot read anything, and it cannot post
  anything. The API tokens that hydration (B11) and registration (B14) use are the vault's, and
  they never go in this table.
- **The grant is the narrowest that works.** `lawang_resolver` is granted `SELECT` on exactly the
  six columns the two candidate queries touch, on one table, with no write of any kind. It is
  entered with `SET LOCAL ROLE` for one transaction that resolves an owner and does nothing else.

The cost is that the secret is in a plain column, so a database backup carries it and an operator
with read access to that table can read it. When the vault lands (B13), encrypting this column with
the vault's key is a change to one query and one function, because `hub.candidates` is the only
reader. The interface it would need is different from `Vault.Fetch`, since it must work before the
tenant is known.

### 2. A delivery nobody can be shown to own is parked under a sentinel tenant, `_parked`

`tenancy.Sentinel` is `_parked`, a tenant id that no tenant may have: the `tenants` table refuses
it with a CHECK, so no operator credential is ever issued for it and nobody can be handed every
stranger's parked delivery. Nothing about parking takes a tenant from its caller: `outbox.Park`
has no tenant parameter at all, which is what makes "a crafted delivery cannot write into a real
tenant" a property of the signature rather than of the code inside it.

A parked row is inserted `dead`, finished, and not the head of its ordering key, so the drain can
never claim it, and its ordering key is its own delivery id, so a flood of unattributable
deliveries queues behind nothing. Why it was parked is `dead_reason`, one of five fixed texts
(`outbox.ParkReason`), because B25 re-resolves rows by that column and an operator reads it:

| Reason | What happened |
|---|---|
| no owner | nothing is registered for the delivery's keys |
| ambiguous owner | more than one tenant's secret verified it, or it selected more candidates than the hub will verify |
| unreadable delivery | the provider could not read its own keys out of it, or the keys cannot identify anything |
| the provider's verification panicked | `Verify` broke its contract, so no candidate's answer can be trusted |
| the delivery cannot be stored | poison: an owner was resolved and the row still cannot be written |

The first two are the ones a subscription registered later can settle, which is what B25 re-runs.

### 3. An accepted delivery's ordering key is the subscription it arrived on

The outbox orders by an ordering key, and the guarantee is that two versions of one entity are
never in flight together. The hub cannot name an entity: that needs `Parse`, which runs in the
worker on verified bytes, and one delivery can carry several entities anyway.

The finest grouping the accept path can name is the registration the delivery arrived on, so the
key is `{provider key}:{subscription id}`, both of them this program's own strings. It is coarser
than one entity, which keeps the guarantee (a coarser queue serializes strictly more), and it
costs parallelism: one subscription's deliveries drain one at a time. For ClickUp, which registers
one webhook per workspace, that is one queue per workspace per tenant.

If that becomes the limit, the way out is for a provider to name its entity cheaply from bytes it
has already looked at, the way `DeliveryKeys` does, and for the hub to append it. That is a change
to one function and one interface, and it belongs with the pipeline (B08) and the first real
provider (B11), where there is something to measure.

## Consequences

- The accept path needs one cross-tenant read and one tenant-scoped write, in two transactions,
  and no vault call and no provider call at all. It holds one pooled connection per transaction
  and nothing across network I/O.
- A delivery costs at most `hub.DefaultMaxCandidates` (32) HMAC computations, which is about 32 ms
  at the 1 MiB body cap against a 200 ms target. More candidates than that is parked as an
  ambiguous owner, never truncated: a candidate set cut short could hide the second tenant that
  verifies and route what should have been parked.
- An operator who rotates a signing secret re-registers the subscription, which updates the row in
  place (architecture section 5) and takes effect on the next delivery.
- B13 has a second thing to encrypt, and B25 has a column to sweep on.
