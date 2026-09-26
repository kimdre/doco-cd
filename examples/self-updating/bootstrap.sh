#!/usr/bin/env sh
# One-shot bootstrap: creates the doco-cd stack from the repo and exits.
# After this the instance is a normal member of a stack it reconciles itself,
# so run it once per host and never again.
set -eu

# replace with the latest version of doco-cd, also set that version it in your deploy repo.
# https://github.com/kimdre/doco-cd/releases
IMAGE="ghcr.io/kimdre/doco-cd:0.122"
DATA_VOLUME="doco-cd_data"
REPO_URL="https://github.com/example/infra.git"

docker volume create "$DATA_VOLUME" >/dev/null

# Add other app settings via environment variables as needed,
# e.g. GIT_ACCESS_TOKEN or SOPS_AGE_KEY
docker run --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$DATA_VOLUME":/data \
  -e SELF_UPDATE_ENABLED=true \
  -e GIT_ACCESS_TOKEN="${GIT_ACCESS_TOKEN:-}" \
  -e POLL_CONFIG="- url: $REPO_URL
  reference: main" \
  "$IMAGE" apply-self --bootstrap
