!!! note "[Reconciliation](/Deploy-Settings/#reconciliation-settings) for non-Swarm deployments follows classic Compose `restart` semantics"

    - Services with `#!yaml restart: always` or `#!yaml restart: unless-stopped` are expected to stay running.
    - Services with no explicit `restart` policy are treated as `#!yaml restart: "no"`.
    - Services with `#!yaml restart: on-failure` may remain exited after success (exited with status code `0`), and `#!yaml restart: "no"` is treated as one-time behavior and is not reconciled back to running.
    - Events from Compose [lifecycle-hook](/Advanced/Pre-Post-Deployment-Scripts/#compose-lifecycle-hooks) runner containers are ignored. These helpers are not workload replicas, so their events do not restart or redeploy the service.
    - Restarts run the `pre_stop` and `post_start` [lifecycle hooks](/Advanced/Pre-Post-Deployment-Scripts/#when-doco-cd-runs-lifecycle-hooks) of the service, like `docker compose restart`. `pre_start` hooks do not run.
    - Restart events use the stop signal that is configured in the container image (`StopSignal` in the image config); you can override this with `#!yaml reconciliation.restart_signal`. If neither is available, `SIGTERM` is used.
    - To prevent endless restart loops caused by flappy health checks, `#!yaml unhealthy` restarts are rate-limited via `#!yaml reconciliation.restart_limit` and `#!yaml reconciliation.restart_window`.

    More information on the restart policies can be found in the [Docker Compose specification](https://docs.docker.com/reference/compose-file/services/#restart).

    !!! abstract "Reconciliation in Docker Swarm"
        Docker Swarm manages some desired-state reconciliation by itself with Swarm service modes and [`#!yaml deploy.restart_policy`](https://docs.docker.com/reference/compose-file/deploy/#restart_policy) behavior. See Docker's documentation on [desired state reconciliation](https://docs.docker.com/engine/swarm/#desired-state-reconciliation).  
        Doco-CD's reconciliation for Swarm deployments only manages service updates and scaling, but not container restarts or health status.
