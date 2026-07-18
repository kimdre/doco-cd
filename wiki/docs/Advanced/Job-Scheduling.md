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

!!! warning "Multiple doco-cd instances on the same Docker host"
    The scheduler discovers runnable jobs from Docker labels and is not scoped by deployment target or by a specific `.doco-cd.*.yaml` file.
    If you run multiple doco-cd instances against the same Docker socket, each instance can discover and trigger the same scheduled jobs.

    To avoid duplicate runs, enable the scheduler only on the instance that should own scheduled jobs and set [`SCHEDULER_ENABLED`](../App-Settings.md#:~:text=when%20not%20specified-,SCHEDULER_ENABLED,-boolean) to `false` on secondary or self-updater instances.

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

### `restart`

By default, scheduled jobs will be executed in `restart` mode, which means the service will be created on deployment 
and then re-/started at the scheduled time without being removed after completion.

### `one_off`

Alternatively, you can configure scheduled jobs to run in `one_off` mode, which means a new ephemeral container will 
be created for each scheduled run and removed after completion.

!!! note
    You won't be able to see the container or its logs after the job has completed, 
    so make sure to configure appropriate logging (e.g., log to a persistent file or logging service like [Loki](https://grafana.com/docs/loki/latest/)) 
    if you need to keep track of job runs and [notifications](Notifications.md) if needed.

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

??? warning "Deprecated: `one_shot` has been renamed to `one_off`"
    `one_shot` has been renamed to `one_off` and will be removed in a future release.
    Use `one_off` instead. The old value is still accepted for backward compatibility but will log a warning.

## Configuration

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

| Label                           | Type    | Description                                                                                                                                                                 | Default   |
|---------------------------------|---------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------|
| `cd.doco.job.enabled`           | boolean | Enable scheduling for this service/container                                                                                                                                | `false`   |
| `cd.doco.job.schedule`          | string  | [Schedule format](#schedule-formats) to use                                                                                                                                 |           |
| `cd.doco.job.wait_running_jobs` | boolean | Override deploy-config-wide [`wait_running_jobs`](../Deploy-Settings.md#wait-for-running-scheduled-jobs-before-deployment) behavior for this job service during deployments | (inherit) |
| `cd.doco.job.execution_mode`    | string  | [`restart`](#restart) (default behavior) or [`one_off`](#one_off) (ephemeral execution)                                                                                     | `restart` |
| `cd.doco.job.skip_running`      | boolean | Do not run the job if a previous scheduled run is still active/running                                                                                                      | `false`   |
| `cd.doco.job.notify_on`         | string  | [Notification](Notifications.md) behavior for scheduled runs: `none`, `success`, `failure`, `all`                                                                           | `all`     |
| `cd.doco.job.swarm.replicas`    | integer | Number of completions/concurrency for swarm one-off jobs in `replicated` [deploy mode](#swarm-deploymode)                                                                   | `1`       |

!!! note "Using scheduled jobs with multiple doco-cd instances"
    `cd.doco.job.skip_running` only prevents overlapping runs within the same doco-cd process.
    It does not coordinate scheduled runs across multiple doco-cd instances that share the same Docker host.

    For multi-instance setups, prefer a single scheduler owner by disabling the scheduler on the other instances with [`SCHEDULER_ENABLED`](../App-Settings.md#:~:text=when%20not%20specified-,SCHEDULER_ENABLED,-boolean).

### Swarm `deploy.mode`

When using Docker Swarm, you can configure the deploy mode for scheduled jobs using the `deploy.mode` field in your docker compose file.

The following mapping applies to scheduled runs in `one_off` mode:

- If the service uses `#!yaml deploy.mode: global`, the job run is created as `global-job`
- If the service uses `#!yaml deploy.mode: replicated` or does not specify a deploy mode, the job run is created as `replicated-job` with the number of completions/concurrency determined by the `cd.doco.job.swarm.replicas` label.

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
You can find a list of all possible timezone values on [wikipedia](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones).

## Daylight saving time (DST)

When DST changes occur in the configured [timezone](#timezone), scheduled jobs will adjust accordingly:

- If a scheduled time is skipped due to DST (e.g., clocks move forward), the job will not run at that time.
- If a scheduled time occurs twice due to DST (e.g., clocks move backward), the job will run at both occurrences of that time.