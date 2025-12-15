# Stage 1: Build CRIU from source
FROM debian:bookworm AS criu-builder

ARG CRIU_BRANCH=master

# Install build dependencies (Comprehensive list for CRIU on Debian)
RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    build-essential \
    pkg-config \
    libprotobuf-dev \
    libprotobuf-c-dev \
    protobuf-c-compiler \
    protobuf-compiler \
    python3-protobuf \
    libcap-dev \
    libnl-3-dev \
    libnet-dev \
    libaio-dev \
    libgnutls28-dev \
    libbsd-dev \
    libnftables-dev \
    libdrm-dev \
    uuid-dev \
    asciidoc \
    xmlto \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Clone and build CRIU
WORKDIR /tmp/criu
RUN git clone --depth 1 --branch ${CRIU_BRANCH} https://github.com/checkpoint-restore/criu.git . \
    && make -j$(nproc) \
    && make install PREFIX=/usr/local

# Identify and copy shared libraries
# We use ldd to find all dependencies of the criu binary, then copy them to a staging directory.
# We AND realpath to ensure we follow symlinks (e.g. /lib -> /usr/lib) to prevent "cannot copy to non-directory" errors in the final stage.
WORKDIR /deps_root
RUN mkdir -p /deps_root/usr/local/sbin \
    && cp /usr/local/sbin/criu /deps_root/usr/local/sbin/ \
    && ldd /usr/local/sbin/criu | awk '{ if ($3 ~ "^/") print $3; else if ($1 ~ "^/") print $1 }' \
    | xargs -I '{}' realpath '{}' \
    | sort | uniq \
    | xargs -I '{}' cp -v --parents '{}' /deps_root/

# Stage 2: Build Krun Agent
FROM golang:1.25-bookworm AS krun-builder

WORKDIR /go/src/github.com/aojea/krun/agent
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -v -o /krun-agent .

# Stage 3: Final Runtime Image
FROM debian:bookworm

# Copy shared libraries and binaries from criu-builder
COPY --from=criu-builder /deps_root /

# Copy Krun binary
COPY --from=krun-builder /krun-agent /usr/local/bin/krun-agent

# Install minimal runtime tools (iproute2/nftables might still be needed for functionality, not just linking)
RUN apt-get update && apt-get install -y --no-install-recommends \
    iproute2 \
    nftables \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENTRYPOINT ["/usr/local/bin/krun-agent"]
