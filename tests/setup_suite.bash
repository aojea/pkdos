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
  cat <<EOF | kind create cluster \
    --name $CLUSTER_NAME \
    -v7 --wait 1m --retain --config=-
  kind: Cluster
  apiVersion: kind.x-k8s.io/v1alpha4
  networking:
    ipFamily: dual
  nodes:
  - role: control-plane
  - role: worker
  - role: worker
EOF
}

function teardown_suite {
    rm -rf "$BATS_TEST_DIRNAME"/../_artifacts || true
    kind export logs "$BATS_TEST_DIRNAME"/../_artifacts --name "$CLUSTER_NAME"
    kind delete cluster --name "$CLUSTER_NAME"
}
