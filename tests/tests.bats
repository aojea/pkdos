#!/usr/bin/env bats

load 'common'

setup_file() {
    # Ensure the binary is built before running tests
    # We go up one level to the root to run make
    echo "Building krun binary..."
    (cd "$BATS_TEST_DIRNAME/.." && make build)
}

@test "krun executes command in parallel on StatefulSet pods" {
    local STS_NAME="krun-test-web"
    local NS="default"
    local LABEL="app=krun-test"

    # 1. Deploy a StatefulSet with 2 replicas
    # We use busybox because it's lightweight and has the 'hostname' command
    cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: ${STS_NAME}
  namespace: ${NS}
spec:
  selector:
    matchLabels:
      ${LABEL}
  serviceName: "krun-service"
  replicas: 2
  template:
    metadata:
      labels:
        ${LABEL}
    spec:
      terminationGracePeriodSeconds: 0
      containers:
      - name: main
        image: registry.k8s.io/e2e-test-images/busybox:1.29-2
        command: ["sleep", "3600"]
EOF

    # 2. Wait for both pods to be ready
    # 'wait_for_pod_ready' is defined in tests/common.bash
    wait_for_pod_ready "${NS}" "${STS_NAME}-0"
    wait_for_pod_ready "${NS}" "${STS_NAME}-1"

    # 3. Run krun
    # We assume the binary is located at ../bin/krun based on the Makefile
    run "$BATS_TEST_DIRNAME/../bin/krun" \
        --kubeconfig="$KUBECONFIG" \
        --namespace="${NS}" \
        --label-selector="${LABEL}" \
        --command="hostname"

    # Debug output if test fails
    echo "--- krun output ---"
    echo "$output"
    echo "-------------------"

    # 4. Assertions
    [ "$status" -eq 0 ]

    # Check that we received output from pod 0
    # Expected format: [krun-test-web-0] krun-test-web-0
    [[ "$output" == *"[${STS_NAME}-0] ${STS_NAME}-0"* ]]

    # Check that we received output from pod 1
    # Expected format: [krun-test-web-1] krun-test-web-1
    [[ "$output" == *"[${STS_NAME}-1] ${STS_NAME}-1"* ]]

    # 5. Cleanup
    kubectl delete statefulset "${STS_NAME}" --namespace="${NS}" --wait=false
}