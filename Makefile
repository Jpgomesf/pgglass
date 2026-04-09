BINARY := mcp-postgres
MODULE := github.com/jpgomesf/mcp-postgres

.PHONY: build lint vet test clean all release-local

all: lint build

build:
	go build -o $(BINARY) .

lint:
	golangci-lint run ./...

vet:
	go vet ./...

test:
	go test -race ./...

clean:
	rm -f $(BINARY)

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $(BINARY)-linux-amd64 .

build-all: build-linux build

release-local:
	goreleaser release --snapshot --clean
