# Self-updating doco-cd (single instance)

doco-cd deploys the stack that contains its own container. One instance, no
updater sidecar. A push that bumps the doco-cd image is a deploy like any other.

Requires doco-cd with `SELF_UPDATE_ENABLED=true`. Off by default: an
auto-updating controller that upgrades itself unattended is a decision you
make, not a default you inherit.

## What this example shows

- A one-shot **bootstrap** that creates the first container already labelled as
  a managed stack, so no second instance and no hand-copied compose file.
- **Zero-downtime** updates via the `scale_out` strategy: the old container
  keeps serving and judges the new one before it steps aside.
- Automatic **rollback**: a new version that never reports healthy is discarded,
  and the failed commit is not retried until a new commit arrives.
- The **caveats** that decide which strategy you get, below.

## Layout

```
infra-repo/               # your Git repository
  .doco-cd.yml            # two deploy configs: doco-cd itself, and an app
  doco-cd/
    compose.yaml          # the doco-cd stack, deployed by doco-cd
  app/
    compose.yaml          # a normal stack, unaffected by self-updates
bootstrap.sh              # run once per host, then never again
```

## Try it

1. Push `infra-repo/` contents to a Git repository.
2. Edit `bootstrap.sh` and `infra-repo/doco-cd/compose.yaml` to point at it.
3. Run `./bootstrap.sh` on the host.

The bootstrap container clones the repo, deploys both stacks and exits. What is
left is a doco-cd container carrying the full `com.docker.compose.*` and
`cd.doco.*` label set, which is what lets the next poll recognise the stack as
its own.

From here, bump the image in `doco-cd/compose.yaml`, push, and doco-cd replaces
itself.

## How an update runs

With `scale_out` (the default when nothing rules it out):

1. A poll finds the new commit. Every other service in the stack deploys first.
2. doco-cd creates a **second** container from the new config and starts it.
   The old one keeps serving the whole time.
3. The old instance watches the new container's health, up to the deploy
   config's `timeout`.
4. Healthy: the old instance finishes its in-flight work, records a handover on
   the data volume, and waits. The new instance stops and removes it, then
   sends the deployment notification and commit status.
5. Unhealthy: the new container is removed. Nothing else changed. The old
   instance keeps running and reports the failure.

With `applier`, doco-cd instead clones its own container into a throwaway
`doco-cd apply-self` container. That clone recreates the doco-cd service from
outside, health-gates it, and exits. If the new version is unhealthy it
restores the previous container from a snapshot. Expect a few seconds of
downtime while the replacement starts.

Either way the handover is journaled on `/data`. A crash at any point is
resolved on the next boot: whoever comes up finishes or reverses it.

## Caveats

**`container_name` and published ports rule out zero downtime.** Two containers
cannot share a name or a host port, so either forces the `applier` strategy.
Drop both from the doco-cd service and reach it through a reverse proxy on the
compose network. `SELF_UPDATE_STRATEGY` accepts `auto`, `scale_out` or
`applier`; `scale_out` errors out rather than silently degrading.

**The container number grows.** Each `scale_out` update leaves the container
named `doco-cd-2`, then `-3`. Cosmetic, and compose tracks it correctly.

**A restart policy is required.** Recovery after a crash relies on Docker
restarting the container. `restart: no` is refused. Note that Docker ignores a
restart policy after an API stop, which is exactly why the successor removing
the predecessor does not bring it back.

**Pin the image.** doco-cd changes quickly. `image: ...:<version>@sha256:<digest>`
plus Renovate keeps a human in the loop per release. With a moving tag the
controller upgrades itself whenever the tag moves, and a breaking change lands
unreviewed.

**A failed update is tried once.** After a rollback the commit is recorded as
poisoned and skipped until a new commit arrives, so a broken version cannot
loop. The reason is in the logs and the failure notification.

**During the handover doco-cd refuses new work.** Webhooks get 503 for a few
seconds while the old instance drains. Deploys already running are allowed to
finish; they are not cancelled.

**Not covered.** Swarm-mode self stacks already update themselves through
rolling updates and need none of this. OCI-sourced self stacks, self stacks on
a non-default Docker context, and installs where doco-cd cannot resolve its own
container ID are refused with an explanatory error.

**A version that boots healthy but misbehaves later is not caught.** The health
gate proves the process starts and answers, nothing more. That is the same
exposure as any other stack doco-cd deploys.
