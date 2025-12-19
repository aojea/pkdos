REPO_ROOT:=${CURDIR}
OUT_DIR=$(REPO_ROOT)/bin

# Go build settings
GO111MODULE=on
CGO_ENABLED=0
export GO111MODULE CGO_ENABLED

.PHONY: all build 

agent-fsync:
	@echo "Building agent-fsync..."
	GOARCH=amd64 go build -ldflags="-s -w" -o internal/assets/krun-agent-fsync-amd64 ./agent/fsync/
	GOARCH=arm64 go build -ldflags="-s -w" -o internal/assets/krun-agent-fsync-arm64 ./agent/fsync/

build: agent-fsync
	@echo "Building all binaries..."
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o ./bin/krun .

clean:
	rm -rf "$(OUT_DIR)/"

test:
	CGO_ENABLED=1 go test -short -v -race -count 1 ./...

lint:
	hack/lint.sh

update:
	gofmt -s -w .
	go mod tidy
