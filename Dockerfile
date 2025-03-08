# syntax=docker/dockerfile:1@sha256:4c68376a702446fc3c79af22de146a148bc3367e73c25a5803d453b6b3f722fb
FROM golang:1.23.6@sha256:adfbe17b774398cb090ad257afd692f2b7e0e7aaa8ef0110a48f0a775e3964f4 AS build-stage

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

FROM gcr.io/distroless/base-debian12@sha256:74ddbf52d93fafbdd21b399271b0b4aac1babf8fa98cab59e5692e01169a1348 AS build-release-stage

WORKDIR /

COPY --from=build-stage /doco-cd /doco-cd

ENV TZ=UTC \
    HTTP_PORT=80 \
    LOG_LEVEL=info

USER nonroot:nonroot

CMD ["/doco-cd"]
