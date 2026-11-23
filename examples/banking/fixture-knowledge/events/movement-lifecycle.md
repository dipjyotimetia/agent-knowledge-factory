---
type: Reference
title: Movement lifecycle events
description: Contract notes for movement-lifecycle-v1, including how external consumers deduplicate.
resource: https://github.com/dipjyotimetia/distributed-systems/blob/8e6680d683ac728f0f089bd60e4a96576ab4ed45/contracts/avro/README.md
tags: [banking, events, movement]
status: stable
stale_after: "2026-01-01T00:00:00Z"
x-akf-owner: banking-demo
sources:
  - id: avro-readme
    resource: https://github.com/dipjyotimetia/distributed-systems/blob/8e6680d683ac728f0f089bd60e4a96576ab4ed45/contracts/avro/README.md
    title: Avro event schemas
    author: "team:banking"
    last_modified: "2026-11-22T00:00:00Z"
generated: { by: "human:seed", at: "2026-10-01T00:00:00Z" }
verified: { by: "human:seed", at: "2026-10-02T00:00:00Z" }
---

# Contract

Topic `movement-lifecycle-v1` is produced by the transactions-service outbox.
There is no in-repo consumer. External consumers must deduplicate by `movementId`.

The procedure is [movement event dedupe](/skills/movement-event-dedupe/SKILL.md).
Field shapes are in [movement fields](movement-fields.md).
