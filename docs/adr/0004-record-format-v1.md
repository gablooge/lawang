# 4. The record format, v1: field names, what is required, and how it may change

Status: accepted, 2026-09-19 (backlog item B05)

## Decision

The envelope a sink receives is format **v1**, as shown in architecture section 6. The contract
is the JSON Schema `internal/record/record.v1.schema.json` (draft 2020-12), embedded in the
binary by `internal/record`, whose Go types produce and read it. It becomes a public contract
when v0.1.0 ships. Until then this record can still be superseded cheaply; afterwards every
change below the line "what forces a new version" costs every sink author a migration.

Ten things are decided here that the proposal left open or did not have (8 to 10 were added in
review, because each of them is something that cannot be tightened or redefined after v0.1.0):

1. The format carries its own version, in a `format` field.
2. The field names of the proposal are kept, with one field dropped (`meta.raw_ref`) and one
   added (`format`).
3. Every field that carries meaning is always present. "None" is `null` or `""`, never absence.
4. Unknown fields are allowed everywhere except inside `visibility`.
5. `op: "delete"` is part of v1 now, with its shape defined, although v0.1 never sends one.
6. A record carries its scope and never the scope's members.
7. **The scope is part of the record id**, so a record that moves to another scope is a new
   record. This changes the id recipe of architecture section 5 and `internal/ids`.
8. A record document is UTF-8 and escapes no half of a surrogate pair.
9. Which characters each string field may hold, field by field.
10. What `origin.untrusted: false` and `origin.automation: false` mean: no signal, never "safe".

### 1. The format version

```json
"format": "sluiceway.record/v1"
```

A required field with one allowed value. A sink checks it first and refuses anything else.

The alternatives were a media type (`application/vnd.sluiceway.record.v1+json`) and the schema's
`$id`. Both describe a transport or a file, and a record outlives both: it sits in a JSONL file,
a queue, a vector store's metadata column. The field is the only version that is still attached
to the record wherever it ends up. The `http` sink (B09) may advertise a media type as well. The
field stays the authority.

The schema's `$id` is `https://gablooge.github.io/sluiceway/schema/record/v1.json`. It is a
name, not a promise that the URL resolves: take the schema from the repository. It was chosen
under a host name the maintainer controls, so that nobody else can ever serve a different schema
at the address a validator might fetch. **The maintainer should confirm or replace it before
v0.1.0**, because it freezes with the release.

**The promise, in the reader's terms: within v1, every record Sluiceway produces validates
against every earlier v1 schema.** A sink author takes the schema file once, validates with it,
sizes columns by its limits, and keeps working for as long as the records say
`sluiceway.record/v1`. Sluiceway is the only writer, so the promise is about what it writes, and
everything below is derived from it.

**What may change within v1:** fields that a reader may safely ignore are added, at the top
level and inside `author`, `container`, `origin`, `edges` and `meta`. An earlier schema allows
additional properties there, so it still accepts the record, and a sink written against it
ignores what it does not know. A new field comes with its own limits, and those freeze with the
release that adds it.

**What forces a new version** (`sluiceway.record/v2`, a new schema file, a new `$id`) is
whatever could make an earlier v1 schema refuse a record, or a sink built on it misread one:

- a field removed or renamed, or a required field made optional or nullable: the earlier schema
  demands it;
- a field changed in type;
- **a limit raised, or a pattern or a character set widened.** `text` allowed to reach 2,097,152
  characters, a hyphen allowed in a provider key or a container kind, a character taken off the
  lists of decision 9, a new spelling of `occurred_at`, a longer scope id: every sink that
  validates refuses the new records, and one that sized a column by the documented maximum
  overflows. This is the direction that is easy to mistake for harmless, because it breaks no
  producer, and the only producer is Sluiceway;
- a new value of `op`, `kind` or `visibility.audience`. These are closed sets, and a sink that
  validates would refuse the new value, so adding one is not additive;
- **anything at all inside `visibility`.** A sink that ignored a new field there (a deny list,
  say) would grant access it should not. That is why `visibility` is the one closed object: an
  unknown field in it is a reason to refuse the record, not to ignore the field;
- **a changed meaning**, which no schema sees and which the promise therefore names on its own:
  what `origin.untrusted: false` says, what an `external_id` is unique within, what `delete`
  asks of a sink, the recipe behind `id`.

**The schema file's constraints on what exists do not move in the other direction either.** A
limit lowered or a pattern narrowed would keep the promise above (new records still pass every
earlier schema), but records a sink has already stored would stop validating against the newer
file, and two v1 schema files that disagree about one record are a trap. If Sluiceway ever needs
to produce less than the schema allows (shorter texts, say), it produces less and the schema
stays as it is. So within v1 the schema changes by added optional properties and by wording,
and by nothing else, and every limit and pattern in it is final on the day v0.1.0 ships.
[ADR 3](0003-scope-id-format.md) says the same of the scope id grammar.

