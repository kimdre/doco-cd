---
tags:
  - Advanced
  - Deployment
  - Docker
  - Swarm Mode
---

# Scheduled jobs

The built-in job scheduler allows you to run containers/services defined in your docker compose files as scheduled jobs based on cron-like schedules or predefined intervals.
This is useful for running periodic tasks such as backups, maintenance scripts, or any recurring workloads without needing an external scheduler.

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

By default, scheduled jobs will be executed in `restart` mode, which means the service container 
will be created on deployment and then re-/started at the scheduled time without being removed after completion.

### `one_shot`

Alternatively, you can configure scheduled jobs to run in `one_shot` mode, which means a new ephemeral container will 
be created for each scheduled run and removed after completion.

Note that this means that you won't be able to see the container or its logs after the job has completed, 
so make sure to configure appropriate logging (e.g., log to a persistent file or external logging service like [Loki](https://grafana.com/docs/loki/latest/)) 
if you need to keep track of job runs and [notifications](Notifications.md) if needed.

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

| Label                              | Type    | Description                                                                                       | Default   |
|------------------------------------|---------|---------------------------------------------------------------------------------------------------|-----------|
| `cd.doco.job.enabled`              | boolean | Enable scheduling for this service/container                                                      | `false`   |
| `cd.doco.job.schedule`             | string  | [Schedule format](#schedule-formats) to use                                                       |           |
| `cd.doco.job.execution_mode`       | string  | `restart` (default behavior) or `one_shot` (ephemeral execution)                                  | `restart` |
| `cd.doco.job.skip_running`         | boolean | Do not run the job if a previous scheduled run is still active/running                            | `false`   |
| `cd.doco.job.notify_on`            | string  | [Notification](Notifications.md) behavior for scheduled runs: `none`, `success`, `failure`, `all` | `all`     |
| `cd.doco.job.swarm.replicas`       | integer | Number of completions/concurrency for swarm one-shot jobs in `replicated-job` mode                | `1`       |

### Swarm `deploy.mode`

When using Docker Swarm, you can configure the deploy mode for scheduled jobs using the `deploy.mode` field in your docker compose file.

- If the service uses `#!yaml deploy.mode: global`, the job run is created as `global-job`
- If the service uses `#!yaml deploy.mode: replicated` or does not specify a deploy mode, the job run is created as `replicated-job` with the number of completions/concurrency determined by the `cd.doco.job.swarm.replicas` label.

## Examples

=== "Prune swarm nodes"

    Prune Docker system every hour on all swarm nodes using a global one-shot job service

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
          cd.doco.job.execution_mode: "one_shot"
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

## Daylight saving time (DST)

Scheduled jobs are triggered based on the timezone of the doco-cd instance, which is determined by the `TZ` environment variable or defaults to UTC if not set.

When DST changes occur in the configured timezone, scheduled jobs will adjust accordingly:

- If a scheduled time is skipped due to DST (e.g., clocks move forward), the job will not run at that time.
- If a scheduled time occurs twice due to DST (e.g., clocks move backward), the job will run at both occurrences of that time.