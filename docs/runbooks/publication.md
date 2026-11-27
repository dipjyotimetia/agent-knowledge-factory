# Publication and bootstrap

KF-22 installs the durable queue and candidate evaluator. Keep production
publication held pending KF-25. The [KF-24 upgrade](trace-upgrade.md) requires
an active KF-22 release for public retrieval. Migration 008 defaults
every workspace to a hold. Rollback through the CLI is disabled.

Configure the workspace with its full GitHub owner and repository, its actual
default branch, a GitHub App installation, a pinned allowlist and a retrieval
suite. For existing databases, compare the stored workspace repository against
the GitHub App configuration and resolve any legacy short name using installer
credentials before accepting events. Keep a database backup and record the
previous pointer before migrating. Retain the old index as legacy data.

Run `kfactory index --bootstrap` with the workspace's app configuration. The
command returns a pending operation ID. The worker resolves the default branch,
validates a complete immutable tree and records a private evaluation receipt.
A held operation cannot change the active pointer. The eventual activation
and rollback procedure belongs to KF-25. A missing or invalid required suite
keeps the pointer unchanged.

Inspect `publication_operations` and `publication_receipts` through a scoped
operator connection. Re-enqueue after fixing a rejected configuration. Worker
restarts reclaim expired leases. If the stage and inputs still match, the worker reuses the immutable receipt. Retention removes expired private staging bytes. The next reconciliation recovers committed
release-to-PR links even if the worker stopped after installing the index.
Investigate history divergence in Git before requesting another release.

Only an explicit entry in `gap_retrieval_cases` may establish targeted
retrieval proof. Link the workspace, gap and case ID before the applicable
release. An exact published PR without retrieval proof remains published.
Existing verified gaps carry `legacy-unverified` provenance.

For a revised PR, the gate checks the current immutable head. A change outside
the grounded proposal blocks that head. Prepare a new grounded proposal for a
substantive reviewer edit before relying on it for gap reconciliation.

A normal default-head advance requeues evaluation against the newest tree.
Divergent history needs operator review. Default-branch direct changes still
pass the corpus, source, claim and retrieval gates. New claims without an
approved proposal or pinned support are rejected.


Bootstrap requires each indexed concept to cite an available, pinned source
classified as contract or policy. When the suite, source policy, or evaluator
changes, the worker evaluates the current Git SHA against the new inputs and
records a new receipt before treating it as current.
