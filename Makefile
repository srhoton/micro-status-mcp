BINARY := micro-status-mcp
PKG    := ./cmd/$(BINARY)

.PHONY: build install test vet lint fmt tidy run clean

build:
	go build -o bin/$(BINARY) $(PKG)

install:
	go install $(PKG)

test:
	go test ./...

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not installed; install with:"; \
		echo "  brew install golangci-lint"; \
		exit 1; \
	}
	golangci-lint run ./...

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

run: build
	./bin/$(BINARY) serve

clean:
	rm -rf bin
