# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
FROM golang:1.27.3@sha256:9048d5e7751706200eb1719c7dae0436950d362f29a1da91fa05e5c63867c60f AS go-base

# Set build environment
ENV GOCACHE=/root/.cache/go-build \
    GOOS=linux

WORKDIR /app

FROM go-base AS prerequisites

ARG DISABLE_BITWARDEN=false
ARG TARGETARCH
ARG TARGETVARIANT

# Automatically disable Bitwarden for armv7 and riscv64
# The Bitwarden Go SDK does not support 32-bit ARM architecture or RISC-V 64-bit architecture
RUN if ([ "$TARGETARCH" = "arm" ] && [ "$TARGETVARIANT" = "v7" ]) || ([ "$TARGETARCH" = "riscv64" ]); then \
    echo "Detected unsupported ${TARGETARCH} ${TARGETVARIANT} architecture - Bitwarden support will be disabled"; \
    fi

# Install prerequisites for Bitwarden SDK (only if not disabled and not armv7 or riscv64)
RUN if [ "$DISABLE_BITWARDEN" != "true" ] && \
    ! ([ "$TARGETARCH" = "arm" ] && [ "$TARGETVARIANT" = "v7" ]) && \
    ! ([ "$TARGETARCH" = "riscv64" ]); then \
    apt-get update && apt-get install -y --no-install-recommends \
    musl-tools \
    && rm -rf /var/lib/apt/lists/*; \
    fi

# Only go.mod and go.sum are mounted, so this step is only invalidated by dependency changes
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download -x

FROM prerequisites AS build

ARG DISABLE_BITWARDEN=false
# Bitwarden SDK build flags https://github.com/bitwarden/sdk-go/blob/main/INSTRUCTIONS.md
ARG BW_SDK_BUILD_FLAGS="-linkmode external -extldflags '-static -Wl,-unresolved-symbols=ignore-all'"
ARG TARGETARCH
ARG TARGETVARIANT

ARG APP_VERSION=dev

# Build with or without Bitwarden support
# armv7 and riscv64 builds are automatically built without Bitwarden
# CGO_ENABLED=1 and CC=musl-gcc are required for Bitwarden SDK when enabled
# The sources are bind-mounted from the build context, so they are not copied into a layer
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    --mount=type=bind,target=. \
    if [ "$DISABLE_BITWARDEN" = "true" ] || ([ "$TARGETARCH" = "arm" ] && [ "$TARGETVARIANT" = "v7" ]) || ([ "$TARGETARCH" = "riscv64" ]); then \
        echo "Building without Bitwarden support"; \
        CGO_ENABLED=1 go build -trimpath -tags nobitwarden -ldflags="-s -w -X github.com/kimdre/doco-cd/internal/config/app.Version=${APP_VERSION}" -o / ./cmd/doco-cd; \
    else \
        echo "Building with Bitwarden support"; \
        CGO_ENABLED=1 CC=musl-gcc go build -trimpath -ldflags="-s -w -X github.com/kimdre/doco-cd/internal/config/app.Version=${APP_VERSION} ${BW_SDK_BUILD_FLAGS}" -o / ./cmd/doco-cd; \
    fi

# The healthcheck is a separate, small binary that is always built without CGO.
# It has its own stage so it builds in parallel and is not affected by the
# Bitwarden toolchain or build arguments of the main binary.
FROM go-base AS build-healthcheck

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    --mount=type=bind,target=. \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o / ./cmd/healthcheck

FROM gcr.io/distroless/base-debian13@sha256:f5e95195e998742ff590b7569bae479accce1226e51ae6c3118954030121cdcb AS distroless-base

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS ssh-client

# Copy the distroless base filesystem so we can skip libraries already present there.
COPY --from=distroless-base / /distroless-root/
RUN apt-get update && \
    apt-get install -y --no-install-recommends openssh-client && \
    rm -rf /var/lib/apt/lists/* && \
    # Collect the ssh binary and only the shared library dependencies that are NOT
    # already present in the distroless base image, to avoid duplicating layers.
    # realpath on dirname resolves /lib -> /usr/lib so paths match distroless layout.
    mkdir -p /ssh-root/usr/bin && \
    cp /usr/bin/ssh /ssh-root/usr/bin/ssh && \
    ldd /usr/bin/ssh | awk '$3 ~ /^\// { print $3 }' | \
        while IFS= read -r lib; do \
          dir=$(realpath "$(dirname "$lib")"); \
          name=$(basename "$lib"); \
          [ -f "/distroless-root$dir/$name" ] && continue; \
          mkdir -p "/ssh-root$dir"; \
          cp -L "$lib" "/ssh-root$dir/$name"; \
        done

FROM distroless-base AS release

WORKDIR /

# buildx plugin so compose v5 picks BuildKit instead of the legacy `/build` endpoint
COPY --from=docker/buildx-bin:0.38.1@sha256:0b7d9e66e81dd1155639421b8342f9731f8bb67cb11b58663f938d9c6e9b6eef \
    /buildx /usr/libexec/docker/cli-plugins/docker-buildx

# SSH client required for Docker contexts using the ssh:// transport.
# The entire /ssh-root tree (binary + all shared library dependencies) is copied in.
COPY --from=ssh-client /ssh-root/ /

# Layers that change with every release come last, so pulling an update
# only downloads the changed binaries and reuses the layers above.
COPY --from=build-healthcheck /healthcheck /healthcheck
COPY --from=build /doco-cd /doco-cd

ENV TZ=UTC \
    HTTP_PORT=80 \
    METRICS_PORT=9120 \
    LOG_LEVEL=info

ENTRYPOINT ["/doco-cd"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/healthcheck"]

EXPOSE 80 9120