**Respelling a pattern without changing the set of strings it matches is wording, not a change
to the format.** What is frozen is that set, not the characters the pattern is written with. It
counts as wording only with an exhaustive proof: the old and the new spelling give the same
verdict for every code point there is, in every dialect the schema claims (decision 9,
"Portability of the patterns"). That has happened once, before v0.1.0: the C1 controls and the
soft hyphen were first written as hex escapes, which Ruby's engine cannot compile, and are now
in the pattern as themselves. The two spellings were compared over all 1,112,064 code points in
Go, in Python and in ECMAScript with and without the `u` flag, with no disagreement, and a test
refuses the old spelling.

Field names are lowercase `a-z 0-9 _`, starting with a letter, now and in every addition, and a
record with any other field name is refused by the schema (`propertyNames`) and by the Go
decoder. Some JSON decoders match names without regard to case. Go's `encoding/json` does, and
it also folds the Kelvin sign and the long s onto `k` and `s`. To such a decoder `"ID"` beside
`"id"` is one field, whose value is whichever came last, while a schema validator sees `id` and
an unknown extra. With lowercase-only names there is no second spelling to smuggle.

### 2. The fields, one by one

| Field in the proposal | Decision | Why |
|---|---|---|
| (none) | **added:** `format` | See above. |
| `id` | keep | `rec_` and 32 lowercase hex. The recipe changes, see decision 7. |
| `op` | keep, `delete` defined now | See decision 5. |
| `source` | keep | The wire name, which is sink configuration (principle 4). Lowercase letter, then up to 63 of `a-z 0-9 _ -`. It is in no id and is not the first segment of the scope (ADR 3), so it is for display and filtering only. `Record.Seal` sets it to the provider key, and a sink with a configured wire name replaces it on the way out (B09). |
| `kind` | keep, closed | `task`, `message`, `ticket`, `document`, `page`. Closed because a sink switches on it. The set freezes with v0.1.0: **B18 (HubSpot) is the last item that can add a kind without a new format version**, if CRM objects turn out not to fit these five. |
| `external_id` | keep, domain made exact | 1 to 1,024 characters of an identifier (decision 9). The same for every version of the entity, and **unique within one tenant, across all of the tenant's sources**, because it begins with the internal provider key and a colon. See "The external id" below. |
| `version` | keep, meaning made exact | Opaque, 1 to 256 characters of an identifier. "Monotonic per `external_id`" is a promise **the provider's normalizer makes to the pipeline** (B08 uses it to keep the supersede chain forward only). The format cannot check it, and says so. To a sink a version is only equal or not equal: a sink never orders records by comparing versions, it follows `supersedes`. |
| `supersedes` | keep, nullable | A record id or `null`. Never the record's own id. |
| `occurred_at` | keep, spelling pinned | RFC 3339, always UTC, always the `Z` suffix (never an offset, not even `+00:00`), second precision or 1 to 9 fractional digits, years 1000 to 9999, never a leap second. One instant then has few spellings, and the zero time of Go (year 1) is refused as "not set". |
| `title`, `text` | keep | Always present, may be empty, at most 1,024 and 1,048,576 characters (Unicode code points, which is what `maxLength` counts: not bytes, not UTF-16 units, not what a reader sees as one character). `text` holds anything but NUL, which a Postgres `text` column cannot store. `title` is one line (decision 9). Cutting a longer text down is the normalizer's job: the format refuses, it does not truncate. |
| `author.id`, `author.display` | keep, meaning made exact | See "The author" below. |
| `container.kind`, `container.id` | keep | Where the entity lives at the source. `kind` follows the container kind grammar of ADR 3. `id` is the provider's id **as the provider spells it**, not escaped (the scope id holds the escaped form). Informational: often the container the scope is made of, and not always (a comment lives in a task, and is decided on the task's list). |
| `visibility.scope` | keep | ADR 3. The one thing access is decided on. |
| `visibility.audience` | keep | `direct` or `group`. It stays inside `visibility` because it describes the scope (a DM is `direct`), and it stays **informational: it grants and denies nothing**. The schema and the Go type both say so, because a sink that reads `direct` as "private to the author" repeats the defect behind principle 9. |
| `origin.automation`, `origin.untrusted` | keep, meaning made exact | Both required. A missing `untrusted` must never read as "trusted", which is what a lenient decoder would make of it, and neither must `false`: see decision 10. |
| `edges.reply_parent` | keep, meaning made exact | The **`external_id`** of the entity this one replies to, or `null`, with the same grammar as `external_id`. Not a record id: the parent has many records, one per version, and a reply hangs under the entity. Further edges are additive (B11 may add one for a comment's task). |
| `meta.raw_ref` | **dropped** | It pointed into a raw payload store (`fs://raw/...`) that the design does not have: raw bodies live in the outbox table and are deleted by retention. A reference a sink cannot resolve is noise, and an internal storage path is not something to publish. |
| `meta.delivery` | keep, optional | Sluiceway's id of the accepted delivery, for support. |

#### The external id

`delete` removes "every stored version of the `external_id`", and `edges.reply_parent` points at
one, so the format has to say within what an external id is unique. It is:

- **An `external_id` is unique within one tenant, across all of that tenant's sources.** It
  begins with the **internal provider key** and a colon (`slack:C0GENERAL:1752064245.000200`,
  `clickup:task:86a1xyz`), and what follows is the provider's to define. The prefix is what keeps
  ClickUp's task `12345` and HubSpot's ticket `12345` from being one entity at a sink, where a
  tombstone for one would delete the other.
- **A sink keys an entity by tenant and `external_id` together, and never by `source`.**
  `source` is the wire name, which is sink configuration and may change: a sink keyed by it
  would find nothing under the new name when a tombstone arrives after a rename, and the deleted
  entity would stay retrievable. The provider key inside the external id is not the wire name
  and never changes, for the reason the scope id uses it (ADR 3).
- It is enforced, not a convention. The schema and `Record.Validate` require the shape (a
  provider key, a colon, at least one more character), for `external_id` and for
  `edges.reply_parent`. `Record.Seal` requires the key to be the sealing provider's, for both, as
  it does for the scope. So a normalizer that hands over the source's bare id fails on its first
  record.
- Beyond the prefix an external id is opaque: a sink compares it for equality and does not take
  it apart, not even to read the provider out of it.

### 3. Required, empty, null, absent

| Field | Present | May be empty | May be null |
|---|---|---|---|
| `format`, `id`, `op`, `source`, `kind`, `external_id`, `version`, `occurred_at` | always | no | no |
| `supersedes` | always | no (`""` is refused) | yes |
| `title`, `text` | always | yes | no |
| `author` with `id` and `display` | always | yes, both | no |
| `container` with `kind` and `id` | always | no | no |
| `visibility` with `scope` and `audience` | always | no | no |
| `origin` with `automation` and `untrusted` | always | (booleans) | no |
| `edges` with `reply_parent` | always | no (`""` is refused) | yes |
| `meta` | may be absent | yes (`{}`) | no |
| `meta.delivery` | may be absent | yes, meaning absent | no |

The rule behind the table: a sink never has to tell an absent field from an empty one. Every
field of v1 that carries meaning is on the wire in every record. Only diagnostics may be absent,
and only fields added later can be missing from older records.

`meta` is **not part of the record's content**. The same `id` may arrive twice with a different
`meta` (a re-drain after a crash is byte-identical, but nothing promises that forever), and a
strict sink that refuses "the same id with different content" (B09) compares without it.

### 4. Unknown fields

Allowed and ignored at the top level and in every object except `visibility`. This is what lets
v1 grow. It also means the schema does not catch a misspelled optional field, which is acceptable
because v1 has exactly one (`meta.delivery`).

### 5. `delete` is in v1 today

The proposal said "delete reserved, ships later". A reserved value that the schema refuses is
not reserved at all: `op` is a closed set, so adding `delete` later would be a new format
version by this document's own rule. So it is in the enum now, and its shape is defined now:

- the same envelope, with `title` and `text` empty (the schema enforces this with `if`/`then`,
  and `Record.Validate` does too);
- `version` differs from every upsert version of the entity, so the tombstone has its own `id`;
- `supersedes` names the last record of the entity, where Sluiceway knows it;
- `visibility.scope` is the scope the entity was last in;
- the sink removes or hides **every** stored version of the `external_id`, for that tenant: what
  it holds under the key (tenant, `external_id`), whatever `source` those records carried.

Sluiceway v0.1 never sends one (deletions are "After v0.1" in the roadmap). A sink written today
knows that one can come. A sink that cannot honour a delete **refuses the record** (which
dead-letters it, visibly), and never accepts and ignores it.

### 6. A record carries its scope, never the scope's members

The README said every record carries a scope "and that scope's members", and the first goal in
architecture section 1 said the same. Architecture sections 3.4 and 6 had the record carry only
`visibility.scope`, with members pushed separately through `AccessSink.SyncMembership`. **The
architecture's version is the format.** The README and goal 2 are corrected in this change.

`visibility` is closed and holds `scope` and `audience`. There is no `members` field, and the
schema refuses one.

This is also how the format avoids a class of bug that another project has open today (found in
the first market research run, `growth/landscape.md`): there, a later copy of a document whose
**permissions** changed while its content and timestamp did not "fails the content-hash check,
which excludes permissions, and never reaches the ACL upsert", so a removed user keeps access
until a separate job repairs it. The bug exists because permissions ride on the document and the
dedupe key does not cover them. Here:

- **A membership change is never a record change.** Somebody joining or leaving a channel
  changes the scope's members, which are not in any record. It travels through access sync, as a
  grant or a revocation under the same scope id (the join key, ADR 3). No record is re-delivered,
  nothing about a record has to be re-hashed or compared, and there is no dedupe check for the
  change to fall through.
- **A scope change is a record change**, and the id covers it. See decision 7.

**Out of scope for B05, on purpose:** the membership message (its JSON, its schema, whether it
is a change or a snapshot, what a revocation looks like) and the **person identifier** inside it.
They belong to B23 (member sources and identity) and B24 (the membership diff and `AccessSink`),
and issue #36 asks for them to be written down as the second half of this contract. Nothing in
v1 stands in their way: a membership message needs the tenant (beside it, as for records), the
scope id (ADR 3, the same string), and person identifiers, and the record format constrains
none of the three.

#### The author

`author.id` is **the provider's own user id, raw, exactly as the provider spells it** (`U0BEN`
in Slack, a GUID in Microsoft Graph, a numeric id in ClickUp). It is not resolved, not an email
address, and not the person identifier that membership will use. Empty means the source did not
say (a system event, a record degraded to the webhook body).

