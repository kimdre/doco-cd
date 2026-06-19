GO_BIN?=$(shell pwd)/.bin
BINARY_DIR=bin
BINARY_NAME=doco-cd
.PHONY: test test-verbose test-coverage test-run build fmt lint update update-all download tools compose-up compose-down wiki-tools wiki-build wiki-serve wiki-version-publish buf buf-lint buf-generate buf-breaking buf-format docker-build docker-build-plugins $(addprefix docker-build-plugin-,$(SECRET_PROVIDER_PLUGINS))

ifneq (,$(wildcard ./.env))
    include .env
    export
endif

BUILD_FLAGS:=-ldflags="-X main.Version=dev"

# Secret-provider plugins. Automatically generated from cmd/secretproviders directories.
# Remove non-plugin directories and trailing slashes from plugin directory names
SECRET_PROVIDER_PLUGINS:=$(filter-out internal .template,$(notdir $(patsubst %/,%,$(wildcard cmd/secretproviders/*/))))
# Determine CGO requirements: plugins with .cgo_required marker file need CGO, others are pure Go.
# For each .cgo_required file, extract the parent directory name
_CGO_FILES:=$(wildcard cmd/secretproviders/*/.cgo_required)
SECRET_PROVIDER_PLUGINS_CGO:=$(foreach file,$(_CGO_FILES),$(notdir $(patsubst %/.cgo_required,%,$(file))))
SECRET_PROVIDER_PLUGINS_PURE_GO:=$(filter-out $(SECRET_PROVIDER_PLUGINS_CGO),$(SECRET_PROVIDER_PLUGINS))
SECRET_PROVIDER_PLUGINS_PURE_GO_PATHS:=$(addprefix ./cmd/secretproviders/,$(SECRET_PROVIDER_PLUGINS_PURE_GO))
SECRET_PROVIDER_PLUGINS_CGO_PATHS:=$(addprefix ./cmd/secretproviders/,$(SECRET_PROVIDER_PLUGINS_CGO))

# CGO toolchain for plugins that need it (Bitwarden SDK, 1Password SDK).
# Bitwarden SDK build flags https://github.com/bitwarden/sdk-go/blob/main/INSTRUCTIONS.md
CGO_LDFLAGS_LINUX:=-linkmode external -extldflags '-static -Wl,-unresolved-symbols=ignore-all'
ifeq ($(shell uname),Linux)
    CGO_LDFLAGS:=$(CGO_LDFLAGS_LINUX)
    CGO_COMPILER:=CGO_ENABLED=1 CC=musl-gcc
else ifeq ($(shell uname),Darwin)
    CGO_LDFLAGS:=
    CGO_COMPILER:=CGO_ENABLED=1 CC=clang CXX=clang++
endif

CGO_BUILD_FLAGS:=-ldflags="-X main.Version=dev $(CGO_LDFLAGS)"

TEST_ENV:=WEBHOOK_SECRET="test_Secret1" API_SECRET="test_apiSecret1"

test:
	@echo "Running tests (core, pure-Go plugins)..."
	@$(TEST_ENV) CGO_ENABLED=0 go test ${BUILD_FLAGS} -cover ./cmd/doco-cd/... ./internal/... $(SECRET_PROVIDER_PLUGINS_PURE_GO_PATHS:%=%/...) -timeout 10m
	@echo "Running tests (CGO plugins)..."
	@$(TEST_ENV) $(CGO_COMPILER) go test $(CGO_BUILD_FLAGS) -cover $(SECRET_PROVIDER_PLUGINS_CGO_PATHS:%=%/...) -timeout 10m

test-verbose:
	@echo "Running tests..."
	@$(TEST_ENV) CGO_ENABLED=0 go test ${BUILD_FLAGS} -v -cover ./cmd/doco-cd/... ./internal/... $(SECRET_PROVIDER_PLUGINS_PURE_GO_PATHS:%=%/...) -timeout 10m
	@$(TEST_ENV) $(CGO_COMPILER) go test $(CGO_BUILD_FLAGS) -v -cover $(SECRET_PROVIDER_PLUGINS_CGO_PATHS:%=%/...) -timeout 10m

test-coverage:
	@echo "Running tests with coverage..."
	@$(TEST_ENV) CGO_ENABLED=0 go test ${BUILD_FLAGS} -v -coverprofile cover.out ./cmd/doco-cd/... ./internal/... $(SECRET_PROVIDER_PLUGINS_PURE_GO_PATHS:%=%/...)
	@$(TEST_ENV) $(CGO_COMPILER) go test $(CGO_BUILD_FLAGS) -v -coverprofile cover-cgo.out $(SECRET_PROVIDER_PLUGINS_CGO_PATHS:%=%/...)
	@go tool cover -html cover.out -o cover.html

# Run specified tests from arguments
test-run:
	@echo "Running tests: $(filter-out $@,$(MAKECMDGOALS))"
	@$(TEST_ENV) CGO_ENABLED=0 go test ${BUILD_FLAGS} -cover ./cmd/doco-cd/... ./internal/... $(SECRET_PROVIDER_PLUGINS_PURE_GO_PATHS:%=%/...) -timeout 10m -run $(filter-out $@,$(MAKECMDGOALS))
	@$(TEST_ENV) $(CGO_COMPILER) go test $(CGO_BUILD_FLAGS) -cover $(SECRET_PROVIDER_PLUGINS_CGO_PATHS:%=%/...) -timeout 10m -run $(filter-out $@,$(MAKECMDGOALS))

build:
	mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 go build ${BUILD_FLAGS} -o $(BINARY_DIR) ./cmd/doco-cd $(SECRET_PROVIDER_PLUGINS_PURE_GO_PATHS)
	$(CGO_COMPILER) go build $(CGO_BUILD_FLAGS) -o $(BINARY_DIR) $(SECRET_PROVIDER_PLUGINS_CGO_PATHS)

lint fmt:
	${GO_BIN}/golangci-lint run --fix ./...
	@go fix ./...

update:
	git pull origin main
	git submodule update --recursive --remote

update-all: update
	git submodule foreach git pull origin master
	git submodule foreach git checkout master

download:
	@echo Download go.mod dependencies
	@go mod download

tools:
	mkdir -p ${GO_BIN}
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b ${GO_BIN} latest
	GOBIN=${GO_BIN} go install tool

compose-up:
	@echo "Starting dev docker-compose..."
	@docker compose -f dev.compose.yaml up --build

compose-down:
	@echo "Stopping dev docker-compose..."
	@docker compose -f dev.compose.yaml down --volumes

cleanup:
	@CONTAINERS=$$(docker container ls -a --format "{{.ID}}" --filter "label=cd.doco.metadata.manager"); \
	if [ -n "$$CONTAINERS" ]; then \
		for PROJECT in $$(for ID in $$CONTAINERS; do docker container inspect --format '{{ index .Config.Labels "com.docker.compose.project" }}' $$ID; done | sort | uniq); do \
			docker compose -p $$PROJECT down -v; \
		done; \
	else \
		echo "No containers to clean up."; \
	fi

clean-testcache:
	go clean -testcache

webhook:
	@SIGNATURE=$$(openssl dgst -sha256 -hmac "test_Secret1" < cmd/doco-cd/testdata/github_payload.json | sed 's/^.* //') && \
  	curl -X POST -H "X-Hub-Signature-256: sha256=$$SIGNATURE" \
  		-H "Content-Type: application/json" \
  		-H "X-GitHub-Event: push" \
  		--data @cmd/doco-cd/testdata/github_payload.json \
  		http://localhost/v1/webhook

BUF_BREAKING_AGAINST?=.git#branch=main

buf: buf-lint buf-generate

buf-lint:
	@echo "Linting protobuf definitions..."
	@go tool buf lint

buf-format:
	@echo "Formatting protobuf definitions..."
	@go tool buf format -w

buf-generate:
	@echo "Generating protobuf code..."
	@go tool buf generate

buf-breaking:
	@echo "Checking for breaking protobuf changes against $(BUF_BREAKING_AGAINST)..."
	@go tool buf breaking --against '$(BUF_BREAKING_AGAINST)'

IMAGE_REPO?=ghcr.io/kimdre/doco-cd
APP_VERSION?=dev
DOCKER_BUILD?=docker buildx build
DOCKER_PLATFORMS?=linux/$(shell go env GOARCH)

docker-build:
	$(DOCKER_BUILD) \
		--platform $(DOCKER_PLATFORMS) \
		--build-arg APP_VERSION=$(APP_VERSION) \
		-t $(IMAGE_REPO):$(APP_VERSION) \
		-f Dockerfile .

docker-build-plugins: $(addprefix docker-build-plugin-,$(SECRET_PROVIDER_PLUGINS))

docker-build-plugin-%:
	@DOCKERFILE_PATH=$$(realpath --relative-to=. cmd/secretproviders/$*/Dockerfile); \
	$(DOCKER_BUILD) \
		--platform $(DOCKER_PLATFORMS) \
		--build-arg APP_VERSION=$(APP_VERSION) \
		--build-arg PLUGIN_NAME=$* \
		-t $(IMAGE_REPO)-secretprovider-$*:$(APP_VERSION) \
		-f $$DOCKERFILE_PATH .

wiki-tools:
	python3 -m venv .venv-wiki
	.venv-wiki/bin/python -m pip install --upgrade pip
	.venv-wiki/bin/python -m pip install -r wiki/requirements.txt

wiki-serve:
	.venv-wiki/bin/zensical serve --config-file wiki/zensical.toml

