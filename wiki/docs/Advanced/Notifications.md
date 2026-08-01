---
tags:
  - Advanced
  - Setup
  - Notifications
---

Doco-CD can be configured to send notifications with [Apprise](https://github.com/caronc/apprise) to various services when a deployment is started, finished, or failed and on [reconciliation](../Deploy-Settings.md#reconciliation-settings) events.
You can find a list of all supported services and platforms in the [Apprise documentation](https://appriseit.com/).

For that, specify the required settings in the app `environment` section and add an Apprise container to your `docker-compose.yml` file.

## Settings

| Key                        | Type   | Description                                                                                                                                                                                                 | Default value |
|----------------------------|--------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------|
| `APPRISE_API_URL`          | string | The URL of the Apprise API to send notifications to (e.g. `http://apprise:8000/notify`)                                                                                                                     |               |
| `APPRISE_NOTIFY_URLS`      | string | A comma-separated list of Apprise-URLs to send notifications to the [supported services/platforms](https://appriseit.com/services/) (e.g. `pover://{user_key}@{token},mailto://{user}:{password}@{domain}`) |               |
| `APPRISE_NOTIFY_URLS_FILE` | string | Path to the file inside the container containing the Apprise-URLs (see `APPRISE_NOTIFY_URLS`). Mutually exclusive with `APPRISE_NOTIFY_URLS`.                                                               |               |
| `APPRISE_NOTIFY_LEVEL`     | string | The minimum level of notifications to send. Possible values: `info`, `success`, `warning`, `failure`                                                                                                        | `success`     |
| `APPRISE_NOTIFY_BODY_TEMPLATE`  | string | Optional [Go `text/template`](https://pkg.go.dev/text/template) rendering the notification body (see [Custom notification body](#custom-notification-body)). Empty uses the built-in format.                  |               |
| `APPRISE_NOTIFY_BODY_TEMPLATE_FILE` | string | Path to a file inside the container containing the template (see `APPRISE_NOTIFY_BODY_TEMPLATE`). Mutually exclusive with `APPRISE_NOTIFY_BODY_TEMPLATE`.                                                          |               |

## Example `docker-compose.yml`

Adjust your `docker-compose.yml` file to include the Apprise service and the necessary environment variables for the app:

```yaml title="docker-compose.yml" hl_lines="5-11 13-26"
services:
  app:
    container_name: doco-cd
    # add the code below to your existing docker-compose.yml file
    depends_on:
      apprise:
        condition: service_healthy
    environment:
      APPRISE_API_URL: http://apprise:8000/notify
      APPRISE_NOTIFY_LEVEL: success
      APPRISE_NOTIFY_URLS: "pover://{user_key}@{token},mailto://{user}:{password}@{domain}"

  apprise:
    image: caronc/apprise:latest
    restart: unless-stopped
    ports:
      - "8000:8000"
    environment:
      TZ: Europe/Berlin
      APPRISE_WORKER_COUNT: 1
    healthcheck:
      test: [ "CMD-SHELL", "curl -fsS http://localhost:8000/status >/dev/null || exit 1" ]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 20s
```

## Metadata fields

When a notification is sent, the following metadata fields are included in the notification body:

| Field name   | Description                                                                                                                                      | Example                            |
|--------------|--------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------|
| `duration`   | Time the deployment took (omitted when no deployment ran, e.g. for [reconciliation notifications](#reconciliation-notifications))                 | `12.483s`                          |
| `job_id`     | Unique ID of the deployment job that triggered the notification (not included for [reconciliation notifications](#reconciliation-notifications)) |                                    |
| `repository` | Repository name                                                                                                                                  | `github.com/my/repo`               |
| `revision`   | Branch/tag and Commit SHA that was deployed                                                                                                      | `main (abc123)`, `v1.0.0 (def456)` |
| `stack`      | Project/Stack name                                                                                                                               | `my-stack`                         |

## Custom notification body

By default the notification body is the message followed by the [metadata fields](#metadata-fields) as `key: value` lines. Set `APPRISE_NOTIFY_BODY_TEMPLATE` (or `APPRISE_NOTIFY_BODY_TEMPLATE_FILE`) to a [Go `text/template`](https://pkg.go.dev/text/template) to render the body yourself — useful to drop noisy fields, add a host label, or produce a one-liner when several stacks report into one channel.

The template is validated at startup: a syntax error or a reference to an unknown field stops doco-cd from starting. The title (emoji + optional `[R]` marker + title text) is not affected by the template.

!!! warning "Don't swallow failure reasons"

    The template replaces the default body for **every** notification level, and on `failure` the error text is only carried by `{{ .Message }}` — a template that never references it produces failure notifications with no failure reason at all (the title only says `Deployment failed`; the details then live only in the logs). On `success`, `.Message` is largely redundant with the title, so compact templates should include it guarded by level:

    ```
    {{ if ne .Level "success" }} — {{ .Message }}{{ end }}
    ```

The following fields are available:

| Field                 | Description                                                              |
|-----------------------|--------------------------------------------------------------------------|
| `.Level`              | `info`, `success`, `warning` or `failure`                                |
| `.Emoji`              | Level emoji (`ℹ️`/`✅`/`⚠️`/`❌`)                                          |
| `.Title`              | Title text, e.g. `Deployment completed`                                  |
| `.Message`            | Core message                                                             |
| `.IsReconciliation`   | `true` when triggered by a [reconciliation](#reconciliation-notifications) event |
| `.Repository`         | Repository name                                                          |
| `.Stack`              | Project/Stack name                                                       |
| `.Target`             | Custom webhook/poll target (empty for the default target)                |
| `.Context`            | Docker context the stack is deployed to (empty for the default context)  |
| `.Revision`           | Branch/tag and commit SHA                                                |
| `.JobID`              | Deployment job ID (empty for reconciliation events)                      |
| `.ReconciliationEvent`| Reconciliation event that triggered the action                           |
| `.TraceID`            | Reconciliation trace ID                                                  |
| `.AffectedActorKind`  | `container` or `service`                                                 |
| `.AffectedActorID`    | Affected container/service ID                                            |
| `.AffectedActorName`  | Affected container/service name                                          |
| `.Commits`            | Commits deployed since the last deploy (see [Commit changelog](#commit-changelog)) |
| `.Duration`           | Time the deployment (or destroy) took, from job start to the notification, e.g. `12.483s`. Zero where no deployment ran (reconciliation restarts, scheduled jobs) — hide it with `{{if .Duration}}...{{end}}` |
| `.ChangedServices`    | Sorted names of the services force-recreated by this deploy (changed mounted/referenced files or a changed auto-discovery label). Empty when the whole stack is (re)deployed for other reasons, e.g. a compose config change, an image update, state drift or `force_recreate` |

`{{ .DefaultBody }}` renders the built-in body (message + metadata), so you can extend the default format instead of replacing it, e.g. `{{ .DefaultBody }}\nhost: my-vm`.

!!! example "One-line body"

    ```yaml
    environment:
      APPRISE_NOTIFY_BODY_TEMPLATE: "{{.Emoji}} {{if .Target}}{{.Target}}/{{end}}{{.Stack}} — {{.Message}} ({{.Revision}})"
    ```

    renders e.g. `✅ prod-vm/app — Successfully deployed stack app (main (abc123))` for target `prod-vm`, or `✅ app — Successfully deployed stack app (main (abc123))` without a custom target.

### Commit changelog

`.Commits` is the list of commits that got deployed since the previously deployed commit, newest first. It is only populated on successful deploy notifications of Git sources; on the first deploy, on failures and for OCI sources it stays empty (a `range` over it just renders nothing). Each entry has:

| Field         | Description                          |
|---------------|--------------------------------------|
| `.Hash`       | Full commit SHA                       |
| `.ShortHash`  | Shortened commit SHA                  |
| `.Subject`    | First line of the commit message     |
| `.Author`     | Commit author name                    |

Printing an entry directly (`{{ . }}`) gives `shortHash subject`.

!!! example "Body with changelog"

    ```yaml
    environment:
      APPRISE_NOTIFY_BODY_TEMPLATE: |
        {{ .DefaultBody }}
        {{ range .Commits }}- {{ .ShortHash }} {{ .Subject }} ({{ .Author }})
        {{ end }}
    ```

Up to 50 commits are listed. After a rebase or force-push the list starts from the point where the histories diverged.

The changelog is best-effort: with a small `GIT_CLONE_DEPTH` the previously deployed commit may sit beyond the shallow boundary, in which case the list is truncated or omitted. It never blocks the notification.

## Reconciliation notifications

If a notification was triggered by reconciliation, the title gets a short `[R]` marker.

!!! example "Example notification titles"

    - Regular deploy notification title: `✅ Deployment completed`
    - Reconciliation notification title: `✅ [R] Deployment completed`

Reconciliation notifications also include a `reconciliation:` block in the body [metadata](#metadata-fields).

### Metadata fields

=== "Docker Standalone"
  
    | Field name       | Description                                    |
    |------------------|------------------------------------------------|
    | `event`          | reconciliation event that triggered the action |
    | `container_id`   | affected container name                        |
    | `container_name` | affected container name                        |
    | `trace_id`       | reconciliation trace ID for log correlation    |

=== "Docker Swarm"
    
    | Field name      | Description                                    |
    |-----------------|------------------------------------------------|
    | `event`         | reconciliation event that triggered the action |
    | `service_id`    | affected service name                          |
    | `service_name`  | affected service name                          |
    | `trace_id`      | reconciliation trace ID for log correlation    |
