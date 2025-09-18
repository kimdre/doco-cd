# syntax=docker/dockerfile:1@sha256:dabfc0969b935b2080555ace70ee69a5261af8a8f1b4df97b9e7fbcf6722eddf
FROM golang:1.25.1@sha256:8305f5fa8ea63c7b5bc85bd223ccc62941f852318ebfbd22f53bbd0b358c07e1 AS build-stage

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

#FROM busybox AS busybox-binaries
#
#ARG BUSYBOX_VERSION=1.31.0-i686-uclibc
#ADD https://busybox.net/downloads/binaries/$BUSYBOX_VERSION/busybox_WGET /wget
#RUN chmod a+x /wget

FROM gcr.io/distroless/base-debian12@sha256:fa15492938650e1a5b87e34d47dc7d99a2b4e8aefd81b931b3f3eb6bb4c1d2f6 AS build-release-stage

WORKDIR /

# /data volume required to deploy from cloned Git repos
VOLUME /data

COPY --from=build-stage /doco-cd /doco-cd
# COPY --from=busybox-binaries /wget /usr/bin/wget

ENV TZ=UTC \
    HTTP_PORT=80 \
    METRICS_PORT=9120 \
    LOG_LEVEL=info

ENTRYPOINT ["/doco-cd"]

#HEALTHCHECK --interval=30s --timeout=5s \
#  CMD ["/usr/bin/wget", "--no-verbose", "--tries=1", "--spider", "http://localhost/v1/health"]
