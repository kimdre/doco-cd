!!! note "[Reconciliation](../Deploy-Settings.md#reconciliation-settings) for non-Swarm deployments follows classic Compose `restart` semantics"

    - Services with `#!yaml restart: always` or `#!yaml restart: unless-stopped` are expected to stay running.
    - Services with no explicit `restart` policy are treated as `#!yaml restart: "no"`.
    - Services with `#!yaml restart: on-failure` may remain exited after success (exited with status code `0`), and `#!yaml restart: "no"` is treated as one-time behavior and is not reconciled back to running.
    - Restart events can override the stop signal using `#!yaml reconciliation.restart_signal`.
    - To prevent endless restart loops caused by flappy health checks, `#!yaml unhealthy` restarts are rate-limited via `#!yaml reconciliation.restart_limit` and `#!yaml reconciliation.restart_window`.

    More information on the restart policies can be found in the [Docker Compose specification](https://docs.docker.com/reference/compose-file/services/#restart).

    !!! abstract "Reconciliation in Docker Swarm"
        Docker Swarm manages some desired-state reconciliation by itself with Swarm service modes and [`#!yaml deploy.restart_policy`](https://docs.docker.com/reference/compose-file/deploy/#restart_policy) behavior. See Docker's documentation on [desired state reconciliation](https://docs.docker.com/engine/swarm/#desired-state-reconciliation).  
        Doco-CD's reconciliation for Swarm deployments only manages service updates and scaling, but not container restarts or health status.
