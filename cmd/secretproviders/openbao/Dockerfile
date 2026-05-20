# syntax=docker/dockerfile:1@sha256:2780b5c3bab67f1f76c781860de469442999ed1a0d7992a5efdf2cffc0e3d769
FROM --platform=$BUILDPLATFORM tonistiigi/xx AS xx

FROM --platform=$BUILDPLATFORM golang:1.26.3@sha256:313faae491b410a35402c05d35e7518ae99103d957308e940e1ae2cfa0aac29b AS build
COPY --from=xx / /

ARG TARGETPLATFORM
ARG APP_VERSION=dev

ENV CGO_ENABLED=0 \
    GOCACHE=/root/.cache/go-build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod/ \
    go mod download -x

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target=/root/.cache/go-build \
    xx-go build \
        -trimpath \
        -ldflags="-s -w -X main.Version=${APP_VERSION}" \
        -o /out/doco-cd-secretprovider-openbao \
        ./cmd/secretproviders/openbao && \
    xx-verify /out/doco-cd-secretprovider-openbao

FROM gcr.io/distroless/base-debian13@sha256:f2df8702d4dcc45ce76df6cbc14ad1975fcf88a04bd0e8947b6194264f9ab75e AS release

COPY --from=build /out/doco-cd-secretprovider-openbao /doco-cd-secretprovider-openbao

ENV SECRET_PROVIDER_GRPC_ENDPOINT=unix:///var/run/doco-cd/secret-provider.sock

ENTRYPOINT ["/doco-cd-secretprovider-openbao"]
