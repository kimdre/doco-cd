# Single repo → single environment

One app repo, one VM. The repo carries the compose files and the deploy config. The VM runs doco-cd, which polls the repo and reconciles the stack on every push to `main`.

## What this example shows

- A deploy config (`.doco-cd.yml`) with the deployed image tag **pinned in Git**. Bumping the tag is the release: commit + push, doco-cd redeploys. No SSH to the VM.
- Secrets that **never touch the repo**: they live on the VM in `secrets.env`, and `PASS_ENV=true` on the daemon forwards them into compose interpolation.
- A poll-only daemon: no published ports, no webhook setup, firewall stays closed.
- Runtime self-healing (`reconciliation`): an unhealthy container gets restarted in place.
- Mounted config auto-apply: edit the `Caddyfile`, push, and doco-cd force-recreates only the service that mounts it.

## Layout

```
app-repo/                 # your Git repository
  .doco-cd.yml            # deploy config: stack name + image tag + non-secret env
  deploy/
    compose.yaml          # the stack
    Caddyfile             # mounted config, edits auto-apply
server/                   # lives on the VM (e.g. /opt/doco-cd/), NOT in Git
  compose.yaml            # doco-cd itself
  poll.yaml               # which repo to watch
  secrets.env.example     # copy to secrets.env on the VM, chmod 600
```

## Try it

1. Push `app-repo/` contents to a Git repository.
2. Copy `server/` to the VM, e.g. to `/opt/doco-cd/`.
3. Copy `secrets.env.example` to `secrets.env` and fill it in. `chmod 600 secrets.env`.
4. Edit `poll.yaml` to point at your repository.
5. Run `docker compose up -d` in `/opt/doco-cd/`.

Within one poll interval the stack is up. From now on, a push to `main` is a deploy.

## How a release works

CI builds the image and tags it with the commit SHA. Then a CI job rewrites `APP_TAG` in `.doco-cd.yml` and commits with `[skip ci]`. doco-cd sees the deploy config changed and redeploys. The repo always states what runs — that is the whole point.

A commit that touches nothing the stack references (docs, other dirs) is skipped: cloned, compared, no `compose up`.
