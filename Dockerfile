# syntax=docker/dockerfile:1@sha256:87999aa3d42bdc6bea60565083ee17e86d1f3339802f543c0d03998580f9cb89
FROM --platform=$BUILDPLATFORM tonistiigi/xx AS xx
FROM --platform=$BUILDPLATFORM golang:1.26.4@sha256:792443b89f65105abba56b9bd5e97f680a80074ac62fc844a584212f8c8102c3 AS build
COPY --from=xx / /

ARG TARGETPLATFORM
ARG APP_VERSION=dev

ENV CGO_ENABLED=0 \
    GOCACHE=/root/.cache/go-build \
    GOMODCACHE=/go/pkg/mod

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=$GOMODCACHE \
    go mod download -x

COPY . .

RUN --mount=type=cache,target=$GOMODCACHE \
    --mount=type=cache,target=$GOCACHE \
    xx-go build \
        -trimpath \
        -ldflags="-s -w -X github.com/kimdre/doco-cd/internal/config/app.Version=${APP_VERSION}" \
        -o /out/doco-cd \
        ./cmd/doco-cd && \
    xx-verify /out/doco-cd

FROM gcr.io/distroless/base-debian13@sha256:57c1e4c72feb5925c4763ae4f6bd2013ad3854f57eff5b60dd9acb1ce0abc66e AS release

WORKDIR /

# /data volume required to deploy from cloned Git repos
VOLUME /data

COPY --from=build /out/doco-cd /doco-cd

# buildx plugin so compose v5 picks BuildKit instead of the legacy `/build` endpoint
COPY --from=docker/buildx-bin:0.34.1@sha256:ba49f75261dd3ac85491d370a9c38306454a84c5554be4e67de601cd59847cb6 \
    /buildx /usr/libexec/docker/cli-plugins/docker-buildx

COPY --from=build /doco-cd /doco-cd

ENV TZ=UTC \
    HTTP_PORT=80 \
    METRICS_PORT=9120 \
    LOG_LEVEL=info

ENTRYPOINT ["/doco-cd"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/doco-cd", "healthcheck"]
