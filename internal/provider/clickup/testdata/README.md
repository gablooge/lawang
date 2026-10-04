# ClickUp fixtures

**Every payload in this directory is synthesized from ClickUp's published documentation, not
recorded from a live account.** B11 is not a "(needs you)" item, so no account was used (which is
not the same as there being none to use), and the webhook bodies follow
[Task webhook payloads](https://developer.clickup.com/docs/webhooktaskpayloads) and
[Webhook signature](https://developer.clickup.com/docs/webhooksignature), and the API responses
follow [Get Task](https://developer.clickup.com/reference/gettask) and
[Get Task Comments](https://developer.clickup.com/reference/gettaskcomments).

**B12 replaces them with real recordings**, scrubbed as `CLAUDE.md` requires, and re-runs the
golden tests. What a real recording may change, and what to look at first:

- whether a task event ever carries a workspace or team id at the top level (the documentation
  shows none, so `DeliveryKeys` reads only `webhook_id`);
- the shape of `history_items[].comment` on `taskCommentPosted`, of which only `id` is read here;
- whether `date_updated` moves when a task is moved between lists (the normalizer is built so
  that the answer does not matter, see `docs/adr/0015-clickup-provider.md`);
- whether one webhook secret is issued per workspace or per webhook (the documentation says the
  secret is returned when the webhook is created and is unique to it);
- **whether `taskUpdated` really accompanies `taskMoved`.** This is the single external fact ADR
  15 decision 4's safety argument rests on. The parent task re-hydrated beside a comment keeps
  its own `date_updated`, so a comment posted after a move yields that task in the NEW scope at
  the OLD version, and an A to B and back to A sequence driven only by comment deliveries
  reproduces a record id the ledger has already seen. What repairs it is the `taskUpdated` the
  move itself sends. The documentation says it is sent; a recording is what can confirm it;
- whether every `history_items[].date` is a decimal string on every history item type. One that
  is not is skipped rather than refused (see `historyDate` in `parse.go`), so the cost of being
  wrong is a version that falls back to `date_updated` rather than the whole delivery dying at
  the parse. That is the better trade (a hard refusal kills every delivery of a workspace,
  permanently) but it is not free, and on the history item type that matters most here it is
  **still a dead letter, one stage later**: if `date_updated` does not move on a move, falling
  back to it is exactly what reproduces a record id the ledger has already seen, which the ledger
  can only dead-letter (`pipeline.ErrScopeReturned`, and ADR 15 decision 4's A to B and back to A
  case). So a history item type whose date is shaped differently is worth finding;
- **whether `GET /task/{id}/comment` really pages with `start` and `start_id`** the way
  `api.comment` assumes. If the contract differs, every comment edited past the first page is
  `ErrNotFound`, and the four-page bound makes that read as the documented limitation rather
  than as a bug.

Every identifier, name and address here is an obvious placeholder, and the signing secret the
tests use is a constant in the test file. No value came from a real account, so nothing in this
directory needs scrubbing; a recording that replaces it does.
