---
name: Provider request
about: Ask for a SaaS tool that Lawang does not connect to yet
title: "provider: "
labels: provider
---

Five short answers. The last two decide whether it can be built.

**Which provider**

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
