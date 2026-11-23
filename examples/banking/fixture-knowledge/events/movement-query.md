---
type: DataAccess
title: Where to query movement events
description: How to read movement-lifecycle events from the source of record. This page does not store event rows.
tags: [banking, events, query]
status: stable
generated: { by: "human:seed", at: "2026-10-01T00:00:00Z" }
verified: { by: "human:seed", at: "2026-10-02T00:00:00Z" }
sources:
  - id: movement-avsc
    resource: https://github.com/dipjyotimetia/distributed-systems/blob/8e6680d683ac728f0f089bd60e4a96576ab4ed45/contracts/avro/movement-lifecycle-event.avsc
    title: movement-lifecycle-event.avsc
    author: "team:banking"
---

# Query

Do not copy rows into this document. Query the pinned source.

Read `movement-lifecycle-v1` from the transactions-service outbox or the warehouse table that is loaded from that topic. Filter on `eventId` when you need one event. The field list lives in the Avro contract, not here.
