# Single repo → single environment

One application repository, one Docker host. The repository carries the Compose files and deployment config; a Doco-CD controller on the host polls the repository for changes.

This is a fuller reference rather than the shortest first run. The demo app includes Caddy and publishes host ports 80/443. Replace `app.example.com` with a domain configured for your host, or adapt the app Compose file for your existing reverse proxy or LAN-only setup. The controller itself publishes no ports.

## What this example shows

- A deploy config (`.doco-cd.yml`) with the deployed image tag **pinned in Git**. Bumping the tag is the release: commit + push, doco-cd redeploys. No SSH to the host.
- Secrets kept out of Git: they live on the host in `secrets.env`.
- A poll-only controller: Doco-CD itself needs no published port. The deployed app's Caddy service publishes 80/443 for its HTTP/HTTPS traffic.
- Mounted config auto-apply: edit the `Caddyfile`, push, and doco-cd force-recreates only the service that mounts it.

`PASS_ENV=true` forwards all environment variables from the Doco-CD container to Compose interpolation for every deployment. Use it only when the deployment repository is trusted; its Compose files can reference those variables. See [`PASS_ENV`](../../wiki/docs/Deploy-Settings.md#app-configuration).

The sample `whoami` service has no healthcheck, so it does not demonstrate unhealthy-container reconciliation. For a real app, add an app-specific healthcheck before configuring that feature; see [Reconciliation Settings](../../wiki/docs/Deploy-Settings.md#reconciliation-settings).

## Layout

```
app-repo/                 # your Git repository
  .doco-cd.yml            # deploy config: stack name + image tag + non-secret env
  deploy/
    compose.yaml          # the stack
    Caddyfile             # mounted config, edits auto-apply
server/                   # lives on the host (e.g. /opt/doco-cd/), NOT in Git
  compose.yaml            # doco-cd itself
  poll.yaml               # which repo to watch
  secrets.env.example     # copy to secrets.env on the host, chmod 600
```

## Try it

1. Create a Git repository from the contents of `app-repo/`. Replace `app.example.com` in `.doco-cd.yml` with your domain. If using Caddy's automatic HTTPS as written, configure that domain to reach the host and allow the required traffic on ports 80/443; otherwise adapt the Caddy service to your local DNS or existing reverse proxy.
2. Copy the `server/` directory to the Docker host, e.g. to `/opt/doco-cd/`. This is the controller configuration, not part of the application Git repository.
3. Copy `secrets.env.example` to `secrets.env`, add a read-only Git token for a private HTTPS repository, and set any app secrets used by your Compose file. Keep the real file on the host and restrict its permissions:

    ```sh
    chmod 600 /opt/doco-cd/secrets.env
    ```

4. Edit `/opt/doco-cd/poll.yaml` with your repository URL and branch. The `https://github.com/example/app-repo.git` URL is a placeholder.
5. Start the controller:

    ```sh
    cd /opt/doco-cd
    docker compose up -d
    docker compose ps
    docker compose logs -f
    ```

The first deployment should start on the initial poll, then later pushes to the configured branch are deployed on the 30-second interval. The app's Caddy service binds host ports 80/443; open or forward them only if you intend to make the app reachable that way. An existing proxy may already use those ports.

## Changing the demo image

For this demo, change `APP_TAG` in `.doco-cd.yml` to another valid `traefik/whoami` image tag, then commit and push. Doco-CD sees the deploy-config change and redeploys. A real project can automate image builds and tag updates in CI, but this example does not include a CI workflow.

A commit that touches nothing the stack references (docs, other dirs) is skipped: cloned, compared, no `compose up`.
