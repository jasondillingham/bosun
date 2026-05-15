BINARY := bosun
PKG := github.com/jasondillingham/bosun/cmd/bosun
DIST := dist

.PHONY: build test test-race test-cover check vet tidy cross clean install demo help

help:
	@echo "Bosun — make targets:"
	@echo "  build       Build the binary for the host OS/arch"
	@echo "  test        Run unit tests"
	@echo "  test-race   Run tests with the race detector"
	@echo "  vet         Run go vet"
	@echo "  tidy        Run go mod tidy"
	@echo "  cross       Cross-compile to dist/ for darwin/linux/windows × amd64/arm64"
	@echo "  install     go install the binary into \$$GOPATH/bin"
	@echo "  clean       Remove dist/ + ./$(BINARY)"
	@echo "  demo        Run the interactive end-to-end demo in a sandbox"
	@echo "  demo-fast   Run the demo without pausing between steps"
	@echo "  check       vet + race tests + demo dry-run (run before commits)"

build:
	go build -o $(BINARY) ./cmd/bosun

test:
	go test ./... -count=1

test-race:
	go test -race ./... -count=1

test-cover:
	go test -race -coverprofile=coverage.out -covermode=atomic ./internal/... -count=1
	@go tool cover -func=coverage.out | tail -1

vet:
	go vet ./...

tidy:
	go mod tidy

install:
	go install $(PKG)

cross:
	mkdir -p $(DIST)
	GOOS=darwin  GOARCH=amd64 go build -o $(DIST)/$(BINARY)-darwin-amd64 ./cmd/bosun
	GOOS=darwin  GOARCH=arm64 go build -o $(DIST)/$(BINARY)-darwin-arm64 ./cmd/bosun
	GOOS=linux   GOARCH=amd64 go build -o $(DIST)/$(BINARY)-linux-amd64 ./cmd/bosun
	GOOS=linux   GOARCH=arm64 go build -o $(DIST)/$(BINARY)-linux-arm64 ./cmd/bosun
	GOOS=windows GOARCH=amd64 go build -o $(DIST)/$(BINARY)-windows-amd64.exe ./cmd/bosun
	GOOS=windows GOARCH=arm64 go build -o $(DIST)/$(BINARY)-windows-arm64.exe ./cmd/bosun
	@ls -la $(DIST)

clean:
	rm -rf $(DIST) ./$(BINARY)

demo: build
	@./examples/demo.sh

demo-fast: build
	@./examples/demo.sh --no-wait

# Full health check: vet, race tests, scenario coverage, and demo dry-run.
# Run this before committing anything non-trivial.
check:
	@printf '\n\033[1;36m▶ go vet\033[0m\n'
	@go vet ./...
	@printf '\n\033[1;36m▶ go test -race (incl. scenarios + scale)\033[0m\n'
	@go test -race -count=1 ./...
	@printf '\n\033[1;36m▶ demo dry-run\033[0m\n'
	@./examples/demo.sh --no-wait >/dev/null 2>&1 && echo "  demo: OK" || (echo "  demo: FAIL" && exit 1)
	@printf '\n\033[1;32m✓ all checks passed\033[0m\n'
