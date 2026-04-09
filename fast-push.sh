#!/bin/bash
set -e

# Default to proxy-gateway if no argument is provided
TARGET_APP=${1:-proxy-gateway}

# Determine the correct container name based on the target app
if [ "$TARGET_APP" == "proxy-gateway" ]; then
    CONTAINER_NAME="proxy-debug"
else
    # Injected sidecars use this container name
    CONTAINER_NAME="ananse-proxy"
fi

echo "🔨 Compiling proxy binary for Linux (with debug flags)..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -gcflags="all=-N -l" -o bin/server-linux ./proxy/main.go

echo "🔍 Finding pods for app=$TARGET_APP..."
PODS=$(kubectl get pods -n ananse -l app=$TARGET_APP -o jsonpath='{.items[*].metadata.name}')

if [ -z "$PODS" ]; then
    echo "❌ No pods found for app=$TARGET_APP!"
    exit 1
fi

for POD in $PODS; do
    echo "🚚 Pushing binary to $POD (container: $CONTAINER_NAME)..."
    
    # 1. Copy the new binary to a temporary path
    kubectl cp bin/server-linux ananse/$POD:/tmp/server-update -c $CONTAINER_NAME
    
    # 2. Hot-swap the binary and restart the process inside the container
    # The 'while true' loop in the pod will instantly catch the new binary and start it
    echo "♻️  Restarting proxy process in $POD..."
    kubectl exec -n ananse $POD -c $CONTAINER_NAME -- sh -c '
        mv /tmp/server-update /server && 
        chmod +x /server && 
        killall dlv || killall server || true
    '
done

echo "✅ Hot-swap complete for $TARGET_APP!"