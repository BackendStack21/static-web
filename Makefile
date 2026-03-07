.PHONY: build run test bench lint precompress clean release install commit bump changelog benchmark benchmark-keep benchmark-down

# Binary output path and name
BIN     := bin/static-web

# Version info injected at link time via -ldflags.
# Falls back to "dev" / "none" / "unknown" when git is unavailable.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X github.com/BackendStack21/static-web/internal/version.Version=$(VERSION) \
           -X github.com/BackendStack21/static-web/internal/version.Commit=$(COMMIT) \
           -X github.com/BackendStack21/static-web/internal/version.Date=$(DATE)

## build: compile the binary to bin/static-web (with version info)
build:
	go build -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/static-web

## release: compile an optimised, stripped binary for distribution
release:
	go build -ldflags="-s -w $(LDFLAGS)" -o $(BIN) ./cmd/static-web

## install: install the binary to $(GOPATH)/bin with version info
install:
	go install -ldflags="$(LDFLAGS)" ./cmd/static-web

## run: run the server directly (no binary output)
run:
	go run -ldflags="$(LDFLAGS)" ./cmd/static-web

## test: run all unit tests
test:
	go test ./...

## bench: run all benchmarks with memory profiling
bench:
	go test -bench=. -benchmem ./...

## lint: run go vet
lint:
	go vet ./...

## precompress: gzip and brotli compress all files in ./public
precompress:
	@echo "Pre-compressing files in ./public ..."
	@find ./public -type f \
		! -name "*.gz" ! -name "*.br" \
		| while read f; do \
			if command -v gzip >/dev/null 2>&1; then \
				gzip -k -f "$$f" && echo "  gzip: $$f.gz"; \
			fi; \
			if command -v brotli >/dev/null 2>&1; then \
				brotli -f "$$f" -o "$$f.br" && echo "  brotli: $$f.br"; \
			fi; \
		done
	@echo "Done."

## clean: remove build artifacts
clean:
	rm -rf bin/

## commit: interactive conventional commit via commitizen
commit:
	cz commit

## bump: bump version tag + update CHANGELOG.md
bump:
	cz bump

## changelog: regenerate CHANGELOG.md without bumping
changelog:
	cz changelog

## benchmark: build containers and run the full benchmark suite (tears down when done)
benchmark:
	@bash benchmark/bench.sh

## benchmark-keep: same as benchmark but leaves containers running afterwards
benchmark-keep:
	@bash benchmark/bench.sh -k

## benchmark-down: tear down any running benchmark containers
benchmark-down:
	docker compose -f benchmark/docker-compose.benchmark.yml down --remove-orphans
