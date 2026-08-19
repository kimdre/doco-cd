# e2e tests

Black-box tests against a real doco-cd instance. The harness builds doco-cd
from the working tree, starts it next to a tiny anonymous git server, and
scenarios drive it the way a user would: push commits, wait, assert on
containers and daemon state with plain `docker` commands.

Needs docker (with compose v2), git and bash. Nothing else, no tokens, no
network access beyond image pulls.

## Run

```sh
./test/e2e/run.sh                       # all scenarios
./test/e2e/run.sh failed-deploy-retry   # one scenario
KEEP=1 ./test/e2e/run.sh <scenario>     # keep the harness up for debugging
```

Also runs in CI on pull requests (`.github/workflows/e2e.yaml`).

## How it works

```
harness/compose.yaml   doco-cd built from source + git smart-HTTP server
lib.sh                 helpers: wait_for, log grep, repo push, container lookup
run.sh                 per scenario: fresh repo -> push fixture -> start daemon
                       -> run scenario script -> teardown
scenarios/<name>/
  fixture/             initial repo content, pushed as the first commit
  run.sh               assertions and further pushes, sources $E2E_LIB
  stacks               compose projects the scenario deploys, for cleanup
```

The daemon polls `http://gitserver/<scenario>.git` every 10s (the minimum).
Pushes happen on the host filesystem directly into the bare repo the server
serves, so no auth and no push protocol is involved. Deployed stacks land on
the same docker daemon the harness runs on.

## Add a scenario

1. Create `scenarios/<name>/fixture/` with a `.doco-cd.yml` and compose files.
   Prefix stack names with `e2e-` and list them in `scenarios/<name>/stacks`.
2. Write `scenarios/<name>/run.sh`: source `$E2E_LIB`, then combine
   `e2e::wait_for`, `e2e::daemon_has_log`, `e2e::container_id` and
   `e2e::repo_push` into the flow you want to prove. Exit 0 means pass.
3. `./test/e2e/run.sh <name>`.

Keep scenarios independent: every scenario gets a fresh daemon, a fresh data
volume and a fresh repo.
