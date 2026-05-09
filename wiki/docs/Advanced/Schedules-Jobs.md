---
tags:
  - Advanced
  - Deployment
  - Docker
  - Swarm Mode
---

# Scheduled jobs

You can schedule service/container runs using `cd.doco.job.*` labels.

By default, a scheduled run triggers a normal service restart/rerun behavior:

- Docker standalone: restart the target container
- Docker Swarm: rerun job-mode services, restart long-running services

Use `#!yaml cd.doco.job.execution_mode: one_shot` to run an ephemeral one-shot execution instead.

In Docker Swarm Mode, `one_shot` creates a temporary job service and removes it after completion. 
If the source service uses `#!yaml deploy.mode: global`, the one-shot run is created as `global-job`; 
otherwise it is created as `replicated-job`. 

## Schedule formats

- [Cron expressions](https://pkg.go.dev/github.com/robfig/cron#hdr-CRON_Expression_Format) **without** seconds (`minute hour day-of-month month day-of-week`)
- [Predefined schedules](https://pkg.go.dev/github.com/robfig/cron#hdr-Predefined_schedules) like `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`
- [Intervals](https://pkg.go.dev/github.com/robfig/cron#hdr-Intervals) like `@every <duration>` (for example `@every 30m`)

!!! tip 
    Use an online cron expression generator like [crontab.guru](https://crontab.guru/) to create and validate cron expressions.

### Examples

=== "Every 15 minutes"

    === "Using cron expression"

        ```yaml title="docker-compose.yml"
        services:
          app:
            image: example/backup:latest
            labels:
              cd.doco.job.enabled: "true"
              cd.doco.job.schedule: "*/15 * * * *"
        ```

    === "Using interval format"

        ```yaml title="docker-compose.yml"
        services:
          app:
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

## Configuration

Use the following service labels to configure scheduled jobs:

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

| Label                              | Type    | Description                                                                                       | Default   |
|------------------------------------|---------|---------------------------------------------------------------------------------------------------|-----------|
| `cd.doco.job.enabled`              | boolean | Enable scheduling for this service/container                                                      | `false`   |
| `cd.doco.job.schedule`             | string  | [Schedule format](#schedule-formats) to use                                                       |           |
| `cd.doco.job.execution_mode`       | string  | `restart` (default behavior) or `one_shot` (ephemeral execution)                                  | `restart` |
| `cd.doco.job.skip_running`         | boolean | Do not run the job if a previous scheduled run is still active/running                            | `false`   |
| `cd.doco.job.notify_on`            | string  | [Notification](Notifications.md) behavior for scheduled runs: `none`, `success`, `failure`, `all` | `all`     |
| `cd.doco.job.swarm.replicas`       | integer | Number of completions/concurrency for swarm one-shot jobs in `replicated-job` mode                | `1`       |

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