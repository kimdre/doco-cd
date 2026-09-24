---
tags:
  - Advanced
  - Deployment
  - Docker
  - Swarm Mode
---

# Job Scheduling

The built-in job scheduler allows you to run containers/services defined in your docker compose files as scheduled jobs based on cron-like schedules or predefined intervals.
This is useful for running periodic tasks such as backups, maintenance scripts, or any recurring workloads without needing an external scheduler.

!!! warning "Multiple doco-cd instances on the same Docker daemon"
    The scheduler discovers jobs from Docker labels, including jobs deployed by another instance.
    If multiple schedulers reach the same daemon, configure [per-job ownership](#multiple-instances-on-one-docker-daemon) on **every** instance to avoid duplicate runs. Docker context names are local aliases, not daemon identities.

## App Configuration

These settings control the behavior of the built-in job scheduler and can be set in the environment of the doco-cd instance.

| Key                                | Type    | Description                                                                                                                                                                                                                                                                                                                                           | Default |
|------------------------------------|---------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------|
| `SCHEDULER_ENABLED`                | boolean | Controls whether this doco-cd instance starts the built-in job scheduler. Disable it on secondary/[self-updater](Self-Updating.md) instances that should not trigger any scheduled jobs.                                                                                                                                                              | `true`  |
| `SCHEDULER_INSTANCE_ID`            | string  | Stable, manually assigned scheduler owner ID (letters, digits, `.`, `_`, `-`; at most 63 characters). When set, scheduled jobs deployed by this instance are stamped with this owner unless `cd.doco.job.owner` is set explicitly. Set a unique value per instance sharing a Docker daemon.                                                           | Unset   |
| `SCHEDULER_REQUIRE_OWNER_CONTEXTS` | list    | Comma-separated local Docker context names where jobs without `cd.doco.job.owner` are not scheduled. Requires `SCHEDULER_INSTANCE_ID`; names must exist in the mounted Docker context store. Use `default` for the local daemon / default docker context. See [multi-instance scheduling](Job-Scheduling.md#multiple-instances-on-one-docker-daemon). | Unset   |

## Schedule formats

- [Cron expressions](https://pkg.go.dev/github.com/robfig/cron#hdr-CRON_Expression_Format) **without** seconds (`minute hour day-of-month month day-of-week`)
- [Predefined schedules](https://pkg.go.dev/github.com/robfig/cron#hdr-Predefined_schedules) like `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`
- [Intervals](https://pkg.go.dev/github.com/robfig/cron#hdr-Intervals) like `@every <duration>` (for example `@every 30m`)

!!! tip 
    Use an online cron expression generator like [crontab.guru](https://crontab.guru/) to create and validate cron expressions.

!!! example "Schedule examples"

    === "Every 15 minutes"
    
        === "Using cron expression"
    
            ```yaml title="docker-compose.yml"
            services:
              backup:
                image: example/backup:latest
                labels:
                  cd.doco.job.enabled: "true"
                  cd.doco.job.schedule: "*/15 * * * *"
            ```
    
        === "Using interval format"
    
            ```yaml title="docker-compose.yml"
            services:
              backup:
                image: example/backup:latest
                labels:
                  cd.doco.job.enabled: "true"
                  cd.doco.job.schedule: "@every 15m"
            ```
    
    === "Weekdays at 02:30"
    
        ```yaml title="docker-compose.yml"
        services:
          backup:
            image: example/backup:latest
            labels:
              cd.doco.job.enabled: "true"
              cd.doco.job.schedule: "30 2 * * 1-5"
        ```
    
    === "First day of month at midnight"
    
        === "Using cron expression"
    
            ```yaml title="docker-compose.yml"
            services:
              cleanup:
                image: example/backup:latest
                labels:
                  cd.doco.job.enabled: "true"
                  cd.doco.job.schedule: "0 0 1 * *"
            ```
    
        === "Using predefined schedule"
    
            ```yaml title="docker-compose.yml"
            services:
              cleanup:
                image: example/backup:latest
                labels:
                  cd.doco.job.enabled: "true"
                  cd.doco.job.schedule: "@monthly"
            ```

## Execution modes

The execution mode determines how scheduled jobs are run and managed by doco-cd and can be configured using the `cd.doco.job.execution_mode` label on the service.

!!! info "Scheduled jobs never run on deployment"
    Scheduled jobs only run when their [schedule](#schedule-formats) fires, never as a side effect of a (re)deployment.
    On deployment the job's service/container is prepared but left idle:

    - Docker (Standalone): the container is created but not started.
    - Docker Swarm: the service is deployed with `0` replicas (see the limitation for `global` restart-mode jobs below).

### `restart`

By default, scheduled jobs will be executed in `restart` mode, which means the service will be created on deployment 
and then re-/started at the scheduled time without being removed after completion.

!!! note "Docker (Standalone) Compose services"
    Scheduled services are started with their Compose service definition, so Compose-defined `secrets` and
    `configs` are applied when the job starts. Restart-mode services must have an effective scale of `1`; use
    [`one_off`](#one_off) for a multi-replica workload.

!!! warning "Docker Swarm `global` + `restart` limitation"
    In Docker Swarm, restart-mode scheduled jobs are deployed with `0` replicas so they do not run on deployment.
    `global` services cannot be scaled to `0` (a global service always runs one task per node), so a `global` service
    combined with `restart` mode **will** run on deployment. Use [`one_off`](#one_off) mode for global scheduled jobs instead.

### `one_off`

Alternatively, you can configure scheduled jobs to run in `one_off` mode, which means a new ephemeral container will
be created for each scheduled run and removed after completion and reporting.

!!! note
    You won't be able to see the container or its logs after the job has completed,
    so make sure to configure appropriate logging (e.g., log to a persistent file or logging service like [Loki](https://grafana.com/docs/loki/latest/)) 
    if you need to keep track of job runs and [notifications](Notifications.md) if needed.

??? info "Recovery after forced termination"
    For `one_off` jobs, doco-cd labels the temporary container or Swarm service with the execution identity
    and retains it until its result has been reported, and it has been cleaned up. 
    It writes only the small finalization record needed to restore `stop_services` and avoid duplicate reporting to [`DATA_MOUNT_PATH`](../App-Settings.md#storage-settings).
    If doco-cd is forcibly terminated and recreated with the same data mount, the replacement adopts the labeled execution, 
    waits for it, restores dependencies, reports the result, and removes the artifact.


    !!! note "Duplicate notifications"
        This does not apply to `restart` mode. Notification delivery is best-effort at-least-once: a termination
        after a notification is sent but before it is recorded can result in a duplicate notification.

??? info "`one_off` behavior in Docker Swarm"

    In Docker Swarm, `one_off` does **not** modify the source service mode permanently.
    Instead, doco-cd creates a temporary job service for each scheduled run, waits for completion,
    and removes that temporary service afterwards.
    
    This means the original service may still show `replicated`/`global` when inspected,
    while each one-off execution runs as a temporary `replicated-job`/`global-job` service.

    See also [Swarm `deploy.mode` configuration](#swarm-deploymode) for how the original service's deploy mode affects the temporary job service's deploy mode in one-off executions.

    **Behavior summary**
    
    | `cd.doco.job.execution_mode` | What doco-cd acts on | Service mode after run |
    |------------------------------|----------------------|------------------------|
    | `restart`                    | Existing service     | Unchanged              |
    | `one_off`                    | Temporary clone      | Source unchanged       |

## Configuration Labels

??? example "How to set service labels in a docker compose file"
    To set service labels in a docker compose file, include them in the `labels` section of your service definition:

    ```yaml title="docker-compose.yml"
    services:
      app:
        image: ghcr.io/example/app:latest
        labels:
          cd.doco.job.enabled: "true"
          cd.doco.job.schedule: "@every 15m"
    ```

!!! note "Restart policy constraints"

    - Docker (Standalone): service `restart` must be unset or `no`
    - Docker Swarm: service `deploy.restart_policy.condition` must be unset or `none`

Use the following service labels to configure scheduled jobs:

| Label                               | Type    | Description                                                                                                                                                                                       | Default                         |
|-------------------------------------|---------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------|
| `cd.doco.job.enabled`               | boolean | Enable scheduling for this service/container                                                                                                                                                      | `false`                         |
| `cd.doco.job.schedule`              | string  | [Schedule format](#schedule-formats) to use                                                                                                                                                       |                                 |
| `cd.doco.job.owner`                 | string  | Scheduler instance ID that owns the job; overrides the deploying instance's `SCHEDULER_INSTANCE_ID` stamp. Letters, digits, `.`, `_`, `-`; at most 63 characters.                                 | (deploying instance ID, if set) |
| `cd.doco.job.wait_running_jobs`     | boolean | Override deploy-config-wide [`wait_running_jobs`](../Deploy-Settings.md#wait-for-running-scheduled-jobs-before-deployment) behavior for this job service during deployments                       | (inherit)                       |
| `cd.doco.job.execution_mode`        | string  | [`restart`](#restart) (default behavior) or [`one_off`](#one_off) (ephemeral execution)                                                                                                           | `restart`                       |
| `cd.doco.job.skip_running`          | boolean | Do not run the job if a previous scheduled run is still active/running                                                                                                                            | `false`                         |
| `cd.doco.job.notify_on`             | string  | [Notification](Notifications.md) behavior for scheduled runs: `none`, `success`, `failure`, `all`                                                                                                 | `all`                           |
| `cd.doco.job.swarm.replicas`        | integer | Number of completions/concurrency for swarm one-off jobs in `replicated` [deploy mode](#swarm-deploymode)                                                                                         | `1`                             |
| `cd.doco.job.stop_services`         | string  | Comma-separated services to [temporarily stop during a job run](#temporarily-stop-services-during-a-job-run) (supports `service` and `project/service`; Swarm requires `execution_mode: one_off`) |                                 |
| `cd.doco.job.stop_services.timeout` | integer | Timeout in seconds when stopping `stop_services` targets; see [stop timeout behavior](#stop-timeout-behavior)                                                                                     | (see below)                     |

!!! note "Using scheduled jobs with multiple doco-cd instances"
    `cd.doco.job.skip_running` prevents overlapping runs within the same doco-cd process. For Swarm
    `one_off` jobs, it also recognizes an active temporary execution left by a restarted process.
    It does not coordinate simultaneously triggered runs across multiple doco-cd instances that share
    the same Docker host.

    Ownership is configured per job as described below; `skip_running` is not a cross-instance lock.

### Multiple instances on one Docker daemon

Give each scheduler-enabled instance a different, stable `SCHEDULER_INSTANCE_ID`. When the instance deploys a scheduled service, it stamps `cd.doco.job.owner` with its ID. Only the matching instance schedules or manually triggers that job; the other instances still show it in job listings with `eligible: false`, a skip reason and no `next_run_at`. You can set `cd.doco.job.owner` explicitly in Compose labels to assign a different owner than the deploying instance.

On every context that reaches a **shared** daemon, set `SCHEDULER_REQUIRE_OWNER_CONTEXTS` to that instance's *local* name for the context. Jobs without an owner label are skipped with a warning there, rather than accidentally running twice. Contexts not listed still run unowned jobs as before.

For example, A is on the Docker host, and B also reaches that daemon through its `docker-host` context:

```yaml title="Instance A environment"
SCHEDULER_ENABLED: "true"
SCHEDULER_INSTANCE_ID: host-a
SCHEDULER_REQUIRE_OWNER_CONTEXTS: default
```

```yaml title="Instance B environment"
SCHEDULER_ENABLED: "true"
SCHEDULER_INSTANCE_ID: host-b
SCHEDULER_REQUIRE_OWNER_CONTEXTS: docker-host
```

Jobs deployed by A carry `owner=host-a`; jobs deployed by B carry `owner=host-b`. Both instances can schedule different jobs on the shared daemon. B's unlabeled jobs on its **local** `default` context still run.

#### Migrating scheduler ownership

??? abstract "Guide: One-time migration from a pre-ownership release (before v0.122.0)"

    You can migrate gradually without redeploying every stack at once:

    1. Upgrade **both** instances and assign different `SCHEDULER_INSTANCE_ID` values. Keep B's scheduler disabled until both instances are upgraded (`#!yaml SCHEDULER_ENABLED: false`).
    2. Set `#!yaml SCHEDULER_REQUIRE_OWNER_CONTEXTS: docker-host` on B, but **leave A's shared-daemon `default` context out** of that setting for now. A continues scheduling existing unlabeled jobs; B skips them.
    3. Enable B's scheduler. New jobs deployed by B receive `owner=host-b` (unless explicitly overridden) and run only on their owner. To transfer an existing unlabeled job to B, redeploy its stack from B or add an explicit `#!yaml cd.doco.job.owner: host-b` label and redeploy. Until then, A continues to run that job. Confirm in the [jobs API](../Endpoints/REST-API.md#scheduled-jobs) that it is `eligible` on B and ineligible on A.
    4. As the remaining stacks are redeployed by their intended owners, their scheduled jobs receive owner labels. Once no shared-daemon jobs that should run remain unlabeled, add `default` to A's `SCHEDULER_REQUIRE_OWNER_CONTEXTS` as shown above.

    During this transition, **A runs every unlabeled job on the shared daemon**, including jobs eventually intended for B. Adding `default` to A's strict contexts too early pauses those jobs; leaving B's shared context non-strict risks duplicate runs.

Changing an instance's ID does not update jobs it already deployed. Until those jobs are redeployed with the new owner, the renamed instance skips them and rejects manual triggers. Update any explicit `cd.doco.job.owner` labels as well; they override automatic stamping. Trigger an actual deployment of each affected stack (a poll or webhook for an unchanged configuration may be skipped), then confirm the new owner in the jobs API. Automatic certificate rotation and managed recreation preserve the existing owner and do not migrate it.

IDs must be unique and stable; two instances using the same ID will both run the job. If both instances deploy the same stack without an explicit owner, the last deployment changes its owner. A job whose owner is down will not run until an operator reassigns it; there is no automatic failover or shared cross-instance lease. Do not expose the same daemon to the *same instance* under multiple context names, or that instance can still schedule the job twice. Older releases ignore ownership labels and must not be left scheduler-enabled alongside ownership-aware instances on the shared daemon.

### Swarm `deploy.mode`

When using Docker Swarm, you can configure the deploy mode for scheduled jobs using the `deploy.mode` field in your docker compose file.

The following mapping applies to scheduled runs in `one_off` mode:

- If the service uses `#!yaml deploy.mode: global`, the job run is created as `global-job`
- If the service uses `#!yaml deploy.mode: replicated` or does not specify a deploy mode, the job run is created as `replicated-job` with the number of completions/concurrency determined by the `cd.doco.job.swarm.replicas` label.

### Temporarily stop services during a job run

Use `cd.doco.job.stop_services` when a scheduled job needs a quiet window (for example, cold backups):

```yaml title="docker-compose.yml"
services:
  backup:
    image: ghcr.io/my-org/backup:1.2.3
    command: ["/backup.sh"]
    labels:
      cd.doco.job.enabled: "true"
      cd.doco.job.schedule: "0 2 * * *"
      cd.doco.job.execution_mode: "one_off"
      cd.doco.job.stop_services: "app,other-project/other-app"
```

Behavior:

- Before the job starts, listed services are stopped, and doco-cd waits until their containers/tasks have actually terminated.
- After the job finishes (success or failure), listed services are started again.
- `service` targets the same project/stack as the job.
- `project/service` targets another compose project (standalone) or stack (swarm).

!!! info "Execution mode support"
    `cd.doco.job.stop_services` is supported in both execution modes for **standalone compose**:

    | Mode | Standalone | Swarm |
    |---|---|---|
    | `one_off` | ✅ | ✅ |
    | `restart` (default) | ✅ | ❌ |

    In Swarm mode, `restart` is not supported because doco-cd cannot detect when the job has finished.

!!! warning "Use service names, not container names"
    Values must reference the **compose service name** (the key under `services:`), **not** `container_name`.

!!! info "`depends_on` is not traversed automatically"
    Only explicitly listed services are stopped/started.
    If dependent services should also be paused, include them explicitly in `cd.doco.job.stop_services`.

!!! info "Swarm: global-mode services are skipped"
    Services deployed with `#!yaml deploy.mode: global` (or `global-job`) cannot be scaled to 0 replicas, so they are skipped with a warning instead of being stopped.

??? note "Concurrency and shared targets"
    While services are held stopped, doco-cd locks the job's own stack **and** every stack referenced by `cd.doco.job.stop_services`, so a concurrent deployment or another scheduled run cannot race with the reconciliation of those stacks.

    If two scheduled jobs happen to list the same target service (e.g. two backup jobs sharing a cache), the target is only actually restarted once every job that stopped it has finished. It will not be brought back up prematurely while another job still needs it stopped.

??? note "Stop window is not drift"
    A service held stopped by a running job is doco-cd's own doing, so it is not treated as drift: reconciliation does not restart it, and a poll or webhook landing inside the stop window does not redeploy its stack because of the missing replicas. The suppression stays active for a short grace period after the service is started again.

#### Stop timeout behavior

By default, doco-cd honors each target's own configured shutdown grace period instead of a fixed timeout:

- **Standalone compose**: the container's [`stop_grace_period`](https://docs.docker.com/reference/compose-file/services/#stop_grace_period) is respected, letting the Docker engine apply it natively. If the target has no `stop_grace_period` configured, a default of 30 seconds is used.
- **Swarm**: the service's configured [`stop_grace_period`]([`stop_grace_period`](https://docs.docker.com/reference/compose-file/services/#stop_grace_period)), plus a small observation buffer, is used as the wait deadline so a long grace period is not treated as a timeout error prematurely. If unset, the default of 30 seconds is used.

Set `cd.doco.job.stop_services.timeout` to a positive number of seconds to control the stop operation:

- **Standalone compose**: overrides each target container's configured grace period.
- **Swarm**: overrides how long doco-cd waits for tasks to stop. Swarm still applies the grace period embedded in each running task; doco-cd cannot rewrite that period during shutdown.

```yaml title="docker-compose.yml"
services:
  backup:
    labels:
      cd.doco.job.enabled: "true"
      cd.doco.job.schedule: "0 2 * * *"
      cd.doco.job.execution_mode: "one_off"
      cd.doco.job.stop_services: "db"
      cd.doco.job.stop_services.timeout: "180"
```

## Examples

=== "Prune swarm nodes"

    Prune Docker system every hour on all swarm nodes using a global one-off job service

    ```yaml title="docker-compose.yml"
    services:
      prune:
        image: docker:latest
        command: ["docker", "system", "prune", "-f"]
        volumes:
          - "/var/run/docker.sock:/var/run/docker.sock"
        deploy:
          mode: global
          restart_policy:
            condition: none
        labels:
          cd.doco.job.enabled: "true"
          cd.doco.job.schedule: "@hourly"
          cd.doco.job.execution_mode: "one_off"
    ```

=== "Backup"

    Run a backup script every day at 02:00, but skip if the previous run is still active

    ```yaml title="docker-compose.yml"
    services:
      backup:
        image: ghcr.io/my-org/backup:1.2.3
        command: ["/backup.sh"]
        restart: no
        labels:
          cd.doco.job.enabled: "true"
          cd.doco.job.schedule: "0 2 * * *"
          cd.doco.job.skip_running: "true"
    ```

## Timezone

Scheduled jobs are triggered based on the timezone of the doco-cd instance, which is determined by the `TZ` environment variable or defaults to UTC if not set.
You can find a list of all possible timezone values on [timeie](https://timeie.com/) and [wikipedia](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones).

## Daylight saving time (DST)

When DST changes occur in the configured [timezone](#timezone), scheduled jobs will adjust accordingly:

- If a scheduled time is skipped due to DST (e.g., clocks move forward), the job will not run at that time.
- If a scheduled time occurs twice due to DST (e.g., clocks move backward), the job will run at both occurrences of that time.

## Manual execution via Job API

Configured jobs can also be triggered manually outside their scheduled intervals by using the [Run Job API endpoint](../Endpoints/REST-API.md#scheduled-jobs).
