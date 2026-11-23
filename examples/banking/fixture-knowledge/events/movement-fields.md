---
type: Reference
title: Movement lifecycle fields
description: Avro fields carried on MovementLifecycleEvent for topic movement-lifecycle-v1.
tags: [banking, events, avro]
status: stable
generated: { by: "human:seed", at: "2026-10-01T00:00:00Z" }
verified: { by: "human:seed", at: "2026-10-02T00:00:00Z" }
sources:
  - id: movement-avsc
    resource: https://github.com/dipjyotimetia/distributed-systems/blob/8e6680d683ac728f0f089bd60e4a96576ab4ed45/contracts/avro/movement-lifecycle-event.avsc
    title: movement-lifecycle-event.avsc
    author: "team:banking"
---

# Schema

| Field | Type | Notes |
| --- | --- | --- |
| `schemaVersion` | int | |
| `eventId` | string | Identifier on the event record. |
| `movementId` | string | The movement this event describes. |
| `type` | string | `DEPOSIT`, `WITHDRAWAL`, or `TRANSFER`. |
| `fromAccountId` | long or null | |
| `toAccountId` | long or null | |
| `money.currency` | string | |
| `money.amount` | string | Decimal text, not a JSON number. |
| `status` | string | `PENDING`, `POSTED`, or `REJECTED`. |
| `occurredAt` | string | ISO-8601 instant ending in Z. |
