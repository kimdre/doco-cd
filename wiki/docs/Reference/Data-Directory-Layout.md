---
tags:
  - Reference
---

# Data Directory Layout

Doco-CD keeps one subdirectory per Git or OCI source under `DATA_MOUNT_PATH`. For Git sources it contains:

- `mirror`: a bare, read-only mirror clone of the repository, updated in place on every deployment. 
    It is never checked out directly.
- `artifacts/<revision>`: an immutable, read-only export of the repository tree at each deployed revision. 
    Deployments read directly from here, so multiple revisions of the same repository can be deployed in parallel without interfering with one another.

OCI sources have no `mirror` directory (there is nothing to keep a mutable local copy of) and use the
same `artifacts/<digest>` layout directly under the source's subdirectory, keyed by the artifact's
content digest instead of a Git commit.

Both directories are managed entirely by Doco-CD; you do not need to interact with them.

!!! info "Upgrading from an older Doco-CD version"
    Versions prior to this layout checked repositories out directly into the source directory instead of using a bare mirror and per-revision artifacts. 
    On first startup after upgrading, Doco-CD automatically migrates any repository still using the old layout, no action is required. 
    Leftover files from the old checkout are only removed once no running container still references them.
