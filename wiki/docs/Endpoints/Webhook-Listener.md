---
tags:
  - Reference
  - Endpoints
  - Webhook
---

# Webhook Listener

The webhook payload is expected to be in JSON format and must contain the payload from a [supported Git Provider](../index.md#supported-git-providers) or for a [OCI artifact](../Advanced/OCI/Webhooks.md).

The application listens for incoming webhooks on the `/v1/webhook` endpoint with the port specified by the `HTTP_PORT` environment variable, see [App Settings](../App-Settings.md#general-settings).
Set both `HTTP_TLS_CERT_FILE` and `HTTP_TLS_KEY_FILE` to serve the endpoint over HTTPS directly from doco-cd.

!!! info "Source URL Rewrites"
    Webhook deployments support rewriting git clone URLs to internal addresses via [`SOURCE_URL_REWRITES`](../App-Settings.md#source-url-rewrites). This is useful when the Git provider advertises a public URL in webhooks but doco-cd should clone via an internal network path.

## Allow/deny trigger events

By default, all incoming webhooks are accepted and trigger deployments if they match a deployment configuration.
See [Webhook Filter](../Deploy-Settings.md#webhook-filter) for more granular control over which webhooks should trigger deployments.

## With custom Target

You can specify multiple deployment target configurations in a mono-repo style setup using the application's dynamic webhook path.
This allows you to deploy to different targets/locations from a single repository.

If a webhook payload gets sent to a custom path suffix `/v1/webhook/<custom_name>`, the application will look for
a deployment configuration file with the same pattern in its name `.doco-cd.<custom_name>.yaml`.

### Examples

| Webhook Target              | Deployment Config File        |
|-----------------------------|-------------------------------|
| `/v1/webhook`               | `.doco-cd.yaml`               |
| `/v1/webhook/gitea`         | `.doco-cd.gitea.yaml`         |
| `/v1/webhook/paperless-ngx` | `.doco-cd.paperless-ngx.yaml` |
| `/v1/webhook/my.server.com` | `.doco-cd.my.server.com.yaml` |

!!! note "Naming convention for nested config overrides"
    [Nested config overrides](../Deploy-Settings.md#nested-config-overrides) always use the standard [naming convention](../Deploy-Settings.md#deployment-configuration-file) (`.doco-cd.y(a)ml`), not the custom target name.

## Query Parameters

### `wait`

Webhooks trigger deployments asynchronously by default, meaning the application will respond immediately to the webhook request and process the deployment in the background.
Use the `wait=true` query parameter to make the application wait for the deployment to finish before responding.
This may increase response time, and some Git/SCM providers might time out the request if the deployment takes too long.
Even if the request times out, the deployment will still continue.

In async mode, the response contains a `job_id` that can be queried through the [Deployment Runs API](REST-API.md#deployment-runs) (requires REST API auth).

**Example:** `/v1/webhook?wait=true`
