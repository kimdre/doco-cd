# Central deployments repo → many apps, many hosts

One repo is the source of truth for a fleet. Each host polls it with its own `target:` and reads only its own `.doco-cd.<host>.yml`. A commit here **is** a deploy.

Needs doco-cd >= 0.108.0 (remote compose `include:`).

## Layout

```
deployments-repo/
  .doco-cd.shop-dev.yml       # per-host: which stacks it runs + every version pin
  .doco-cd.shop-prod.yml
  .sops.yaml
  shared/db/compose.yaml      # a compose several envs include
  shop-dev/                   # one dir = everything this host runs
    app/{compose.yaml,env/app.env,secrets/app.sops.env}
    db/{compose.yaml,env/db.env,secrets/db.sops.env}
  shop-prod/…
app-repo/
  deploy/compose.yaml           # the app's compose, in the app's own repo
  .github/workflows/deploy.yml  # build image → bump pins = deploy
server/                       # copy to /opt/doco-cd/ on each host
```

## How it fits together

- **A host maps to a directory.** `shop-dev/` holds every stack that host runs, one subdir each.
- **A stack is self-contained.** `shop-dev/app/` holds its own compose, `env/` and `secrets/`, and refers to them by plain relative paths (`./env/app.env`). Copy a stack directory to start a new one.
- **Compose files live where the thing lives:** the app's compose in the app repo, anything shared by several envs in `shared/`. Each env pulls one in with `include:` at a pinned sha, so the two envs can run different revisions.
- **Versions live in `.doco-cd.<host>.yml`** — your images by git sha, third-party by exact tag. That file is the full inventory of what a host runs, and its diff is the changelog.
- **Config and secrets are per stack.** Cleartext in `env/`, SOPS-encrypted in `secrets/`. The daemon decrypts at deploy time; values land as container env.

## Try it

1. Push `deployments-repo/` to a Git repo; put `app-repo/deploy/compose.yaml` in your app repo.
2. `age-keygen -o age.key` → put the public key in `.sops.yaml`.
3. For each `*.sops.env.example`: fill it in, drop the `.example`, `sops encrypt --in-place <file>`.
4. Copy `server/` to `/opt/doco-cd/`, set `target:` in `poll.yaml`, put `age.key` beside it.
5. `docker compose up -d`.

## Common tasks

**Deploy new app code** — push to `dev`. CI builds the image and bumps `SHOP_BE_TAG` + `SHOP_COMPOSE_SHA` together in `.doco-cd.shop-dev.yml`. For prod, run the workflow manually with `host: shop-prod`.

**Change a `shared/` compose** — edit it, commit, push. Then take that commit's sha and set `DB_COMPOSE_SHA` in each env you want it on. Roll one env at a time; rollback is putting the old sha back.

**Bump a third-party image** — change its tag in `.doco-cd.<host>.yml`. Renovate can raise these PRs.

**Check a secret reached the container** — `docker exec <container> printenv`.
