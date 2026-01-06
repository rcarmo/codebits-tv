
.PHONY: all build build-server build-proxy build-cli fmt test clean

all: build

build: build-server build-proxy build-cli

build-server:
	go build -o bin/server ./cmd/server

build-proxy:
	go build -o bin/proxy ./cmd/proxy

build-cli:
	go build -o bin/cli ./cmd/cli

fmt:
	gofmt -w .

test:
	go test ./...

clean:
	rm -rf bin

