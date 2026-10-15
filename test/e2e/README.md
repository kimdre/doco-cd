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

CI sets `E2E_TEST_MODE` to `standalone` or `swarm`. The Swarm selection skips
scenarios that explicitly force Compose mode and are already covered by the
standalone job. It retains mode-dependent scenarios, both migration directions,
mixed-context deployment, and Swarm-specific recovery. The selector does not
configure Docker or override a scenario's Swarm requirements. Invalid values
fail before any scenarios start.

Locally, leave `E2E_TEST_MODE` unset to run all compatible scenarios, or use
`E2E_TEST_MODE=swarm make test-e2e` on a Swarm manager to match that CI selection.
Explicit Compose-only scenarios call `requireStandaloneSuite(t)` before creating a harness.

## Coverage boundaries

Related checks share a harness through sequential phases, not through mutable
state shared by parallel tests:

| Scenario              | Coverage                                                                                                                          |
|-----------------------|-----------------------------------------------------------------------------------------------------------------------------------|
| Deployment lifecycle  | Initial deployment, invalid config preservation, recovery with an image tag change, and destroy                                   |
| Image bump            | Pinned image digest changes; tag changes are covered by the lifecycle                                                             |
| OCI source            | Encrypted env file and local include deployment, followed by destroy from a republished artifact                                  |
| Changelog             | Real notifications, selected-target config changes, and pull-request merge attribution                                            |
| Appended documents    | Plain additions, first encrypted addition, and later additions after decryption, preserving existing containers                   |
| Scheduled status      | Uninterrupted automatic execution, dependency stop/restore, cleanup, failed-source isolation, manual execution, and API reporting |
| Scheduled pre_start   | Unrelated webhook skips, repeated hooks without source recreation, hook failure/retry, and unsupported one-off rejection          |
| Autostart drift       | Explicit and discovered stopped-service polling, cached skips, external start/stop, missing-container recovery, and config changes |
| Self-update scale-out | Bootstrap adoption and labels, followed by healthy overlap and handover                                                           |

Config decoding, successive artifact decryption, and detailed changelog filtering
also have lower-level tests in `internal/config/deploy`, `internal/source/store`,
`internal/stages`, and `internal/docker`. Those checks do not need a daemon
container. Forced scheduled-job recovery remains separate for Compose and Swarm.
Self-update strategies, network rollback, cross-stack updates, and all three
process-crash windows retain their E2E coverage.

Keep Swarm scheduled-job recovery serial: scheduler discovery scans the shared
cluster, so different stack names do not isolate concurrent schedulers.

`TestScheduledPreStart_UnrelatedWebhooks` uses an isolated remote Docker context
and a label-based restart-mode job scheduled every 24 hours. A long poll interval
allows the initial deployment but leaves both subsequent unrelated commits for
real signed GitHub push webhooks. Each webhook's returned job ID is checked
through the run API for a completed skip of the discovered stack. Docker
inventory includes stopped containers (`All: true`), distinguishes hook runners
by Compose's hook label, and checks that source and hook IDs, Created states,
container counts, and deployment metadata remain unchanged. The scheduled job
API must still report the real source with no last run. This catches hook runners
incorrectly supplying deployment labels or replacing scheduled-job sources
without executing the workload.

The same scenario then triggers consecutive executions through the scheduled-job
API. Persistent counters verify that hooks run before each workload, a failing
hook blocks the workload, and the next execution recovers. The source container
and its deployment labels remain unchanged, and an unrelated autostart-disabled
service stays Created. Finally, a deployment that changes the job to `one_off`
must fail with the unsupported `pre_start` error and preserve the existing source.

`TestAutostartDrift` runs explicit and auto-discovered deployments in separate
isolated remote Docker contexts, with reconciliation disabled to isolate polling.
An autostart-disabled service with `restart: unless-stopped` starts in Created
state. Fresh stack-specific log markers and completed poll API outcomes prove
full-path skips and auto-discovery warm-cache skips. After an external start/stop
simulates Sablier, three distinct unchanged polls must preserve the container ID,
deployment labels, and Exited state without new deployment notifications or
commits. Removing the container must create a stopped replacement. A real service
configuration change must still deploy without starting it, and enabling
autostart must start the service. This coverage is Standalone-only.

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
   `EnableAPI` enables the REST API before `Start` and publishes the daemon
   HTTP port, so `APIRequest` can call the API from the test.
4. Scenarios asserting on notifications call `CaptureNotifications` before
   `Start`: the gitserver also answers as the Apprise API and logs every
   request, and `NextNotification` returns them per stack with the commit
   changelog as a list. `SetPollTarget` reads `.doco-cd.<target>.yml`, and
   `RepoCheckout` + `RepoMerge` build pull request merges.
5. `go test -tags e2e ./test/e2e/... -run Test<Name> -v`.

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
