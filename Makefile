BINARY := bosun
PKG := github.com/jasondillingham/bosun/cmd/bosun
DIST := dist

# VERSION is injected into the binary via -ldflags so `bosun --version`
# reports something meaningful. `git describe --tags --always --dirty`
# yields e.g. v0.10.0, v0.10.0-7-gc2785aa, or v0.10.0-7-gc2785aa-dirty.
# Falls back to "dev" outside a git checkout (e.g. tarball builds).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test test-race test-cover check vet tidy cross clean install demo help fuzz stress fmt fmt-check lint install-hooks

# How long each fuzz target runs when invoked via `make fuzz`. Override
# with `FUZZTIME=5m make fuzz` for deeper sessions. The Go fuzzer keeps
# any new failing inputs in testdata/fuzz/<target>/ so a regression
# stays caught even after the time-bounded run completes.
FUZZTIME ?= 30s

help:
	@echo "Bosun — make targets:"
	@echo "  build         Build the binary for the host OS/arch"
	@echo "  test          Run unit tests"
	@echo "  test-race     Run tests with the race detector"
	@echo "  vet           Run go vet"
	@echo "  fmt           Auto-fix gofmt drift across the tree"
	@echo "  fmt-check     Report gofmt drift; non-zero exit if any"
	@echo "  lint          Run golangci-lint (requires the binary installed)"
	@echo "  tidy          Run go mod tidy"
	@echo "  cross         Cross-compile to dist/ for darwin/linux/windows × amd64/arm64"
	@echo "  install       go install the binary into \$$GOPATH/bin"
	@echo "  install-hooks Wire .githooks/ as the repo's hooks dir (one-time)"
	@echo "  clean         Remove dist/ + ./$(BINARY)"
	@echo "  demo          Run the interactive end-to-end demo in a sandbox"
	@echo "  demo-fast     Run the demo without pausing between steps"
	@echo "  check         fmt-check + vet + race tests + demo dry-run (run before commits)"
	@echo "  fuzz          Run every Fuzz* target for FUZZTIME each (default 30s)"
	@echo "  stress        Run stress + concurrency tests (no -short)"

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/bosun

test:
	go test ./... -count=1

test-race:
	go test -race ./... -count=1

test-cover:
	go test -race -coverprofile=coverage.out -covermode=atomic ./internal/... -count=1
	@go tool cover -func=coverage.out | tail -1

vet:
	go vet ./...

# `go list -f '{{.Dir}}' ./...` is the canonical way to enumerate the
# Go-toolchain-visible directories — vendor/, testdata/, and dot-prefixed
# dirs (.claude/, .git/) are excluded automatically, so the surface
# matches CI's golangci-lint scope.
fmt:
	@gofmt -w $$(go list -f '{{.Dir}}' ./...)

fmt-check:
	@drift=$$(gofmt -l $$(go list -f '{{.Dir}}' ./...)); \
	if [ -n "$$drift" ]; then \
		printf 'gofmt drift in:\n%s\n\nRun `make fmt` to fix.\n' "$$drift" >&2; \
		exit 1; \
	fi

# Optional: only runs if golangci-lint is on PATH. CI is the authoritative
# lint gate; this target is for local pre-push validation if you have the
# binary installed.
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run --timeout=5m; \
	else \
		echo "golangci-lint not installed; skipping (CI will still run it)"; \
	fi

# One-time setup: point this clone's hook dir at the tracked .githooks/.
# Safe to re-run; idempotent. See .githooks/pre-commit for what's enforced.
install-hooks:
	@git config core.hooksPath .githooks
	@echo "hooks installed → $$(git config --get core.hooksPath)"

tidy:
	go mod tidy

install:
	go install $(PKG)

cross:
	mkdir -p $(DIST)
	GOOS=darwin  GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-amd64 ./cmd/bosun
	GOOS=darwin  GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-arm64 ./cmd/bosun
	GOOS=linux   GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-amd64 ./cmd/bosun
	GOOS=linux   GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-arm64 ./cmd/bosun
	GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-windows-amd64.exe ./cmd/bosun
	GOOS=windows GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(DIST)/$(BINARY)-windows-arm64.exe ./cmd/bosun
	@ls -la $(DIST)

clean:
	rm -rf $(DIST) ./$(BINARY)

demo: build
	@./examples/demo.sh

demo-fast: build
	@./examples/demo.sh --no-wait

# Run every Fuzz* target for FUZZTIME each. Go fuzz is per-package
# (only one Fuzz* can run at a time per `go test` invocation), so we
# enumerate packages explicitly. Failing corpus entries land in
# testdata/fuzz/<target>/ and get replayed on every subsequent run.
#
# Use:
#   make fuzz                       # default 30s per target
#   FUZZTIME=5m make fuzz           # deeper run before a release
#   FUZZTIME=1h make fuzz           # overnight session
fuzz:
	@for pkg in \
		./internal/brief/ \
		./internal/predict/ \
		./internal/session/ \
		./internal/phantom/ \
		./internal/preflight/; do \
		echo ""; \
		printf '\033[1;36m▶ fuzz %s (%s)\033[0m\n' $$pkg "$(FUZZTIME)"; \
		funcs=$$(grep -h '^func Fuzz' $$pkg*_test.go 2>/dev/null | awk '{print $$2}' | sed 's/(.*//') ; \
		for fn in $$funcs; do \
			echo "  $$fn"; \
			go test -run=^$$ -fuzz=^$$fn$$ -fuzztime=$(FUZZTIME) $$pkg || exit 1; \
		done; \
	done

# Stress + concurrency tests. Skipped under -short; some sleep ~hundred
# ms wall-clock to surface races that wouldn't fire under a couple of
# goroutines. Run periodically (weekly?) to catch concurrency drift.
# Pattern intentionally broad: any test whose name includes Stress,
# Concurrent, Serializ(es|e), or NoTear qualifies.
stress:
	@printf '\n\033[1;36m▶ stress + race tests\033[0m\n'
	go test -race -count=1 -run 'Stress|Concurrent|Serializ|NoTear|NoGoroutineLeak' \
		./internal/claims/ \
		./internal/state/ \
		./internal/lockfile/ \
		./internal/mcp/

# Full health check: gofmt, vet, race tests, scenario coverage, and demo
# dry-run. Run this before committing anything non-trivial. gofmt runs
# first because it's the cheapest gate and the failure mode that
# regressed twice in the v0.11 round — catching it before the heavier
# checks is the point.
check:
	@printf '\n\033[1;36m▶ gofmt drift\033[0m\n'
	@drift=$$(gofmt -l $$(go list -f '{{.Dir}}' ./...)); \
	if [ -n "$$drift" ]; then \
		printf '  drift in:\n%s\n  run `make fmt` to fix\n' "$$drift" >&2; \
		exit 1; \
	fi
	@printf '\n\033[1;36m▶ go vet\033[0m\n'
	@go vet ./...
	@printf '\n\033[1;36m▶ go test -race (incl. scenarios + scale)\033[0m\n'
	@go test -race -count=1 ./...
	@printf '\n\033[1;36m▶ demo dry-run\033[0m\n'
	@./examples/demo.sh --no-wait >/dev/null 2>&1 && echo "  demo: OK" || (echo "  demo: FAIL" && exit 1)
	@printf '\n\033[1;32m✓ all checks passed\033[0m\n'
