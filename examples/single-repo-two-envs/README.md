# Single repo → two environments

One app repo, two Docker hosts (staging + prod). The repo carries **two deploy configs**. Each host's doco-cd instance picks its own via the poll `target:` field. Two stacks per environment: `app` and `db`.

## What this example shows

- **`target:` is real isolation.** The staging daemon (no `target:`) reads only the bare `.doco-cd.yml`. The prod daemon (`target: prod`) reads only `.doco-cd.prod.yml`. Staging values cannot leak into prod.
- The prod config repeats only what differs: image tags, domain, log level. Behavior blocks are copied on purpose, so the two files cannot argue.
- **Separate tags per stack.** The db stack pins its own tag. A web-only release must not recreate the stateful side.
- Why `reconciliation.events` stays `[unhealthy]` and never gets `die`: the one-shot `migrate` container exits 0 on every deploy by design, and `die` reads that as a failure, causing an unbounded reconcile loop.
- Cross-stack networking via an **external network**, since each stack is its own compose project.
- **Deploy notifications** to Telegram (or anything else) via an Apprise sidecar, with a compact body template.

## Layout

```
app-repo/                  # your Git repository
  .doco-cd.yml             # staging config (both stacks)
  .doco-cd.prod.yml        # prod config with only the values that differ
  deploy/
    app/compose.yaml       # web stack
    db/compose.yaml        # postgres + one-shot migrate
server/                    # one copy per host, e.g. /opt/doco-cd/
  compose.yaml             # doco-cd + apprise sidecar (same file on both hosts)
  poll.staging.yaml        # staging host: no target
  poll.prod.yaml           # prod host: target: prod
  secrets.env.example
```

## Try it

1. Push `app-repo/` contents to a Git repository.
2. On each host: copy `server/` to `/opt/doco-cd/`, rename the host's poll file to `poll.yaml`.
3. Fill in `secrets.env` (from the example), `chmod 600`.
4. Create the shared network once per host: `docker network create appnet`.
5. `docker compose up -d` in `/opt/doco-cd/`.

## Release flow

Merge to `main` → staging deploys within one poll interval. To release to prod, bump the tags in `.doco-cd.prod.yml` (by hand or by a CI release job) and push.

Want prod to move **only** on releases, config included? Point the prod poll at a moving tag (`refs/tags/live`) that your release job force-pushes. Then a compose or Caddyfile merge to `main` cannot reach prod between releases, and moving the tag backward is a complete rollback. See the comment in `poll.prod.yaml`.
