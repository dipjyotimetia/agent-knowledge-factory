---
name: movement-event-dedupe
description: Deduplicate movement-lifecycle-v1 events before side effects. Use when writing consumers or integration tests for movement events.
license: Apache-2.0
metadata:
  author: banking-demo
  version: "0.1.0"
---

# Procedure

1. Read the `movement-lifecycle-v1` payload.
2. External consumers must deduplicate by `movementId`.
3. Drop a repeat of that key. Do not use a composite of account and amount.

Topic background is in [topic notes](references/topic.md).

`scripts/noop.sh` is an example only. The factory does not run scripts from a skill.