**Access is never decided on the author.** It is decided on scope membership only. The author of
a record may not even be a member of its scope any more. `author.id` lets a sink group records
by who wrote them and, once B23 defines the person identifier, join an author to a person if the
membership message also carries provider user ids (issue #39 asks for that, and the record
already does its half). `author.display` is a name to show and nothing else.

### 7. The scope is part of the record id

The record id was `blake3(provider, external_id, version, tenant)`. It is now

```text
"rec_" + hex(blake3(provider, external_id, version, scope, tenant))[:32]
```

with `scope` being the record's `visibility.scope`.

**The case.** A task is moved to another list. A message is moved to another channel. The record
is now decided on a different scope: the people of the new list should see it, the people of the
old list should not. Is that a new version? It has to be: the sink must replace what it stored,
and the only way a sink replaces anything is a new record with `supersedes`.

**Can `version` alone express it? No.** `version` comes from the provider (`date_updated`, a
message `ts`, a change key), and a provider's version does not have to change when an entity
moves. Where it does not, the old recipe gives the moved record **the same id** as before. The
ledger then skips it as already delivered (architecture section 3.2, step 5), the sink keeps the
old scope, and the old list's members keep a record they lost access to while the new list's
members never get it. That is the bug class above, by another road: a permission change hidden
from the dedupe key. Nothing would report an error.

