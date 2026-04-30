!!! note "[Reconciliation](../Deploy-Settings.md#reconciliation-settings) for non-Swarm deployments follows classic Compose `restart` semantics"

    - Services with `#!yaml restart: always` or `#!yaml restart: unless-stopped` are expected to stay running.
    - Services with no explicit `restart` policy are treated as `#!yaml restart: "no"`.
    - Services with `#!yaml restart: on-failure` may remain exited after success, and `#!yaml restart: "no"` is treated as one-time behavior and is not reconciled back to running.
    - Swarm handling is separate and uses Swarm service modes and `#!yaml deploy.restart_policy` behavior.

    More information on the restart policies can be found in the [Docker Compose specification](https://docs.docker.com/reference/compose-file/services/#restart).