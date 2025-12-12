#!/bin/bash

set -eu

function setup_suite {
  export BATS_TEST_TIMEOUT=120
  echo "Building krun binary..."
  (cd "$BATS_TEST_DIRNAME/.." && make build)

  # Define the name of the kind cluster
  export CLUSTER_NAME="test-cluster"
  if kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
    echo "Kind cluster ${CLUSTER_NAME} already exists. Skipping creation."
    kind get kubeconfig --name "$CLUSTER_NAME" > "$BATS_SUITE_TMPDIR/kubeconfig"
    export KUBECONFIG="$BATS_SUITE_TMPDIR/kubeconfig"
    return
  fi
  # Create cluster
  kind create cluster --config "$BATS_TEST_DIRNAME/kind.yaml" --name "$CLUSTER_NAME" --wait 1m --retain

  echo "Building krun image..."
  (cd "$BATS_TEST_DIRNAME/.." && TAG=test make image-build)

  echo "Pushing krun image..."
  kind load docker-image "ghcr.io/aojea/krun-agent:$TAG" --name "$CLUSTER_NAME"
}

function teardown_suite {
    rm -rf "$BATS_TEST_DIRNAME"/../_artifacts || true
    kind export logs "$BATS_TEST_DIRNAME"/../_artifacts --name "$CLUSTER_NAME"
    kind delete cluster --name "$CLUSTER_NAME"
}