Putting the rule on each normalizer ("change the version on a move") would fix it one provider
at a time and fail silently for the provider that forgot. Hashing the scope fixes it by
construction:

- a moved record gets a new id whatever the provider's version did, so the ledger does not skip
  it;
- the pipeline links it to what it was in the old scope through `supersedes`, like any other
  new version, and the sink replaces it;
- a sink may rely on it: **one `id` never appears with two scopes.** The strict stub (B09)
  checks exactly that when it refuses the same id with different content.

`Record.Seal` is the only way to an id. It hashes the scope the record carries and no other, and
it refuses a scope outside the sealing provider's namespace. Because a promise to sinks rests on
it, this is enforced and not only said:

- `ids.RecordID` has to be exported for `Seal` to call it, so a test in `internal/ids` parses
  every Go file of the repository and fails when anything outside `internal/record` refers to it
  (a call, a function value, an aliased or a dot import, a `go:linkname` directive), or when
  `internal/ids` itself says its name anywhere but in its declaration (a wrapper would hand the
  recipe on under another name). That scan is a tripwire: it reads source text, and it is there
  to tell an honest second caller why there must not be one. The guard is the next point.
- The fields the id stands for (`ID`, `ExternalID`, `Version`, `Visibility.Scope`) stay
  assignable after `Seal`, because a `Record` is a plain value and `Supersedes` and `Source` are
  meant to be set afterwards. So `Seal` also keeps a private copy of the four, and of `Op` and
  `Kind` (an upsert turned into a tombstone after sealing would go out under the id of the
  version it was, and a sink that is idempotent on `id` would drop it as a repeat), and **`Marshal`
  refuses a record in which they no longer say what was sealed**, or that was never sealed. A
  pipeline stage that reassigns the scope after sealing fails at once, where the mistake is, and
  not as a dead letter at a strict sink. Decoding pins what it read in the same way. The
  alternative, hashing again in `Marshal`, would need the provider key and the tenant, which are
  deliberately not in the envelope, and would cost a BLAKE3 per record. The comparison costs
  six string compares: `Marshal` measured 2.5 microseconds per record before and after.
