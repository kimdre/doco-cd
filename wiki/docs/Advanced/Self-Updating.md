---
tags:
  - Advanced
  - Deployment
---

# Self-Updating Doco-CD

doco-cd can deploy the stack that contains its own container, from a **single
instance**. A push that changes the doco-cd compose file is a deploy like any
other.

Set [`SELF_UPDATE_ENABLED`](../App-Settings.md) to `true`. It is off by default:
a controller that upgrades itself unattended is a decision, not a default.
Without it, a deployment that would replace this container is refused with an
error rather than attempted.

!!! warning "Pin the image"
    Use `image: ghcr.io/kimdre/doco-cd:<version>@sha256:<digest>` and let a bot
    such as Renovate open the bump. With a moving tag like `latest`, doco-cd
    upgrades itself whenever the tag moves and a breaking change lands
    unreviewed.

A full worked setup is in
[`examples/self-updating`](https://github.com/kimdre/doco-cd/tree/main/examples/self-updating).

## Bootstrap

The first container must already carry the labels of a managed stack, otherwise
the running instance cannot recognise the stack as its own. Run the one-shot
bootstrap once per host:

```shell
docker volume create doco-cd_data

docker run --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v doco-cd_data:/data \
  -e SELF_UPDATE_ENABLED=true \
  -e POLL_CONFIG="- url: https://github.com/example/infra.git
  reference: main
  interval: 300" \
  ghcr.io/kimdre/doco-cd:latest apply-self --bootstrap
```

It clones the repository, deploys every configured target once, and exits. What
remains is a doco-cd container created by the normal deploy path, with the
correct `com.docker.compose.*` and `cd.doco.*` labels.

## Strategies

[`SELF_UPDATE_STRATEGY`](../App-Settings.md) selects how the container is
replaced. The default `auto` uses `scale_out` unless the compose file rules it
out.

### `scale_out` (no downtime)

1. Every other service in the stack deploys normally first.
2. doco-cd creates a **second** container from the new configuration and starts
   it. The running instance keeps serving throughout.
3. The running instance waits for the new container to report healthy, bounded
   by the deploy config's `timeout`.
4. On success it finishes its in-flight work, records the handover on the data
   volume and waits to be stopped. The new instance removes it, then sends the
   deployment notification and the commit status.
5. On failure the new container is removed. Nothing else changed, and the
   running instance reports the failure.

`scale_out` is impossible when the service sets `container_name`, publishes
host ports, uses `network_mode: host`, or when a project network must be
recreated. Two containers cannot share a name or a host port. Drop both from
the doco-cd service and reach the API through a reverse proxy on the compose
network.

### `applier` (short restart)

doco-cd clones its own container into a throwaway container running
`doco-cd apply-self`. The clone recreates the doco-cd service from outside,
waits for health, and exits. If the new version never becomes healthy, the
clone restores the previous container from a snapshot taken before the attempt.

Expect a few seconds without doco-cd while the replacement starts. Webhooks
sent in that window are lost; the next poll catches up.

## Requirements

- The doco-cd service needs a restart policy (`always`, `unless-stopped` or
  `on-failure`). Crash recovery depends on Docker bringing the container back.
- The self service must run exactly one replica.
- The self stack must be on the default Docker context and must not come from
  an OCI source.
- Give the service a `healthcheck`. Without one doco-cd falls back to running
  `doco-cd healthcheck` inside the new container, which is slower.

## Failure handling

Every handover is journaled on the data volume, so a crash at any point is
resolved on the next boot: whichever instance comes up finishes the handover or
reverses it.

A self-update that fails is recorded against that commit and **not retried**
until a new commit arrives, so a broken version cannot loop. The reason appears
in the logs and in the failure notification.

While a handover runs, new webhook and poll work is refused for a few seconds.
Deploys of other stacks that are already running are allowed to finish.

!!! note "Swarm"
    Docker Swarm already replaces a service through a rolling update performed
    by the swarm manager, so a Swarm stack containing doco-cd updates itself
    without any of this.

## Alternative: two instances

The older approach runs two instances that deploy each other: a **main**
instance handling your deployments, and an **updater** instance whose only job
is redeploying the main one. It still works and needs no `SELF_UPDATE_ENABLED`,
at the cost of a second container, a second clone of every repository and a
second scheduler to keep disabled.

```yaml title=".doco-cd.yml"
name: doco-cd-updater
reference: main
working_dir: ./doco-cd
compose_files:
  - compose.updater.yaml
force_recreate: true
```

```yaml title=".doco-cd.updater.yml"
name: doco-cd
reference: main
working_dir: ./doco-cd
compose_files:
  - compose.main.yaml
```

The updater polls with `target: updater` and deploys the main instance. Set
[`SCHEDULER_ENABLED`](../App-Settings.md) to `false` on the updater so both
instances do not pick up the same scheduled jobs. If Docker reports a container
name conflict during the handover, set `force_recreate: true` for that stack or
remove the old container once by hand.
