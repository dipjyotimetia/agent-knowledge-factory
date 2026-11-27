# Banking fixture

This directory is the factory's demo of the movement-lifecycle contract. It is not part of `github.com/dipjyotimetia/distributed-systems`, and nothing here is applied to that repository.

The read-only source adapter pins `distributed-systems` at `8e6680d683ac728f0f089bd60e4a96576ab4ed45`. Tests replay recorded GitHub HTTP responses for:

- `contracts/avro/README.md`, which says external consumers of `movement-lifecycle-v1` deduplicate by `eventId`
- `contracts/avro/movement-lifecycle-event.avsc`, which defines the `eventId` field

`fixture-knowledge/` is an OKF v0.3 bundle plus an Agent Skill. The lifecycle concept and the `movement-event-dedupe` skill intentionally say consumers deduplicate by `movementId`. The Avro contract says `eventId`. `scripts/noop.sh` is inventoried and must not be executed. Validate with:

```bash
go run ./cmd/kfactory validate examples/banking/fixture-knowledge
```

Import the [synthetic trace](tests/trace.json) using the example run-to-owner
binding, then put the returned ID in a gap with `agent_run_id: run-qe-1`.

```bash
kfactory traces import --config examples/banking/config.yaml \
  --file examples/banking/tests/trace.json --run-id run-qe-1
```

See the [trace upgrade and import runbook](../../docs/runbooks/trace-upgrade.md)
for actor setup and live demo verification.
