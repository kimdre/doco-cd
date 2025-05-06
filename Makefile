BINARY_DIR=bin
BINARY_NAME=doco-cd
.PHONY: test build run lint fmt update update-all submodule-commit generate-coverage

ifneq (,$(wildcard ./.env))
    include .env
    export
endif

test:
	@echo "Running tests..."
	@WEBHOOK_SECRET="test_Secret1" go test -cover -p 1 ./... -timeout 5m

test-verbose:
	@echo "Running tests..."
	@WEBHOOK_SECRET="test_Secret1" go test -v -cover -p 1 ./... -timeout 5m

test-coverage:
	@echo "Running tests with coverage..."
	@WEBHOOK_SECRET="test_Secret1" go test -v -coverprofile cover.out ./...
	@go tool cover -html cover.out -o cover.html

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

wiki-commit:
	git submodule foreach "git add . && git commit -m 'docs: update wiki' && git push"
	git add docs/ && git commit -m 'docs: update wiki' && git push

download:
	@echo Download go.mod dependencies
	@go mod download

install-devtools: download
	@echo Installing tools from tools.go
	@cat tools/tools.go | grep _ | awk -F'"' '{print $$2}' | xargs -tI % go install %