- **The tenant** is in no field, so a record sealed for tenant A marshals identically when it is
  delivered under tenant B, and neither `Marshal` nor any sink can notice. `Seal` therefore
  keeps the tenant privately as well, and `Record.SealedFor(tenant)` answers at the one place
  where a tenant and a record meet again, the stage that writes the ledger and calls
  `Sink.Deliver` (B08): it asserts it there and treats false as a refusal. It fails closed: a
  decoded record is sealed for no tenant, because a document does not say whose it is.
- **What none of this defends against** is a caller determined to get around it. A conversion
  to a type of the caller's own (`type w record.Record`, then `json.Marshal(w(r))`) sheds the
  methods, and with them `Validate` and the seal. The seal guards against accidents, which is
  what a pipeline produces. It does not try to defeat the conversion.

Nothing has been delivered yet, so re-keying costs nothing today. After v0.1.0 it would re-key
every record at every sink. That is why it is decided here and not when the first provider with
movable entities arrives.

**What remains: A, B, and back to A.** An entity that moves from scope A to B **and back to A**,
with a provider version that changed at neither move, produces the first record's id again. A
ledger that only asks "have I delivered this id" skips it, and the sink keeps the record in B:
B's members go on reading what they lost access to, A's members never get it back, and nothing
reports an error. Undoing a move made by mistake is the most likely reason for a second move, so
this is not a corner. The sink cannot repair it either. It is idempotent on `id`, and the first
record would have to supersede the second, which supersedes the first.

> **Decided by default, the maintainer may overrule.** The reviewer of B05 raised this and the
> orchestrator adopted the position below, because failing loudly is this project's rule. It is
> written as a requirement on B08 and copied to issue #8. Whether a public contract may ship
> with the remaining window at all is the maintainer's call.

**The format does not change for it.** `version` is opaque to a sink, so the repair can arrive
later inside v1 without re-keying a record already delivered: a normalizer derives the version
from the move event, or the pipeline folds a counter that the ledger holds into `version`.
Neither needs a new field.

**The ledger stage (B08) must detect the case and must never skip it silently.** It is
detectable with state the ledger needs anyway, which is, per entity (tenant, provider,
`external_id`), the head of the supersede chain and the scope of that head:

1. the incoming record's id is already in the ledger, and
2. it is **not the head** of its entity's chain, and
3. **the head's scope differs from the incoming record's scope.**

A repeat of the head (a re-drain, a backfill overlap) fails condition 2. A late re-send of an
old version fails condition 3, because a record hydrated at drain time carries the scope the
entity is in now, which is the head's. What passes all three is either an entity that moved
back, or a stale record from the old scope (a degraded record built from an old webhook body,
see below), and the ledger cannot tell which. So it does not guess: such a record is
**dead-lettered with a reason of its own and counted in a metric**, never skipped and never
delivered.

**What an operator does with one.** The dead letter names the tenant, the provider and the
entity. The operator looks at the entity at the source. If it is in the scope the dead letter
says (it moved back), the provider's normalizer has broken its contract: it is fixed so that a
move changes the version, and the dead letter is replayed, which now yields a new id that
supersedes the record in B. If the entity is in the head's scope, the dead letter was a stale
record and is discarded. Until the first case is resolved the sink still holds the record in B.
That is the window that remains, and it is now a visible one: a dead letter and a counter that
is not zero.

