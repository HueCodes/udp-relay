# Drone Telemetry Server Makefile
# ================================

.PHONY: all build run test clean lint fmt vet check help

# Build settings
BINARY_NAME := drone-server
BUILD_DIR := ./build
CMD_DIR := ./cmd/server
GO := go

# Version info (from git if available)
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME)"

# Default target
all: check build

## build: Build the server binary
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) $(CMD_DIR)
	@echo "Binary created: $(BUILD_DIR)/$(BINARY_NAME)"

## build-race: Build with race detector enabled
build-race:
	@echo "Building with race detector..."
	$(GO) build -race $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-race $(CMD_DIR)

## run: Build and run the server
run: build
	@echo "Starting server..."
	$(BUILD_DIR)/$(BINARY_NAME)

## run-debug: Run with debug logging
run-debug: build
	$(BUILD_DIR)/$(BINARY_NAME) -log-level=debug

## test: Run all tests
test:
	@echo "Running tests..."
	$(GO) test -v ./...

## test-race: Run tests with race detector
test-race:
	@echo "Running tests with race detector..."
	$(GO) test -race -v ./...

## test-cover: Run tests with coverage report
test-cover:
	@echo "Running tests with coverage..."
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

## bench: Run benchmarks
bench:
	@echo "Running benchmarks..."
	$(GO) test -bench=. -benchmem ./...

## lint: Run golangci-lint
lint:
	@echo "Running linter..."
	@which golangci-lint > /dev/null || (echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
	golangci-lint run ./...

## fmt: Format code
fmt:
	@echo "Formatting code..."
	$(GO) fmt ./...
	@which goimports > /dev/null && goimports -w . || true

## vet: Run go vet
vet:
	@echo "Running go vet..."
	$(GO) vet ./...

## check: Run all checks (fmt, vet, test)
check: fmt vet test

## tidy: Tidy and verify go.mod
tidy:
	$(GO) mod tidy
	$(GO) mod verify

## clean: Remove build artifacts
clean:
	@echo "Cleaning..."
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html

## deps: Download dependencies
deps:
	@echo "Downloading dependencies..."
	$(GO) mod download

## docker: Build Docker image
docker:
	@echo "Building Docker image..."
	docker build -t $(BINARY_NAME):$(VERSION) .

## simulate: Run server with built-in traffic simulator
simulate: build
	@echo "Starting server with simulated traffic..."
	@$(BUILD_DIR)/$(BINARY_NAME) -log-level=debug & SERVER_PID=$$!; \
	sleep 1; \
	echo "Starting simulator (5 drones)..."; \
	$(GO) run ./cmd/simulator -drones 5 -rate 10 & SIM_PID=$$!; \
	trap "kill $$SERVER_PID $$SIM_PID 2>/dev/null" EXIT; \
	wait $$SERVER_PID

## tui: Run the terminal UI dashboard
tui:
	$(GO) run ./cmd/tui

## help: Show this help
help:
	@echo "Drone Telemetry Server - Build Commands"
	@echo "========================================"
	@echo ""
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
	@echo ""
	@echo "Examples:"
	@echo "  make build          # Build the server"
	@echo "  make run            # Build and run"
	@echo "  make test           # Run tests"
	@echo "  make check          # Run all checks"
