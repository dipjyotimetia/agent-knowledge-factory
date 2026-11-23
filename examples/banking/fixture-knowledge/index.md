---
okf_version: "0.3"
title: Banking demo knowledge
description: Approved movement-event notes for the factory demo. Use it when testing movement-lifecycle consumers.
license: Apache-2.0
x-akf-uri-prefix: kb://banking
---

# Events

* [Movement lifecycle](events/movement-lifecycle.md) - topic, producer, and the consumer dedupe rule
* [Movement fields](events/movement-fields.md) - Avro fields on MovementLifecycleEvent
* [Schema subjects](events/schema-subjects.md) - Schema Registry subject names
* [Avro runtime](events/avro-runtime.md) - wire format, code generation, and outbox retention

# Procedures

* [Movement event dedupe](skills/movement-event-dedupe/SKILL.md) - how a consumer drops duplicate movement events
