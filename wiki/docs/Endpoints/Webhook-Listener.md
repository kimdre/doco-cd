# Webhook Listener

The webhook payload is expected to be in JSON format and must contain the payload from a supported the Git provider, see [Supported Git Providers](../index.md#supported-git-providers).

The application listens for incoming webhooks on the `/v1/webhook` endpoint with the port specified by the `HTTP_PORT` environment variable, see [App Settings](../App-Settings.md#available-settings).

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

## Query Parameters

### `wait`

Webhooks trigger deployments asynchronously by default, meaning the application will respond immediately to the webhook request and process the deployment in the background.
Use the `wait=true` query parameter to make the application wait for the deployment to finish before responding.
This may increase response time, and some Git/SCM providers might time out the request if the deployment takes too long.
Even if the request times out, the deployment will still continue.

**Example:** `/v1/webhook?wait=true`