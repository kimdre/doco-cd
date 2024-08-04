BINARY_DIR=bin
BINARY_NAME=docker-compose-webhook
.PHONY: test build run lint fmt update update-all submodule-commit

test:
	@echo "Running tests..."
	@WEBHOOK_SECRET="test_Secret1" go test -p 1 -v ./... -timeout 5m

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

submodule-commit:
	git submodule foreach "git add . && git commit -m 'docs: update wiki' && git push"
	git add docs/ && git commit -m 'docs: update wiki' && git push