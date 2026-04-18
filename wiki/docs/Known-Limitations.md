---
tags:
  - Reference
  - Docker
  - Deployment
  - Limitations
---

# Known Limitations

This page is about the known limitations of the current implementation of the project. 
It is important to know these limitations before using the project in a production environment.

## Renaming projects

Docker currently [does not support updating labels](https://github.com/moby/moby/issues/21721), which means that renaming a deployed project is not possible. 
If you need to rename a project, you will need to create a new project with the new name and migrate any data manually.

## Running pre- and post-deployment scripts

Doco-CD does not provide a shell environment for security reasons. Therefore, running shell scripts inside the Doco-CD container is not supported.
Instead, you can use init containers, sidecar containers, or compose lifecycle hooks to run scripts in the context of the deployed containers.
See the [Pre- and Post-Deployment Scripts](Advanced/Pre-Post-Deployment-Scripts.md) page for more information on how to set this up.
