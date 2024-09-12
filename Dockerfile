# syntax=docker/dockerfile:1@sha256:865e5dd094beca432e8c0a1d5e1c465db5f998dca4e439981029b3b81fb39ed5
FROM golang:1.23.1@sha256:2fe82a3f3e006b4f2a316c6a21f62b66e1330ae211d039bb8d1128e12ed57bf1 AS build-stage

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

FROM gcr.io/distroless/base-debian12@sha256:c925d12234f8d3fbef2256012b168004d4c47a82c4f06afcfd06fd208732fbe0 AS build-release-stage

WORKDIR /

COPY --from=build-stage /doco-cd /doco-cd

ENV TZ=UTC \
    HTTP_PORT=80 \
    LOG_LEVEL=info

USER nonroot:nonroot

CMD ["/doco-cd"]
