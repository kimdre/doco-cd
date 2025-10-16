# syntax=docker/dockerfile:1@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
FROM golang:1.25.3@sha256:6ea52a02734dd15e943286b048278da1e04eca196a564578d718c7720433dbbe AS build-stage

ARG APP_VERSION=dev

# Set destination for COPY
WORKDIR /app

# Install prerequisites for Bitwarden SDK
RUN apt-get update && apt-get install -y \
    musl-tools \
    && rm -rf /var/lib/apt/lists/*

# Set build environment
# CGO_ENABLED=1 and CC=musl-gcc are required for Bitwarden SDK
ENV GOCACHE=/root/.cache/go-build \
    CGO_ENABLED=1 \
    CC=musl-gcc \
    GOOS=linux

# Bitwarden SDK build flags https://github.com/bitwarden/sdk-go/blob/main/INSTRUCTIONS.md
ARG BW_SDK_BUILD_FLAGS="-linkmode external -extldflags '-static -Wl,-unresolved-symbols=ignore-all'"

# Download Go modules
COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    go mod download -x

# Copy source code
COPY . ./

# Build and strip binary
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    --mount=type=bind,target=. \
    go build -ldflags="-s -w -X main.Version=${APP_VERSION} ${BW_SDK_BUILD_FLAGS}" -o / ./...

FROM busybox:1.37-uclibc@sha256:34f9fe72626aece25b6dd6eefab9224fb3338ad61cfefcbe480b1e9cb86e3a2c AS busybox-binaries

FROM gcr.io/distroless/base-debian12@sha256:9e9b50d2048db3741f86a48d939b4e4cc775f5889b3496439343301ff54cdba8 AS build-release-stage

WORKDIR /

# /data volume required to deploy from cloned Git repos
VOLUME /data

COPY --from=build-stage /doco-cd /doco-cd
COPY --from=busybox-binaries /bin/wget /usr/bin/wget

ENV TZ=UTC \
    HTTP_PORT=80 \
    METRICS_PORT=9120 \
    LOG_LEVEL=info

ENTRYPOINT ["/doco-cd"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/usr/bin/wget", "--no-verbose", "--tries=1", "--spider", "http://localhost:80/v1/health"]
