VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: help build build-arm64 test lint install validate-examples demo-prepare demo-up demo-watch demo-open demo-down demo-reset

.DEFAULT_GOAL := help

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build bin/duod
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/duod ./cmd/duod

build-arm64: ## Build bin/duod-linux-arm64
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/duod-linux-arm64 ./cmd/duod

test: ## Run tests with race detector
	go test -race ./...

lint: ## Run gofmt and go vet
	gofmt -l . && go vet ./...

install: build ## Install binary and systemd unit (needs root)
	install -m 0755 bin/duod /usr/local/bin/duod
	install -d /etc/duod /etc/duod/traefik
	install -m 0644 examples/systemd/duod.service /etc/systemd/system/duod.service

EXAMPLE_CONFIGS := examples/duod.yaml examples/duod-apache.yaml demo/duod/duod.yaml

validate-examples: build ## Validate every example config (output hidden, errors shown)
	@for f in $(EXAMPLE_CONFIGS); do \
		./bin/duod validate -config $$f >/dev/null && echo "ok  $$f" || exit 1; \
	done

demo-prepare: ## Demo: pre-pull and pre-build images
	cd demo && ./demo.sh prepare

demo-up: ## Demo: start everything and push v1.0.0 of all apps
	cd demo && ./demo.sh up

demo-watch: ## Demo: watch rollout state of all apps
	cd demo && ./demo.sh watch

demo-open: ## Demo: open the demo URLs
	cd demo && ./demo.sh open

demo-down: ## Demo: stop the demo
	cd demo && ./demo.sh down

demo-reset: ## Demo: stop and wipe demo state
	cd demo && ./demo.sh reset
