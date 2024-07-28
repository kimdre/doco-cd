# syntax=docker/dockerfile:1
FROM golang:1.22.5 AS build-stage

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

# Build
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    --mount=type=bind,target=. \
    go build -o / ./...

# Run the tests in the container
FROM build-stage AS run-test-stage

RUN go test -v ./...

FROM gcr.io/distroless/base-debian12 AS build-release-stage

WORKDIR /

COPY --from=build-stage /docker-compose-webhook /docker-compose-webhook

ENV TZ=UTC \
    HTTP_PORT=80 \
    LOG_LEVEL=info

USER nonroot:nonroot

CMD ["/docker-compose-webhook"]
