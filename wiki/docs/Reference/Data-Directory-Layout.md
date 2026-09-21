---
tags:
  - Reference
---

# Data Directory Layout

## Source Directory

!!! tip "See also [Artifact Garbage Collection](Artifact-Garbage-Collection.md) for more information on how an artifact is cleaned up."

Doco-CD stores and manages source data at [`DATA_MOUNT_PATH`](../App-Settings.md#storage-settings).
The source directory is organized by source type and source name, and contains the following subdirectories:

=== "Git source"

    ### Git source layout

    A Git source may have the following layout:

    ```text title="Example Git Source Layout"
    <DATA_MOUNT_PATH>/
    └── github.com/
        └── org/
            ├── example/  # Source directory
            │   ├── artifacts/  # Immutable Git tree exports
            │   │   ├── <revision>/  # Immutable export of a Git tree for a specific revision
            │   │   ├── <revision>.lock  # Lock file for artifact access
            │   │   └── ...
            │   ├── mirror/  # Bare Git repository mirror
            │   │   ├── HEAD
            │   │   ├── config
            │   │   ├── objects/
            │   │   ├── refs/
            │   │   └── ...
            │   ├── mirror.lock  # Lock file for mirror access
            │   └── submodules/  # Cached submodule data
            │       ├── <submodule-revision>/  # Submodule data for a specific revision
            │       ├── <submodule-revision>.lock  # Lock file for submodule access
            │       └── ...
            ├── example.gc-use.lock  # Lock file for the garbage collector while the source is in use
            └── example.lock  # Lock file for source-level operations
    ```

    - `mirror` is a bare Git mirror used to resolve revisions. It is never checked out directly.
      - `artifacts/<revision>` is an immutable export of a Git tree. Deployments use this directory,
        allowing multiple revisions of the same source to be deployed in parallel.
      - `mirror.lock`, `<revision>.lock`, and `<submodule-cache>.lock` coordinate access to shared
        source data to prevent race conditions when multiple deployments are running in parallel.
      - `submodules/<submodule-revision>` contains cached submodule data when Git submodules are used
        in the source repository.

=== "OCI source"

    ### OCI source layout

    An OCI source may have the following layout:

    ```text title="Example OCI Source Layout"
    <DATA_MOUNT_PATH>/
    └── ghcr.io/
        └── org/
            ├── example/  # Source directory
            │   └── artifacts/  # Immutable OCI artifact exports
            │       ├── sha256-<digest>/  # Extracted artifact for a specific digest
            │       ├── sha256-<digest>.lock  # Lock file for artifact access
            │       └── ...
            ├── example.gc-use.lock  # Lock file for the garbage collector while the source is in use
            └── example.lock  # Lock file for source-level operations
    ```

    - `artifacts/<digest>` is an immutable extraction of the OCI artifact for a
      content digest. The digest is encoded in the directory name because `:`
      is not safe in Docker bind-mount source paths.
    - `<digest>.lock` coordinates access while an artifact is being published
      or used by a deployment.

!!! danger "Do not write persistent application data to the source directory"
    Do not store persistent application data inside the source directory or an artifact directory.
    Bind mounts must use an absolute host path or a named volume; otherwise, the data is lost when the service is re-/deployed from a new artifact revision.

## Upgrading Doco-CD from v0.119.x or earlier

Versions prior to v0.120.0 checked repositories out directly into the source directory instead of using a bare mirror and per-revision artifacts.
On first startup after upgrading, Doco-CD automatically migrates any repository still using the old layout, no action is required.
Leftover files from the old checkout are only removed once no running container still references them.
