VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/Muriano/OctoGuard/internal/cli.Version=$(VERSION)
# Go tooling skips testdata/ in ./..., so the fake GitHub server is listed explicitly.
PKGS := ./... ./testdata/fakegh/...

.PHONY: build test lint ci

build:
	go build -ldflags "$(LDFLAGS)" -o bin/octoguard ./cmd/octoguard

test:
	go vet $(PKGS)
	go test -race $(PKGS)

lint:
	golangci-lint run $(PKGS)

ci: test lint
