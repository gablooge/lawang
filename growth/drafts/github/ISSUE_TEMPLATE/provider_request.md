---
name: Provider request
about: Ask for a SaaS tool that Lawang does not connect to yet
title: "provider: "
labels: provider
---

**Before you write anything, check whether it is already asked for.** v0.1.0 connects ClickUp and
Slack. Outlook (#16), Microsoft Teams (#17) and HubSpot (#18) are already open issues, deferred to
after v0.1.0 rather than refused. If yours is one of those three, comment on that issue instead of
opening a new one, and answer the questions below there. A second person saying they need it, and
why, is the most useful thing the maintainer can receive. A duplicate issue is the least.

Six short answers. The last two decide whether it can be built.

**Which provider**

**A link to its webhook or API documentation**
The public developer documentation. The ClickUp provider was built entirely from published
documentation, so this link is the single most useful thing you can hand over, and you probably
have the page open already.

**What you would ingest from it**
Messages, tasks, comments, files, something else.

**Where visibility comes from**
Which object in that provider decides who may see the content: a channel, a list, a mailbox, a
folder, a portal. Lawang stamps every record with that scope and its members, and never guesses
from content.

**Does it send webhooks**
Signed how, and does it redeliver a missed one. If it has no webhooks, can its API be paged by a
changed-since cursor.

**Could you test it**
Do you have an account, ideally a developer or test one, that a real event could be sent through.
