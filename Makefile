BINARY_NAME=trip-service
BUILD_DIR=bin
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
VERSION_FLAGS=-X main.version=$(VERSION)
LDFLAGS=-ldflags "-w -s -extldflags '-static' $(VERSION_FLAGS)"
CMD_DIR=cmd/trip-service

.PHONY: build build-arm build-host dist test lint fmt deps clean

build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./$(CMD_DIR)

build-arm: build

build-host:
	mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./$(CMD_DIR)

dist: build

test:
	go test ./... -v

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

deps:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR)
