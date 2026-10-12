# 16. The vault's encryption key comes from the environment, and what a KMS would change

Status: accepted, 2026-10-12 (backlog item B13). Settles open decision 7.

## Context

The vault holds a tenant's provider credentials, and architecture section 7 says raw tokens never
reach a plain table. Something has to hold the key those credentials are sealed with, and that
choice decides what a database backup is worth on its own, what a local setup needs installed, and
what an operator does when the key changes.

Open decision 7 in [roadmap.md](../roadmap.md) already carried the answer: **an environment master
key for v0.1, a KMS interface later**, because it keeps the local setup dependency-free. This
record accepts that and does not re-open it.

What was not settled, and is what this record is for: where the key is read and what happens
without one, what an envelope has to carry so that a later key source can be added without
rewriting rows, what a KMS would mean for the shape of the `Vault` interface, and what rotation
would take. The last one matters most to a later reader, because the honest answer today is that
the interface cannot express it.

## Decision

### 1. The key is one environment variable, and production has no default for it

`LAWANG_VAULT_KEY` holds 32 bytes (AES-256) in standard base64 with padding, which is 44
characters and what `openssl rand -base64 32` prints. `config.Load` reads it, validates it, and
carries it as a `config.MasterKey` that redacts itself in every printing method.

One spelling, deliberately. Hex was considered and refused: 64 hex characters are also 64 valid
base64 characters, so a value could be read two ways and the wrong reading would be a key nothing
was ever encrypted under, found out at the first fetch rather than at start-up. A hex key still
gets a clear refusal, because it decodes as base64 to 48 bytes and the length rule names what the
spelling is.

### 2. A missing key in production is a startup error, and development generates one

Production refuses to produce a configuration at all without a key, so no role starts: `serve`,
`worker` and `migrate` all print the variable's name and exit 1. Development, which has to be
asked for by name (`LAWANG_ENV=development`, and anything unrecognised stays production), generates
a key per process instead, so a local setup needs nothing installed and nothing configured. What
that costs is in the variable's own documentation: the key goes with the process, so a credential
stored in development cannot be read again after a restart.

**There is no backend selector in front of that rule.** A deployment cannot say "I use Nango, so I
need no key" and reach the generating branch, because the branch is reached only by naming
development. A selector is the kind of input that turns a refusal into a reachable convenience, and
if one is added with the operator API (B14) the key stays required for the `local` backend.

`vault.NewLocal` refuses the same thing again, for every caller that is not `config.Load`: no key,
a key of the wrong length, and the all-zero key, which is what an uninitialized buffer, a cleared
secret manager entry and a base64 run of A's all produce. There is no fallback behind it. A local
vault with no key could only store credentials in the clear, and a vault that stores credentials in
the clear is not one.

### 3. The envelope names the key and the format it was sealed under

What the table holds is not a ciphertext but an envelope: a format byte, the id of the key, and
what AES-256-GCM produced (its own random nonce, the ciphertext and the tag). The key id is the
first 8 bytes of a SHA-256 over a label and the key, so it tells two keys apart without saying
anything about either. The envelope is authenticated with the tenant, the provider key and that
header, so a row copied from one tenant to another, or from one provider to another, no longer
decrypts: the key alone is not enough to move a credential between tenants by editing a row.

Those two bytes of bookkeeping are what make a later key source additive rather than a migration:

- the **format byte** is how a reader that meets an envelope it does not understand refuses by
  name instead of guessing (`ErrNotSealed`), which is also how a plaintext row, whose first byte is
  `{`, is refused as plaintext rather than as a decryption failure;
- the **key id** is how a row sealed under a key this process does not hold is refused as
  `ErrWrongKey` rather than as corruption, which is the difference between "restore the old key"
  and "this row is gone".

Migration 00007 carries the same two numbers in a CHECK, so the rule holds for what is written as
well as for what is read, and a test holds the constraint and the code together.

### 4. A KMS changes nothing about the `Vault` interface, and one thing behind it

This is the part a later reader needs, so it is written out rather than left as "a KMS interface
later".

The interface is keyed by tenant and provider and says nothing about keys:

```go
type Vault interface {
	Store(ctx context.Context, t tenancy.ID, provider string, c Credential) error
	Fetch(ctx context.Context, t tenancy.ID, provider string) (Credential, error)
	Revoke(ctx context.Context, t tenancy.ID, provider string) error
}
```

