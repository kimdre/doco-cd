# e2e tests

Black-box tests against a real doco-cd instance. The harness builds doco-cd
from the working tree, starts it next to a tiny anonymous git server, and
scenarios drive it the way a user would: push commits, wait, assert on
containers and daemon state with the docker client - the same way a real
user's repo would be polled and deployed.

Needs docker (with BuildKit) and go. Nothing else, no tokens, no network
access beyond image pulls, no local git binary - repo state is written
directly with [go-git](https://github.com/go-git/go-git).

The doco-cd binary is compiled on the host (`-tags nobitwarden`, so CGO is not
needed) and injected into the image build as the Dockerfile's `build` stage via
`docker build --build-context build=<dir>`. Compiling inside the image instead
would add minutes to every run: BuildKit's `--mount=type=cache` Go build cache
is not exported by `--cache-to`, so it is always cold in CI.

## Run

```sh
make test-e2e                                      # all scenarios
make test-e2e E2E_RUN=TestFailedDeployRetry         # one scenario
go test -tags e2e ./test/e2e/... -run TestFailedDeployRetry -v
```

Set `E2E_KEEP_COMPONENTS_RUNNING=0` to stop each harness when its test ends,
rather than at suite teardown.

Also runs in CI on pull requests (`.github/workflows/test.yaml`, job `e2e`).

## How it works

```
harness.go             Harness: builds/starts gitserver + doco-cd containers
                        via testcontainers-go, tears them down after the suite
helpers.go              wait-for, log grep, repo push, container lookup,
                        stack cleanup helpers used by scenario tests
harness/gitserver/      tiny anonymous git-over-HTTP server image
scenarios/<name>/
  fixture/              initial repo content, pushed as the first commit
<name>_test.go          the scenario itself: a plain Go test function using
                        the Harness helpers
```

Each scenario gets its own Harness: its own docker network, its own
gitserver and doco-cd containers, its own host-side git repo and worktree,
and its own `/data` volume - so scenarios are isolated and can run in
parallel (`t.Parallel()`).

The daemon polls `http://gitserver/<scenario>.git` every 10s (the minimum).
Commits are written directly into the repo's storage directory via go-git,
which is the same directory the gitserver container mounts read-only - so no
push and no git wire protocol is involved on the host side. Deployed stacks
land on the same docker daemon the harness runs on (the daemon container
mounts the host's `/var/run/docker.sock`), and are cleaned up after each test
by reading the stack name(s) straight from the fixture's `.doco-cd.yml`
`name` field(s) - there's no separate list to keep in sync.

## Add a scenario

1. Create `scenarios/<name>/fixture/` with a `.doco-cd.yml` and compose
   files. Prefix stack names with `e2e-`.
2. Write `<name>_test.go`: call `NewHarness(t, "<name>")`, `Start()`, then
   combine `WaitForLog`, `WaitFor`, `ContainerID`, `WaitForContainerRecreate`,
   `RepoPush` and `ReplaceInWorktree` into the flow you want to prove.
3. `go test -tags e2e ./test/e2e/... -run Test<Name> -v`.

Keep scenarios independent: every scenario gets a fresh daemon, a fresh data
volume and a fresh repo. Harness containers remain running until the e2e suite
finishes; deployed stacks are still cleaned up after each test.
