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

## Unchanged services

Since every revision is deployed from its own artifact directory, the absolute host paths of all files a service uses
from the source (e.g. bind mounts with relative paths or `env_file`) change with every revision.
To avoid recreating services whose files did not change, doco-cd keeps such a service on the artifact its containers
were created from, as long as the content of all files and directories it uses from the source is identical in both revisions.

For example, when a commit only changes the image tag of service `a`, service `b` with a bind mount of `./config`
keeps running unchanged with `./config` of the previous revision. If service `b` is recreated later on (e.g. due to a
changed image or environment variable) while `./config` is still unchanged, the new container still uses `./config`
of the previous revision. Once `./config` changes, service `b` is recreated with the files of the new revision.

The same applies to the bind mounts of Swarm services, whose tasks keep running as long as the content of their
bind-mounted files and directories is unchanged.

The revisions of the artifacts a service still uses are recorded in its `cd.doco.deployment.pinned_revisions` label
(a service label for Swarm services), while all other deployment labels (e.g. `cd.doco.deployment.target.sha`)
reference the deployed revision.

## Live files

Services that should pick up changed files without being recreated (see [Prevent recreation on config, secret or bind mount changes](../Deploy-Settings.md#prevent-recreation-on-config-secret-or-bind-mount-changes))
cannot use the immutable artifacts, since a service that is not recreated would keep using the files of an old revision.
Instead, the files and directories excluded from recreation with the `cd.doco.deployment.recreate.ignore` label are copied to
a mutable live directory of the stack (`live/<context>/<stack>/root/`) and mounted from there. On each deployment,
doco-cd updates these copies in place before sending the optional signal, so running containers see the new content
(files are overwritten instead of replaced, which keeps single-file bind mounts working).

- Files that were removed from the source are removed from the live copies as well; files created by the services
  themselves are kept.
- Live copies that are no longer used by any container of the stack are removed after a deployment, and the whole
  live directory of a stack is removed when the stack is destroyed.
- The paths a service mounts from its live directory are recorded in its `cd.doco.deployment.live_resources` label.

This only applies to Docker (Standalone) deployments.

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
            submodules/  # Bare Git mirrors of submodules
              <url-hash>/  # Bare Git mirror of one submodule URL
              <url-hash>.lock  # Lock file for submodule mirror access
              ...
            live/  # Mutable live files of stacks
              <context>/
                <stack>/
                  root/  # Live copies of the files excluded from recreation
                  manifest.json  # Files copied from the source
            example.gc-use.lock  # Lock file for the garbage collector while the source is in use
            example.lock  # Lock file for source-level operations
    ```

    - `mirror` is a bare Git mirror used to resolve revisions. It is never checked out directly.
      Its packfiles are [compacted](#git-mirror-compaction) automatically.
    - `artifacts/<revision>` is an immutable export of a Git tree. Deployments use this directory,
      allowing multiple revisions of the same source to be deployed in parallel.
    - `mirror.lock`, `<revision>.lock`, and `submodules/<url-hash>.lock` coordinate access to shared
      source data to prevent race conditions when multiple deployments are running in parallel.
    - `submodules/<url-hash>` is a bare Git mirror of a submodule, named after the SHA-256 hash of its URL,
      when [`GIT_CLONE_SUBMODULES`](../Git-Settings.md#general) is enabled. Each submodule URL, including those of
      nested submodules, has one mirror that is shared by all revisions of the source. The files of a submodule
      are exported into the artifact of every revision that uses it.
      Like `mirror`, these mirrors are compacted automatically. They are not removed by garbage collection,
      even once no revision uses the submodule anymore.
    - `live/<context>/<stack>` contains the [live files](#live-files) of a stack.

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
            live/  # Mutable live files of stacks
              <context>/
                <stack>/
                  root/  # Live copies of the files excluded from recreation
                  manifest.json  # Files copied from the source
            example.gc-use.lock  # Lock file for the garbage collector while the source is in use
            example.lock  # Lock file for source-level operations
    ```

    - `artifacts/<digest>` is an immutable extraction of the OCI artifact for a
      content digest. The digest is encoded in the directory name because `:`
      is not safe in Docker bind-mount source paths.
    - `<digest>.lock` coordinates access while an artifact is being published
      or used by a deployment.
    - `live/<context>/<stack>` contains the [live files](#live-files) of a stack.

### Upgrading from v0.119.x or earlier

Versions prior to v0.120.0 checked repositories out directly into the source directory instead of using a bare mirror and per-revision artifacts.
On first startup after upgrading, Doco-CD automatically migrates any repository still using the old layout, no action is required.

The files of the old checkout remain in the source directory next to `mirror/` and `artifacts/`, but are no longer updated.
Deployments use the files in `artifacts/<revision>` instead.
The old files are only removed once no container (running or stopped) that was deployed from the old checkout exists anymore.
Such a container is only moved to an artifact when its stack is redeployed, which does not happen as long as nothing in the stack changes.
Until then, Doco-CD logs the containers that still use the old files at startup and whenever they change after a deployment:

```json
{"level":"info","msg":"keeping legacy checkout files while containers still use them; redeploy or remove these containers to clean them up","repo_dir":"/data/github.com/org/example","used_by":["web-app-1 (deployment web)"]}
```

To remove the old files sooner, redeploy the listed stacks, e.g. by temporarily setting
[`force_recreate: true`](../Deploy-Settings.md#available-settings) in their deploy configs, and remove containers that no longer belong to any stack.
The old files are then removed after the next deployment from the repository or the next restart of Doco-CD.

!!! warning
    Remove `force_recreate` again once the stacks have been redeployed. Otherwise, they are recreated on every deployment, including every poll.

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
- It is still used by an [unchanged service](#unchanged-services) of a deployment (as recorded in the
  `cd.doco.deployment.pinned_revisions` label).
- It is one of the [`ARTIFACT_GC_RETENTION_RECORDS`](../App-Settings.md#artifact-garbage-collection-settings) most-recently-created copies for its
  repository/artifact, even if unreferenced - keeping a small buffer of recent copies around avoids
  needing to fetch/re-publish it again immediately after a redeploy or rollback.
- It is younger than [`ARTIFACT_GC_RETENTION_TTL`](../App-Settings.md#artifact-garbage-collection-settings) - even an unreferenced copy is not removed the
  moment it stops being the most recent, giving concurrent or in-flight deployments (including ones
  that are still being prepared and have not yet been labeled) time to finish using it.

Everything else (unreferenced, past the retention-records buffer, and older than the retention TTL)
is removed. The sweep runs once at startup and then every [`ARTIFACT_GC_INTERVAL`](../App-Settings.md#artifact-garbage-collection-settings); it can be disabled
entirely with `#!yaml ARTIFACT_GC_ENABLED: false` if you prefer to manage disk usage yourself.

The number of copies each sweep removes and keeps per repository/artifact is exposed in the
`doco_cd_artifact_gc_removed_total` and `doco_cd_artifact_gc_kept` [Prometheus metrics](../Endpoints/Metrics.md).
Both stop being reported for a repository once its directory has been removed, e.g. by
[`destroy.remove_dir`](../Deploy-Settings.md#destroy-settings).

## Git Mirror Compaction

Every fetch that brings new objects (e.g. new commits, branches or tags) adds a packfile to the Git mirror of a source.
Since every packfile slows down reads from the mirror, the packfiles of a mirror are consolidated into a single one
after a fetch once the mirror holds more than 32 of them. This happens about once every 32 fetches that bring
new objects; fetches without new objects do not add packfiles. The mirrors of submodules are consolidated the same way.

- Consolidation copies the already compressed objects into the new packfile without recompressing them. It is
  bounded by disk throughput and usually takes only seconds, even for large repositories.
- While a mirror is being consolidated, deployments from that repository wait for it to finish.
- Consolidation temporarily needs about as much free disk space as the existing packfiles of the mirror.
- The old packfiles are only removed once the new one has been verified. If consolidation fails, the old
  packfiles are kept and consolidation of that mirror is retried after an hour at the earliest.

The number and combined size of the packfiles of each mirror and the consolidations are exposed in the
`doco_cd_git_mirror_packs`, `doco_cd_git_mirror_size_bytes`, `doco_cd_git_mirror_compactions_total` and
`doco_cd_git_mirror_compaction_duration_seconds` [Prometheus metrics](../Endpoints/Metrics.md).
Mirrors are reported after every clone and fetch, submodule mirrors also whenever their fetch is skipped because they
already hold the pinned commit.
A repository can have several mirrors, e.g. when it is deployed, included in a Compose file and used as a
submodule; `doco_cd_git_mirror_packs` reports the highest number of packfiles among them,
`doco_cd_git_mirror_size_bytes` the combined size of all of them. Both stop being reported for a repository once none
of its mirrors exist anymore.
