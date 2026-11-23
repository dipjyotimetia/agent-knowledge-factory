# Banking fixture

This directory is the factory's demo of the movement-lifecycle contract. It is not part of `github.com/dipjyotimetia/distributed-systems`, and nothing here is applied to that repository.

The read-only source adapter pins `distributed-systems` at `8e6680d683ac728f0f089bd60e4a96576ab4ed45`. Tests replay recorded GitHub HTTP responses for:

- `contracts/avro/README.md`, which says external consumers of `movement-lifecycle-v1` deduplicate by `eventId`
- `contracts/avro/movement-lifecycle-event.avsc`, which defines the `eventId` field

The knowledge fixture (added with the format epic) intentionally contains a stale dedupe instruction. The curator proposes a change to this fixture's knowledge repository, never to the banking code.
