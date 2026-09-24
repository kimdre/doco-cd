#!/usr/bin/env sh
# One-shot bootstrap: creates the doco-cd stack from the repo and exits.
# After this the instance is a normal member of a stack it reconciles itself,
# so run it once per host and never again.
set -eu

REPO_URL="https://github.com/example/infra.git"
IMAGE="ghcr.io/kimdre/doco-cd:0.121.0"
DATA_VOLUME="doco-cd_data"

docker volume create "$DATA_VOLUME" >/dev/null

docker run --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$DATA_VOLUME":/data \
  -e SELF_UPDATE_ENABLED=true \
  -e GIT_ACCESS_TOKEN="${GIT_ACCESS_TOKEN:-}" \
  -e POLL_CONFIG="- url: $REPO_URL
  reference: main
  interval: 300" \
  "$IMAGE" apply-self --bootstrap
