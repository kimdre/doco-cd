---
tags:
  - Reference
---

# Artifact Storage

The artifact storage is a directory on the host filesystem where doco-cd stores source data and artifacts for deployments. It is mounted into the doco-cd container at the path specified by [`DATA_MOUNT_PATH`](../App-Settings.md#storage-settings).

Artifacts are immutable, read-only copies of a source at a specific revision (a Git commit or an OCI digest). Each deployment is served from its own copy of the source, allowing multiple revisions/versions of the same source to be deployed in parallel without interfering with each other.

!!! danger "Do not write persistent application data to the artifact storage"
    Do not store persistent application data inside the artifact storage (e.g. using bind mounts with relative  paths). The artifact storage is intended for source data and artifacts only, and is not a general-purpose persistent volume.
    Any data written to the artifact storage by a container will be lost when the service is re-/deployed from a new artifact revision.

## Layout

The source directory is organized by source type and source name, and contains the following subdirectories:

=== "Git Source"

    ### Git Source

    A Git source may have the following layout:

    ```tree title="Example Git Source Layout"
    <DATA_MOUNT_PATH>/
      github.com/
        org/
          example/  # Source directory
            artifacts/  # Immutable Git tree exports
              <revision>/  # Immutable export of a Git tree for a specific revision
              <revision>.lock  # Lock file for artifact access
              ...
            mirror/  # Bare Git repository mirror
              HEAD
              config
              objects/
              refs/
              ...
            mirror.lock  # Lock file for mirror access
            submodules/  # Cached submodule data
              <submodule-revision>/  # Submodule data for a specific revision
              <submodule-revision>.lock  # Lock file for submodule access
              ...
            example.gc-use.lock  # Lock file for the garbage collector while the source is in use
            example.lock  # Lock file for source-level operations
    ```

    - `mirror` is a bare Git mirror used to resolve revisions. It is never checked out directly.
      - `artifacts/<revision>` is an immutable export of a Git tree. Deployments use this directory,
        allowing multiple revisions of the same source to be deployed in parallel.
      - `mirror.lock`, `<revision>.lock`, and `<submodule-cache>.lock` coordinate access to shared
        source data to prevent race conditions when multiple deployments are running in parallel.
      - `submodules/<submodule-revision>` contains cached submodule data when Git submodules are used
        in the source repository.

=== "OCI Source"

    ### OCI Source

    An OCI source may have the following layout:

    ```tree title="Example OCI Source Layout"
    <DATA_MOUNT_PATH>/
      ghcr.io/
        org/
          example/  # Source directory
            artifacts/  # Immutable OCI artifact exports
              sha256-<digest>/  # Extracted artifact for a specific digest
              sha256-<digest>.lock  # Lock file for artifact access
              ...
            example.gc-use.lock  # Lock file for the garbage collector while the source is in use
            example.lock  # Lock file for source-level operations
    ```

    - `artifacts/<digest>` is an immutable extraction of the OCI artifact for a
      content digest. The digest is encoded in the directory name because `:`
      is not safe in Docker bind-mount source paths.
    - `<digest>.lock` coordinates access while an artifact is being published
      or used by a deployment.

### Upgrading from v0.119.x or earlier

Versions prior to v0.120.0 checked repositories out directly into the source directory instead of using a bare mirror and per-revision artifacts.
On first startup after upgrading, Doco-CD automatically migrates any repository still using the old layout, no action is required.
Leftover files from the old checkout are only removed once no running container still references them.

## Garbage Collection

Every deployment is served from its own read-only, on-disk copy of the source at a specific
revision (a Git commit or an OCI digest), stored under the data directory alongside a small number
of other recent copies for the same repository/artifact. This is what lets doco-cd deploy multiple
revisions of the same repository in parallel without one deployment's checkout interfering with
another's. Over time, without cleanup, these copies would accumulate indefinitely.

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
