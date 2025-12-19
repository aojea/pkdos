#!/usr/bin/env bats

load 'common'

setup_file() {
    # Install latest jobset
    echo "Installing jobset version v0.10.1..."
    JOBSET_VERSION=v0.10.1
    kubectl apply --server-side -f https://github.com/kubernetes-sigs/jobset/releases/download/$JOBSET_VERSION/manifests.yaml
    
    echo "Waiting for JobSet controller..."
    kubectl wait --for=condition=Available deployment/jobset-controller-manager -n jobset-system --timeout=120s

    sleep 3
    # Deploy a target workload (JobSet)
    cat <<EOF | kubectl apply -f -
apiVersion: jobset.x-k8s.io/v1alpha2
kind: JobSet
metadata:
  name: upload-test
  namespace: default
spec:
  replicatedJobs:
  - name: worker
    replicas: 1
    template:
      spec:
        parallelism: 2
        completions: 2
        backoffLimit: 0
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

    echo "Waiting for pods..."
    kubectl wait --for=condition=Ready pods -l jobset.sigs.k8s.io/jobset-name=upload-test -n default --timeout=120s

    # --- DYNAMIC POD DISCOVERY ---
    # fetch pod names sorted by name (ensures 0-0 comes before 0-1)
    # output format will be space separated: "upload-test-worker-0-0-abcde upload-test-worker-0-1-fghij"
    POD_NAMES=($(kubectl get pods -l jobset.sigs.k8s.io/jobset-name=upload-test -n default --sort-by=.metadata.name -o jsonpath='{.items[*].metadata.name}'))

    # Export these so individual @test functions can see them
    export POD_0="${POD_NAMES[0]}"
    export POD_1="${POD_NAMES[1]}"

    echo "Discovered Pod 0: $POD_0"
    echo "Discovered Pod 1: $POD_1"
}

teardown_file() {
    kubectl delete jobset upload-test --namespace=default --wait=false
}

setup() {
    # Create a unique temp directory for each test case
    TEST_DIR=$(mktemp -d)
}

teardown() {
    rm -rf "$TEST_DIR"
}

@test "krun jobsetcommand on pods" {

    # Run krun hostname
    run "$BATS_TEST_DIRNAME/../bin/krun" jobset run \
        --kubeconfig="$KUBECONFIG" \
        --namespace="default" \
        --name="upload-test" \
        -- hostname
    
    # Debug output if test fails
    echo "--- krun output ---"
    echo "$output"
    echo "-------------------"
    [ "$status" -eq 0 ]
    
    # Verify the output contains the ACTUAL pod names we discovered
    [[ "$output" == *upload-test-worker-0-0* ]]
    [[ "$output" == *upload-test-worker-0-1* ]]
}

@test "krun jobset uploads a local folder to pods" {
    # Prepare Local Files
    mkdir -p "$TEST_DIR/data"
    echo "Hello World" > "$TEST_DIR/data/file1.txt"
    echo "Config Data" > "$TEST_DIR/data/config.cfg"

    # Run krun upload
    run "$BATS_TEST_DIRNAME/../bin/krun" jobset run \
        --kubeconfig="$KUBECONFIG" \
        --namespace="default" \
        --name="upload-test" \
        --upload-src="$TEST_DIR/data" \
        --upload-dest="/tmp/remote-data"

    [ "$status" -eq 0 ]

    # 3. Verify content on Pod 0 (using dynamic name)
    run kubectl exec -n default "$POD_0" -- cat /tmp/remote-data/file1.txt
    [ "$status" -eq 0 ]
    [[ "$output" == "Hello World" ]]

    # 4. Verify content on Pod 1 (using dynamic name)
    run kubectl exec -n default "$POD_1" -- cat /tmp/remote-data/config.cfg
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
    run "$BATS_TEST_DIRNAME/../bin/krun" jobset run \
        --kubeconfig="$KUBECONFIG" \
        --namespace="default" \
        --name="upload-test" \
        --upload-src="$TEST_DIR/scripts" \
        --upload-dest="/tmp/bin" \
        -- /bin/sh -c "/tmp/bin/myscript.sh"

    [ "$status" -eq 0 ]

    # 3. Assert Output from both pods contains their dynamic hostnames
    [[ "$output" == *"I am running on upload-test-worker-0-0"* ]]
    [[ "$output" == *"I am running on upload-test-worker-0-1"* ]]
}

