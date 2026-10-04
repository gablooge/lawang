# 15. The ClickUp provider: what a delivery identifies, how a registration is shaped, and how a version is spelled

Status: accepted, 2026-10-04 (backlog item B11)

## Context

B11 builds the first real provider. Three questions had to be answered before it could be
written, and all three were left open on purpose for the item that would meet the real API:

1. [ADR 11](0011-hub-resolution.md) decision 5 left "at most one subscription row per (provider,
   workspace) per tenant" as a requirement on a registration design nobody had written yet, and
   left open whether the `subscriptions` table should enforce it with a unique index: "B11 and
   B14 to settle".
2. [ADR 4](0004-record-format-v1.md) decision 7 requires a normalizer to move `version` on every
   change the provider reports, **a move between containers included**, and says ClickUp's
   `date_updated` is the candidate without saying whether it moves on a move.
3. B09's `provider.VersionOrder` (ADR 13 and the registry) requires every provider to declare how
   it spells a version, and refuses to start without one.

**Everything below was checked against ClickUp's published documentation, not against a live
workspace.** B11 is not a "(needs you)" item: it had no account, so every fixture under
`internal/provider/clickup/testdata` is synthesized from the documentation and B12 replaces them
with real recordings. Where a decision rests on something only a live account can confirm, it
says so and is built so that the answer does not change the outcome.

## Decision

### 1. A ClickUp delivery identifies the webhook, and nothing else

