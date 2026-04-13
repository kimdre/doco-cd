# syntax=docker/dockerfile:1@sha256:2780b5c3bab67f1f76c781860de469442999ed1a0d7992a5efdf2cffc0e3d769
FROM golang:1.26.2@sha256:fcdb3e42c5544e9682a635771eac76a698b66de79b1b50ec5b9ce5c5f14ad775 AS prerequisites

ARG APP_VERSION=dev
ARG DISABLE_BITWARDEN=false
ARG TARGETARCH
ARG TARGETVARIANT

# Set destination for COPY
WORKDIR /app

# Automatically disable Bitwarden for armv7
# The Bitwarden Go SDK does not support 32-bit ARM architecture
RUN if [ "$TARGETARCH" = "arm" ] && [ "$TARGETVARIANT" = "v7" ]; then \
    echo "Detected armv7 architecture - Bitwarden support will be disabled"; \
    fi

# Install prerequisites for Bitwarden SDK (only if not disabled and not armv7)
RUN if [ "$DISABLE_BITWARDEN" != "true" ] && ! ([ "$TARGETARCH" = "arm" ] && [ "$TARGETVARIANT" = "v7" ]); then \
    apt-get update && apt-get install -y \
    musl-tools \
    && rm -rf /var/lib/apt/lists/*; \
    fi

# Set build environment
ENV GOCACHE=/root/.cache/go-build \
    GOOS=linux

COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    go mod download -x

FROM prerequisites AS build

ARG APP_VERSION=dev
ARG DISABLE_BITWARDEN=false
# Bitwarden SDK build flags https://github.com/bitwarden/sdk-go/blob/main/INSTRUCTIONS.md
ARG BW_SDK_BUILD_FLAGS="-linkmode external -extldflags '-static -Wl,-unresolved-symbols=ignore-all'"
ARG TARGETARCH
ARG TARGETVARIANT

COPY . .

# Build with or without Bitwarden support
# armv7 builds are automatically built without Bitwarden
# CGO_ENABLED=1 and CC=musl-gcc are required for Bitwarden SDK when enabled
# For builds without Bitwarden, CGO is not needed
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    --mount=type=bind,target=. \
    if [ "$DISABLE_BITWARDEN" = "true" ] || ([ "$TARGETARCH" = "arm" ] && [ "$TARGETVARIANT" = "v7" ]); then \
        echo "Building without Bitwarden support"; \
        CGO_ENABLED=1 go build -tags nobitwarden -ldflags="-s -w -X github.com/kimdre/doco-cd/internal/config.AppVersion=${APP_VERSION}" -o / ./...; \
    else \
        echo "Building with Bitwarden support"; \
        CGO_ENABLED=1 CC=musl-gcc go build -ldflags="-s -w -X github.com/kimdre/doco-cd/internal/config.AppVersion=${APP_VERSION} ${BW_SDK_BUILD_FLAGS}" -o / ./...; \
    fi

FROM gcr.io/distroless/base-debian13@sha256:c83f022002fc917a92501a8c30c605efdad3010157ba2c8998a2cbf213299201 AS release

WORKDIR /

# /data volume required to deploy from cloned Git repos
VOLUME /data

COPY --from=build /doco-cd /doco-cd

ENV TZ=UTC \
    HTTP_PORT=80 \
    METRICS_PORT=9120 \
    LOG_LEVEL=info

ENTRYPOINT ["/doco-cd"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/doco-cd", "healthcheck"]
