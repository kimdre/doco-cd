# syntax=docker/dockerfile:1@sha256:b6afd42430b15f2d2a4c5a02b919e98a525b785b1aaff16747d2f623364e39b6
FROM golang:1.25.5@sha256:20b91eda7a9627c127c0225b0d4e8ec927b476fa4130c6760928b849d769c149 AS prerequisites

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

# Copy source code
COPY . ./

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    go mod download -x

FROM prerequisites AS build

# Build and strip binary
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    --mount=type=bind,target=. \
    go build -ldflags="-s -w -X github.com/kimdre/doco-cd/internal/config.AppVersion=${APP_VERSION} ${BW_SDK_BUILD_FLAGS}" -o / ./...

FROM busybox:1.37-uclibc@sha256:e58014df10240c35c7b1df7ff8e859ad6a54d061bde77c249f96880e15d83049 AS busybox-binaries

FROM gcr.io/distroless/base-debian12@sha256:f5a3067027c2b322cd71b844f3d84ad3deada45ceb8a30f301260a602455070e AS release

WORKDIR /

# /data volume required to deploy from cloned Git repos
VOLUME /data

COPY --from=build /doco-cd /doco-cd
COPY --from=busybox-binaries /bin/wget /usr/bin/wget

ENV TZ=UTC \
    HTTP_PORT=80 \
    METRICS_PORT=9120 \
    LOG_LEVEL=info

ENTRYPOINT ["/doco-cd"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/usr/bin/wget", "--no-verbose", "--tries=1", "--spider", "http://localhost:80/v1/health"]
