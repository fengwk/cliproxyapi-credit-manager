SHELL := /bin/bash

PLUGIN_ID := cliproxyapi-credit-manager
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
VERSION ?= $(shell bash scripts/plugin-version.sh)
SDK_VERSION ?= $(shell bash scripts/plugin-sdk.sh)
GIT_COMMIT ?= $(shell bash scripts/build-commit.sh)

ifeq ($(GOOS),darwin)
LIB_EXT := dylib
else
LIB_EXT := so
endif

DIST_DIR := dist/$(GOOS)/$(GOARCH)
LIB := $(DIST_DIR)/$(PLUGIN_ID).$(LIB_EXT)
LDFLAGS := -s -w \
	-X github.com/fengwk/cliproxyapi-credit-manager/internal/plugin.PluginVersion=$(VERSION) \
	-X github.com/fengwk/cliproxyapi-credit-manager/internal/plugin.GitCommit=$(GIT_COMMIT) \
	-X github.com/fengwk/cliproxyapi-credit-manager/internal/plugin.SDKVersion=$(SDK_VERSION)

.PHONY: help build vet test race static package clean

help:
	@echo "cliproxyapi-credit-manager"
	@echo ""
	@echo "  make build     build the native plugin into $(DIST_DIR)/"
	@echo "  make test      go vet, go test and the discovered Node web tests"
	@echo "  make static    dependency-free delivery checks (scripts + packaging)"
	@echo "  make race      go test -race"
	@echo "  make package   zip the built plugin for the current GOOS/GOARCH"
	@echo "  make clean     remove dist/"
	@echo ""
	@echo "Variables: GOOS, GOARCH, VERSION=$(VERSION), SDK_VERSION=$(SDK_VERSION)"
	@echo "Install: copy the library from $(DIST_DIR)/ into the configured plugins dir"

build:
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -buildmode=c-shared \
		-ldflags '$(LDFLAGS)' -o $(LIB) .
	@rm -f $(DIST_DIR)/$(PLUGIN_ID).h
	@echo "built $(LIB) (version $(VERSION), sdk $(SDK_VERSION))"

vet:
	go vet ./...

test: vet
	go test ./...
	bash scripts/web-tests.sh

# Delivery checks that need neither the business packages nor a C toolchain, so
# they are runnable while the implementation slices are still being merged.
static:
	bash scripts/check-delivery.sh
	bash scripts/test-package-plugin.sh
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/test-verify-release-assets.py
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/test-release-pipeline.py

race:
	go test -race ./...

package: build
	bash scripts/package-plugin.sh "$(VERSION)" "$(GOOS)" "$(GOARCH)" "dist/pkg"

clean:
	rm -rf dist
