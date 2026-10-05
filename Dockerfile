# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
FROM golang:1.27.1@sha256:e0174e51e81218523251d85d248a90d24c3d5e81543b4f07a5d66229397db190 AS prerequisites

ARG DISABLE_BITWARDEN=false
ARG TARGETARCH
ARG TARGETVARIANT

# Set destination for COPY
WORKDIR /app

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

# Set build environment
ENV GOCACHE=/root/.cache/go-build \
    GOOS=linux

COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod/ \
    go mod download -x

FROM prerequisites AS build

ARG DISABLE_BITWARDEN=false
# Bitwarden SDK build flags https://github.com/bitwarden/sdk-go/blob/main/INSTRUCTIONS.md
ARG BW_SDK_BUILD_FLAGS="-linkmode external -extldflags '-static -Wl,-unresolved-symbols=ignore-all'"
ARG TARGETARCH
ARG TARGETVARIANT

COPY . .

ARG APP_VERSION=dev

# Build with or without Bitwarden support
# armv7 and riscv64 builds are automatically built without Bitwarden
# CGO_ENABLED=1 and CC=musl-gcc are required for Bitwarden SDK when enabled
# For builds without Bitwarden, CGO is not needed
# The healthcheck is a separate, small binary that is always built without CGO
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    --mount=type=bind,target=. \
    if [ "$DISABLE_BITWARDEN" = "true" ] || ([ "$TARGETARCH" = "arm" ] && [ "$TARGETVARIANT" = "v7" ]) || ([ "$TARGETARCH" = "riscv64" ]); then \
        echo "Building without Bitwarden support"; \
        CGO_ENABLED=1 go build -tags nobitwarden -ldflags="-s -w -X github.com/kimdre/doco-cd/internal/config/app.Version=${APP_VERSION}" -o / ./cmd/doco-cd; \
    else \
        echo "Building with Bitwarden support"; \
        CGO_ENABLED=1 CC=musl-gcc go build -ldflags="-s -w -X github.com/kimdre/doco-cd/internal/config/app.Version=${APP_VERSION} ${BW_SDK_BUILD_FLAGS}" -o / ./cmd/doco-cd; \
    fi && \
    CGO_ENABLED=0 go build -ldflags="-s -w" -o / ./cmd/healthcheck

FROM gcr.io/distroless/base-debian13@sha256:389cad21f73e4c37b94ffe5b13736d5a92bd5bd3c6c6b38c2be1c881e14ba2bd AS distroless-base

FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS ssh-client

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
COPY --from=docker/buildx-bin:0.37.2@sha256:f3acee6a2e18c8528ea096762e21e4c793c5a8001380f28801701b2112ae932a \
    /buildx /usr/libexec/docker/cli-plugins/docker-buildx

COPY --from=build /doco-cd /doco-cd
COPY --from=build /healthcheck /healthcheck

# SSH client required for Docker contexts using the ssh:// transport.
# The entire /ssh-root tree (binary + all shared library dependencies) is copied in.
COPY --from=ssh-client /ssh-root/ /

ENV TZ=UTC \
    HTTP_PORT=80 \
    METRICS_PORT=9120 \
    LOG_LEVEL=info

ENTRYPOINT ["/doco-cd"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/healthcheck"]

EXPOSE 80 9120
