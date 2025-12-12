#!/usr/bin/env bats

setup() {
    # Generate a unique ID for this test run to ensure isolation
    export RUN_ID="test-$RANDOM"
    export NS="ns-$RUN_ID"
    
    echo "--- SETUP: Creating Namespace $NS ---"
    kubectl create ns "$NS"
}

teardown() {
    echo "--- TEARDOWN: Deleting Namespace $NS ---"
    kubectl delete ns "$NS" --wait=false
}

# ==============================================================================
# TEST CASE 1: Simple Memory Counter
# Verifies that a shell variable loop is preserved.
# ==============================================================================
@test "Independent: Migrate Process Memory (Shell Counter)" {
    local POD_NAME="counter-$RUN_ID"
    local IMAGE="ttl.sh/mig-test-counter-$RUN_ID:1h"

    # 1. Start workload
    kubectl run "$POD_NAME" -n "$NS" \
        --image=alpine:latest \
        --restart=Never \
        -- /bin/sh -c 'i=0; while true; do echo "MEM=$i"; i=$((i+1)); sleep 1; done'

    kubectl wait --for=condition=Ready pod/"$POD_NAME" -n "$NS" --timeout=60s

    # 2. Accumulate State
    echo "Waiting for counter > 5..."
    for i in {1..20}; do
        VAL=$(kubectl logs "$POD_NAME" -n "$NS" | tail -n1 | cut -d= -f2)
        if [[ "$VAL" -ge 5 ]]; then break; fi
        sleep 1
    done

    # 3. Migrate
    run krun "$POD_NAME" "$IMAGE" -n "$NS" --keep-old
    [ "$status" -eq 0 ]

    # 4. Verify New Pod
    local NEW_POD="$POD_NAME-migrated"
    kubectl wait --for=condition=Ready pod/"$NEW_POD" -n "$NS" --timeout=120s
    
    # Check if memory state continued (should be >= 5, not reset to 0)
    sleep 2
    FIRST_LOG=$(kubectl logs "$NEW_POD" -n "$NS" | head -n 1)
    NEW_VAL=$(echo "$FIRST_LOG" | cut -d= -f2)
    
    echo "Old Pod stopped at: $VAL"
    echo "New Pod started at: $NEW_VAL"
    
    [ "$NEW_VAL" -ge "$VAL" ]
}

# ==============================================================================
# TEST CASE 2: TCP Service State
# Verifies a Client -> Service -> Pod connection where the backend holds RAM state.
# ==============================================================================
@test "Independent: Migrate TCP Service (Preserve RAM State)" {
    local APP_NAME="tcp-server-$RUN_ID"
    local IMAGE="ttl.sh/mig-test-tcp-$RUN_ID:1h"
    
    # 1. Create a Python TCP Server ConfigMap
    # This script stores messages in a GLOBAL LIST (RAM Only)
    kubectl create configmap server-script -n "$NS" --from-literal=server.py='
import socket
import sys

# Simple TCP Server that keeps state in memory
memory = []
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.bind(("0.0.0.0", 8080))
s.listen(5)
print("Listening on 8080...")
sys.stdout.flush()

while True:
    conn, addr = s.accept()
    data = conn.recv(1024).decode().strip()
    if not data:
        continue
        
    print(f"Received: {data}")
    
    if data == "DUMP":
        # Return all state
        resp = ",".join(memory)
        conn.sendall(resp.encode())
    else:
        # Store state in RAM
        memory.append(data)
        conn.sendall(b"ACK")
    
    conn.close()
'

    # 2. Deploy the Stateful Backend
    cat <<EOF | kubectl apply -n "$NS" -f -
apiVersion: v1
kind: Pod
metadata:
  name: $APP_NAME
  labels:
    app: $APP_NAME
spec:
  containers:
  - name: python-server
    image: python:3.9-slim
    command: ["python", "-u", "/scripts/server.py"]
    ports: [{containerPort: 8080}]
    volumeMounts:
    - name: script
      mountPath: /scripts
  volumes:
  - name: script
    configMap:
      name: server-script
EOF

    # 3. Create Service
    kubectl expose pod "$APP_NAME" -n "$NS" --port=80 --target-port=8080 --name=my-service

    # Wait for backend
    kubectl wait --for=condition=Ready pod/"$APP_NAME" -n "$NS" --timeout=60s

    # 4. Start a Client Pod
    kubectl run client -n "$NS" --image=alpine:latest --restart=Never -- sleep 3600
    kubectl wait --for=condition=Ready pod/client -n "$NS" --timeout=60s

    # 5. CLIENT ACTION: Send Data (State Creation)
    # We send "DATA_1" and "DATA_2". This is stored in the Python process RAM.
    echo "Sending data to service..."
    kubectl exec client -n "$NS" -- sh -c "echo 'DATA_1' | nc my-service 80"
    kubectl exec client -n "$NS" -- sh -c "echo 'DATA_2' | nc my-service 80"

    # Verify State exists
    STATE_BEFORE=$(kubectl exec client -n "$NS" -- sh -c "echo 'DUMP' | nc my-service 80")
    echo "State Before Migration: $STATE_BEFORE"
    [[ "$STATE_BEFORE" == *"DATA_1,DATA_2"* ]]

    # 6. MIGRATE THE BACKEND
    # The IP will change. The Pod name will change. The Service IP stays the same.
    echo "Migrating Backend..."
    run krun "$APP_NAME" "$IMAGE" -n "$NS" --keep-old
    [ "$status" -eq 0 ]

    # 7. CLIENT ACTION: Reconnect (State Verification)
    # The client connects to the SAME Service DNS (my-service).
    # It should hit the NEW pod (because the old one is deleted or labels switched).
    # Since we used --keep-old in the script but the script updates the OLD pod labels or deletes it?
    # Wait, the script creates "pod-migrated". We need to make sure the Service selects the NEW pod.
    # Our script copies labels? Yes, 'kubectl get pod -o json' copies labels.
    # But if we use --keep-old, BOTH pods match the service.
    # To test strictly, we should manually delete the old pod OR rely on the script default (delete old).
    
    # Let's clean up the old pod manually to ensure traffic goes to new one
    kubectl delete pod "$APP_NAME" -n "$NS" --wait=true

    local NEW_POD="$APP_NAME-migrated"
    kubectl wait --for=condition=Ready pod/"$NEW_POD" -n "$NS" --timeout=120s

    # 8. Re-query State
    echo "Querying new backend..."
    STATE_AFTER=$(kubectl exec client -n "$NS" -- sh -c "echo 'DUMP' | nc my-service 80")
    echo "State After Migration: $STATE_AFTER"

    # ASSERT: The Python process RAM (the list) should still contain the data.
    [[ "$STATE_AFTER" == *"DATA_1,DATA_2"* ]]
}