No caller of it can tell where a key comes from, and none of its three methods has a key, a key id
or a key version in it. So a KMS arrives as **another implementation or another option, never as a
change to this shape**, and the seam it replaces is one unexported type inside `internal/vault`:
`sealer`, which holds a key schedule and an id and has exactly two methods, `seal` and `open`.

Concretely, a KMS-backed vault is one of two shapes, and the envelope is ready for both:

- **Envelope encryption**, which is what AWS KMS, Google Cloud KMS and Azure Key Vault are all
  built for: the KMS issues a data key per credential, the data key seals the credential locally,
  and the wrapped data key travels in the envelope. That costs a new format byte (`2`), a
  variable-length header, and nothing else: the table, the queries, the interface and every caller
  stay as they are, and a v1 envelope keeps opening because its format byte says what it is.
- **Remote seal and open**, where the KMS does the cryptography: the same `sealer` seam, with a
  network call in each method, and a key id that is the KMS's own key version rather than a hash.
  This one adds a round trip to every credential fetch, on the drain's path, which is why it is
  the less likely of the two and why architecture section 11's "vault unreachable: fail closed"
  already covers it (`ErrUnavailable`).

A deployment with both would run two vaults for a while, and the format byte is what lets one
process read what the other wrote.

### 5. Rotation is not built, and the interface cannot express it

