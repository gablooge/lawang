# 3. The scope id: `{provider}:{container_kind}:{container_id}`, escaped, canonical, without the tenant

Status: accepted, 2026-09-19 (backlog item B05)

## Decision

A scope id names the container that access to a record is decided on: a channel, a DM, a list, a
mailbox, a portal. It has exactly three segments and exactly two colons:

```text
{provider}:{container_kind}:{container_id}

slack:channel:C0GENERAL
clickup:list:901100
outlook:mailbox:ben%40example.com
teams:channel:19%3Aabc123%40thread.tacv2
```

**It is a join key between two messages.** The record carries it in `visibility.scope`. The
membership that access sync pushes for the same container (architecture section 3.4) carries the
same string. A person may see a record exactly when the two strings are equal, byte for byte, and
the person is a member. Everything below follows from that: the string has to come out the same
on both sides, every time, for as long as the record is stored. So both sides build it with one
function, `record.ScopeID`, from the same three parts, and no provider writes one by hand.
`record.ParseScopeID` takes one apart again.

The membership message itself, and what identifies a person in it, are not decided here. They are
decided with B23 and B24 ([ADR 4](0004-record-format-v1.md) says why). Nothing in this grammar
stands in their way: a membership message needs the tenant (which travels beside it, as it does
for records), this string, and person identifiers.

### Grammar

| Segment | Rule |
|---|---|
| `provider` | The **internal provider key** (`Provider.Key()`): a lowercase letter, then up to 31 of `a-z 0-9 _`. |
| `container_kind` | The provider's word for the kind of container, same rule. An open set: each provider defines its own (`channel`, `dm`, `list`, `mailbox`, `portal`). |
| `container_id` | The provider's id of the container, **byte for byte**: never trimmed, case-folded or normalized. The characters `A-Z a-z 0-9 . _ ~ -` (RFC 3986 "unreserved") are kept as they are. Every other byte is written as `%XX` with **uppercase** hex. At least one byte. |

The whole string is ASCII, **case-sensitive**, and at most **512 bytes**. As a regular expression
(this is the pattern in the schema):

```text
^[a-z][a-z0-9_]{0,31}:[a-z][a-z0-9_]{0,31}:(?:[A-Za-z0-9._~-]|%(?:2[0-9A-CF]|3[A-F]|40|5[B-E]|60|7[B-D]|[89A-F][0-9A-F]))+$
```

Refused, by `ScopeID`, by `ParseScopeID`, by `Record.Validate` and by the schema alike:

- a **control character** in the container id (`0x00` to `0x1F`, `0x7F`), raw or escaped. No
  provider id has one, and they are what log forging, NUL truncation and the `0x1F` separator of
  the id recipes are made of. Such an id is a refusal, not something to encode around.
- an **escape of a character that needs none** (`%41` for `A`), and **lowercase hex** (`%3a`).
- anything else outside the alphabet: a raw `:`, `/`, `@`, space, or non-ASCII character.

### One scope, one spelling

The escaping rule has no choices in it: a byte is either in the unreserved set and written as
itself, or it is not and is written as `%XX` in uppercase. Together with the three refusals
above, that makes the form **canonical**: two different strings never name the same scope, and a
string never parses two ways. `ParseScopeID` accepts exactly the strings `ScopeID` produces, so
`ScopeID(ParseScopeID(s)) == s` for every `s` that parses (a fuzz test holds this).

This matters because a sink compares scope ids as opaque strings. If `a%3Ab` and `a%3ab` were
both valid, a record stamped with one and a membership pushed with the other would never meet,
and everybody would lose access with no error anywhere (fail closed, but broken). With two
spellings of one scope, the opposite mistake is also possible in a sink that normalizes one side
and not the other.

**Why escape at all, when the container id is the last segment and could simply run to the end
of the string?** Slack and ClickUp ids are plain alphanumerics, but Microsoft Graph ids contain
almost anything: a Teams channel is `19:abc123@thread.tacv2`, and mail and folder ids are base64
with `/`, `+` and `=`. Left raw, the string would still parse (split on the first two colons),
but it would hold characters that need quoting in a URL path, a query string, a log line, a CSV
file and a shell, and every one of those is a place a scope id ends up at a sink. Escaped, a
scope id is safe in all of them as it stands, has exactly two colons, and no Unicode
normalization or look-alike character can make two scopes look the same. The cost is length:
each escaped byte takes three.

