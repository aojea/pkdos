REPO_ROOT:=${CURDIR}
OUT_DIR=$(REPO_ROOT)/bin

# Go build settings
GO111MODULE=on
CGO_ENABLED=0
export GO111MODULE CGO_ENABLED

.PHONY: all build 
build: 
	@echo "Building all binaries..."
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o ./bin/krun .

clean:
	rm -rf "$(OUT_DIR)/"

test:
	CGO_ENABLED=1 go test -short -v -race -count 1 ./...

lint:
	hack/lint.sh

update:
	go mod tidy


.PHONY: ensure-buildx
ensure-buildx:
	./hack/init-buildx.sh

# get image name from directory we're building
IMAGE_NAME=krun-agent
# docker image registry, default to upstream
REGISTRY?=ghcr.io/aojea
# tag based on date-sha
TAG?=$(shell echo "$$(date +v%Y%m%d)-$$(git describe --always --dirty)")
PLATFORMS?=linux/amd64,linux/arm64
PROGRESS?=auto
# git branch to build CRIU from
CRIU_BRANCH?=v4.2
# required to enable buildx
export DOCKER_CLI_EXPERIMENTAL=enabled

image-build: ensure-buildx
	docker buildx build agent/ \
		--progress="${PROGRESS}" \
		--build-arg CRIU_BRANCH="${CRIU_BRANCH}" \
		--tag="$(REGISTRY)/$(IMAGE_NAME):$(TAG)" --load

image-push: ensure-buildx
	docker buildx build agent/ \
		--progress="${PROGRESS}" \
		--platform="${PLATFORMS}" \
		--build-arg CRIU_BRANCH="${CRIU_BRANCH}" \
		--tag="$(REGISTRY)/$(IMAGE_NAME):$(TAG)" --push

release: image-push
	@echo "Released image: $(REGISTRY)/$(IMAGE_NAME):$(TAG)"