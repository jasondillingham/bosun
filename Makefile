BINARY := bosun
PKG := github.com/jasondillingham/bosun/cmd/bosun
DIST := dist

.PHONY: build test test-race vet tidy cross clean install help

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

build:
	go build -o $(BINARY) ./cmd/bosun

test:
	go test ./... -count=1

test-race:
	go test -race ./... -count=1

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
