---
tags:
  - Advanced
  - Configuration
  - Deployment
---

# Sync Windows

Sync windows control *when* doco-cd is allowed to change your stacks. For example, you can deploy only during business hours, freeze deployments over the weekend, or restrict production to a nightly maintenance window.
Each window is either an `allow` or a `deny` window. It opens on a cron schedule and stays active for a fixed duration.

!!! info "Windows are configured on the doco-cd instance"
    You set sync windows with [`SYNC_WINDOWS`](#configuration) or [`SYNC_WINDOWS_FILE`](#configuration) on the doco-cd container, **not** in the `.doco-cd.yaml` deploy config or in a poll config.
    That way a commit can't lift the window that is supposed to hold it back.

## Configuration

| Key                 | Type   | Description                                                                                | Default                    |
|---------------------|--------|--------------------------------------------------------------------------------------------|----------------------------|
| `SYNC_WINDOWS`      | list   | A YAML list of sync windows (see below).                                                   | Ignored when not specified |
| `SYNC_WINDOWS_FILE` | string | Path to a file inside the container that contains the list of sync windows in YAML format. | Ignored when not specified |

`SYNC_WINDOWS` and `SYNC_WINDOWS_FILE` are mutually exclusive. If a window is invalid, doco-cd refuses to start. At startup, doco-cd logs every configured window and whether it is currently active.

Each window supports the following settings:

!!! note "Settings without a default value are required."

| Key            | Type            | Description                                                                                                                                                                                                                                                                                                                                                                                               | Default                                                           |
|----------------|-----------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------|
| `name`         | string          | Name of the window, shown in logs, metrics and the API. Names must be unique (case-insensitive).                                                                                                                                                                                                                                                                                                          | `<kind>-<position>`                                               |
| `kind`         | string          | `allow` or `deny`. See [evaluation](#evaluation).                                                                                                                                                                                                                                                                                                                                                         |                                                                   |
| `schedule`     | string          | When the window opens. Use a 5-field [cron expression](https://pkg.go.dev/github.com/robfig/cron#hdr-CRON_Expression_Format) without seconds (`minute hour day-of-month month day-of-week`) or a [predefined schedule](https://pkg.go.dev/github.com/robfig/cron#hdr-Predefined_schedules) such as `@daily`. `@every` intervals and `TZ=`/`CRON_TZ=` prefixes are not supported (use `timezone` instead). |                                                                   |
| `duration`     | string          | How long the window stays active after each opening, as a [Go duration](https://pkg.go.dev/time#ParseDuration) (e.g. `30m`, `10h`, `62h`). Minimum `1m`.                                                                                                                                                                                                                                                  |                                                                   |
| `timezone`     | string          | [IANA timezone](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones) the `schedule` is evaluated in, e.g. `Europe/Berlin`.                                                                                                                                                                                                                                                                       | Timezone of doco-cd ([`TZ`](../App-Settings.md#runtime-settings)) |
| `repositories` | list of strings | [Glob patterns](#selectors) matched against the repository name, e.g. `github.com/acme/*`.                                                                                                                                                                                                                                                                                                                | All repositories                                                  |
| `deployments`  | list of strings | [Glob patterns](#selectors) matched against the deployment `name` from the [deploy config](../Deploy-Settings.md) (the Compose project or Swarm stack name).                                                                                                                                                                                                                                              | All deployments                                                   |
| `contexts`     | list of strings | [Glob patterns](#selectors) matched against the [Docker context](Docker-Contexts.md) name. The default context is `default`.                                                                                                                                                                                                                                                                              | All contexts                                                      |
| `manual_sync`  | boolean         | Allow [manual deployments](#manual-deployments) that this window would block.                                                                                                                                                                                                                                                                                                                             | `false`                                                           |

### Selectors

`repositories`, `deployments` and `contexts` select the deployments a window applies to. A window applies to a deployment only if **all** of its selectors match. An empty or omitted selector matches everything.

- Patterns are matched case-insensitively against the whole value.
- `*` matches any number of characters, including `/`, and `?` matches exactly one character.
- The repository name is the one doco-cd logs in the `repository` field, i.e. host and path without scheme and `.git` suffix, e.g. `github.com/acme/app`.
  For deploy configs with a [`repository_url`](../Deploy-Settings.md), a pattern matches if it matches either the repository in `repository_url` or the repository that contains the deploy config.

### Timezone and DST

Each window's `schedule` is evaluated in its `timezone`, or in the timezone of doco-cd ([`TZ`](../App-Settings.md#runtime-settings)) if none is set. Windows follow daylight saving time changes: a window scheduled at `0 8 * * *` in `Europe/Berlin` always opens at 08:00 local time.

## Evaluation

A window is **active** from each time its `schedule` fires until `duration` has passed. Occurrences may overlap, for example a daily schedule with a `36h` duration.

When doco-cd is about to change a stack, it only considers the windows that match the deployment:

1. If no window matches, the deployment is allowed.
2. If a matching `deny` window is active, the deployment is blocked. Deny windows always win over allow windows.
3. If there are matching `allow` windows but none of them is active, the deployment is blocked.
4. Otherwise, the deployment is allowed.

Only actual changes are checked. A poll or webhook that finds nothing to deploy is never reported as deferred.

The decision is made once, when the webhook, poll or API request arrives. A deployment that was allowed then finishes, even if the window closes while it waits for a free deployment slot ([`MAX_CONCURRENT_DEPLOYMENTS`](../Deploy-Settings.md#app-configuration)) or while it runs.

If a single trigger deploys several stacks and only some of them are blocked, the allowed stacks are deployed and the blocked ones are deferred.

### What sync windows apply to

| Action                                                                                                                               | Affected by sync windows                                                     |
|--------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------------------------|
| Deployments triggered by [webhooks](../Endpoints/Webhook-Listener.md)                                                                | Yes                                                                          |
| Deployments triggered by [poll jobs](../Poll-Settings.md) (with `interval`, `schedule` or the local repository watcher)              | Yes                                                                          |
| [Poll runs](../Endpoints/REST-API.md#polling) triggered via the REST API or the MCP `trigger_poll` tool                              | Yes, unless every blocking window has [`manual_sync`](#manual-deployments)   |
| The one-shot [self-update bootstrap](Self-Updating.md#bootstrap)                                                                     | Yes, unless every blocking window has [`manual_sync`](#manual-deployments)   |
| `destroy: true` deploy configs and the removal of auto-discovered stacks that were deleted from the repository                       | Yes                                                                          |
| [Reconciliation](../Deploy-Settings.md#reconciliation-settings) restoring the already deployed revision, e.g. after a container died | No, unless it would deploy another revision, see [limitations](#limitations) |
| [Scheduled jobs](Job-Scheduling.md)                                                                                                  | No                                                                           |
| Project and stack actions via the REST API or MCP (start, stop, restart, scale, remove, …)                                           | No                                                                           |

A self-update bootstrap that a window blocks deploys nothing and exits with a non-zero exit code, see [Bootstrap](Self-Updating.md#bootstrap).

### Manual deployments

Poll runs you trigger via the [REST API](../Endpoints/REST-API.md#polling) or the [MCP server](../Endpoints/MCP-Server.md) count as manual deployments.
A manual deployment may bypass a block only if **every** window that blocks it has `manual_sync: true`. This lets you do emergency deployments during a freeze while webhooks and polls stay blocked.
doco-cd logs `sync window bypassed by manual deployment` whenever this happens.

## Deferred deployments

A blocked deployment is **skipped, not queued**. doco-cd doesn't replay it once the window opens.
Instead, the next trigger that arrives while the window is open deploys the then-latest revision:

- **Polling**: the next poll inside the window picks up the change automatically. This is the recommended setup with sync windows.
- **Webhooks only**: nothing happens until the next webhook arrives inside a window, e.g. with the next push, or until you [trigger a poll run](../Endpoints/REST-API.md#polling).

!!! tip "Catch up when an allow window opens"
    Combine an allow window with a poll job whose [`schedule`](../Poll-Settings.md) fires when the window opens, so deferred changes are deployed right away:

    ```yaml title="SYNC_WINDOWS"
    - name: business-hours
      kind: allow
      schedule: "0 8 * * 1-5"
      duration: 10h
    ```

    ```yaml title="POLL_CONFIG"
    - url: https://github.com/acme/app.git
      schedule: "0 8-17 * * 1-5" # every hour during business hours, starting when the window opens
    ```

    Poll schedules use the timezone of doco-cd ([`TZ`](../App-Settings.md#runtime-settings)). If the window has a different `timezone`, prefix the poll schedule with it, e.g. `CRON_TZ=Europe/Berlin 0 8-17 * * 1-5`.

### Reporting

When a deployment is deferred, doco-cd reports it as follows:

- **Logs**: `deployment deferred by sync window` with the blocking `sync_windows` and the `next_open` time, if known. It is logged at `info` level the first time a revision of a stack is deferred and at `debug` level on repeated polls.
  Deferred removals of deleted auto-discovered stacks are logged as `removal of obsolete auto-discovered stack deferred by sync window`.
- **Webhooks**: if every stack of the webhook was deferred, the response is `202 Accepted` with the message `deployment deferred by sync window until <time>`.
- **Deployment runs**: runs where every stack was deferred have the status `skipped` in the [deployment runs API](../Endpoints/REST-API.md#deployment-runs). A poll run triggered with `wait=true` responds with `202 Accepted` and the deferral message. The MCP `trigger_poll` tool returns the status `skipped`.
- **Commit status**: with [`GIT_COMMIT_STATUS`](../Git-Settings.md) enabled, the commit gets a `pending` status `Deferred by sync window until <time>`. It becomes `success` once the same commit is deployed inside a window.
- **Metrics**: `doco_cd_sync_window_blocked_total{repository,deployment,context,window}` counts every deferral, once per blocking window, see [Prometheus Metrics](../Endpoints/Metrics.md).
- **Notifications**: deferrals do not send [notifications](Notifications.md), because they would repeat on every poll.

### Inspecting sync windows

Use the [`GET /v1/api/sync-windows`](../Endpoints/REST-API.md#sync-windows) endpoint or the MCP tool [`list_sync_windows`](../Endpoints/MCP-Server.md#available-tools) to list all windows and whether they are active.
With a repository, deployment or context, they also tell you whether that deployment would be allowed right now and when it may deploy next.

## Examples

=== "Business hours"

    Only deploy on weekdays between 08:00 and 18:00 in Berlin:

    ```yaml
    SYNC_WINDOWS: |
      - name: business-hours
        kind: allow
        schedule: "0 8 * * 1-5"
        duration: 10h
        timezone: Europe/Berlin
    ```

=== "Weekend freeze"

    Block all deployments from Friday 18:00 until Monday 08:00:

    ```yaml
    SYNC_WINDOWS: |
      - name: weekend-freeze
        kind: deny
        schedule: "0 18 * * 5"
        duration: 62h
    ```

=== "Production maintenance window"

    Deploy to the `production` Docker context only at night, while other contexts are unrestricted:

    ```yaml
    SYNC_WINDOWS: |
      - name: prod-maintenance
        kind: allow
        schedule: "0 2 * * *"
        duration: 2h
        contexts: ["production"]
    ```

=== "Freeze with emergency deployments"

    Block the deployments of the `acme` organization over the holidays, but allow poll runs triggered via the REST API or MCP:

    ```yaml
    SYNC_WINDOWS: |
      - name: holiday-freeze
        kind: deny
        schedule: "0 0 24 12 *"
        duration: 192h # until January 1st, 00:00
        repositories: ["github.com/acme/*"]
        manual_sync: true
    ```

=== "With `SYNC_WINDOWS_FILE`"

    ```yaml title="sync-windows.yaml"
    - name: business-hours
      kind: allow
      schedule: "0 8 * * 1-5"
      duration: 10h
      deployments: ["prod-*"]
    - name: weekend-freeze
      kind: deny
      schedule: "0 18 * * 5"
      duration: 62h
    ```

    ```yaml title="docker-compose.yaml" hl_lines="7 10"
    services:
      app:
        container_name: doco-cd
        image: ghcr.io/kimdre/doco-cd:latest
        environment:
          TZ: Europe/Berlin
          SYNC_WINDOWS_FILE: /sync-windows.yaml
        volumes:
          - /var/run/docker.sock:/var/run/docker.sock
          - ./sync-windows.yaml:/sync-windows.yaml:ro
          - data:/data

    volumes:
      data:
    ```

## Limitations

- **No queue**: deferred deployments are not replayed when a window opens, see [deferred deployments](#deferred-deployments).
- **Reconciliation while a revision is deferred**: reconciliation keeps restoring the revision that was deployed before, and the stack is not removed as an obsolete auto-discovered stack. If doco-cd restarts while a revision is deferred, it doesn't know the previous revision of that stack anymore, so the stack is not reconciled until a deployment inside a window succeeds.
- **Reconciliation after a restart with scheduled poll jobs**: reconciliation of a repository starts with its first deployment run after doco-cd started. A [poll job with a `schedule`](../Poll-Settings.md#cron-schedules) does not poll at startup, so its stacks are not reconciled until the first scheduled poll.
- **Reconciliation of stacks with their own reference**: reconciliation resolves the reference of deploy configs with their own `reference`, `repository_url` or `git_depth` again. While a window blocks such a stack, it is only restored if the resolved revision matches the revision its containers are labeled with. If the reference moved on, or none of its containers are left to compare with, reconciliation is deferred like an automatic deployment.
- **Superseded commits stay pending**: if a deferred commit is superseded by a newer one before a window opens, its `pending` commit status is never updated.
- **Admitted deployments finish**: a deployment that was allowed when its trigger arrived is not cancelled when a window closes.
