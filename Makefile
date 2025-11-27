.PHONY: all build build-cli build-electron clean test deps lint help
.PHONY: build-linux build-darwin build-windows
.PHONY: electron-deps electron-dev electron-build electron-build-all

# Version information
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -ldflags "-X github.com/rancher-kubeconfig-proxy/cmd.Version=$(VERSION)"

# Binary names
BINARY_NAME := rancher-kubeconfig-proxy
BINARY_NAME_WIN := $(BINARY_NAME).exe

# Directories
BIN_DIR := bin
ELECTRON_DIR := electron

# Go parameters
GOCMD := go
GOBUILD := $(GOCMD) build
GOCLEAN := $(GOCMD) clean
GOTEST := $(GOCMD) test
GOGET := $(GOCMD) get
GOMOD := $(GOCMD) mod

## help: Display this help message
help:
	@echo "Rancher Kubeconfig Proxy - Build System"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed 's/^/ /'

## all: Build everything (CLI for current platform)
all: build

## deps: Download Go module dependencies
deps:
	$(GOMOD) download
	$(GOMOD) tidy

## build: Build CLI binary for current platform
build: deps
	$(GOBUILD) $(LDFLAGS) -o $(BIN_DIR)/$(BINARY_NAME) .

## build-linux: Build CLI binary for Linux (amd64)
build-linux: deps
	mkdir -p $(BIN_DIR)/linux
	GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BIN_DIR)/linux/$(BINARY_NAME) .

## build-linux-arm64: Build CLI binary for Linux (arm64)
build-linux-arm64: deps
	mkdir -p $(BIN_DIR)/linux-arm64
	GOOS=linux GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BIN_DIR)/linux-arm64/$(BINARY_NAME) .

## build-darwin: Build CLI binary for macOS (amd64)
build-darwin: deps
	mkdir -p $(BIN_DIR)/darwin
	GOOS=darwin GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BIN_DIR)/darwin/$(BINARY_NAME) .

## build-darwin-arm64: Build CLI binary for macOS (arm64/Apple Silicon)
build-darwin-arm64: deps
	mkdir -p $(BIN_DIR)/darwin-arm64
	GOOS=darwin GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BIN_DIR)/darwin-arm64/$(BINARY_NAME) .

## build-windows: Build CLI binary for Windows (amd64)
build-windows: deps
	mkdir -p $(BIN_DIR)/win32
	GOOS=windows GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BIN_DIR)/win32/$(BINARY_NAME_WIN) .

## build-all: Build CLI binaries for all platforms
build-all: build-linux build-linux-arm64 build-darwin build-darwin-arm64 build-windows

## test: Run tests
test:
	$(GOTEST) -v ./...

## test-coverage: Run tests with coverage
test-coverage:
	$(GOTEST) -v -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out -o coverage.html

## lint: Run linter
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed, skipping..."; \
	fi

## clean: Clean build artifacts
clean:
	$(GOCLEAN)
	rm -rf $(BIN_DIR)
	rm -rf $(ELECTRON_DIR)/dist
	rm -rf $(ELECTRON_DIR)/node_modules
	rm -f coverage.out coverage.html

## electron-deps: Install Electron dependencies
electron-deps:
	cd $(ELECTRON_DIR) && npm install

## electron-dev: Run Electron app in development mode (requires backend to be built)
electron-dev: build electron-deps
	mkdir -p $(BIN_DIR)/linux $(BIN_DIR)/darwin $(BIN_DIR)/win32
	cp $(BIN_DIR)/$(BINARY_NAME) $(BIN_DIR)/linux/ 2>/dev/null || true
	cp $(BIN_DIR)/$(BINARY_NAME) $(BIN_DIR)/darwin/ 2>/dev/null || true
	cp $(BIN_DIR)/$(BINARY_NAME) $(BIN_DIR)/win32/ 2>/dev/null || true
	cd $(ELECTRON_DIR) && npm start

## electron-build-linux: Build Electron app for Linux
electron-build-linux: build-linux electron-deps
	cd $(ELECTRON_DIR) && npm run build:linux

## electron-build-mac: Build Electron app for macOS
electron-build-mac: build-darwin build-darwin-arm64 electron-deps
	cd $(ELECTRON_DIR) && npm run build:mac

## electron-build-win: Build Electron app for Windows
electron-build-win: build-windows electron-deps
	cd $(ELECTRON_DIR) && npm run build:win

## electron-build-all: Build Electron app for all platforms
electron-build-all: build-all electron-deps
	cd $(ELECTRON_DIR) && npm run build

## run: Run the CLI application
run: build
	./$(BIN_DIR)/$(BINARY_NAME)

## serve: Run the web server
serve: build
	./$(BIN_DIR)/$(BINARY_NAME) serve

## install: Install the CLI to GOPATH/bin
install: deps
	$(GOCMD) install $(LDFLAGS) .