@test "krun excludes files and folders matching regex pattern" {
    # 1. Prepare Local Files
    mkdir -p "$TEST_DIR/data/secret"
    mkdir -p "$TEST_DIR/data/subdir"
    
    echo "Keep this" > "$TEST_DIR/data/keep.txt"
    echo "Ignore this" > "$TEST_DIR/data/ignore.log"
    echo "Secret Key" > "$TEST_DIR/data/secret/key.pem"
    echo "Keep sub" > "$TEST_DIR/data/subdir/keep_sub.txt"

    # 2. Run krun with exclude pattern
    run "$BATS_TEST_DIRNAME/../bin/krun" jobset run \
        --kubeconfig="$KUBECONFIG" \
        --namespace="default" \
        --name="upload-test" \
        --upload-src="$TEST_DIR/data" \
        --upload-dest="/tmp/exclude_test" \
        --exclude="\.log$|secret"

    [ "$status" -eq 0 ]

    # 3. Verify 'keep.txt' EXISTS on Pod 0
    run kubectl exec -n default "$POD_0" -- cat /tmp/exclude_test/keep.txt
    [ "$status" -eq 0 ]
    [[ "$output" == "Keep this" ]]

    # 4. Verify 'subdir/keep_sub.txt' EXISTS
    run kubectl exec -n default "$POD_0" -- cat /tmp/exclude_test/subdir/keep_sub.txt
    [ "$status" -eq 0 ]
    [[ "$output" == "Keep sub" ]]

    # 5. Verify 'ignore.log' does NOT exist
    run kubectl exec -n default "$POD_0" -- ls /tmp/exclude_test/ignore.log
    [ "$status" -ne 0 ]
    [[ "$output" == *"No such file"* ]]

    # 6. Verify 'secret' directory does NOT exist
    run kubectl exec -n default "$POD_0" -- ls /tmp/exclude_test/secret
    [ "$status" -ne 0 ]
    [[ "$output" == *"No such file"* ]]
}

@test "krun jobset with deletes extraneous files" {
    # 1. Prepare Local Files
    mkdir -p "$TEST_DIR/data_mirror"
    echo "Mirror File" > "$TEST_DIR/data_mirror/mirror.txt"

    # 2. Pre-create extraneous file and dir on Pod 0
    run kubectl exec -n default "$POD_0" -- sh -c "mkdir -p /tmp/mirror_test/extra_dir && echo 'Extra' > /tmp/mirror_test/extra.txt"
    [ "$status" -eq 0 ]

    # 3. Run krun
    run "$BATS_TEST_DIRNAME/../bin/krun" jobset run \
        --kubeconfig="$KUBECONFIG" \
        --namespace="default" \
        --name="upload-test" \
        --upload-src="$TEST_DIR/data_mirror" \
        --upload-dest="/tmp/mirror_test"

    [ "$status" -eq 0 ]

    # 4. Verify 'mirror.txt' exists
    run kubectl exec -n default "$POD_0" -- cat /tmp/mirror_test/mirror.txt
    [ "$status" -eq 0 ]
    [[ "$output" == "Mirror File" ]]

    # 5. Verify 'extra.txt' is DELETED
    run kubectl exec -n default "$POD_0" -- ls /tmp/mirror_test/extra.txt
    [ "$status" -ne 0 ]
    [[ "$output" == *"No such file"* ]]

    # 6. Verify 'extra_dir' is DELETED
    run kubectl exec -n default "$POD_0" -- ls -d /tmp/mirror_test/extra_dir
    [ "$status" -ne 0 ]
    [[ "$output" == *"No such file"* ]]
}

@test "krun jobset --shell flag enables shell command piping" {
    # Test that --shell flag allows running commands with && and other shell features
    # This tests the fix for issue #18: "Piping commands together doesn't work"
    run "$BATS_TEST_DIRNAME/../bin/krun" jobset run \
        --kubeconfig="$KUBECONFIG" \
        --namespace="default" \
        --name="upload-test" \
        --shell \
        -- "echo hello && echo world"

    # Debug output if test fails
    echo "--- krun output ---"
    echo "$output"
    echo "-------------------"
    [ "$status" -eq 0 ]
    
    # Verify both echo commands executed (output from both pods)
    [[ "$output" == *"hello"* ]]
    [[ "$output" == *"world"* ]]
}

@test "krun jobset --shell flag enables cd command" {
    # Test that --shell flag allows using cd (a shell builtin)
    # This tests another scenario from issue #18
    
    run "$BATS_TEST_DIRNAME/../bin/krun" jobset run \
        --kubeconfig="$KUBECONFIG" \
        --namespace="default" \
        --name="upload-test" \
        --shell \
        -- "cd /tmp && pwd"
    
    # Debug output if test fails
    echo "--- krun output ---"
    echo "$output"
    echo "-------------------"
    [ "$status" -eq 0 ]
    
    # Verify pwd shows /tmp (output from both pods)
    [[ "$output" == *"/tmp"* ]]
}