The normalizer contract stays what it was (B11 and every provider after it): a version changes
with every change the provider reports, a move included, and where the provider's own version
does not, the normalizer derives one from something that does (the move event's time). Hashing
the scope makes the common case safe when that contract is broken, and the ledger rule makes the
A, B, A case loud when it is. Neither makes the contract optional.

Two relatives of this case, recorded so that B08 and B11 meet them knowingly:

- **Delete, then restore, with an unchanged version.** The restored entity produces the id it
  had before the delete. The ledger knows that id, it is not the head (the tombstone is), and
  the head's scope is the same, so condition 3 does not see it and the record is skipped: the
  entity stays deleted at the sink. That **fails closed**, nobody reads what they should not.
  But the scope check cannot help, so the normalizer rule (a restore changes the version) is
  the only protection. v0.1 sends no deletes, so this cannot happen before deletions ship, and
  the item that ships them has to settle it.
- **A degraded record must derive the same scope as the hydrated one would.** When hydration
  fails and a record is built from the webhook body, its scope goes into its id. If the
  degraded path derived a different scope for the same version of the same entity (a missing
  field, another fallback), one version would get two ids, be delivered twice, and look like a
  move to the ledger. So both paths build the scope from the same inputs through the same
  function, and where the webhook body does not carry what the scope is made of, the record
  cannot be degraded: that delivery is retried or dead-lettered, never given a guessed scope.

### 8. The bytes of a document

**A record document is UTF-8, and no `\u` escape in it names half of a surrogate pair.** An
escaped pair (`\uD83D\uDE00`) is fine, and so is the astral character itself. This is a rule of
the format that a sink may rely on, and Sluiceway never writes a document that breaks it.

It has to be said because JSON parsers do three different things with such bytes, and the format
exists so that two consumers never read two different records in one document:

- Go's `encoding/json` accepts an invalid byte and a lone surrogate escape alike and puts U+FFFD
  in their place. A document with the byte `0xFF` in `external_id` and the same document with
  `0xFE` there decode to **one** external id, so two documents become one entity.
- Python's `json` refuses a document that is not UTF-8, and **keeps** a lone surrogate, so
  `\uD800` and `\uDC00` are two distinct values there, both of which pass the schema, and one
  value in Go.
- A JSON Schema never sees any of it: it is handed a parsed document.

So the rule is checked on the bytes, before parsing. `Record.UnmarshalJSON` refuses a document
that is not valid UTF-8 and one with an unpaired surrogate escape, anywhere in it, unknown
fields included. **A sink that validates with the schema alone checks the same two things
itself**, and the schema's description says so. It is the third rule beyond the schema, next to
the two below.

### 9. Which characters a field may hold

The first draft refused the C0 control characters and DEL in identifiers and names, "for log
forging". That was half a rule: U+0085, U+2028 and U+2029 break a line in many log viewers and
terminals just as a line feed does, and a right-to-left override (U+202E) in a display name makes
a sink's UI show one person's name as another's, or `invoice<U+202E>gnp.exe` as
`invoiceexe.png`. Tightening after v0.1.0 is a new format version, so it is decided now, field by
field. Every rule is in the schema (`$defs` `identifier`, `displayName`, `oneLine`) and in
`Record.Validate`. The two are held together by a test that compares the schema's patterns, the
Go rule and the ranges of the table below for **every code point of Unicode** (1,112,064 for
each of the four rules, a tenth of a second, or about six under the race detector), and by one
that runs about 1,800 code points through every field on both sides, which proves that each
field is wired to its own rule. The same run was made with Python's `jsonschema`, over the
whole Basic Multilingual Plane.

| Fields | Refused | Why |
|---|---|---|
| **Identifiers:** `external_id`, `version`, `author.id`, `container.id`, `edges.reply_parent`, `meta.delivery` | Every control character: C0 (U+0000 to U+001F), DEL and C1 (U+007F to U+009F). The line and paragraph separators U+2028, U+2029. Every bidirectional formatting character: the embeddings and overrides U+202A to U+202E, the isolates U+2066 to U+2069, the marks U+200E, U+200F and U+061C. The zero-width and invisible format characters: U+00AD, U+200B to U+200D, U+2060 to U+2065, U+206A to U+206F, U+FEFF. As ranges: U+0000 to U+001F, U+007F to U+009F, U+00AD, U+061C, U+200B to U+200F, U+2028 to U+202E, U+2060 to U+206F, U+FEFF. | A program compares them and an operator reads them in a log. No provider id holds any of these, so nothing is lost, and an identifier can neither break a log line, nor reorder what is printed beside it, nor differ from another one invisibly by the commonest means. |
| **`author.display`** | The same, **except that U+200C and U+200D are allowed.** | A name somebody chose for themselves, so it is where an attack would be planted, and it is shown in a UI next to other names. The zero-width non-joiner and joiner stay because Persian and Indic names are spelled with them and emoji sequences (a family, a profession) are built with them. The directional marks and the soft hyphen go: a name loses nothing visible without them. |
| **`title`** | Every control character (C0, DEL, C1), so also tab, line feed and carriage return, and U+2028, U+2029. **Nothing else.** | Human content in any language, on **one line**: a task name, a subject, a page title, shown as a heading or a list row. Right-to-left titles legitimately use U+200E, U+200F and the isolates, and older ones the embeddings, so bidirectional formatting is content here and stays. |
| **`text`** | NUL. Nothing else. | Content. Newlines, tabs, form feeds, escape characters, every kind of line break and all bidirectional formatting are what somebody wrote. |

Four things follow, and a sink author should know them:

- **`title` and `text` are not safe to display as they stand**, and neither is any other field
  in a context that the lists above do not cover. They may hold bidirectional overrides, and
  `text` may hold terminal escape sequences. They are foreign text and a sink treats them as
  such.
- **The lists are fixed code points, never a Unicode category.** A category (`Cf`, say) grows
  with every Unicode version, and the format may not grow (decision 1). So they are not every
  invisible character there is: the Hangul fillers, the variation selectors and the tag
  characters (U+E0000 to U+E007F, which can smuggle text past a human reader) are not refused,
  and neither is the ideographic space U+3000. Each of these is an accepted case with a name in
  the tests, because each is an invitation to "close the gap" on one side later, and within v1
  that is a break in either direction (decision 1): whoever tightens a rule deletes a named test
  first.
  The tag characters were left out for a stated reason: regular expression dialects do not agree
  on how to name a character above U+FFFF (one needs surrogate pairs, where a range across them
  does not even compile), and a schema that only some validators can load is worse than a
  shorter list. `display` still cannot stop one name imitating another with look-alike letters.
  No list of characters can.
- **The format refuses, it never repairs.** A display name with a right-to-left override makes
  a record that `Seal` refuses, and a person can put one into their own Slack name. So a
  normalizer removes the refused characters from the name the source gave, turns the line breaks
  of a title into spaces, and cuts a text to its limit, before `Seal`. That belongs to the
  normalizer contract (B11 and every provider after it). A normalizer that forgets fails loudly
  on the first such record, which is a dead letter and not a leak.
- **Identifiers are never cleaned. Only `author.display` and `title` are.** Removing a character
  from an `external_id`, a `version`, an `author.id` or a `container.id` merges two different
  source ids into one: for `external_id` that is two entities under one key, and a tombstone
  that deletes the wrong one. For an identifier refusal is the only right answer, and no id a
  provider mints holds a refused character. An identifier a **sender** controls is another
  matter: a mail `Message-ID` or `In-Reply-To` used for `edges.reply_parent` or an `external_id`
  may hold tabs, control characters and raw 8-bit bytes (RFC 5322's obsolete syntax allows them,
  and real spam carries them), so passed through raw it lets a stranger's mail dead-letter
  itself. A normalizer prefers the provider's own id (Graph's message id is ASCII), and where a
  header has to be used it escapes or hashes it **injectively** (the percent escaping of the
  scope id is in the package): never cleaned, and never passed through raw.
