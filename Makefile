.DEFAULT_GOAL := help

ifneq (,$(wildcard .env))
include .env
export
endif

# libvips is installed by Homebrew on macOS, where cgo cannot find it without
# an explicit pkg-config path.
ifeq ($(shell uname), Darwin)
export PKG_CONFIG_PATH := /opt/homebrew/lib/pkgconfig:/opt/homebrew/share/pkgconfig:$(PKG_CONFIG_PATH)
endif

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: run
run: ## Run a worker from source
	go run ./cmd/worker

.PHONY: build
build: ## Compile the worker binary into bin/
	go build -o bin/worker ./cmd/worker

.PHONY: test
test: ## Run the test suite
	go test ./...

.PHONY: tidy
tidy: ## Sync go.mod and go.sum
	go mod tidy

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: docker-build
docker-build: ## Build the worker container image
	docker build -t filecosystem-worker .
