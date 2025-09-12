# syntax=docker/dockerfile:1@sha256:38387523653efa0039f8e1c89bb74a30504e76ee9f565e25c9a09841f9427b05
FROM golang:1.25.1@sha256:bb979b278ffb8d31c8b07336fd187ef8fafc8766ebeaece524304483ea137e96 AS build-stage

ARG APP_VERSION=dev

# Set destination for COPY
WORKDIR /app

# Install prerequisites for Bitwarden SDK
RUN apt-get update && apt-get install -y \
    musl-tools \
    && rm -rf /var/lib/apt/lists/*

# Set build environment
ENV GOCACHE=/root/.cache/go-build \
    CGO_ENABLED=1 \
    CC=musl-gcc \
    GOOS=linux

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
    go build -ldflags="-s -w -X main.Version=${APP_VERSION} -linkmode external -extldflags '-static -Wl,-unresolved-symbols=ignore-all'" -o / ./...

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