Changing `LAWANG_VAULT_KEY` today makes every stored credential unreadable. The refusal is
`ErrWrongKey`, which names the cause and is the reason the key id exists, and the operator's
recovery is to connect each provider again (B14's `lawang connect`), not to read the rows.

Rotating properly needs two things this item deliberately did not half-build:

1. **More than one key accepted for reading.** `sealer` holds one key and one id. Rotation needs
   the current key for sealing and a list of previous keys for opening, selected by the envelope's
   key id, which is a change to one type and to one variable (`LAWANG_VAULT_KEY_PREVIOUS`, or a
   KMS's own key versions, which it manages itself).
2. **An operation that re-seals every row**, which `Vault` has no way to say. Its three methods
   are each about one tenant and one provider, and there is no "for every row" among them;
   `Fetch` then `Store` per pair would work only for a caller that can enumerate the pairs, which
   is a query no package outside `internal/vault` should have. So rotation lands as a new method
   or a new operator command (`lawang vault reseal`), with its own reporting, and that is a
   decision for the item that builds it.

Writing half of it now (a previous-key list with nothing to re-seal with, or a re-seal pass with
no second key) would leave a deployment that can start with the wrong key and read nothing, which
is what the refusal above already does, with more code.

### 6. The vault's key does not reach `subscriptions.secret`, and B14 owns that column now

[ADR 11](0011-hub-resolution.md) decision 1 put the webhook verification secret in a plain `bytea`
column of `subscriptions`, because the accept path has to verify a delivery before it knows whose
delivery it is and `Vault.Fetch(ctx, tenant, provider)` is keyed by the tenant it does not have
yet. That decision named this item as the one that pays the cost: "when the vault lands (B13),
encrypting this column with the vault's key is a change to the two candidate queries and to
`hub.candidates`". The maintainer's first comment on issue #13 then listed every reader and writer
of the column and the two constraints such an interface has, "so that this item does not have to
rediscover it".

**B13 does not encrypt that column. This is the record of the choice, so that a later reader meets
it instead of a ticked box.** Three reasons, in the order they carry weight:

1. **It is not what this vault holds.** ADR 11's own first bullet is the argument: the
   verification secret proves a delivery came from the provider. It authenticates nothing to the
   provider, it reads nothing and it posts nothing. What the vault holds is the other kind, the
   API tokens hydration (B11) and registration (B14) bear, and the two have different blast
   radii. The scan test of this item uses `subscriptions.secret` as the known-positive control for
   the bytea half of its search precisely because it is a plain column on purpose: the control and
   the omission are the same fact, which is why saying it out loud here matters.
2. **The seam it needs is not the one this item built.** Every method of `Vault` takes a tenant,
   and the whole reason this column exists is a path that has none yet, so the decryption is a
   second seam beside the vault rather than a caller of it. The three backends make that concrete:
   `nango` and `azureapp` have no way to seal a value of Lawang's own, so only `local`'s `sealer`
   could serve it, and a deployment that keeps provider credentials in Nango would then still need
   `LAWANG_VAULT_KEY` for this one column. That is a statement about what a backend selector means
   (decision 2 above), and it belongs to the item that adds one.
3. **It runs on the accept path.** Issue #13 writes the budget down: once per delivery, inside the
   resolver transaction, which targets 200 ms and must not be held across network I/O, over up to
   `hub.DefaultMaxCandidates` (32) candidate rows. Designing that is work with a decision in it,
   not a change of call site, and doing it badly is worse than not doing it: the column's
   `subscriptions_secret_is_not_empty` CHECK and `hub.newCandidate`'s refusal of an empty secret
   both still have to hold, and a half-done version parks every delivery as an unowned one.

**What the plain column costs, in the open.** A database backup carries the verification secrets,
and an operator or a role with `SELECT` on that table can read them. What reading one buys is
forged deliveries for that subscription: the forgery resolves to a real owner and its bytes enter
that tenant's outbox, to be normalized and delivered as if the provider had sent them. It does not
read anything from a provider, it does not reach another tenant's rows, and `lawang_resolver`
cannot write. The grant is already the narrowest that works (ADR 11 decision 1), and it is still
the only grant on the column.

**Who owns it now: B14.** It is the item that first constructs a vault inside the binary, it owns
the column's only writer (`UpsertSubscription`, through `hub.Subscriptions.Register` and the
ClickUp registrar it adds), and a backend selector, if one is wanted, arrives there too, which is
the open question in reason 2. If B14 declines it, the work is an item of its own, before the
hardening milestone rather than inside it, and which it is belongs to whoever holds the queue, not
to this record.

**And B13's box stays ticked, on purpose.** The item's own three "Done when" clauses are the ones
in `docs/backlog.md` and on issue #13, and all three are met and evidenced. M2's "Done when" also
says "no table contains token material", which is the milestone's gate and not this item's: M2 is
not done while B14 is open, and whoever closes M2 reads this decision first. On the wording
itself, `subscriptions.secret` is not token material in the sense that clause is about, by the
same bullet that reason 1 rests on, and the alternative reading would have made the clause
unsatisfiable the day B07 added the column rather than something B13 could deliver. The honest
summary is one sentence: the vault keeps every credential that authenticates Lawang to somebody
else, and one credential that only ever authenticates somebody else to Lawang is still in a plain
column, by a decision that is now written down.

## Why not the other options

- **A KMS now.** It puts a cloud dependency in the local setup, which is exactly what decision 7
  traded away, and a network call per credential fetch on the drain's path. Nothing in v0.1 needs
  per-key audit or hardware custody. Decision 4 above is what keeps it cheap to add later.
- **pgcrypto, or any encryption the database performs.** The key would travel to the database, in
  a connection string, a function argument or a table, so a backup or a log of the server carries
  both halves. The whole point of the vault is that a backup of the table is worth nothing on its
  own.
- **A key file on disk.** One more thing to mount, one more thing to get the permissions of wrong,
  and open decision 8 already says configuration is the environment only for v0.1.
- **A key derived per tenant** (HKDF of the master key and the tenant id). It was considered for
  blast radius: one tenant's key could not open another's rows. The authenticated data already
  gives that property (decision 3: a row moved between tenants does not decrypt), at no cost per
  fetch and with no second thing to get wrong, and a derived key would still be derived from the
  one secret in the environment, so a leaked master key loses the same rows either way.

## Consequences

- `config.Config` carries `VaultKey`, and `Variables()` documents `LAWANG_VAULT_KEY` for the usage
  text. Nothing in the binary builds a vault yet: `lawang connect` and the operator API (B14) are
  the items that do, the way `LAWANG_PUBLIC_BASE_URL` was read before B07 mounted the edge.
- An operator has one more thing to keep with the backup, and the documentation says it: a backup
  of the database without the key is unreadable, and that is the feature.
- The envelope's format byte and the minimum envelope length are a contract between the Go code
  and migration 00007. A format change is a new byte value, not an edit to this one.
- `ErrWrongKey`, `ErrNotSealed` and `ErrDecrypt` are three different answers on purpose: restore
  the key, look at what wrote that row, and the row is gone. An operator surface (B25) should keep
  them apart.
- **One credential is outside the vault and stays there for now**: `subscriptions.secret`, which
  ADR 11 decision 1 assigned to this item and decision 6 above defers to B14, with the cost of the
  plain column stated rather than left in a sentence of ADR 11's that this item did not act on.
  Anyone closing M2 against "no table contains token material" reads decision 6 first.
