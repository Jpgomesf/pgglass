BINARY := pgglass
MODULE := github.com/jpgomesf/pgglass

.PHONY: build lint vet test test-short clean all release-local bench bench-baseline

all: lint build

# Without cgo, like the release binaries (.goreleaser.yml).
build:
	CGO_ENABLED=0 go build -o $(BINARY) .

lint:
	golangci-lint run ./...

vet:
	go vet ./...

# Unit and integration tests. Integration tests run Postgres in Docker
# (image from PGGLASS_PG_IMAGE, default postgres:17-alpine) and fail without it.
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

# Token benchmark (docs/benchmark.md): fails when a response grows more than
# 10% past internal/bench/testdata/bench-baseline.json. Needs Docker.
bench:
	go test -race -count=1 -v ./internal/bench

# Rewrites the benchmark baseline and the table in docs/benchmark.md.
bench-baseline:
	go test -count=1 -run '^TestTokenBudget$$' -v ./internal/bench -update
