.DEFAULT_GOAL := help

WAILS ?= $(shell go env GOPATH)/bin/wails
BIN   := build/bin

.PHONY: help install build build-windows dev dev-web test lint clean

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# Go modules are vendored (vendor/, committed): go build/test/vet use
# -mod=vendor on their own. After changing go.mod, run `make install` (or
# `go mod vendor`) and commit the vendor changes.
install: ## Install all dependencies (vendor Go modules, Wails CLI, both frontends)
	go mod vendor
	go install github.com/wailsapp/wails/v2/cmd/wails@latest
	npm --prefix frontend install
	npm --prefix web install

# Ubuntu 24.04 and Arch ship WebKitGTK 4.1 only; wails links 4.0 unless told otherwise.
WEBKIT_TAGS := $(shell pkg-config --exists webkit2gtk-4.0 2>/dev/null || (pkg-config --exists webkit2gtk-4.1 2>/dev/null && echo -tags webkit2_41))

build: ## Build the app for this machine (both UIs embedded) into build/bin/
	$(WAILS) build -skipbindings $(WEBKIT_TAGS) # bindings under frontend/wailsjs are committed and hand-adjusted, as in CI

build-windows: ## Build both UIs, then cross-compile a CGO-free Windows binary
	npm --prefix frontend run build
	npm --prefix web run build
	$(WAILS) build -platform windows/amd64 -s -skipbindings

dev: ## Run the admin window with hot reload
	$(WAILS) dev

dev-web: ## Run the guest UI dev server (proxies /api to the running guest server)
	npm --prefix web run dev

# node_modules ships Go files (flatted); keep the Go tools to our own packages
GO_PKGS = $$(go list ./... | grep -v /node_modules/)

test: ## Run Go tests
	go test $(GO_PKGS)

lint: ## gofmt check, go vet, and Vue type-check for both UIs
	@test -z "$$(gofmt -l *.go internal web | tee /dev/stderr)" || (echo "gofmt: files need formatting" && exit 1)
	go vet $(GO_PKGS)
	npm --prefix frontend run typecheck
	npm --prefix web run typecheck

clean: ## Remove build output and built UIs
	rm -rf $(BIN) frontend/dist/assets frontend/dist/index.html web/dist/assets web/dist/index.html
