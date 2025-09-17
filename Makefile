GO_BIN?=$(shell pwd)/.bin
BINARY_DIR=bin
BINARY_NAME=doco-cd
.PHONY: test test-verbose test-coverage test-run build fmt lint update update-all wiki-commit download tools compose-up compose-down

ifneq (,$(wildcard ./.env))
    include .env
    export
endif

BUILD_FLAGS:=
ifeq ($(shell uname), Linux)
    BUILD_FLAGS:=-linkmode external -extldflags '-static -Wl,-unresolved-symbols=ignore-all'
else ifeq ($(shell uname), Darwin)
		BUILD_FLAGS:=""
endif

BUILD_FLAGS:=-ldflags="-X main.Version=dev $(BUILD_FLAGS)"

test:
	@echo "Running tests..."
	@WEBHOOK_SECRET="test_Secret1" API_SECRET="test_apiSecret1" CGO_ENABLED=1 CC=musl-gcc go test ${BUILD_FLAGS} -cover -p 1 ./... -timeout 10m

test-verbose:
	@echo "Running tests..."
	@WEBHOOK_SECRET="test_Secret1" API_SECRET="test_apiSecret1" go test ${BUILD_FLAGS} -v -cover -p 1 ./... -timeout 10m

test-coverage:
	@echo "Running tests with coverage..."
	@WEBHOOK_SECRET="test_Secret1" API_SECRET="test_apiSecret1" go test ${BUILD_FLAGS} -v -coverprofile cover.out ./...
	@go tool cover -html cover.out -o cover.html

# Run specified tests from arguments
test-run:
	@echo "Running tests: $(filter-out $@,$(MAKECMDGOALS))"
	@WEBHOOK_SECRET="test_Secret1" API_SECRET="test_apiSecret1" go test ${BUILD_FLAGS} -cover -p 1 ./... -timeout 10m -run $(filter-out $@,$(MAKECMDGOALS))

build:
	mkdir -p $(BINARY_DIR)
	go build ${BUILD_FLAGS} -o $(BINARY_DIR) ./...

lint fmt:
	${GO_BIN}/golangci-lint run --fix ./...

update:
	git pull origin main
	git submodule update --init --recursive

update-all: update
	git submodule foreach git pull origin master
	git submodule foreach git checkout master

wiki-commit:
	git submodule foreach "git add . && git commit -m 'docs: update wiki' && git push"
	git add docs/ && git commit -m 'docs: update wiki' && git push

download:
	@echo Download go.mod dependencies
	@go mod download

tools:
	mkdir -p ${GO_BIN}
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b ${GO_BIN} latest
	GOBIN=${GO_BIN} go install tool

compose-up:
	@echo "Starting dev docker-compose..."
	@docker compose -f dev.compose.yaml up -d --build

compose-down:
	@echo "Stopping dev docker-compose..."
	@docker compose -f dev.compose.yaml down