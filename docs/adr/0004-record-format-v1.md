# 4. The record format, v1: field names, what is required, and how it may change

Status: accepted, 2026-09-19 (backlog item B05)

## Decision

The envelope a sink receives is format **v1**, as shown in architecture section 6. The contract
is the JSON Schema `internal/record/record.v1.schema.json` (draft 2020-12), embedded in the
binary by `internal/record`, whose Go types produce and read it. It becomes a public contract
when v0.1.0 ships. Until then this record can still be superseded cheaply; afterwards every
change below the line "what forces a new version" costs every sink author a migration.

Seven things are decided here that the proposal left open or did not have:

1. The format carries its own version, in a `format` field.
2. The field names of the proposal are kept, with one field dropped (`meta.raw_ref`) and one
   added (`format`).
3. Every field that carries meaning is always present. "None" is `null` or `""`, never absence.
4. Unknown fields are allowed everywhere except inside `visibility`.
5. `op: "delete"` is part of v1 now, with its shape defined, although v0.1 never sends one.
6. A record carries its scope and never the scope's members.
7. **The scope is part of the record id**, so a record that moves to another scope is a new
   record. This changes the id recipe of architecture section 5 and `internal/ids`.

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

**What may change within v1:** fields that a reader may safely ignore are added, at the top
level and inside `author`, `container`, `origin`, `edges` and `meta`. A sink written against
today's schema keeps working, because it ignores what it does not know and the schema allows
additional properties there.

**What forces a new version** (`sluiceway.record/v2`, a new schema file, a new `$id`):

- a field removed, renamed, or changed in type or meaning;
- a limit tightened, or a required field made optional;
- a new value of `op`, `kind` or `visibility.audience`. These are closed sets, and a sink that
  validates would refuse the new value, so adding one is not additive;
- **anything at all inside `visibility`.** A sink that ignored a new field there (a deny list,
  say) would grant access it should not. That is why `visibility` is the one closed object: an
  unknown field in it is a reason to refuse the record, not to ignore the field.

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
| `external_id` | keep | Opaque, 1 to 1,024 characters, no control characters. The same for every version of the entity. Built from the provider key, since it is hashed into the id. |
| `version` | keep, meaning made exact | Opaque, 1 to 256 characters, no control characters. "Monotonic per `external_id`" is a promise **the provider's normalizer makes to the pipeline** (B08 uses it to keep the supersede chain forward only). The format cannot check it, and says so. To a sink a version is only equal or not equal: a sink never orders records by comparing versions, it follows `supersedes`. |
| `supersedes` | keep, nullable | A record id or `null`. Never the record's own id. |
| `occurred_at` | keep, spelling pinned | RFC 3339, always UTC, always the `Z` suffix (never an offset, not even `+00:00`), second precision or 1 to 9 fractional digits, years 1000 to 9999, never a leap second. One instant then has few spellings, and the zero time of Go (year 1) is refused as "not set". |
| `title`, `text` | keep | Always present, may be empty, at most 1,024 and 1,048,576 characters. No NUL, which a Postgres `text` column cannot store. Newlines and tabs are content. Cutting a longer text down is the normalizer's job: the format refuses, it does not truncate. |
| `author.id`, `author.display` | keep, meaning made exact | See "The author" below. |
| `container.kind`, `container.id` | keep | Where the entity lives at the source. `kind` follows the container kind grammar of ADR 3. `id` is the provider's id **as the provider spells it**, not escaped (the scope id holds the escaped form). Informational: often the container the scope is made of, and not always (a comment lives in a task, and is decided on the task's list). |
| `visibility.scope` | keep | ADR 3. The one thing access is decided on. |
| `visibility.audience` | keep | `direct` or `group`. It stays inside `visibility` because it describes the scope (a DM is `direct`), and it stays **informational: it grants and denies nothing**. The schema and the Go type both say so, because a sink that reads `direct` as "private to the author" repeats the defect behind principle 9. |
| `origin.automation`, `origin.untrusted` | keep | Both required. A missing `untrusted` must never read as "trusted", which is what a lenient decoder would make of it. |
| `edges.reply_parent` | keep, meaning made exact | The **`external_id`** of the entity this one replies to, or `null`. Not a record id: the parent has many records, one per version, and a reply hangs under the entity. Further edges are additive (B11 may add one for a comment's task). |
| `meta.raw_ref` | **dropped** | It pointed into a raw payload store (`fs://raw/...`) that the design does not have: raw bodies live in the outbox table and are deleted by retention. A reference a sink cannot resolve is noise, and an internal storage path is not something to publish. |
| `meta.delivery` | keep, optional | Sluiceway's id of the accepted delivery, for support. |

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
- the sink removes or hides **every** stored version of the `external_id`, for that tenant.

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
it refuses a scope outside the sealing provider's namespace.

Nothing has been delivered yet, so re-keying costs nothing today. After v0.1.0 it would re-key
every record at every sink. That is why it is decided here and not when the first provider with
movable entities arrives.

**What remains.** An entity that moves from A to B **and back to A**, with a provider version
that changed at neither move, produces the first id again, and the ledger skips it. Only state
can tell that apart from a late re-send. The format does not need another field for it: what it
needs is that `version` moves, so the normalizer contract (B11 and every provider after it) says
that a version must change with every change the provider reports, a move included, and where
the provider's own version does not, the normalizer derives one from something that does (the
move event's time). Hashing the scope makes the common case safe when that contract is broken.
It does not make the contract optional.

### The Go side and the schema agree

Principle 5, applied to the format: `internal/record` refuses what the schema refuses, in both
directions. A `Record` that fails `Validate` cannot be marshalled. A document the schema refuses
cannot be unmarshalled into a `Record`: decoding checks that required fields are present and not
null (plain `encoding/json` would read a missing `origin` as "trusted"), that field names are
lowercase, that `visibility` holds nothing unknown, and that `occurred_at` is spelled as above.
About 250 documents, 46 Go values and a fuzz target run through both and must get the same
answer.

Two rules only the Go side has, because no JSON Schema can state them. A sink that validates
with the schema alone should add both:

- **`supersedes` is not the record's own `id`.** JSON Schema cannot compare two fields.
- **No field name twice in one object.** Decoders disagree on which one counts, so a validator
  that keeps the first `visibility` and a consumer that keeps the last would see two different
  scopes in one record. Sluiceway never writes such a document. The Go decoder refuses one.

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
  That is the consumer's side. On Sluiceway's side a record costs a `Validate`: about 0.2
  microseconds and no allocation for a short message, about 0.25 milliseconds for the largest
  text the format allows.
- `delete` is defined before anything sends it. If deletions turn out to need more (a whole
  container deleted at once), that is an additive field or a v2, decided then.
- The A, B, A move with an unchanged provider version is documented and not solved.
