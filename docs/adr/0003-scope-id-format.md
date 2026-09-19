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
`ScopeID(ParseScopeID(s)) == s` for every `s` that parses (a fuzz test holds this). The escape
space is small enough to test in full, and it is: all 256 bytes as `%XX`, in uppercase, in
lowercase and in both mixtures, go through the Go parser **and** through the schema's pattern,
and each must give the verdict the rule above gives (157 accepted: 256 bytes, less the 66
unreserved ones, less the 33 control bytes, and only in uppercase). The same is done for every
character in the first and in a later position of the `provider` and `container_kind` segments.
Examples alone did not hold this: the parser and the pattern could drift together on a byte no
example named (`%61`), and an agreement test then sees two halves that agree.

This matters because a sink compares scope ids as opaque strings. If `a%3Ab` and `a%3ab` were
both valid, a record stamped with one and a membership pushed with the other would never meet,
and everybody would lose access with no error anywhere (fail closed, but broken). With two
spellings of one scope, the opposite mistake is also possible in a sink that normalizes one side
and not the other.

**Why escape at all, when the container id is the last segment and could simply run to the end
of the string?** Slack and ClickUp ids are plain alphanumerics, but Microsoft Graph ids contain
almost anything: a Teams channel is `19:abc123@thread.tacv2`, and mail and folder ids are base64
with `/`, `+` and `=`. Left raw, the string would still parse (split on the first two colons),
but it would hold spaces, quotes, commas, non-ASCII characters and whatever else a provider puts
into an id, and a scope id ends up in log lines, database columns, CSV files and metric labels at
a sink. Escaped, it is printable ASCII from a small alphabet, has exactly two colons, and no
Unicode normalization or look-alike character can make two scopes look the same. The cost is
length: each escaped byte takes three. What the escaping does **not** buy is safety inside a
URL, which the next section is about.

### Where a scope id is safe as it stands, and where it is not

**Safe as it stands:** a JSON string, a database text column, a log line, a CSV field, a metric
label, a word of a POSIX shell. Its alphabet is `A-Z a-z 0-9 . _ ~ - % :`, which none of those
gives a meaning to.

**Not safe as it stands: a URL path or a query string.** A scope id holds percent signs, and
every HTTP server decodes a path and a query **once**. A client that splices
`teams:channel:19%3Aabc%40thread.tacv2` into `GET /scopes/{scope}/members` reaches a handler
that sees `teams:channel:19:abc@thread.tacv2` (in Go, `r.URL.Path` and `r.PathValue`), a string
that equals no stored scope id. Every lookup misses: fail closed, and broken, which is the
"never meet" failure this ADR exists to prevent, by another road. With an escaped slash it is
worse. `..%2F..%2Fadmin` is a valid escaped container id, and a proxy or a router that decodes
`%2F` turns it into path segments.

So **in a URL a scope id is percent-encoded again, as one opaque value, like any other string
that holds a percent sign** (`%3A` becomes `%253A`; `url.PathEscape` and `url.QueryEscape` in
Go, `encodeURIComponent` in JavaScript), it is never spliced into a path by hand, and the
receiving side compares after **exactly one** decoding, which is the one its HTTP library has
already done. `TestAScopeIDInAURLIsEncodedOnceMore` documents the round trip against a real
server, both ways: encoded once more the scope id arrives byte for byte, as it stands it
arrives as something else.

**Not safe as it stands either:**

- **a file path.** It holds colons, which Windows and classic macOS refuse, and `..%2F` becomes
  `../` in any layer that decodes. A sink that needs a file per scope names it by a hash of the
  scope id.
- **where a percent sign means something:** a Windows batch file (`%3A` there starts a
  parameter expansion), a crontab line (where `%` is a line break), a `printf` format string.
  Pass it as data, never as a format or a script.

The schema's description of `scopeId` says the same in short, because that is what a sink
author reads.

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
  was not measured against real Graph tenants. B16 and B17 must check it against recorded ids,
  **and they come before v0.1.0 for that reason**: after the release a longer scope id is a
  wider pattern, and by [ADR 4](0004-record-format-v1.md) (decision 1) that is a new format
  version.
- The `provider` and `container_kind` grammar has no hyphen. Provider keys and container kinds
  are ours to choose, so living without one costs nothing. **Adding one later is not free:** it
  widens the pattern of the scope id (and of `external_id` and `container.kind`), a sink that
  validates with the earlier schema would refuse every such record, and so it is a new format
  version (ADR 4, decision 1). The grammar of this ADR is final when v0.1.0 ships, in both
  directions.
- A scope id has to be encoded once more wherever it enters a URL, and sink authors will forget.
  The schema says so where they read it, and a test documents the round trip.
- A scope is one container. A record visible through two containers at once (a file shared into
  two channels) is not expressible as one scope id. v1 does not try: such a provider has to pick
  the scope that matches where the record lives, or emit the record once per scope under
  different external ids. If that turns out to matter, it is a change inside `visibility`, and
  so a new format version (ADR 4).
- The provider key is visible to sinks inside the scope id even when the wire name hides it.
  That is a name like `slack`, not a secret.
