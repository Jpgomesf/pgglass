BINARY := mcp-postgres
MODULE := github.com/jpgomesf/mcp-postgres

.PHONY: build lint vet test test-short clean all release-local

all: lint build

build:
	go build -o $(BINARY) .

lint:
	golangci-lint run ./...

vet:
	go vet ./...

# Unit and integration tests. Integration tests run Postgres in Docker
# (image from MCP_PG_IMAGE, default postgres:17-alpine) and fail without it.
test:
	go test -race ./...

# Unit tests only: integration tests skip under -short.
test-short:
	go test -race -short ./...

clean:
	rm -f $(BINARY)

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $(BINARY)-linux-amd64 .

build-all: build-linux build

release-local:
	goreleaser release --snapshot --clean
