# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
FROM golang:1.27.0@sha256:65b6f280bf050ec5af12716857e8ea8439d694dbba8f31ceeb7630670071f2bb AS prerequisites

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
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    --mount=type=bind,target=. \
    if [ "$DISABLE_BITWARDEN" = "true" ] || ([ "$TARGETARCH" = "arm" ] && [ "$TARGETVARIANT" = "v7" ]) || ([ "$TARGETARCH" = "riscv64" ]); then \
        echo "Building without Bitwarden support"; \
        CGO_ENABLED=1 go build -tags nobitwarden -ldflags="-s -w -X github.com/kimdre/doco-cd/internal/config/app.Version=${APP_VERSION}" -o / ./...; \
    else \
        echo "Building with Bitwarden support"; \
        CGO_ENABLED=1 CC=musl-gcc go build -ldflags="-s -w -X github.com/kimdre/doco-cd/internal/config/app.Version=${APP_VERSION} ${BW_SDK_BUILD_FLAGS}" -o / ./...; \
    fi

FROM gcr.io/distroless/base-debian13@sha256:20dc7edae3f7efe09b934aca4b347b00bb4ae0f2864b6131771687ae6d54891f AS distroless-base

FROM debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258 AS ssh-client

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
COPY --from=docker/buildx-bin:0.36.1@sha256:1f2f6b2be4a2511ada67336e76892f1a588c89746009dd4b21069e4d867465be \
    /buildx /usr/libexec/docker/cli-plugins/docker-buildx

COPY --from=build /doco-cd /doco-cd

# SSH client required for Docker contexts using the ssh:// transport.
# The entire /ssh-root tree (binary + all shared library dependencies) is copied in.
COPY --from=ssh-client /ssh-root/ /

ENV TZ=UTC \
    HTTP_PORT=80 \
    METRICS_PORT=9120 \
    LOG_LEVEL=info

ENTRYPOINT ["/doco-cd"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/doco-cd", "healthcheck"]

EXPOSE 80 9120
