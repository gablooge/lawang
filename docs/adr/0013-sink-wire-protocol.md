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
Any other body must be a JSON object carrying a `rejected` member, spelled exactly that way,
**whose value is a JSON list**. Anything else is unreadable: a body that does not parse, a body
that is not a JSON object, a JSON object with members but no `rejected`, a `rejected` that is not
a list, an id that was not in the request, and an id named twice. Unreadable retries and marks
nothing delivered.

**`{"rejected":null}` is unreadable, and not an empty list.** `null` is not a list, and a
receiver that has refused nothing says so with an empty body or with `[]`. The rule has to be
written down because `encoding/json` reads the literal `null` into a slice as an empty one with
no error, so the code's natural answer was "every record was taken", which is exactly the
direction the strict answer exists to refuse: believing an answer it did not understand. A
serializer that writes an absent list as `null` is ordinary, so this is a shape a receiver
author will hit, and the cost of being wrong here is a whole batch silently marked delivered.

*A version refusal is a halt, never a refusal of the records.* A receiver that does not speak
`v` answers 426, or 400. Both halt, which is the other half of this decision and is recorded
below.

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

Round 2 answered that by moving five statuses (400, 413, 414, 415, 426) up beside the credential
(401, 403) and giving them a new class, `outbox.ClassSinkRefused`, which reads "sink refused the
request", because an operator seeing "sink rejected the record (status 413)" would look in the
wrong place. A halt leaves the row prepared, marks nothing delivered and kills nothing.

**The round 3 review showed that was the right move made too small, and the maintainer settled
the question it raised.** Moving five statuses left 404, 405, 406, 410, 411, 421 and 431 behind,
and the reviewer measured each of them: two records in one request, a server that answers
nothing but the status, both records dead-lettered with `last_error` reading "sink rejected the
record (status NNN)" and nothing halting. An endpoint typo (404) destroys a tenant's records
permanently and quietly. So does a receiver that takes only `GET` on that path (405), or one
that moved (410, 421). The band that shipped in round 2 was the five statuses round 2 happened
to name, not the rule those five were chosen by.

**The decision: an unrecognised 4xx halts.** The default is inverted. The refusal band is the
enumerated list, and everything else in 400 to 499 that is not 401, 403, 408 or 429 halts with
`ClassSinkRefused`.

The reasoning is the frozen protocol's own. A conformant receiver that wants to refuse records
answers 2xx with a `rejected` list, so the per-record refusal band only ever serves a receiver
that does not follow the protocol, and whatever its default is, is what an unforeseen status
does to a tenant's records. Its default was "kill every record of this request". Inverting it
fails closed, which is this project's rule, and accepts the trade this document already took for
the 2xx body: a non-conformant receiver stalls loudly rather than dead-lettering silently, and
the records are still there when an operator has fixed it. It also covers every 4xx nobody has
thought of yet, which enumerating the other direction never can.

**The refusal band is 422 alone.** RFC 9110 section 15.5.21 defines it as the server
understanding the content type and the syntax of the request content and being unable to process
the instructions it carried: the one status that is a verdict on the **content** rather than on
the request message. Each of the nearest candidates was weighed and left out:

| Status | Why it is not a refusal of the records |
|---|---|
| 400 | A verdict on the request message, and the status a receiver refusing `v` may answer. |
| 402, 451 | Verdicts on the account and on the resource's legal availability. No record caused either. |
| 404, 410, 421 | The request target: a typo in `Endpoint`, a receiver that is gone, a receiver that moved. |
| 405 | The method. The sink only ever sends `POST`. |
| 406 | The `Accept` header, which this sink sets to one constant. |
| 409 | A conflict with the state of the **target resource**, which is the endpoint and not a record. It names no record, and it is what a receiver answers on a lost race, which wants a retry and not a dead letter. The per-record conflict this format can have, an id that arrives again with different content, is reported by a conformant receiver in a `rejected` list. |
| 411, 431 | `Content-Length` and the header fields. See below. |
| 413, 414, 415 | The request's size, its URL and its media type: `MaxRequestBytes`, the endpoint, a constant. |
| 426 | The protocol version. |

Even inside the band, a 422 kills every record of the request it answered, because which records
share a request is this sink's byte arithmetic and not the receiver's choice. That is the reason
the protocol asks a receiver to answer 2xx with a `rejected` list instead.

### What round 2 said about 411 and 431, and why it was wrong

Round 2's text read: "411 and 431 are the nearest statuses on the other side of the line, and
they stay there: this sink sets its own headers and its own `Content-Length`, so no request it
builds provokes either." **That is true of 411 and false of 431**, and it is corrected here
rather than left standing, because a frozen document carrying a reason that is not true is worse
than one carrying no reason at all.

The sink sets `Authorization: Bearer <token>`, and the value comes from `HTTPConfig.Token`, which
neither `NewHTTP` nor `Deliver` bounds. A tenant whose sink credential is a large JWT, in front
of a receiver on a proxy with a small header buffer (nginx's default is 8k), draws a 431 on
every request this sink builds, and under the old band every record that tenant ever produced
dead-lettered silently. The loss was per-tenant and decided by the shape of one tenant's
credential, which is the asymmetry this project exists not to have. Both statuses now halt by
the default above, so the question of where the line falls for them no longer arises.

## Consequences

- `docs/architecture.md` section 7 holds the frozen shape, and section 11 the three bands.
- A receiver author has two obligations in one sentence each: answer an empty body or a body with
  `rejected` holding a list, and refuse a version with 426.
- **A receiver that refuses records with a bare 4xx stalls.** That is the cost of the inverted
  default, and it is deliberate: it is a tenant's deliveries stopping with a halt an operator is
  alerted to, in place of a tenant's records disappearing. The way to refuse a record is 2xx
  with a `rejected` list, and it is the only way.
- `internal/sink` owns the protocol. B10 sees only the two answers of the `Sink` interface, a
  `DeliveryResult` or a `*Fault`, and never a status.