- **Portability of the patterns.** Only ASCII characters are written as regular expression
  escapes (`\x00` to `\x1f`, `\x7f`), which Go's, Python's, ECMAScript's and Ruby's dialects all
  read alike. For anything above there is no escape they share: `\x9f` is "invalid multibyte
  escape" to Ruby's engine in a UTF-8 pattern, so a Ruby validator could not even load a schema
  that used it, `\u2028` is not RE2, and `\x{2028}` is not Python or ECMAScript. So every
  character from U+0080 up is in the pattern **as itself**, written in the schema file with a
  JSON escape, which every JSON parser turns into the character before any regular expression
  engine sees it. The file stays pure ASCII. A test refuses any other escape in any pattern of
  the schema. All 16 patterns compile in the four dialects, and the three character classes
  give the verdict of the table above for every code point in each of them.

A container id inside a **scope id** is a different thing: it is carried as escaped bytes, the
result is ASCII, and ADR 3 refuses only control bytes there. A provider id with a zero-width
space in it (none is known) would make a valid scope id and an invalid `container.id`, so the
record is refused, loudly, which is the right failure.

### 10. What `origin` says, and what it does not

Both fields are **signals, never clearances**:

- `origin.untrusted: true` means Sluiceway has a **positive signal** that the author is outside
  the tenant: inbound mail from a stranger, an external guest in a shared channel.
- `origin.untrusted: false` means **no signal.** The source did not say, the provider cannot
  tell, or this version of Sluiceway does not look. It never means that the author is inside
  the tenant, and never that the text is safe to follow.
- `origin.automation` likewise: `true` is a positive signal that a bot or an integration wrote
  it (the source marks the author as one), `false` is no signal, never "a person wrote it".

This has to be pinned before v0.1.0 because the roadmap populates the untrusted marking only
**after v0.1**. In v0.1 no provider sets it, so mail from a stranger, delivered through Outlook,
says `"untrusted": false`. A sink author who reads `false` the natural way ("checked, and fine")
and lets such text past an injection guard is wrong from the first day, and a meaning cannot be
repaired within v1. With `false` defined as "no signal", populating the marking later changes no
meaning: more records say `true`, and every sink that was correct stays correct.

So the rule for a sink: **every text is untrusted content, whatever `origin` says. Use `true` to
be stricter, never `false` to be laxer.** And for a normalizer: set a field to `true` only on a
positive signal from the source, leave it `false` otherwise.

**Why two values and not three** (`true`, `false`, `unknown`). A third value would earn its
place only if a sink could do something with "known to be inside" that it cannot do with "no
signal", and the only such thing is to relax its guard. Sluiceway can never license that: an
insider's message quotes an outsider's mail, a forwarded thread, a pasted web page, and the
author's membership says nothing about where the words came from. A value that means "safe" is
one Sluiceway cannot honestly send, so the field has no use for a way to say it, and the
two-valued form already says everything true: "we saw a reason for extra care" or "we saw none".