The container id is treated as bytes. Non-ASCII ids are escaped byte by byte, and bytes that are
not valid UTF-8 are carried like any other, because an id is opaque and a scope id must never be
the reason a record cannot be delivered.

### The provider segment is the internal key, not the wire name

Principle 4 of the architecture says what a sink calls a source is sink configuration, and that
ids hash the internal key so they survive a renamed wire name. The scope id follows the same
rule, for a stronger reason:

- The scope id is computed once, when the provider normalizes a change and when its member source
  lists a container. Neither knows which sinks exist or what they call the source.
- Access sync has to produce the **same** scope id for the membership it pushes as the records
  carry. With the wire name inside, the string would differ per sink, the local record of "what
  was last sent" (B24) would have to be kept per sink spelling, and **renaming a source at a
  sink would cut every record already stored there off from its members**: the stored records
  keep the old string, the memberships arrive under the new one.
- The record id hashes the scope (ADR 4), so a scope id that followed the wire name would re-key
  records on a rename, which is exactly what principle 4 exists to prevent.

So a record delivered to a sink that calls Slack `chat` has `"source": "chat"` and
`"scope": "slack:channel:C0GENERAL"`. The first segment of a scope id is not the record's
`source`, and a sink must not expect them to match. **To a sink a scope id is opaque: it compares
for equality and never parses.** Parsing is for Sluiceway, which needs the container back (access
sync, diagnostics).

`Record.Seal` refuses a scope whose first segment is not the sealing provider's key, so one
provider cannot stamp a record into another provider's scope.

### The tenant is not part of it

The scope id names a container at the provider. It does not say whose data it is. Two tenants
that connect the same Slack workspace (architecture section 5) get the same scope id for
`#general`, deliberately:

- The tenant is established by trust, not by content: for a sink it comes from the per-tenant
  sink credential (architecture section 4), and it travels beside the records and the
  memberships (`Sink.Deliver(ctx, tenant, records)`). A tenant inside a payload string would be
  a second source of truth that could disagree with the first, which is how data crosses tenants
  (principle 2).
- Tenant ids are Sluiceway's internal names. They have no business in a string that is stored in
  every record and every index entry at the sink.
- Where the tenant has to make two things differ, it already does: it is hashed into the record
  id (principle 3).

The consequence is a rule for sinks, stated in the schema: **key memberships and records by
tenant and scope together.** A sink that serves several tenants and keys by scope id alone would
let tenant B's members of `slack:channel:C0GENERAL` see tenant A's records.

## Why

- Readable and deterministic, as the roadmap leaned: an operator can read a scope id in a log and
  know which channel it is.
- One function on both sides of the join is the only way the two sides stay equal. A format that
  each provider assembled with `fmt.Sprintf` would hold until the first id with a colon in it.
- Percent-encoding is a rule every language already implements, and the canonical form (one fixed
  unreserved set, uppercase hex) is the normalized form RFC 3986 section 6.2.2 recommends, so a
  sink author who does need to take one apart needs no Sluiceway code.
- Refusing instead of normalizing (lowercase hex, redundant escapes) keeps the parser from being
  more generous than the builder. A parser that accepted and canonicalized would make "equal as
  strings" and "equal as scopes" two different relations again.

## Cost

- A scope id is up to three times as long as the provider's id. In the worst case, an id made
  only of bytes that need escaping, about 160 bytes fit in the 512. Base64 ids are mostly letters
  and digits, so a Graph id grows by a few bytes, not threefold. If an id ever does not fit, the
  record is refused with a clear error, which is the right failure for an access key. The limit
  was not measured against real Graph tenants: B16 and B17 should check it against recorded ids.
- The `provider` and `container_kind` grammar has no hyphen. Provider keys are ours to choose, so
  this costs nothing today.
- A scope is one container. A record visible through two containers at once (a file shared into
  two channels) is not expressible as one scope id. v1 does not try: such a provider has to pick
  the scope that matches where the record lives, or emit the record once per scope under
  different external ids. If that turns out to matter, it is a change inside `visibility`, and
  so a new format version (ADR 4).
- The provider key is visible to sinks inside the scope id even when the wire name hides it.
  That is a name like `slack`, not a secret.
