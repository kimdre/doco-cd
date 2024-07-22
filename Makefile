BINARY_DIR=bin
BINARY_NAME=docker-compose-webhook
.PHONY: test build run lint fmt

test:
	@echo "Running tests..."
	@go test -v ./... -timeout 30m

build:
	mkdir -p $(BINARY_DIR)
	go build -o $(BINARY_DIR) ./...

lint:
	golangci-lint run ./...

fmt:
	go fmt ./...
	-go run mvdan.cc/gofumpt@latest -l -w .
	-go run golang.org/x/tools/cmd/goimports@latest -l -w .
	-go run github.com/bombsimon/wsl/v4/cmd...@latest -strict-append -test=true -fix ./...
	-go run github.com/catenacyber/perfsprint@latest -fix ./...

update:
	git pull origin main
	git submodule update --init --recursive

update-all: update
	git submodule foreach git pull origin master
	git submodule foreach git checkout master