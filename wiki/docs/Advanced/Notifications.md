---
tags:
  - Advanced
  - Setup
  - Notifications
---

Doco-CD can be configured to send notifications with [Apprise](https://github.com/caronc/apprise) to various services when a deployment is started, finished, or failed.
You can find a list of all supported services and platforms in the [Apprise documentation](https://github.com/caronc/apprise/wiki#notification-services).

For that, specify the required settings in the app `environment` section and add an Apprise container to your `docker-compose.yml` file.

## Settings

| Key                        | Type   | Description                                                                                                                                                                                                                              | Default value |
|----------------------------|--------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------|
| `APPRISE_API_URL`          | string | The URL of the Apprise API to send notifications to (e.g. `http://apprise:8000/notify`)                                                                                                                                                  |               |
| `APPRISE_NOTIFY_URLS`      | string | A comma-separated list of Apprise-URLs to send notifications to the [supported services/platforms](https://github.com/caronc/apprise/wiki#notification-services) (e.g. `pover://{user_key}@{token},mailto://{user}:{password}@{domain}`) |               |
| `APPRISE_NOTIFY_URLS_FILE` | string | Path to the file inside the container containing the Apprise-URLs (see `APPRISE_NOTIFY_URLS`). Mutually exclusive with `APPRISE_NOTIFY_URLS`.                                                                                            |               |
| `APPRISE_NOTIFY_LEVEL`     | string | The minimum level of notifications to send. Possible values: `info`, `success`, `warning`, `failure`                                                                                                                                     | `success`     |

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