BINARY_NAME=ollama-exporter
MAIN_PATH=./cmd/exporter
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-X main.Version=$(VERSION) -s -w"
GOLANGCI_VERSION=v1.57.2

.PHONY: all build clean test vet lint run docker-build docker-push help

all: vet lint test build

build:
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(BINARY_NAME) $(MAIN_PATH)

clean:
	rm -f $(BINARY_NAME)
	go clean ./...

test:
	go test -v -race -coverprofile=coverage.out ./...

vet:
	go vet ./...

lint:
	@which golangci-lint > /dev/null 2>&1 || \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
		| sh -s -- -b $(shell go env GOPATH)/bin $(GOLANGCI_VERSION)
	golangci-lint run ./...

run: build
	./$(BINARY_NAME)

docker-build:
	docker build -t ghcr.io/maravexa/ollama-exporter:$(VERSION) .

docker-push: docker-build
	docker push ghcr.io/maravexa/ollama-exporter:$(VERSION)

coverage: test
	go tool cover -html=coverage.out

help:
	@echo "Targets:"
	@echo "  build        - compile binary"
	@echo "  test         - run tests with race detector"
	@echo "  vet          - run go vet"
	@echo "  lint         - run golangci-lint"
	@echo "  run          - build and run locally"
	@echo "  clean        - remove build artifacts"
	@echo "  docker-build - build Docker image"
	@echo "  docker-push  - build and push Docker image"
	@echo "  coverage     - open coverage report in browser"
	@echo "  all          - vet + lint + test + build"
