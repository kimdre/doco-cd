# Central deployments repo → many apps, many hosts

One Git repo is the source of truth for a fleet. Each host's doco-cd instance polls the same repo with its own `target:` and reads only its own `.doco-cd.<host>.yml`. A commit here **is** a deploy.

Needs doco-cd >= 0.108.0 (remote compose `include:`).

## The model

- **One target file per host.** It lists the host's stacks and pins **every** version the host runs: your images by git sha, third-party by exact tag. Nothing floats. A version change is a commit — reviewable, revertable, and the diff is the changelog.
- **Compose lives with the app code, not here.** Each per-env compose is a small stub that `include:`s the app repo's `deploy/compose.yaml`, pinned at a sha. The compose travels through envs together with the images it describes — a compose change cannot hit prod before its code does.
- **Non-secret config** is a cleartext env file per environment, committed here.
- **Secrets** are SOPS-encrypted env files, committed here. The daemon decrypts them at deploy time with an age key (KMS works the same). Secrets land as container env — verify on the host with `docker exec <c> printenv`, never by reading files.
- **App CI closes the loop:** after building an image, a bump job rewrites the sha pins in the target files of the envs that should follow, and commits. doco-cd does the rest.

## Layout

```
deployments-repo/            # the fleet's Git repository
  .doco-cd.shop-dev.yml      # per-host deploy config: stack list + ALL version pins
  .doco-cd.shop-prod.yml
  .sops.yaml                 # SOPS creation rules (age recipient)
  shop/
    shop-dev/compose.yaml    # include stub, pinned at ${SHOP_COMPOSE_SHA}
    shop-prod/compose.yaml
    env/shop-dev.env         # cleartext config
    env/shop-prod.env
    secrets/shop-dev.sops.env         # SOPS-encrypted (example is a template)
app-repo/                    # the app's own repo (e.g. github.com/example/shop-be)
  deploy/compose.yaml        # THE family compose — single source for every env
server/                      # per host, e.g. /opt/doco-cd/
  compose.yaml
  poll.yaml                  # target: shop-dev on the dev host, shop-prod on prod
  secrets.env.example
```

Add more apps as more families (`blog/`, `api/`, …) and more target files. One host can also run several stacks — add more YAML documents to its target file.

## Try it

1. Push `deployments-repo/` contents to a Git repository. Put `app-repo/deploy/compose.yaml` in your app's repo.
2. Generate an age key: `age-keygen -o age.key`. Put the public key in `.sops.yaml`.
3. Create the secret files: `sops encrypt shop/secrets/shop-dev.sops.env` (start from the `.example`).
4. On each host: copy `server/` to `/opt/doco-cd/`, set the host's `target:` in `poll.yaml`, place `age.key` next to it.
5. `docker compose up -d` in `/opt/doco-cd/`.

## Why a stub and not a plain compose here?

You can start with full compose files in this repo — that also works and is simpler. The stub + pinned `include:` pays off when app developers own their compose: they change it in the app repo, CI bumps `SHOP_COMPOSE_SHA` here per env, and dev/test/prod each run exactly the compose revision they were promoted to. `project_directory: .` makes the included file's relative paths (`../env/*`, `../secrets/*`) resolve against the per-env stub directory, so one compose serves every env.
