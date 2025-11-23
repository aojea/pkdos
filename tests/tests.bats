#!/usr/bin/env bats

load 'common'

setup_file() {
    # 1. Build the latest krun binary
    echo "Building krun binary..."
    (cd "$BATS_TEST_DIRNAME/.." && make build)

    # 2. Deploy a target app (Busybox ensures 'tar' is available)
    # Using a StatefulSet to get stable names (upload-test-0, upload-test-1)
    cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: upload-test
  namespace: default
spec:
  selector:
    matchLabels:
      app: upload-target
  serviceName: "upload-service"
  replicas: 2
  template:
    metadata:
      labels:
        app: upload-target
    spec:
      terminationGracePeriodSeconds: 0
      containers:
      - name: main
        image: registry.k8s.io/e2e-test-images/busybox:1.29-2
        command: ["sleep", "3600"]
EOF

    # 3. Wait for pods
    wait_for_pod_ready "default" "upload-test-0"
    wait_for_pod_ready "default" "upload-test-1"
}

teardown_file() {
    kubectl delete statefulset upload-test --namespace=default --wait=false
}

setup() {
    # Create a unique temp directory for each test case
    TEST_DIR=$(mktemp -d)
}

teardown() {
    rm -rf "$TEST_DIR"
}

@test "krun command on pods" {

    # Run krun hostname
    run "$BATS_TEST_DIRNAME/../bin/krun" \
        --kubeconfig="$KUBECONFIG" \
        --namespace="default" \
        --label-selector="app=upload-target" \
        --command="hostname"
    
    # Debug output if test fails
    echo "--- krun output ---"
    echo "$output"
    echo "-------------------"
    [ "$status" -eq 0 ]
    [[ "$output" == *"[upload-test-0] upload-test-0"* ]]
    [[ "$output" == *"[upload-test-1] upload-test-1"* ]]
}

@test "krun uploads a local folder to pods" {
    # 1. Prepare Local Files
    mkdir -p "$TEST_DIR/data"
    echo "Hello World" > "$TEST_DIR/data/file1.txt"
    echo "Config Data" > "$TEST_DIR/data/config.cfg"

    # 2. Run krun upload
    run "$BATS_TEST_DIRNAME/../bin/krun" \
        --kubeconfig="$KUBECONFIG" \
        --namespace="default" \
        --label-selector="app=upload-target" \
        --upload-src="$TEST_DIR/data" \
        --upload-dest="/tmp/remote-data"

    [ "$status" -eq 0 ]

    # 3. Verify content on Pod 0
    run kubectl exec -n default upload-test-0 -- cat /tmp/remote-data/file1.txt
    [ "$status" -eq 0 ]
    [[ "$output" == "Hello World" ]]

    # 4. Verify content on Pod 1 (Parallel check)
    run kubectl exec -n default upload-test-1 -- cat /tmp/remote-data/config.cfg
    [ "$status" -eq 0 ]
    [[ "$output" == "Config Data" ]]
}

@test "krun uploads a script and executes it immediately" {
    # 1. Create a local script
    mkdir -p "$TEST_DIR/scripts"
    cat <<EOF > "$TEST_DIR/scripts/myscript.sh"
#!/bin/sh
echo "I am running on \$(hostname)"
EOF
    chmod +x "$TEST_DIR/scripts/myscript.sh"

    # 2. Run krun: Upload -> Execute
    # Note: We upload to /tmp/bin and then execute the specific file
    run "$BATS_TEST_DIRNAME/../bin/krun" \
        --kubeconfig="$KUBECONFIG" \
        --namespace="default" \
        --label-selector="app=upload-target" \
        --upload-src="$TEST_DIR/scripts" \
        --upload-dest="/tmp/bin" \
        --command="/bin/sh /tmp/bin/myscript.sh"

    [ "$status" -eq 0 ]

    # 3. Assert Output from both pods
    [[ "$output" == *"[upload-test-0] I am running on upload-test-0"* ]]
    [[ "$output" == *"[upload-test-1] I am running on upload-test-1"* ]]
}

@test "krun excludes files and folders matching regex pattern" {
    # 1. Prepare Local Files
    # Structure:
    #   data/
    #    ├── keep.txt          (Should be kept)
    #    ├── ignore.log        (Should be excluded by extension)
    #    ├── secret/           (Should be excluded by directory name)
    #    │   └── key.pem
    #    └── subdir/
    #        └── keep_sub.txt  (Should be kept)

    mkdir -p "$TEST_DIR/data/secret"
    mkdir -p "$TEST_DIR/data/subdir"
    
    echo "Keep this" > "$TEST_DIR/data/keep.txt"
    echo "Ignore this" > "$TEST_DIR/data/ignore.log"
    echo "Secret Key" > "$TEST_DIR/data/secret/key.pem"
    echo "Keep sub" > "$TEST_DIR/data/subdir/keep_sub.txt"

    # 2. Run krun with exclude pattern
    # We use a regex that matches either .log extension OR the secret directory
    # Regex: \.log$|secret
    run "$BATS_TEST_DIRNAME/../bin/krun" \
        --kubeconfig="$KUBECONFIG" \
        --namespace="default" \
        --label-selector="app=upload-target" \
        --upload-src="$TEST_DIR/data" \
        --upload-dest="/tmp/exclude_test" \
        --exclude="\.log$|secret"

    [ "$status" -eq 0 ]

    # 3. Verify 'keep.txt' EXISTS
    run kubectl exec -n default upload-test-0 -- cat /tmp/exclude_test/keep.txt
    [ "$status" -eq 0 ]
    [[ "$output" == "Keep this" ]]

    # 4. Verify 'subdir/keep_sub.txt' EXISTS (nested file check)
    run kubectl exec -n default upload-test-0 -- cat /tmp/exclude_test/subdir/keep_sub.txt
    [ "$status" -eq 0 ]
    [[ "$output" == "Keep sub" ]]

    # 5. Verify 'ignore.log' does NOT exist
    # 'ls' returns exit code 1 if the file is missing
    run kubectl exec -n default upload-test-0 -- ls /tmp/exclude_test/ignore.log
    [ "$status" -ne 0 ]
    [[ "$output" == *"No such file"* ]]

    # 6. Verify 'secret' directory does NOT exist
    run kubectl exec -n default upload-test-0 -- ls /tmp/exclude_test/secret
    [ "$status" -ne 0 ]
    [[ "$output" == *"No such file"* ]]
}