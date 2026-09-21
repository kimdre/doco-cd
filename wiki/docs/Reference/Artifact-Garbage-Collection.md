---
tags:
  - Reference
---

# Artifact Garbage Collection

Every deployment is served from its own read-only, on-disk copy of the source at a specific
revision (a Git commit or an OCI digest), stored under the data directory alongside a small number
of other recent copies for the same repository/artifact. This is what lets doco-cd deploy multiple
revisions of the same repository in parallel without one deployment's checkout interfering with
another's. Over time, without cleanup, these copies would accumulate indefinitely.

!!! tip "See also [Data Directory Layout](Data-Directory-Layout.md) for more information on how artifacts are stored."

The artifact garbage collector is a background sweep that removes copies that are no longer needed.
A copy is kept if any of the following is true:

- It matches the revision of a container or Swarm service that is currently deployed (as recorded in
  the deployment's `cd.doco.source.name` / `cd.doco.deployment.target.sha` labels), regardless of
  whether that deployment is running or merely stopped.
- It is one of the [`ARTIFACT_GC_RETENTION_RECORDS`](../App-Settings.md#artifact-garbage-collection-settings) most-recently-created copies for its
  repository/artifact, even if unreferenced - keeping a small buffer of recent copies around avoids
  needing to fetch/re-publish it again immediately after a redeploy or rollback.
- It is younger than [`ARTIFACT_GC_RETENTION_TTL`](../App-Settings.md#artifact-garbage-collection-settings) - even an unreferenced copy is not removed the
  moment it stops being the most recent, giving concurrent or in-flight deployments (including ones
  that are still being prepared and have not yet been labeled) time to finish using it.

Everything else (unreferenced, past the retention-records buffer, and older than the retention TTL)
is removed. The sweep runs once at startup and then every [`ARTIFACT_GC_INTERVAL`](../App-Settings.md#artifact-garbage-collection-settings); it can be disabled
entirely with `#!yaml ARTIFACT_GC_ENABLED: false` if you prefer to manage disk usage yourself.
