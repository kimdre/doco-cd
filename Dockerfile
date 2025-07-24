# syntax=docker/dockerfile:1@sha256:9857836c9ee4268391bb5b09f9f157f3c91bb15821bb77969642813b0d00518d
FROM golang:1.24.5@sha256:ef5b4be1f94b36c90385abd9b6b4f201723ae28e71acacb76d00687333c17282 AS build-stage

ARG APP_VERSION=dev

# Set destination for COPY
WORKDIR /app

# Download Go modules
COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    go mod download -x

# Copy source code
COPY . ./

# Set build environment
ENV GOCACHE=/root/.cache/go-build \
    CGO_ENABLED=0 \
    GOOS=linux

# Build and strip binary
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    --mount=type=bind,target=. \
    go build -ldflags="-s -w -X main.Version=${APP_VERSION}" -o / ./...

#FROM busybox AS busybox-binaries
#
#ARG BUSYBOX_VERSION=1.31.0-i686-uclibc
#ADD https://busybox.net/downloads/binaries/$BUSYBOX_VERSION/busybox_WGET /wget
#RUN chmod a+x /wget

FROM gcr.io/distroless/base-debian12@sha256:201ef9125ff3f55fda8e0697eff0b3ce9078366503ef066653635a3ac3ed9c26 AS build-release-stage

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
