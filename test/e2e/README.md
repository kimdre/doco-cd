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
rather than at suite teardown. CI sets this so finished scenarios release their
Docker networks before later scenarios start, avoiding exhaustion of the
runner's default address pools.

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
parallel (`t.Parallel()`). Scenarios that call `EnableRemoteContext` also
receive a privileged Docker-in-Docker daemon registered as the `remote`
context.

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
   combine `WaitFor`, `ContainerID`, `ContainerImage`,
   `WaitForContainerRecreate`, `RepoPush` and `ReplaceInWorktree` into the
   flow you want to prove. Use `LogMark` with `WaitForLogAfter` for multi-phase
   scenarios so old log entries cannot satisfy later assertions.
3. Files a later commit should add, rather than edit in place, go in a sibling
   directory of `fixture/` (e.g. `scenarios/<name>/update/`) and are overlaid
   onto the worktree with `CopyScenarioDir("update")`. Stack cleanup reads
   every `.doco-cd.yml` under `scenarios/<name>/`, so stacks added that way are
   torn down too. `SetEnv` adds environment variables to the daemon before
   `Start`, e.g. `SOPS_AGE_KEY` for a scenario with encrypted fixtures.
4. `go test -tags e2e ./test/e2e/... -run Test<Name> -v`.

## Self-update scenarios

Scenarios that call `EnableSelfUpdate(stack, service)` do not start doco-cd with
testcontainers. They run `doco-cd apply-self --bootstrap` in a throwaway
container, which deploys the fixture's own doco-cd stack. The container under
test is then a real member of a compose project doco-cd reconciles, which is
what a self-update needs.

Consequences for scenario code:

- `LogMark` / `WaitForLogAfter` follow a succession of containers, not one.
  Marks are per container, so a container appearing later cannot shift an
  older offset. A background collector snapshots logs every 250ms, because a
  handover deletes containers within seconds and their output is the only
  record of what happened.
- `SelfContainers`, `SelfAppliers`, `SelfContainerID`, `RunsSelfImage` and
  `StackNetwork` query by label instead of holding a container or network
  handle, since a handover can replace both.
- The fixture image is tagged `<scenario>:v1` and `:v2` from the same build, so
  bumping the tag in the fixture is a pure config change with no rebuild. Use
  `pull_policy: never` for it: the tag exists only on the local daemon.
- `SELF_UPDATE_CRASH_AT=<journal state>` makes the process exit once at that
  point of the handover. It is a test-only hook; the marker file on the data
  volume makes it fire once so the restarted actor can make progress.

Keep scenarios independent: every scenario gets a fresh daemon, a fresh data
volume and a fresh repo. Locally, harness containers remain running until the
e2e suite finishes unless `E2E_KEEP_COMPONENTS_RUNNING=0` is set (as in CI);
deployed stacks are still cleaned up after each test.

Prefer Docker state for the primary assertion. Logs are useful for proving that
a specific path ran, but a success log alone does not prove that the expected
container or image reached the Docker daemon.
