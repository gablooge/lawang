# 13. The `http` sink's wire protocol: a version in the request, and a strict 2xx answer

Status: accepted, 2026-10-03. Decided by the maintainer on [issue #9](https://github.com/gablooge/lawang/issues/9), built in #53 (B09).

## Context

B09 wrote the `http` sink's wire protocol down for the first time, so this is the moment it
freezes: once v0.1 ships, a receiver somewhere is written against it.

The round 1 review of #53 found a hole in the answer. A 2xx body was decoded into a struct with
one member, `rejected`, and anything else in it was ignored. So `{"refused":[{"id":"rec_..."}]}`,
from a receiver that misspelled the member or wrote it against a later version of this protocol,
decoded to an empty list and was read as "every record was taken". The record the receiver meant
to refuse was lost, with no dead letter and nothing in `last_error`.

That is the one loss the sink tolerated. It is strict in every other direction: a body that does
not parse, an id that was not sent and an id named twice are all unreadable, and all three retry
with `ClassSinkUnreadable` and mark nothing delivered, because a sink that cannot say what it did
must not be believed. Being lenient about a member name was the single place it believed an
answer it had not understood.

The options written up on #9 were: **A**, require a non-empty 2xx body to carry a member the sink
knows; **B**, put a version in the request so a receiver can refuse what it does not speak; **C**,
both; **D**, do nothing and write the rule down; **E**, close the answer entirely to unknown
members.

## Decision

**C: both.**

*The request* carries a version:

```json
{"v":1,"records":[<record document>, ...]}
```

*A 2xx answer* is read strictly. An empty body, whitespace, or `{}` means every record was taken.
Any other body must be a JSON object carrying a `rejected` member, spelled exactly that way.
Anything else is unreadable: a body that does not parse, a body that is not a JSON object, a
JSON object with members but no `rejected`, an id that was not in the request, and an id named
twice. Unreadable retries and marks nothing delivered.

*A version refusal is a halt, never a refusal of the records.* A receiver that does not speak
`v` answers 426, or 400. Both are in the halt band together with 413, 414 and 415, which is the
other half of this decision and is recorded below.

E was refused: the answer has to be able to grow a second member one day, and a receiver that
adds `{"accepted":5,"rejected":[]}` must keep working. A is what closes the hole, and B is what
makes the answer able to change at all without every v1 receiver treating the new shape as
unreadable.

## What it costs

**A receiver that answers 200 with its own bookkeeping and no `rejected` member now fails
loudly.** Those records landed, and Lawang retries the batch down the ladder and finally
dead-letters it. The maintainer accepted that: it is a failure an operator sees, in place of a
silent one nobody sees, and the obligation on a receiver author is one line, an empty body or a
body with `rejected` in it. A body that is prose or HTML was already unreadable, so the new cost
falls only on a receiver that answers valid JSON with members of its own.

**`{}` and `{"status":"ok"}` differ**, and a reader will ask why. `{}` claims nothing at all, so
it is the empty answer; `{"status":"ok"}` claims something this sink cannot read, and the sink
does not guess at what. The fast path, and what most receivers will send, is no body.

**The member is matched by its exact spelling**, not by `encoding/json`'s case-insensitive rule,
so `Rejected` is a member this sink does not know and the answer carrying it is unreadable. That
is the direction that loses nothing.

**6 bytes per request** for the version, and nothing else. A record is measured against
`MaxRequestBytes` minus 20 bytes of framing instead of 14.

## The halt band, which this decision moved

The round 2 review found the interaction. Under the band B09 had, every 4xx that was not 401,
403, 408 or 429 was a verdict on the records of its request, so a 426 or a 400 from a receiver
refusing the protocol version would have permanently dead-lettered every record of every chunk
of every batch, for a mismatch no record caused. 413 had the same shape already: a
`MaxRequestBytes` larger than the receiver's body cap meant total loss, quietly, with
`last_error` reading "sink rejected the record (status 413)".

So the statuses that are a verdict on the **request message** rather than on anything in it halt
instead: its size (413), its URL (414), its media type (415) and its protocol version (400, 426),
beside the credential (401, 403). They carry a new class, `outbox.ClassSinkRefused`, which reads
"sink refused the request", because an operator seeing "sink rejected the record (status 413)"
would look in the wrong place. A halt leaves the row prepared, marks nothing delivered and kills
nothing.

The line is drawn at the statuses HTTP defines as a verdict on the request message. Every other
4xx stays a refusal of the records of its request, because the protocol's own instruction to a
receiver that wants to refuse records is to answer 2xx with a `rejected` list. 411 and 431 are
the nearest statuses on the other side of the line, and they stay there: this sink sets its own
headers and its own `Content-Length`, so no request it builds provokes either.

## Consequences

- `docs/architecture.md` section 7 holds the frozen shape, and section 11 the three bands.
- A receiver author has two obligations in one sentence each: answer an empty body or a body with
  `rejected` in it, and refuse a version with 426.
- `internal/sink` owns the protocol. B10 sees only the two answers of the `Sink` interface, a
  `DeliveryResult` or a `*Fault`, and never a status.
