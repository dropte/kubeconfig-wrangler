.PHONY: all build build-all clean test test-coverage deps tidy lint help run serve install
.PHONY: build-linux build-linux-arm64 build-darwin build-windows
.PHONY: electron-deps electron-dev electron-build-linux electron-build-mac electron-build-win electron-build-all

# Version information
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
MODULE := github.com/kubeconfig-wrangler
LDFLAGS := -ldflags "-X $(MODULE)/cmd.Version=$(VERSION)"

# Binary names
BINARY_NAME := kubeconfig-wrangler
BINARY_NAME_WIN := $(BINARY_NAME).exe

# Directories. The per-platform directories must match electron-builder's
# extraResources ("../bin/${os}", where ${os} is mac, win or linux).
BIN_DIR := bin
MAC_DIR := $(BIN_DIR)/mac
WIN_DIR := $(BIN_DIR)/win
LINUX_DIR := $(BIN_DIR)/linux
ELECTRON_DIR := electron

# Go parameters
GOCMD := go
GOBUILD := $(GOCMD) build
GOTEST := $(GOCMD) test
GOMOD := $(GOCMD) mod

# Host platform, used to stage the backend for `electron-dev`
HOST_OS := $(shell $(GOCMD) env GOOS)
HOST_ARCH := $(shell $(GOCMD) env GOARCH)
ifeq ($(HOST_OS),darwin)
  HOST_BIN_DIR := $(MAC_DIR)
  HOST_EXE := $(BINARY_NAME)
else ifeq ($(HOST_OS),windows)
  HOST_BIN_DIR := $(WIN_DIR)
  HOST_EXE := $(BINARY_NAME_WIN)
else
  HOST_BIN_DIR := $(LINUX_DIR)
  HOST_EXE := $(BINARY_NAME)
endif

## help: Display this help message
help:
	@echo "Kubeconfig Wrangler - Build System"
	@echo ""
	@echo "Usage: make [target] [VERSION=x.y.z]"
	@echo ""
	@echo "Targets:"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed 's/^/ /'

## all: Build the CLI for the current platform
all: build

## deps: Download Go module dependencies
deps:
	$(GOMOD) download

## tidy: Tidy go.mod/go.sum
tidy:
	$(GOMOD) tidy

## build: Build the CLI for the current platform into bin/
build: deps
	$(GOBUILD) $(LDFLAGS) -o $(BIN_DIR)/$(HOST_EXE) .

## build-linux: Build the Linux (amd64) backend into bin/linux
build-linux: deps
	mkdir -p $(LINUX_DIR)
	GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(LINUX_DIR)/$(BINARY_NAME) .

## build-linux-arm64: Build the Linux (arm64) CLI into bin/linux-arm64
build-linux-arm64: deps
	mkdir -p $(BIN_DIR)/linux-arm64
	GOOS=linux GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BIN_DIR)/linux-arm64/$(BINARY_NAME) .

## build-darwin: Build a universal (amd64+arm64) macOS backend into bin/mac
build-darwin: deps
	mkdir -p $(BIN_DIR)/darwin-amd64 $(BIN_DIR)/darwin-arm64 $(MAC_DIR)
	GOOS=darwin GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BIN_DIR)/darwin-amd64/$(BINARY_NAME) .
	GOOS=darwin GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BIN_DIR)/darwin-arm64/$(BINARY_NAME) .
	@# lipo only exists on macOS; elsewhere fall back to a single-arch binary.
	@if [ "$$(uname -s)" = "Darwin" ] && command -v lipo >/dev/null 2>&1; then \
		echo "lipo -create -> $(MAC_DIR)/$(BINARY_NAME) (universal)"; \
		lipo -create -output $(MAC_DIR)/$(BINARY_NAME) \
			$(BIN_DIR)/darwin-amd64/$(BINARY_NAME) $(BIN_DIR)/darwin-arm64/$(BINARY_NAME); \
	else \
		echo "lipo not available; using darwin-$(HOST_ARCH) only"; \
		cp $(BIN_DIR)/darwin-$(HOST_ARCH)/$(BINARY_NAME) $(MAC_DIR)/$(BINARY_NAME); \
	fi

## build-windows: Build the Windows (amd64) backend into bin/win
build-windows: deps
	mkdir -p $(WIN_DIR)
	GOOS=windows GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(WIN_DIR)/$(BINARY_NAME_WIN) .

## build-all: Build binaries for all platforms
build-all: build-linux build-linux-arm64 build-darwin build-windows

## test: Run tests
test:
	$(GOTEST) -race ./...

## test-coverage: Run tests with coverage
test-coverage:
	$(GOTEST) -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out -o coverage.html

## lint: Run golangci-lint (v2)
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed, running via go run..."; \
		$(GOCMD) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...; \
	fi

## clean: Remove build artifacts
clean:
	rm -rf $(BIN_DIR)
	rm -rf $(ELECTRON_DIR)/dist
	rm -f coverage.out coverage.html

## electron-deps: Install Electron dependencies
electron-deps:
	cd $(ELECTRON_DIR) && npm install

## electron-dev: Run the Electron app in development mode with a host backend
electron-dev: build electron-deps
	mkdir -p $(HOST_BIN_DIR)
	cp $(BIN_DIR)/$(HOST_EXE) $(HOST_BIN_DIR)/$(HOST_EXE)
	cd $(ELECTRON_DIR) && npm start

## electron-build-linux: Build the Electron app for Linux
electron-build-linux: build-linux electron-deps
	test -x $(LINUX_DIR)/$(BINARY_NAME)
	cd $(ELECTRON_DIR) && npm run build:linux -- --publish never

## electron-build-mac: Build the Electron app for macOS (universal backend)
electron-build-mac: build-darwin electron-deps
	test -x $(MAC_DIR)/$(BINARY_NAME)
	cd $(ELECTRON_DIR) && npm run build:mac -- --publish never

## electron-build-win: Build the Electron app for Windows
electron-build-win: build-windows electron-deps
	test -f $(WIN_DIR)/$(BINARY_NAME_WIN)
	cd $(ELECTRON_DIR) && npm run build:win -- --publish never

# The macOS app can only be built (and signed) on a macOS host, so
# electron-build-all skips it elsewhere. Windows builds on a non-Windows
# host need wine.
ELECTRON_ALL_TARGETS := electron-build-linux electron-build-win
ifeq ($(HOST_OS),darwin)
  ELECTRON_ALL_TARGETS := electron-build-mac $(ELECTRON_ALL_TARGETS)
endif

## electron-build-all: Build the Electron app for Linux and Windows, plus macOS on a macOS host
electron-build-all:
	@for t in $(ELECTRON_ALL_TARGETS); do $(MAKE) $$t || exit 1; done

## run: Build and run the CLI
run: build
	./$(BIN_DIR)/$(HOST_EXE)

## serve: Build and run the web GUI server
serve: build
	./$(BIN_DIR)/$(HOST_EXE) serve

## install: Install the CLI into GOPATH/bin
install: deps
	$(GOCMD) install $(LDFLAGS) .
