BINARY_NAME=ollama-exporter
MAIN_PATH=./cmd/exporter
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-X main.Version=$(VERSION) -s -w"

.PHONY: all build clean test vet fmt lint vulncheck ci run docker-build docker-push coverage help

all: vet lint test build

build:
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(BINARY_NAME) $(MAIN_PATH)

clean:
	rm -f $(BINARY_NAME)
	go clean ./...

test:
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out

vet:
	go vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || (echo "Run gofmt -w ." && exit 1)

lint:
	golangci-lint run --timeout=5m

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

ci: fmt vet lint test vulncheck
	@echo "All CI checks passed"

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
	@echo "  test         - run tests with race detector and coverage"
	@echo "  vet          - run go vet"
	@echo "  fmt          - check formatting (use gofmt -w . to fix)"
	@echo "  lint         - run golangci-lint"
	@echo "  vulncheck    - run govulncheck for known vulnerabilities"
	@echo "  ci           - run all CI checks locally (fmt vet lint test vulncheck)"
	@echo "  run          - build and run locally"
	@echo "  clean        - remove build artifacts"
	@echo "  docker-build - build Docker image"
	@echo "  docker-push  - build and push Docker image"
	@echo "  coverage     - open coverage report in browser"
	@echo "  all          - vet + lint + test + build"
