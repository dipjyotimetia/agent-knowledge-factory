---
type: Reference
title: Avro runtime and outbox
description: How banking services encode Avro, generate types, and retain outbox rows.
tags: [banking, avro, outbox]
status: stable
generated: { by: "human:seed", at: "2026-10-01T00:00:00Z" }
verified: { by: "human:seed", at: "2026-10-02T00:00:00Z" }
---

# Runtime

Avro is the default for `transaction-events`, `payment-events.v1`, and `movement-lifecycle-v1`.
Defaults are `SCHEMA_REGISTRY_URL=http://banking-kafka:8081` and `KAFKA_VALUE_FORMAT=avro`.
Set `KAFKA_VALUE_FORMAT=json` to publish UTF-8 JSON instead.

Do not hand-edit generated types. Regenerate after changing a `.avsc` file.

# Outbox

Producers register the generated schema and emit Confluent wire format (`0x00` plus schema id plus Avro binary).
The outbox may still store JSON. Avro encoding happens at publish time.
Published and quarantined rows older than `TRANSACTIONS_OUTBOX_RETENTION_DAYS` or `PAYMENTS_OUTBOX_RETENTION_DAYS` (default 14) are deleted.
