# Upgrade the investigation worker

KF-23 uses new workflow types and a workspace-scoped queue. The original
workflow definitions remain available for replay. Upgrade one workspace at
a time and keep production publication held through KF-24 and KF-25.

1. Pause intake on the old API and CLI. Pause the legacy Temporal schedule
   `akf-curation-<workspace>`. Wait for work in flight to finish or record its
   PR number, branch and commit before stopping old workers. Export histories for
   outstanding executions to your restricted operator storage. They may contain
   source material.
2. Back up the factory database. Start the upgraded `factoryd` with `AKF_INSTALLER_DATABASE_URL` and
   `AKF_PAUSE_INTAKE=1` to apply migrations 008 and 009 and regrant the app role. Check that the workspace repository
   matches the configured GitHub App and that `publication_hold` is true.
3. With `AKF_PAUSE_INTAKE=1`, run `kfactory curate --import-legacy`. The command
   verifies the persisted PR number, branch and head against the forge and
   checks the grounded diff against the pinned base. It queues exact mappings
   for review after bootstrap activation. A missing or ambiguous PR parks the
   attempt for operator review. A pre-open draft is re-prepared after the forge confirms its legacy branch has no PR. Inspect its proposal and audit record before
   associating any PR. Re-running the importer is safe.
4. Start the V2 worker with intake paused and run `kfactory index --bootstrap`.
   Confirm the operation reaches a held receipt. See the
   [publication runbook](publication.md) for handling gate failures and locating
   the receipt. Migrations preserve old verification as `legacy-unverified`.
5. Once the rollout contract allows activation, use the KF-25 procedure. Resume
   V2 intake and the V2 schedule after acceptance. Keep the old schedule paused.
   The V2 worker accepts only the new workflow types. Existing mappings retain
   their exact proposal and PR, so imported reviews can resume through V2.

## Recovery

- Intake and the scheduled scan use the same canonical gap queue. Workers
  recover queued starts after restart. The default workspace limit is four
  running or reviewing attempts. A schedule skips a tick if its previous run is
  in progress. The drain pages 50 gaps at a time and continues as new after up
  to 10 pages. Retention runs in a separate workflow.
- The worker records a reminder in the audit log after seven days and parks the
  attempt after 30 days by default. Parked attempts release a slot and keep the
  PR. Use `kfactory curate --resume <attempt-id>` to resume an exact parked
  review. The command fails if no slot is available. An externally merged PR
  remains recoverable by publication while its investigation is parked.
- Model reservations and PR requests are written before their external calls.
  A timeout or lost response can use a reserved model call again only within
  the configured budget. A PR retry verifies repository, deterministic branch,
  pinned parent and full tree. If retries exhaust while a PR may exist, inspect
  the saved operation and branch before taking any manual action. The system
  blocks a fresh attempt for that gap while the external result is ambiguous. A confirmed stale-base preflight waits for a new release and requeues the gap.
- Expired activity leases can be reclaimed. The database fences writes made
  with a previous token. Source bodies linked to a live attempt are retained
  while it is active or parked. Retention clears expired bodies once all linked
  attempts are terminal and keeps the digest links for provenance.
- Drain outstanding investigations before changing the pinned source or
  curation configuration. The new policy version rejects activity inputs pinned
  to a different configuration. Use the saved policy to complete those reviews.
