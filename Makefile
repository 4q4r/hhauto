# hhauto — build, test, lint, release (Task 15).

BINARY  := hhauto
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build test test-fast lint snapshot release clean

## build: static binary (CGO disabled) with release-stamped version metadata.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/hhauto

## test: full suite with the race detector, two packages at a time.
test:
	go test -race -p 2 ./...

## test-fast: suite without the race detector.
test-fast:
	go test -p 2 ./...

## lint: gofmt (report must be empty), go vet, golangci-lint when installed.
lint:
	go vet ./...
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "gofmt -l reported:"; echo "$$out"; exit 1; fi
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; \
	else echo "golangci-lint not installed: skipped (vet+gofmt still enforced)"; fi

## snapshot: local multi-target build without publishing (goreleaser).
snapshot:
	CGO_ENABLED=0 goreleaser build --snapshot --clean

## release: publish a release (goreleaser; requires GITHUB_TOKEN and a v* tag).
release:
	CGO_ENABLED=0 goreleaser release --clean

## clean: remove build and release artifacts.
clean:
	rm -rf dist $(BINARY) $(BINARY).exe
