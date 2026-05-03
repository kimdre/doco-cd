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

## Example `docker-compose.yml`

Adjust your `docker-compose.yml` file to include the Apprise service and the necessary environment variables for the app:

```yaml title="docker-compose.yml" hl_lines="5-10 12-19"
services:
  app:
    container_name: doco-cd
    # add the code below to your existing docker-compose.yml file
    depends_on:
      - apprise
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
```

## Metadata fields

When a notification is sent, the following metadata fields are included in the notification body:

| Field name   | Description                                                                                                                                      | Example                            |
|--------------|--------------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------|
| `job_id`     | Unique ID of the deployment job that triggered the notification (not included for [reconciliation notifications](#reconciliation-notifications)) |                                    |
| `repository` | Repository name                                                                                                                                  | `github.com/my/repo`               |
| `revision`   | Branch/tag and Commit SHA that was deployed                                                                                                      | `main (abc123)`, `v1.0.0 (def456)` |
| `stack`      | Project/Stack name                                                                                                                               | `my-stack`                         |

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
