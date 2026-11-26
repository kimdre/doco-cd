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

## Compose native `jobs:` are not scheduled

Docker Compose 5.8.0 and later can parse a top-level `jobs:` element with `triggers.schedule`. Doco-CD does not schedule these jobs yet and logs a warning with the job names.
Native jobs are experimental in Docker. They need Docker Engine 29.10.0 or later with the jobs extension, which is off by default.
Doco-CD also never registers them with the Docker Engine jobs extension. Doco-CD disables native jobs before it calls Compose, so the rest of the stack deploys normally.
Jobs that the Docker Compose CLI registered earlier stay on the Engine while the Compose file still declares them. Remove them with `docker compose down` or a doco-cd destroy.
In Swarm mode, the `jobs:` element is ignored and doco-cd logs a warning.

To run scheduled jobs today, use the [service labels](Advanced/Job-Scheduling.md#configuration) of the built-in job scheduler.

## Running pre- and post-deployment scripts

Doco-CD does not provide a shell environment for security reasons. Therefore, running shell scripts inside the Doco-CD container is not supported.
Instead, you can use init containers, sidecar containers, or compose lifecycle hooks to run scripts in the context of the deployed containers.
See the [Pre- and Post-Deployment Scripts](Advanced/Pre-Post-Deployment-Scripts.md) page for more information on how to set this up.
