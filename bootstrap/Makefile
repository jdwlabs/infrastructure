# Talops Makefile

BINARY_NAME=talops
BUILD_DIR=./build

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod

# Build flags
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-s -w -X github.com/jdwlabs/infrastructure/bootstrap/cmd.version=$(VERSION)"

.PHONY: all build clean test test-race deps run help

all: deps build

build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) .
	@echo "Built: $(BUILD_DIR)/$(BINARY_NAME)"

clean:
	@echo "Cleaning..."
	$(GOCLEAN)
	@rm -rf $(BUILD_DIR)
	@rm -rf clusters/

test:
	@echo "Running tests..."
	$(GOTEST) -v ./...

test-race:
	@echo "Running tests with race detector..."
	CGO_ENABLED=1 $(GOTEST) -race -v ./...

deps:
	@echo "Downloading dependencies..."
	$(GOMOD) download
	$(GOMOD) tidy

run: build
	@echo "Running $(BINARY_NAME)..."
	$(BUILD_DIR)/$(BINARY_NAME) status

# Cross compilation
build-linux:
	@echo "Building for Linux (amd64)..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 .

build-linux-arm64:
	@echo "Building for Linux (arm64)..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 .

build-windows:
	@echo "Building for Windows (amd64)..."
	@mkdir -p $(BUILD_DIR)
	GOOS=windows GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe .

build-mac:
	@echo "Building for macOS (amd64)..."
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 .

build-mac-arm64:
	@echo "Building for macOS (arm64)..."
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 .

build-all: build-linux build-linux-arm64 build-windows build-mac build-mac-arm64

# Development helpers
fmt:
	@echo "Formatting code..."
	gofmt -w .

lint:
	@echo "Linting code..."
	golangci-lint run ./...

vet:
	@echo "Running go vet..."
	$(GOCMD) vet ./...

# Install locally
install: build
	@echo "Installing to GOPATH/bin..."
	cp $(BUILD_DIR)/$(BINARY_NAME) $(GOPATH)/bin/

clean-logs:
	@echo "Removing logs older than 30 days..."
	find logs/ -maxdepth 1 -type d -mtime +30 -exec rm -rf {} + 2>/dev/null || true

help:
	@echo "Available targets:"
	@echo "  make build        - Build the binary"
	@echo "  make build-all    - Build for all platforms (linux/darwin/windows, amd64/arm64)"
	@echo "  make test         - Run tests"
	@echo "  make test-race    - Run tests with race detector"
	@echo "  make clean        - Clean build artifacts"
	@echo "  make clean-logs   - Remove logs older than 30 days"
	@echo "  make deps         - Download dependencies"
	@echo "  make run          - Build and run status command"
	@echo "  make fmt          - Format code"
	@echo "  make lint         - Run linter"
	@echo "  make vet          - Run go vet"
