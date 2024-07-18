# syntax=docker/dockerfile:1

FROM golang:1.22 AS build-stage

# Set destination for COPY
WORKDIR /app

# Download Go modules
COPY src/go.mod src/go.sum ./
RUN go mod download

# Copy source code
COPY src/*.go ./

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -o /webhook-app

# Run the tests in the container
FROM build-stage AS run-test-stage
RUN go test -v ./...

FROM gcr.io/distroless/base-debian11 AS build-release-stage

WORKDIR /

COPY --from=build-stage /webhook-app /webhook-app

ENV HTTP_PORT=8080

EXPOSE $HTTP_PORT

USER nonroot:nonroot

# Run
CMD ["/webhook-app"]
