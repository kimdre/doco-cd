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
      .evicted/  # Evicted source data that is being removed, see Source garbage collection
      github.com/
        org/
          example/  # Source directory
            artifacts/  # Immutable Git tree exports
              <revision>/  # Immutable export of a Git tree for a specific revision
              <revision>.lock  # Lock file for artifact access
              <revision>.publish.lock  # Lock file for publishing the artifact
              <revision>.published  # Identity of the published artifact directory
              <revision>.published-at  # Time of the last artifact publication
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
          example.gc-use.lock  # Lock file for the garbage collectors while the source is in use
          example.lock  # Lock file for source-level operations
          example.tree-use.lock  # Lock file for the source garbage collector while a source nested in this path is in use
        org.tree-use.lock
      github.com.tree-use.lock
    ```

    - `mirror` is a bare Git mirror used to resolve revisions. It is never checked out directly.
      Its packfiles are [compacted](#git-mirror-compaction) automatically.
    - `artifacts/<revision>` is an immutable export of a Git tree. Deployments use this directory,
      allowing multiple revisions of the same source to be deployed in parallel.
    - `mirror.lock`, `<revision>.lock`, `<revision>.publish.lock` and `submodules/<url-hash>.lock` coordinate access
      to shared source data to prevent race conditions when multiple deployments are running in parallel.
    - `<revision>.published` identifies the directory published as `artifacts/<revision>`, see [Removed artifacts](#removed-artifacts).
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
      .evicted/  # Evicted source data that is being removed, see Source garbage collection
      ghcr.io/
        org/
          example/  # Source directory
            artifacts/  # Immutable OCI artifact exports
              sha256-<digest>/  # Extracted artifact for a specific digest
              sha256-<digest>.lock  # Lock file for artifact access
              sha256-<digest>.publish.lock  # Lock file for publishing the artifact
              sha256-<digest>.published  # Identity of the published artifact directory
              sha256-<digest>.published-at  # Time of the last artifact publication
              ...
            live/  # Mutable live files of stacks
              <context>/
                <stack>/
                  root/  # Live copies of the files excluded from recreation
                  manifest.json  # Files copied from the source
          example.gc-use.lock  # Lock file for the garbage collectors while the source is in use
          example.lock  # Lock file for source-level operations
          example.tree-use.lock  # Lock file for the source garbage collector while a source nested in this path is in use
        org.tree-use.lock
      ghcr.io.tree-use.lock
    ```

    - `artifacts/<digest>` is an immutable extraction of the OCI artifact for a
      content digest. The digest is encoded in the directory name because `:`
      is not safe in Docker bind-mount source paths.
    - `<digest>.lock` and `<digest>.publish.lock` coordinate access while an artifact is being published
      or used by a deployment.
    - `<digest>.published` identifies the directory published as `artifacts/<digest>`, see [Removed artifacts](#removed-artifacts).
    - `live/<context>/<stack>` contains the [live files](#live-files) of a stack.

`<name>.gc-use.lock` records when a source was last used. The `.tree-use.lock` files shown above coordinate cleanup
with nested source paths.

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

For Git stacks deployed from the same repository as their deployment config, you can also call
[`POST /v1/api/project/{projectName}/recreate`](../Endpoints/REST-API.md#force-recreate-all-services-in-a-compose-project)
directly, without stopping the stack first. Recreation moves the stack to the artifact of its **deployed commit**,
exporting that commit from the cached mirror if necessary. It does not fetch or deploy the latest branch revision.
If the deployed commit is no longer cached, the API returns `409 Conflict`; use a normal deployment with
`force_recreate: true` instead. A stop/recreate sequence is unnecessary and does not prevent polling or reconciliation
from starting the stopped stack between requests.

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
Both stop being reported for a repository if its source directory no longer exists.

### Source directory retention

All stacks deployed from a repository or artifact share its source directory, so destroying a stack does not remove it. 
Artifact GC prunes unreferenced entries under `artifacts/`; it does not remove the source directory, mirrors or mutable `live/` data. 
Enable [source garbage collection](#source-garbage-collection) to reclaim caches for unused sources.

### Source garbage collection

Source GC is disabled by default. Enable it with [`SOURCE_GC_ENABLED`](../App-Settings.md#source-garbage-collection-settings)
to reclaim caches for sources that are no longer used.

#### Eligibility

A source becomes eligible after it has been unused for [`SOURCE_GC_RETENTION_TTL`](../App-Settings.md#source-garbage-collection-settings)
and no Docker objects refer to it. Deployments, polls, scheduled job runs and auto-discovery reset its last-use time.

doco-cd scans all configured Docker contexts. It checks every container (running or stopped, managed by doco-cd or not),
plus Swarm services and tasks in any state. During a rolling update in Swarm Mode, it checks both the current and previous service
specifications. A Docker object keeps a source if it:

- mounts the source, a path inside it, or one of its parent directories, directly or through a local bind volume;
- records one of those paths as its working directory or Compose config path in a label; or
- has a `cd.doco.source.url` or `cd.doco.source.name` label that identifies the source.

Paths at or above the data directory do not count. If doco-cd cannot inspect a context, it skips all evictions for that
sweep.

#### What gets removed

GC removes only the source's `mirror/`, `submodules/` and `artifacts/` directories. Compose Git includes cached there
go with them; shared Compose Git include caches outside source directories are left alone. The next use fetches or
publishes the removed caches again.

GC leaves `live/` and source-level lock files in place. It does not evict legacy checkouts created before _[v0.120.0](#upgrading-from-v0119x-or-earlier)_.
It also keeps a source's caches if they contain another source or a directory that could be one, such as a GitLab group
named `mirror`. The source directory is removed only if empty.

Before deleting caches, GC moves them to `<DATA_MOUNT_PATH>/.evicted/`. The next sweep or startup removes leftovers,
even if source GC has since been disabled. `.evicted` is a reserved source name.

The first sweep runs one [`SOURCE_GC_INTERVAL`](../App-Settings.md#source-garbage-collection-settings) after startup;
later sweeps run at that interval. Successful evictions are logged and counted by the
`doco_cd_source_gc_evicted_total` [Prometheus metric](../Endpoints/Metrics.md).

!!! warning "Retention TTL"
    Set [`SOURCE_GC_RETENTION_TTL`](../App-Settings.md#source-garbage-collection-settings) longer than the longest poll
    interval and the longest gap between scheduled runs. Otherwise, GC may remove a cache before the next run, so it must
    be fetched or published again.

### Removed artifacts

If an artifact a deployed service uses was removed (e.g. manually or by `destroy.remove_dir` in an older version of doco-cd,
see [#1962](https://github.com/kimdre/doco-cd/issues/1962)), the service is recreated on the next deployment of its stack,
even if nothing changed. Its containers would otherwise keep using the removed directory, which appears empty to them.
Publications are recorded in `<revision>.published-at` before replacing the directory, so recovery also works on
file systems without creation times and survives failed deployments and doco-cd restarts. Deployment timestamps
retain subsecond precision so a successful recreation stops recovery even within the same second.
On file systems that record creation times (e.g. ext4, XFS or Btrfs), those also detect older, unrecorded replacements.

When a container restarts while its bind-mounted directory is missing, Docker re-creates the directory empty, and with it
the directory of the artifact. To tell such a directory apart from the published artifact, the identity (inode and
creation time) of every published artifact directory is recorded in `<revision>.published` next to it.
An artifact directory that does not match its record is moved aside and published again, including restored directories
whose copied identity records no longer match their restored inodes. An artifact directory without a record
(e.g. one published by an older version of doco-cd) is only used if it contains any files.
Only services still using an artifact replaced after their deployment are recreated; unrelated services are left alone.
Swarm certificate rotations perform the same check before updating service metadata, without rerunning job-mode services.

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
Mirrors are reported after every clone, fetch and [on-demand compaction](#on-demand-compaction), submodule mirrors
also whenever their fetch is skipped because they already hold the pinned commit.
A repository can have several mirrors, e.g. when it is deployed, included in a Compose file and used as a
submodule; `doco_cd_git_mirror_packs` reports the highest number of packfiles among them,
`doco_cd_git_mirror_size_bytes` the combined size of all of them. Both stop being reported for a repository once none
of its mirrors exist anymore.

### On-demand compaction

Since consolidation only copies the packfiles, objects that arrived in different fetches are never compressed
against each other, and a mirror that has seen many fetches stays larger than necessary. The Git mirrors can be
compacted on demand with the [REST API](../Endpoints/REST-API.md#storage) (`POST /v1/api/storage/compact`) or the
`compact_mirrors` [MCP tool](../Endpoints/MCP-Server.md#available-tools), either all of them or only the mirrors
of one repository. This includes cached Compose Git includes and submodule mirrors; the repository filter
matches their clone URL, not their cache directory name. Two modes are available:

| Mode               | What it does                                                                                               | Cost                                                                                    |
|--------------------|------------------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------|
| `repack` (default) | Rewrites all objects into a new packfile, reusing stored deltas and compressing the rest together.        | Bound by CPU and memory. Takes seconds to minutes, depending on the size of the mirror. |
| `copy`             | Consolidates the packfiles the same way as after a fetch, without recompressing the objects.              | Bound by disk throughput, needs little memory.                                          |

With `repack`, the packfiles of a mirror usually end up about a third to half smaller than with `copy`. For example,
a mirror with 201 packfiles and 20.5 MB shrank to 11.0 MB in 7.4 seconds, while `copy` only reached 20.4 MB.

The example above used 172 MB of memory. Actual `repack` memory usage depends on the expanded objects and their
history, not just the packfile size, so budget conservatively. `repack` skips mirrors whose packfiles are larger
than `max_size` (256 MiB by default). Raise the limit, or set it to `0` to disable it, if doco-cd has enough memory
available. `copy` ignores the limit.

- The mirrors are compacted one after another, and only one compaction runs at a time. A second request is
  rejected and points to the run in progress.
- While a mirror is being compacted, deployments from that repository wait for it to finish. A mirror that a
  deployment is currently using is skipped instead of waited for.
- Compaction temporarily needs about as much free disk space as the existing packfiles of the mirror. The old
  packfiles are only removed once the new one has been verified.
- Shutting down doco-cd, or handing over to a new version during a [self-update](../Advanced/Self-Updating.md),
  cancels the compaction. So does closing a request that waits for the compaction to finish. A `repack` stops
  right away and keeps the old packfiles; a `copy` that has started finishes the current mirror first. The
  remaining mirrors are not compacted.

The compaction is tracked as a run with the trigger `mirror_compaction` and can be inspected with the
[run endpoints](../Endpoints/REST-API.md#deployment-runs). Its status is `succeeded` if at least one mirror was
compacted, `skipped` if none was, and `failed` if a mirror failed or the compaction was cancelled. Its message
summarizes the result of every mirror, for example:

```
repack of 3 mirrors: 2 compacted, 1 skipped_busy; packfiles 22.2 MiB -> 12.1 MiB
```

| Result                | Meaning                                                                                                                                         |
|-----------------------|-------------------------------------------------------------------------------------------------------------------------------------------------|
| `compacted`           | The mirror now has a single packfile.                                                                                                           |
| `skipped_single_pack` | The mirror is already compact: it has a single packfile and no loose objects. A `repack` only replaces that packfile if the new one is smaller. |
| `skipped_size`        | `repack` only: the packfiles of the mirror are larger than `max_size`.                                                                          |
| `skipped_busy`        | A deployment or another operation is using the mirror.                                                                                          |
| `failed`              | The compaction of the mirror failed, or the mirror could not be read. The old packfiles are kept.                                               |
| `cancelled`           | The compaction was cancelled before it finished. The old packfiles are kept.                                                                    |

The compactions are counted in `doco_cd_git_mirror_compactions_total` with the `mode` label set to `repack` or
`copy`. The consolidation after a fetch is counted with `mode="copy"` as well.
