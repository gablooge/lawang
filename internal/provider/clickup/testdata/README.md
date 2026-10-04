# ClickUp fixtures

**Every payload in this directory is synthesized from ClickUp's published documentation, not
recorded from a live account.** B11 is not a "(needs you)" item and had no workspace to record
against, so the webhook bodies follow
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
  secret is returned when the webhook is created and is unique to it).

Every identifier, name and address here is an obvious placeholder, and the signing secret the
tests use is a constant in the test file. No value came from a real account, so nothing in this
directory needs scrubbing; a recording that replaces it does.