[Task webhook payloads](https://developer.clickup.com/docs/webhooktaskpayloads) shows the whole
body of every task event, and the identifiers on it are `webhook_id`, `task_id` and, for events
about other objects, `list_id`, `folder_id`, `space_id` or `goal_id`. **There is no workspace or
team id at the top level of any of them.** (A `team_id` appears inside custom-field metadata,
which is not an identifier of the delivery.)

So `DeliveryKeys` returns the webhook id as `Subscription` and leaves `Workspace` empty, which is
what that field's contract asks for ("as it will appear on a delivery, or empty for a provider
that sends none").

**ADR 11 decision 4 says the opposite in passing** ("a ClickUp webhook names a workspace and a
Microsoft Graph notification names the subscription"), and that sentence is corrected there
rather than left standing. Nothing else in ADR 11 depended on it: the candidate lookup already
unions over the keys a delivery actually carries.

### 2. One subscription row per workspace per tenant, keyed by the resource

A ClickUp subscription row is:

| Field | Value | Why |
|---|---|---|
| `Resource` | the workspace (team) id | `hub.Subscriptions.Register` upserts on (tenant, provider, resource), so this is what makes a second registration replace the first rather than add a row. |
| `External` | the ClickUp webhook id | the only key a delivery carries, so a row without it is selected by nothing. |
| `Workspace` | empty | a delivery carries none, so a value here would be a key nothing is looked up by. |

`clickup.SubscriptionFor` is the only way to build one, and B14's registrar uses it rather than
filling a `provider.Subscription` by hand. The consequence for the registrar: **Lawang creates
one ClickUp webhook per workspace per tenant, at workspace level, with no list, folder or space
filter**, and deregisters the webhook it replaces.

This satisfies ADR 11 decision 5 by construction. Two rows of one tenant on one workspace cannot
exist, because the second registration updates the first.

**ADR 11 decision 5's worked example cannot happen for ClickUp**, which is worth saying because
it is not what that ADR predicted. The example was "one subscription row per list, each carrying
the workspace id and that one secret, produces two or more verifying rows on the first delivery".
That needs a delivery that carries a workspace id, and decision 1 above says ClickUp sends none:
the candidate lookup selects by webhook id. **The failure that shape really produces is the
opposite one**: rows that record a workspace and no webhook id are selected by nothing, and every
delivery is parked as "no owner", for ever and just as silently.
`TestASubscriptionWithoutTheWebhookIDIsNeverFound` is that failure, written down.

**The park itself is still reachable, by another route**, and decision 3 is what closes it. Two
rows of ONE tenant on TWO workspaces carrying ONE webhook id are both candidates for a delivery
that names it, both verify (one webhook has one secret), and the hub parks. Nothing about a
webhook id makes it unique per row on its own: `subscriptions_by_external` is a plain index and
the upsert key is (tenant, provider, resource), so the two rows cover two resources and the
upsert does not reach them. A registrar that recreated a webhook before the row it replaced was
written, or that was handed the wrong workspace id, produces exactly that.

### 3. A unique index on the registration id, per tenant

ADR 11 decision 5 left open whether the `subscriptions` table should enforce the registration
rule. It stays open no longer: **migration 00006 adds
`UNIQUE (tenant_id, provider, external_id) WHERE external_id <> ''`.**

**The column is the registration id and not the workspace, which is the whole of why this is
safe.** The index ADR 11 was imagining, on (tenant, provider, workspace), would be wrong for the
table: Microsoft Graph registers one subscription per resource with a secret each, several rows
of one tenant on one workspace are correct there (ADR 11 decision 5 says as much, "two rows of
one tenant on one workspace with different secrets resolve correctly and are fine"), and an index
on the workspace column would move the failure from ClickUp, where it is designed out, to Graph,
where the design needs the rows. The index that is here does not touch that case: each of those
Graph rows carries its own Graph subscription id in `external_id`, so none of them collides with
another.

**What it encodes is something the hub already requires of every provider**, which is why it
belongs to the table and not to ClickUp. The accept path selects candidates by the registration
id. Two rows of one tenant carrying one are therefore two candidates for every delivery that
names it, and since a registration has one secret, both verify and the delivery is parked as an
ambiguous owner, permanently, until a human removes the collision. A unique index turns that into
an error the registrar sees the moment it writes the row.

**Per tenant, not globally.** `UNIQUE (provider, external_id)` was measured and is not viable: it
fails nine tests of the hub suite, `TestTwoTenantsOnOneWorkspaceWithDifferentSecretsRouteToTheOwner`
and `TestTwoTenantsWhoseSecretsBothVerifyAreParkedNeverRouted` among them, every one of which is
designed cross-tenant behaviour. A provider's id space is not this deployment's to make unique across
accounts, and the hub already decides two tenants' rows on their secrets.

**Partial, because a row may legitimately carry no registration id at all.** A provider that
sends none leaves the column empty (migration 00003's CHECK allows it as long as the workspace id
is there), and without the predicate the second such row of a tenant would collide with the
first. The reason migration 00003's lookup indexes are *not* partial is about a generic plan not
being able to prove a parameter is non-empty, which is a statement about lookups and says nothing
about a constraint checked against the row's own value.

Three tests hold it: `TestOneWorkspaceIsOneSubscriptionRow` (the upsert key),
`TestTwoWorkspacesOfOneTenantCannotShareAWebhookID` (this index, and that the surviving delivery
still resolves) and `TestOneRegistrationIDIsOneRowPerTenant` in `internal/hub` (the rule as the
table's, with the empty-key case the predicate exists for).

One hub test changed with it: `TestMoreCandidatesThanTheHubWillVerifyAreParked` built its three
colliding candidates as rows of one tenant, and they are now one row per tenant, which is the
shape that bound exists for anyway.

### 4. `VersionOrder` is decimal, and the version is the later of two epoch milliseconds

Every time ClickUp reports is **epoch milliseconds as a decimal string**: a task's `date_updated`
([Get Task](https://developer.clickup.com/reference/gettask)), a comment's `date`
([Get Task Comments](https://developer.clickup.com/reference/gettaskcomments)), and a history
item's `date`. So the declaration is `provider.VersionOrderDecimal`, and the pipeline proves it
rather than trusting it: a version that is not a digit run is a dead letter.

Lexical was the alternative and it is wrong, quietly: epoch milliseconds are 13 digits now and
were 12 until 2001, so two versions of one entity are the same width in practice and the
fixed-width promise would hold until it did not. Decimal is also the only one of the three orders
the pipeline can check for itself.

**The version is `max(the entity's own timestamp, the delivery's own event time)` for the entity
a change is about.** The event time is the newest `history_items[].date` of the delivery.

A history item whose `date` cannot be read is **skipped, and never refuses the delivery**. The
dates contribute the event time, which is an optimisation and not identity, and a delivery with
no history at all is already accepted and falls back to `date_updated`, so a hard refusal would
buy nothing the lenient path does not already give and would dead-letter every delivery of a
workspace, permanently, the day ClickUp ships a history item type whose date is absent, null or
a JSON number. That is the same argument the lenient decoder rests on (unknown fields are
ignored, for the same reason), applied to a value rather than to a field name. The fields
identity is built from, `event`, `webhook_id`, `task_id` and a comment event's comment id, stay
strictly refused.

The reason is ADR 4 decision 7. A task that moves to another list is decided on a different scope,
and the record id hashes the scope, so the move produces a new id whatever the version does. What
the id cannot fix is the A to B and **back to A** case: if `date_updated` does not move on a move,
the task coming back to its first list reproduces an id the ledger has already seen, and the
ledger can only dead-letter it (`pipeline.ErrScopeReturned`). Whether ClickUp moves `date_updated`
on a move is not something the documentation says, and B11 could not try it.

So the normalizer does not depend on the answer. A move arrives as a `taskUpdated` delivery whose
history item is the move, and that item's `date` is the moment of the move, which always moves.
`TestAMoveAndAMoveBackMoveTheVersion` runs the three deliveries with one unchanged `date_updated`
and requires three distinct versions and three distinct ids.

**The parent task re-hydrated beside a comment is the exception**, and it keeps its own
`date_updated`. It is not the subject of that change, so a task nobody edited mints the id it
already has and the ledger skips it, instead of being re-delivered once per comment.

That exception has a cost, and it is reproducible rather than hypothetical. With `date_updated`
unchanged, a comment posted after a move yields the parent task in the NEW scope at the OLD
version, so an A to B and back to A sequence driven only by comment deliveries reproduces a
record id the ledger has already seen, the ledger skips it, and the sink's last word is scope B
while the task is in A:

```
before: scope=clickup:list:901100 version=1791100000000
after:  scope=clickup:list:901200 version=1791100000000
```

What repairs it is that a move always also arrives as its own `taskUpdated`, whose history item
is the move, which moves the version and mints the right id. That is one documented behaviour
deep, so it is on `testdata/README.md`'s list of what B12's recording must confirm. The
alternative, giving the parent task the comment's time, re-delivers every task once per comment
on it, which is a cost paid on every delivery rather than on a move back.

### 5. ClickUp is not a `Degrader`

A webhook body does not carry the list the task is in. (`history_items[].parent_id` is documented
as the list for `taskCreated`, and as "a reference to a parent task, if any" in general, which is
not a rule to build a scope on.) The list is what the scope is made of, and a degraded record
that guessed a scope would give one version of one entity a second id, which is exactly what ADR
4 decision 7 forbids. So there is no degraded path: a hydration failure waits for ClickUp's API
and the retry ladder parks it if it never comes back, which is what `provider.Degrader` says to
do when the body does not carry the scope.

### 6. The events Lawang registers, and the events it will parse

`clickup.Events()` is what the registrar asks for: `taskCreated`, `taskUpdated`,
`taskCommentPosted`, `taskCommentUpdated`. ClickUp sends `taskUpdated` alongside most other task
events, `taskMoved` included, so the narrow set keeps one change from arriving three times.

`Parse` accepts a wider set (every task event, so that a workspace registered by hand still
works) and **refuses everything else as a dead letter**, `taskDeleted` above all: v0.1 produces no
tombstone (ADR 4 decision 5), and a deleted task cannot be hydrated, so there is nothing honest to
build from one. A deletion is therefore not ingested in v0.1, and the item that ships deletions
has to add it here.

### 7. Cleaning, and where it lives

The format refuses and never repairs, and the other half of that rule landed with this item:
`record.CleanDisplay`, `record.CleanTitle` and `record.CleanText`, one per cleanable field class,
each bounded to its field's limit and each holding U+FFFD in place of bytes that are not UTF-8.
There is deliberately **no cleaner for an identifier**: removing a character from an id makes it a
different id, and a sender-controlled identifier needs an injective encoding or a hash of its own
(the correction on #16).

`internal/provider/providertest` is the conformance harness the round 2 review of #43 asked for:
a set of values real sources really send, which every normalizer must turn into records that
`record.Seal` accepts. A provider that forgets to clean fails its own tests. The harness checks
more than "Seal returned no error", because that alone would pass a normalizer that dropped the
fields: the cleaned values have to be in the records.

What cleaning does to a title, which ADR 4 decision 9 left to the normalizer: **every character
the one-line rule refuses becomes a space**, and the result is trimmed, so a line break is a word
boundary and a title that was only whitespace is empty.

## Consequences

- B12 replaces every fixture with a real recording and re-runs the golden tests. Seven things it
  can confirm or contradict are listed in `internal/provider/clickup/testdata/README.md`.
- B14's registrar builds its rows with `clickup.SubscriptionFor` and must deregister the webhook
  it replaces, or ClickUp goes on posting for a webhook id no row records. A registration that
  would collide with another of the tenant's rows on the webhook id is now refused by the table
  (decision 3), so the registrar sees an error rather than a workspace that receives nothing.
- **Nothing checks that a hydrated task belongs to the workspace the subscription covers**, and
  it belongs to B14 rather than here. The Get Task answer carries `team_id`, `Hydrate` is not
  given the subscription, and a delivery therefore hydrates whatever task id it names, including
  a task in another workspace the tenant's own token can see. It is the one place the "this
  registration covers workspace W" promise is not checked against what is ingested. It is not a
  cross-tenant hole (the token and the row are one tenant's), which is why it waits.
- Nothing registers the provider in `cmd/lawang` yet. Hydration needs a per-tenant API token and
  the vault is B13, so the registry is still built empty and `/ingress/clickup` is still a 404.
- A ClickUp comment that is edited long after it was posted may not be in the pages the hydrator
  reads (the newest 25 per request, at most four requests). It is `ErrNotFound`, which walks the
  ladder and parks: visible, and never the wrong comment's text.
- The documentation says a webhook's secret is "unique to the webhook" and returned when the
  webhook is created, where ADR 11 said ClickUp issues one per workspace. Nothing here depends on
  which is true, because one row per workspace holds either way, but B12 can settle it.
