.PHONY: all build test lint fmt clean

all: lint test build

build:
	go build ./...

test:
	go test -v -race -coverprofile=coverage.out ./...

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed, running go vet only"; \
		go vet ./...; \
	fi

fmt:
	go fmt ./...
	@if command -v goimports >/dev/null 2>&1; then \
		goimports -w .; \
	fi

clean:
	rm -f coverage.out

coverage: test
	go tool cover -html=coverage.out

check: fmt lint test
