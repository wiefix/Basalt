VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

PKG     := github.com/wiefix/basalt/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)
GOFLAGS := -trimpath
export CGO_ENABLED := 0

.PHONY: build build-linux test lint run clean web web-dev

build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/basalt ./cmd/basalt

build-linux:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/basalt-linux-amd64 ./cmd/basalt
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/basalt-linux-arm64 ./cmd/basalt

test:
	CGO_ENABLED=1 go test -race ./...

lint:
	gofmt -l . | (! grep .) || (echo "run gofmt"; exit 1)
	go vet ./...

run: build
	./bin/basalt server --data-dir .data --log-level debug --insecure-cookies --agent-listen 127.0.0.1:8443 --agent-url https://127.0.0.1:8443

web:
	cd web && npm ci && npm run build

# Vite dev server with hot reload; proxies /api to `make run` on :8080.
web-dev:
	cd web && npm run dev

clean:
	rm -rf bin .data
