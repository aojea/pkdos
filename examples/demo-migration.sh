#!/bin/bash
set -e

POD_NAME="counter-pod"
NAMESPACE="default"
TARGET_IMAGE="ttl.sh/krun-migration-test:2h"

# 1. Cleanup previous runs
echo "🧹 Cleanup..."
kubectl delete pod "$POD_NAME" "$POD_NAME-migrated" -n "$NAMESPACE" --wait=false 2>/dev/null || true

# 2. Deploy Pod
echo "🚀 Deploying Pod..."
kubectl apply -f examples/counter-pod.yaml -n "$NAMESPACE"
kubectl wait --for=condition=Ready pod/"$POD_NAME" -n "$NAMESPACE" --timeout=60s

# 3. Wait for some state
echo "⏳ Accumlating state..."
sleep 5
echo "Current Logs:"
kubectl logs "$POD_NAME" -n "$NAMESPACE" | tail -n 3

# 4. Migrate
echo "📦 Migrating..."
# Assuming krun is in path or built
./krun migrate "$POD_NAME" "$TARGET_IMAGE" -n "$NAMESPACE" --keep-old

# 5. Validate
NEW_POD="$POD_NAME-migrated"
echo "🔍 Validating migration..."
kubectl wait --for=condition=Ready pod/"$NEW_POD" -n "$NAMESPACE" --timeout=60s

echo "Logs from NEW pod:"
kubectl logs "$NEW_POD" -n "$NAMESPACE" | head -n 5

echo "✅ Done! Check if counter continued from previous value."