### The Go side and the schema agree

Principle 5, applied to the format: `internal/record` refuses what the schema refuses, in both
directions. A `Record` that fails `Validate` cannot be marshalled. A document the schema refuses
cannot be unmarshalled into a `Record`: decoding checks that required fields are present and not
null (plain `encoding/json` would read a missing `origin` as "trusted"), that field names are
lowercase, that `visibility` holds nothing unknown, and that `occurred_at` is spelled as above.
About 330 documents, about 60 Go values and a fuzz target run through both and must get the same
answer. Two parts of the grammar are small enough to run in full, and are: every `%XX` escape of
a scope id, all 256 bytes in uppercase, lowercase and mixed hex (ADR 3), and the four character
rules over every code point of Unicode, the schema's compiled patterns against the Go rule
(decision 9). Both are judged against the rule as the ADRs state it and not against each other,
because two halves that drift together agree.

Three rules only the Go side has, because no JSON Schema can state them. A sink that validates
with the schema alone should add all three, and the schema's description lists them:

- **`supersedes` is not the record's own `id`.** JSON Schema cannot compare two fields.
- **No field name twice in one object.** Decoders disagree on which one counts, so a validator
  that keeps the first `visibility` and a consumer that keeps the last would see two different
  scopes in one record. Sluiceway never writes such a document. The Go decoder refuses one.
- **The bytes of the document** (decision 8): UTF-8, and no escape for half of a surrogate pair.
  The tests state the expected outcome of these cases themselves, because the schema validator
  they use parses with `encoding/json` and so judges a document that has already been rewritten.

Two properties of the schema exist for validators other than the one in our tests:

- `format: date-time` is an annotation unless a validator is told to assert formats, and many
  are not. `occurred_at` therefore also has a `pattern` that rejects everything except a day
  that does not exist in its month (`2026-02-30`), which only format assertion catches. The
  tests compile the schema both ways and hold both.
- In some regular expression dialects (Python's is one) `$` also matches before a trailing
  newline, so `^rec_[0-9a-f]{32}$` alone would accept `rec_...\n` there. Every anchored pattern
  in the schema has a companion that does not depend on the anchor: a fixed length, or a `not`
  with an unanchored character class. A test walks the schema and fails if one is missing.

Errors name the field and the rule and never quote a value: a record is somebody's message, and
an error ends up in a log or an outbox row.

## Why

- A contract that cannot say which version it is cannot change, and one that may change anywhere
  cannot be relied on. The rule "additions a reader may ignore, never inside `visibility`" is
  short enough to remember and strict in the one place where leniency leaks data.
- "Always present" makes a sink's code and its tests smaller, and removes the question of what
  an absent `origin.untrusted` means.
- Keeping the proposal's names: they were already in the architecture, the backlog refers to
  them (`edges.reply_parent`, `origin.untrusted`), and none of them was wrong. A rename has to
  buy something.

## Cost

- The id recipe of B02 changed after it was merged: `ids.RecordID` takes the scope, the golden
  vectors were regenerated and re-verified with the Python `blake3` package, and architecture
  section 5 says so. A provider can no longer compute a record id before it knows the scope,
  which it always does by the time it normalizes.
- Closed sets mean a sixth `kind` after v0.1.0 is a v2. The alternative, an open `kind`, would
  push "what do I do with a kind I have never seen" onto every sink.
- Decoding is strict and makes several passes (about 25 microseconds for the example record).
  That is the consumer's side. On Sluiceway's side a record costs a `Validate`: about 0.25
  microseconds and no allocation for a short message, and about 0.02 milliseconds for the
  largest text the format allows, which is only searched for NUL and checked for UTF-8.
- `delete` is defined before anything sends it. If deletions turn out to need more (a whole
  container deleted at once), that is an additive field or a v2, decided then.
- The A, B, A move with an unchanged provider version is not solved by the format. It is made
  loud: B08 has to dead-letter and count it (decision 7), and until an operator acts on that,
  the record stays in the wrong scope at the sink. Decided by default, for the maintainer to
  confirm or overrule.
- Every limit, pattern and character list is frozen in both directions from v0.1.0 on
  (decision 1). What was not measured against real providers by then (the 512 bytes of a scope
  id against Graph ids, ADR 3) can only be corrected in a v2.
- The character rules put work on every normalizer: names and titles from the source have to be
  cleaned before `Seal`, or the record is refused (decision 9). That is deliberate, the format
  does not repair, and it is one more thing a provider can forget, loudly.
- The character lists are not every invisible character (decision 9 says which are missing and
  why), and `external_id` now has a grammar, so a sink can no longer be told "it is just an
  opaque string": it is opaque after the provider key.
