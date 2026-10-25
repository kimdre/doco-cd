#!/usr/bin/env sh
# One-shot bootstrap: creates the doco-cd stack from the repo and exits.
# After this, doco-cd manages its own stack like any other application.
# Run this script once per host and never again.
set -eu

# Use the same release version as infra-repo/doco-cd/compose.yaml.
# https://github.com/kimdre/doco-cd/releases
IMAGE="ghcr.io/kimdre/doco-cd:0.125.1"
DATA_VOLUME="doco-cd_data"
REPO_URL="https://github.com/example/infra.git"
# Hosts that may receive GIT_ACCESS_TOKEN. Use the host of REPO_URL.
GIT_AUTH_DOMAINS="${GIT_AUTH_DOMAINS:-"- domains: [github.com]"}"

docker volume create "$DATA_VOLUME" >/dev/null

# Add other app settings via environment variables as needed,
# e.g. GIT_ACCESS_TOKEN or SOPS_AGE_KEY
docker run --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$DATA_VOLUME":/data \
  -e SELF_UPDATE_ENABLED=true \
  -e GIT_ACCESS_TOKEN="${GIT_ACCESS_TOKEN:-}" \
  -e GIT_AUTH_DOMAINS="$GIT_AUTH_DOMAINS" \
  -e GIT_AUTH_ALLOW_ANY_HOST="${GIT_AUTH_ALLOW_ANY_HOST:-false}" \
  -e POLL_CONFIG="- url: $REPO_URL
  reference: main" \
  "$IMAGE" apply-self --bootstrap
